package gateway

// The sync policy on the gateway side: what leaves the facility.
//
//	full        every reading, as before
//	aggregated  one mean per sensor and hour, sent once the hour is over;
//	            on/off states (types ending in _state) are not sent
//	local_only  no readings; they stay in the local store
//
// Events (controller alarms, link status) are sent under every policy. Under
// aggregated and local_only the raw readings are kept in the local store for
// LocalRetentionDays and deleted after.

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// HourlyMeanMethod marks an aggregated reading (the platform accepts nothing
// else from an aggregated gateway).
const HourlyMeanMethod = "hourly_mean"

// DefaultLocalRetentionDays is how long the local store keeps readings.
const DefaultLocalRetentionDays = 7

type hourKey struct {
	SensorID, Type, Probe, ZoneID, DeviceID string
	Hour                                    int64 // unix seconds of the hour start
}

type hourSum struct {
	sum   float64
	count int
}

// HourlyAggregator collects readings and gives back the mean of every
// completed hour. A restart loses the open hour's samples taken before it.
type HourlyAggregator struct {
	sums map[hourKey]*hourSum
}

func NewHourlyAggregator() *HourlyAggregator {
	return &HourlyAggregator{sums: map[hourKey]*hourSum{}}
}

// Add files readings under their hour. State metrics are skipped: a mean of
// on/off is not a state, and the platform refuses it.
func (a *HourlyAggregator) Add(rs []Reading) {
	for _, r := range rs {
		if strings.HasSuffix(r.Type, "_state") {
			continue
		}
		ts, err := time.Parse(time.RFC3339, r.Ts)
		if err != nil {
			continue
		}
		k := hourKey{SensorID: r.SensorID, Type: r.Type, Probe: r.Probe, ZoneID: r.ZoneID, DeviceID: r.DeviceID,
			Hour: ts.UTC().Truncate(time.Hour).Unix()}
		s := a.sums[k]
		if s == nil {
			s = &hourSum{}
			a.sums[k] = s
		}
		s.sum += r.Value
		s.count++
	}
}

// Flush returns the means of every hour that ended before now, stamped at
// the hour start, and forgets them. Ordered by hour, then sensor.
func (a *HourlyAggregator) Flush(now time.Time) []Reading {
	current := now.UTC().Truncate(time.Hour).Unix()
	var keys []hourKey
	for k := range a.sums {
		if k.Hour < current {
			keys = append(keys, k)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Hour != keys[j].Hour {
			return keys[i].Hour < keys[j].Hour
		}
		return keys[i].SensorID+keys[i].Probe+keys[i].ZoneID < keys[j].SensorID+keys[j].Probe+keys[j].ZoneID
	})
	out := make([]Reading, 0, len(keys))
	for _, k := range keys {
		s := a.sums[k]
		delete(a.sums, k)
		out = append(out, Reading{
			SensorID: k.SensorID, Type: k.Type, Probe: k.Probe, ZoneID: k.ZoneID, DeviceID: k.DeviceID,
			Ts:               time.Unix(k.Hour, 0).UTC().Format(time.RFC3339),
			Value:            math.Round(s.sum/float64(s.count)*100) / 100,
			ValueOrigin:      "derived",
			DerivationMethod: HourlyMeanMethod,
		})
	}
	return out
}

// LocalStore keeps readings on the gateway, one JSON-lines file per UTC day,
// directory 0700 and files 0600, and deletes days older than the retention.
type LocalStore struct {
	Dir           string
	RetentionDays int
}

// Append writes readings to today's file and prunes old days.
func (l *LocalStore) Append(rs []Reading, now time.Time) error {
	if len(rs) == 0 {
		return l.Prune(now)
	}
	if err := os.MkdirAll(l.Dir, 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(l.Dir, "readings-"+now.UTC().Format("2006-01-02")+".jsonl"),
		os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(f)
	for _, r := range rs {
		if err := enc.Encode(r); err != nil {
			f.Close()
			return err
		}
	}
	if err := f.Close(); err != nil {
		return err
	}
	return l.Prune(now)
}

// Prune deletes the day files older than the retention.
func (l *LocalStore) Prune(now time.Time) error {
	days := l.RetentionDays
	if days <= 0 {
		days = DefaultLocalRetentionDays
	}
	cutoff := now.UTC().AddDate(0, 0, -days).Format("2006-01-02")
	entries, err := os.ReadDir(l.Dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "readings-") || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		if day := strings.TrimSuffix(strings.TrimPrefix(name, "readings-"), ".jsonl"); day < cutoff {
			if err := os.Remove(filepath.Join(l.Dir, name)); err != nil {
				return err
			}
		}
	}
	return nil
}

// SyncFilter decides, at each flush, which collected readings leave the
// gateway. Policy may change between flushes (the backend's policy arrives
// with the config); leaving aggregated drops the open hour's sums, whose raw
// readings are already in the local store.
type SyncFilter struct {
	Policy string
	Local  *LocalStore
	agg    *HourlyAggregator
	last   string
}

// Filter takes the readings collected since the last flush and returns the
// ones to send now.
func (f *SyncFilter) Filter(rs []Reading, now time.Time) ([]Reading, error) {
	policy := orFull(f.Policy)
	if policy != f.last {
		f.agg = nil
		f.last = policy
	}
	switch policy {
	case SyncFull:
		return rs, nil
	case SyncAggregated:
		if f.agg == nil {
			f.agg = NewHourlyAggregator()
		}
		f.agg.Add(rs)
		return f.agg.Flush(now), f.keep(rs, now)
	case SyncLocalOnly:
		return nil, f.keep(rs, now)
	}
	return nil, fmt.Errorf("unknown sync policy %q: nothing sent", policy)
}

func (f *SyncFilter) keep(rs []Reading, now time.Time) error {
	if f.Local == nil {
		return nil
	}
	return f.Local.Append(rs, now)
}
