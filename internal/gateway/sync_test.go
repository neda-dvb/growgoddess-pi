package gateway

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func at(h, m int) time.Time { return time.Date(2026, 10, 14, h, m, 0, 0, time.UTC) }

func rd(zone, typ string, ts time.Time, v float64) Reading {
	return Reading{SensorID: zone + ":" + typ, Type: typ, Ts: ts.Format(time.RFC3339), Value: v}
}

func TestHourlyAggregatorSendsMeansOfFinishedHours(t *testing.T) {
	a := NewHourlyAggregator()
	a.Add([]Reading{
		rd("room-1", "air_temp", at(10, 5), 24.0), rd("room-1", "air_temp", at(10, 35), 25.0),
		rd("room-1", "rh", at(10, 5), 60), rd("room-1", "light_state", at(10, 5), 1),
		rd("room-1", "air_temp", at(11, 1), 30.0),
	})
	if out := a.Flush(at(10, 59)); len(out) != 0 {
		t.Fatalf("an open hour is never sent: %+v", out)
	}
	out := a.Flush(at(11, 2))
	if len(out) != 2 {
		t.Fatalf("want the 10:00 means of air_temp and rh (no state), got %+v", out)
	}
	for _, r := range out {
		if r.Ts != "2026-10-14T10:00:00Z" || r.ValueOrigin != "derived" || r.DerivationMethod != HourlyMeanMethod {
			t.Errorf("mean must be stamped at the hour start and marked: %+v", r)
		}
		if r.Type == "air_temp" && r.Value != 24.5 {
			t.Errorf("air_temp mean = %g, want 24.5", r.Value)
		}
	}
	if again := a.Flush(at(11, 3)); len(again) != 0 {
		t.Fatalf("a mean is sent once: %+v", again)
	}
	if next := a.Flush(at(12, 0)); len(next) != 1 || next[0].Value != 30 {
		t.Fatalf("the 11:00 hour after it ends: %+v", next)
	}
}

func TestLocalOnlySendsNoReadingsAndKeepsThemOnTheGateway(t *testing.T) {
	dir := t.TempDir()
	f := &SyncFilter{Policy: SyncLocalOnly, Local: &LocalStore{Dir: filepath.Join(dir, "local"), RetentionDays: 2}}
	rs := []Reading{rd("room-1", "air_temp", at(10, 5), 24.0), rd("room-1", "rh", at(10, 5), 61)}
	send, err := f.Filter(rs, at(10, 6))
	if err != nil {
		t.Fatal(err)
	}
	if len(send) != 0 {
		t.Fatalf("local_only must send no readings, got %+v", send)
	}
	path := filepath.Join(dir, "local", "readings-2026-10-14.jsonl")
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("the readings must stay on the gateway: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("local file mode %v, want 0600", fi.Mode().Perm())
	}
	if di, _ := os.Stat(filepath.Join(dir, "local")); di.Mode().Perm() != 0o700 {
		t.Errorf("local dir mode %v, want 0700", di.Mode().Perm())
	}
	file, _ := os.Open(path)
	defer file.Close()
	n := 0
	for sc := bufio.NewScanner(file); sc.Scan(); n++ {
		var r Reading
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatal(err)
		}
	}
	if n != 2 {
		t.Fatalf("kept %d readings, want 2", n)
	}

	// days older than the retention are deleted
	old := filepath.Join(dir, "local", "readings-2026-10-11.jsonl")
	if err := os.WriteFile(old, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Filter(nil, at(12, 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("a day older than 2 days must be deleted: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("today's file stays: %v", err)
	}
}

func TestSyncFilterPolicies(t *testing.T) {
	rs := []Reading{rd("room-1", "air_temp", at(10, 5), 24.0)}
	full := &SyncFilter{}
	if send, _ := full.Filter(rs, at(10, 6)); len(send) != 1 || send[0].ValueOrigin != "" {
		t.Fatalf("full (the default) sends every reading as it is: %+v", send)
	}
	agg := &SyncFilter{Policy: SyncAggregated, Local: &LocalStore{Dir: t.TempDir()}}
	if send, _ := agg.Filter(rs, at(10, 6)); len(send) != 0 {
		t.Fatalf("aggregated sends nothing during the hour: %+v", send)
	}
	if send, _ := agg.Filter(nil, at(11, 0)); len(send) != 1 || send[0].DerivationMethod != HourlyMeanMethod {
		t.Fatalf("aggregated sends the mean after the hour: %+v", send)
	}
	unknown := &SyncFilter{Policy: "everything"}
	if send, err := unknown.Filter(rs, at(10, 6)); len(send) != 0 || err == nil {
		t.Fatalf("an unknown policy sends nothing and says so: %+v %v", send, err)
	}
}

func TestSpoolFilesArePrivate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "spool")
	s, err := NewSpool(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(Batch{ID: "gw-x-1", DataMode: "live"}); err != nil {
		t.Fatal(err)
	}
	if di, _ := os.Stat(dir); di.Mode().Perm() != 0o700 {
		t.Errorf("spool dir mode %v, want 0700", di.Mode().Perm())
	}
	if fi, _ := os.Stat(filepath.Join(dir, "gw-x-1.json")); fi.Mode().Perm() != 0o600 {
		t.Errorf("spool file mode %v, want 0600", fi.Mode().Perm())
	}
}
