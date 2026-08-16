// Package gateway is the core of the Cannabits Edge Gateway: the small
// program that stands between physical sensors and the ingestion API.
//
// Design rules:
//   - One source interface; vendors become adapters, the core never changes.
//   - Every batch is spooled to disk BEFORE the first send attempt and
//     deleted only after the platform confirmed it - a network outage or a
//     crash loses nothing, and the deterministic batch id makes every retry
//     idempotent on the server.
//   - A gateway is either a live instrument or a labeled fixture source,
//     never both: one simulated source anywhere makes the whole gateway
//     dataMode "fixture", and mixing simulated with real sources is a
//     configuration error, not a warning.
//
// Everything in this file is pure or filesystem-local and fully testable;
// the network client lives in client.go, hardware in its adapter files.
package gateway

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

// Reading is one sampled value on its way to the platform.
type Reading struct {
	SensorID    string  `json:"sensorId"`
	Type        string  `json:"type"`
	Ts          string  `json:"ts"`
	Value       float64 `json:"value"`
	Probe       string  `json:"probe,omitempty"`
	ValueOrigin string  `json:"valueOrigin,omitempty"`
	// ZoneID/DeviceID place a reading in a sub-zone of the room and name the
	// device it came from. Empty on room-level sources (e.g. OptiClimate),
	// which the platform stores as room-level (zone_id NULL) unchanged.
	ZoneID   string `json:"zoneId,omitempty"`
	DeviceID string `json:"deviceId,omitempty"`
}

// Source is the adapter contract. Read-only by design: control interfaces,
// when they ever exist, will be a separate contract with separate consent.
type Source interface {
	// Describe names the adapter for logs and the gateway label.
	Describe() string
	// Simulated reports whether this source invents its values. One
	// simulated source turns the whole gateway into a fixture instrument.
	Simulated() bool
	// Poll samples the hardware once. Implementations return canonical
	// metrics only; unit conversion happens inside the adapter.
	Poll(now time.Time) ([]Reading, error)
	// Interval is the sampling cadence.
	Interval() time.Duration
}

// Batch is one spooled transmission unit. The id is fixed at creation and
// reused for every retry - the server's idempotency key.
type Batch struct {
	ID       string    `json:"id"`
	DataMode string    `json:"dataMode"`
	Created  time.Time `json:"created"`
	Readings []Reading `json:"readings"`
}

// BatchID derives the deterministic idempotency key of a flush moment.
func BatchID(label string, at time.Time) string {
	clean := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' {
			return r
		}
		return '-'
	}, label)
	return fmt.Sprintf("gw-%s-%d", clean, at.UnixMilli())
}

// ── the spool: durability before transmission ──

// Spool is a directory of pending batches, oldest first.
type Spool struct {
	Dir string
	// MaxBatches bounds the disk footprint (default 5760 = four days at one
	// batch per minute). Beyond it the OLDEST batches are dropped with an
	// error returned, so data loss is loud, never silent.
	MaxBatches int
}

func NewSpool(dir string, maxBatches int) (*Spool, error) {
	if maxBatches <= 0 {
		maxBatches = 5760
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &Spool{Dir: dir, MaxBatches: maxBatches}, nil
}

func (s *Spool) path(id string) string {
	return filepath.Join(s.Dir, id+".json")
}

// Put persists a batch before any send attempt. Returns the ids of batches
// dropped to respect the bound (normally none).
func (s *Spool) Put(b Batch) ([]string, error) {
	buf, err := json.Marshal(b)
	if err != nil {
		return nil, err
	}
	tmp := s.path(b.ID) + ".tmp"
	if err := os.WriteFile(tmp, buf, 0o644); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp, s.path(b.ID)); err != nil {
		return nil, err
	}
	ids, err := s.Pending()
	if err != nil {
		return nil, err
	}
	var dropped []string
	for len(ids) > s.MaxBatches {
		oldest := ids[0]
		if err := s.Delete(oldest); err != nil {
			return dropped, err
		}
		dropped = append(dropped, oldest)
		ids = ids[1:]
	}
	return dropped, nil
}

// Pending lists spooled batch ids, oldest first (ids embed creation time).
func (s *Spool) Pending() ([]string, error) {
	entries, err := os.ReadDir(s.Dir)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, e := range entries {
		name := e.Name()
		if strings.HasSuffix(name, ".json") {
			ids = append(ids, strings.TrimSuffix(name, ".json"))
		}
	}
	sort.Strings(ids)
	return ids, nil
}

func (s *Spool) Get(id string) (Batch, error) {
	raw, err := os.ReadFile(s.path(id))
	if err != nil {
		return Batch{}, err
	}
	var b Batch
	err = json.Unmarshal(raw, &b)
	return b, err
}

func (s *Spool) Delete(id string) error {
	return os.Remove(s.path(id))
}

// ── configuration ──

// SourceConfig selects and parameterizes one adapter.
type SourceConfig struct {
	Type            string `json:"type"` // "sht31" | "sim" | "modbus-listen" | "opticlimate"
	Bus             string `json:"bus,omitempty"`
	Address         int    `json:"address,omitempty"`
	IntervalSeconds int    `json:"intervalSeconds,omitempty"`
	Probe           string `json:"probe,omitempty"`
	// opticlimate: the controller's HTTP base URL (e.g. http://192.168.2.110:4001).
	URL string `json:"url,omitempty"`
	// modbus-listen: the serial tap and the human-pinned register map.
	Device string `json:"device,omitempty"`
	Baud   int    `json:"baud,omitempty"` // 0 = auto-detect
	Parity string `json:"parity,omitempty"`
	// modbus-listen and opticlimate share the register map: modbus keys on
	// table/slave/address, opticlimate keys on the register Name.
	Registers []ModbusRegisterMap `json:"registers,omitempty"`
}

// Config is the gateway's reviewed runtime contract.
type Config struct {
	API          string `json:"api"`
	PlanID       string `json:"planId"`
	Key          string `json:"key,omitempty"`
	KeyFile      string `json:"keyFile,omitempty"`
	Zone         string `json:"zone"`
	GatewayLabel string `json:"gatewayLabel"`
	FlushSeconds int    `json:"flushSeconds,omitempty"`
	// ConfigPollSeconds: in agent mode, how often to pull room/controller
	// config from the backend (default 15s). Ignored in static mode.
	ConfigPollSeconds int            `json:"configPollSeconds,omitempty"`
	SpoolDir          string         `json:"spoolDir"`
	Sources           []SourceConfig `json:"sources"`
}

// Validate enforces the contract, including the honesty rule.
func (c Config) Validate() []string {
	var problems []string
	if c.API == "" {
		problems = append(problems, "api is required")
	}
	if c.PlanID == "" {
		problems = append(problems, "planId is required")
	}
	if c.Zone == "" {
		problems = append(problems, "zone is required (a cycle id of the plan)")
	}
	if c.GatewayLabel == "" {
		problems = append(problems, "gatewayLabel is required")
	}
	if c.SpoolDir == "" {
		problems = append(problems, "spoolDir is required")
	}
	if len(c.Sources) == 0 {
		problems = append(problems, "at least one source is required")
	}
	sim, real := false, false
	for i, s := range c.Sources {
		switch s.Type {
		case "sim":
			sim = true
		case "sht31":
			real = true
			if s.Bus == "" {
				problems = append(problems, fmt.Sprintf("source %d: sht31 needs a bus (e.g. /dev/i2c-1)", i))
			}
		case "modbus-listen":
			real = true
			if s.Device == "" {
				problems = append(problems, fmt.Sprintf("source %d: modbus-listen needs a device (e.g. /dev/ttyUSB0)", i))
			}
			for j, r := range s.Registers {
				if r.Metric == "" || (r.Table != TableInput && r.Table != TableHolding) {
					problems = append(problems, fmt.Sprintf("source %d register %d: needs a metric and table input|holding", i, j))
				}
			}
		case "opticlimate":
			real = true
			if s.URL == "" {
				problems = append(problems, fmt.Sprintf("source %d: opticlimate needs a url (e.g. http://192.168.2.110:4001)", i))
			}
			named := false
			for _, r := range s.Registers {
				if r.Name != "" && r.Metric != "" {
					named = true
					break
				}
			}
			if !named {
				problems = append(problems, fmt.Sprintf("source %d: opticlimate needs at least one register with a name and a metric", i))
			}
		default:
			problems = append(problems, fmt.Sprintf("source %d: unknown type %q", i, s.Type))
		}
	}
	if sim && real {
		problems = append(problems, "a gateway is either a live instrument or a fixture source: simulated and real sources cannot mix")
	}
	return problems
}

// DataMode of everything this gateway sends: one simulated source makes the
// whole gateway a fixture instrument.
func (c Config) DataMode() string {
	for _, s := range c.Sources {
		if s.Type == "sim" {
			return "fixture"
		}
	}
	return "live"
}

// ── the sim source: the gateway's self-test instrument ──

// SimSource produces deterministic synthetic values (pure sine composites,
// no randomness) so the gateway itself can be exercised end to end without
// hardware. Everything it emits is labeled simulated and travels as fixture.
type SimSource struct {
	Zone  string
	Probe string
	Every time.Duration
}

func (s SimSource) Describe() string        { return "sim" }
func (s SimSource) Simulated() bool         { return true }
func (s SimSource) Interval() time.Duration { return s.Every }

func (s SimSource) Poll(now time.Time) ([]Reading, error) {
	t := float64(now.Unix())
	temp := 24.0 + 1.2*math.Sin(t/1700) + 0.3*math.Sin(t/430)
	rh := 52.0 + 2.5*math.Sin(t/2100) + 0.8*math.Sin(t/510)
	ts := now.UTC().Format(time.RFC3339)
	return []Reading{
		{SensorID: s.Zone + ":air_temp", Type: "air_temp", Ts: ts, Value: round1(temp), Probe: s.Probe, ValueOrigin: "simulated"},
		{SensorID: s.Zone + ":rh", Type: "rh", Ts: ts, Value: round1(rh), Probe: s.Probe, ValueOrigin: "simulated"},
	}, nil
}

func round1(v float64) float64 { return float64(int(v*10+0.5)) / 10 }
