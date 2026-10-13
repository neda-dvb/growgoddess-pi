package gateway

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSendReportsARefusedKeyAsUnauthorized(t *testing.T) {
	for _, code := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(code)
			_, _ = w.Write([]byte(`{"error":"invalid API key"}`))
		}))
		_, err := NewClient(srv.URL, "p", "cbk_x", "pi").Send(Batch{ID: "b1"})
		srv.Close()
		if !errors.Is(err, ErrUnauthorized) {
			t.Errorf("HTTP %d: %v, want ErrUnauthorized", code, err)
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusInternalServerError) }))
	defer srv.Close()
	if _, err := NewClient(srv.URL, "p", "cbk_x", "pi").Send(Batch{ID: "b1"}); err == nil || errors.Is(err, ErrUnauthorized) {
		t.Errorf("HTTP 500 is not an auth problem: %v", err)
	}
}

func TestAuthStateLogsOnceAnHour(t *testing.T) {
	var lines []string
	a := &AuthState{Logf: func(f string, args ...any) { lines = append(lines, fmt.Sprintf(f, args...)) }}
	t0 := time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC)
	a.Observe(errors.New("network down"), t0)
	if a.Unauthorised() || len(lines) != 0 {
		t.Fatal("a network error says nothing about the key")
	}
	a.Observe(fmt.Errorf("%w: HTTP 401", ErrUnauthorized), t0)
	a.Observe(fmt.Errorf("%w: HTTP 401", ErrUnauthorized), t0.Add(time.Minute))
	if !a.Unauthorised() || len(lines) != 1 || !strings.Contains(lines[0], "UNAUTHORISED") {
		t.Fatalf("entering the state logs once: %v", lines)
	}
	a.Tick(t0.Add(59*time.Minute), 42)
	if len(lines) != 1 {
		t.Fatalf("no repeat within the hour: %v", lines)
	}
	a.Tick(t0.Add(61*time.Minute), 42)
	if len(lines) != 2 || !strings.Contains(lines[1], "42 batch(es) kept") {
		t.Fatalf("the hourly line says what is kept: %v", lines)
	}
	a.Observe(nil, t0.Add(2*time.Hour))
	if a.Unauthorised() || len(lines) != 3 || !strings.Contains(lines[2], "authorised again") {
		t.Fatalf("an accepted call leaves the state: %v", lines)
	}
	a.Tick(t0.Add(5*time.Hour), 0)
	if len(lines) != 3 {
		t.Fatal("no hourly line while authorised")
	}
}

func spoolOf(t *testing.T, max, holdMax int) *Spool {
	t.Helper()
	s, err := NewSpool(filepath.Join(t.TempDir(), "spool"), max)
	if err != nil {
		t.Fatal(err)
	}
	s.HoldMax = holdMax
	return s
}

func put(t *testing.T, s *Spool, n int) ([]string, error) {
	t.Helper()
	return s.Put(Batch{ID: fmt.Sprintf("gw-x-%04d", n), DataMode: "live"})
}

// While the key is refused nothing is dropped; at the raised bound new
// batches are refused and every stored one is kept.
func TestHeldSpoolNeverDropsBecauseOfAuth(t *testing.T) {
	s := spoolOf(t, 3, 5)
	s.SetHold(true)
	for i := 1; i <= 5; i++ {
		if dropped, err := put(t, s, i); err != nil || len(dropped) != 0 {
			t.Fatalf("held put %d: dropped %v err %v", i, dropped, err)
		}
	}
	if _, err := put(t, s, 6); !errors.Is(err, ErrSpoolHeld) {
		t.Fatalf("at the raised bound the new batch is refused: %v", err)
	}
	ids, _ := s.Pending()
	if len(ids) != 5 || ids[0] != "gw-x-0001" {
		t.Fatalf("every stored batch is kept: %v", ids)
	}
	// authorised again, but the backlog is above the normal bound: still no drop
	s.SetHold(false)
	_ = s.Delete("gw-x-0005")
	if dropped, err := put(t, s, 7); err != nil || len(dropped) != 0 {
		t.Fatalf("draining backlog must not drop: %v %v", dropped, err)
	}
	for _, id := range []string{"gw-x-0001", "gw-x-0002", "gw-x-0003"} {
		_ = s.Delete(id)
	}
	// below the normal bound again: the normal rule (drop the oldest) is back
	if dropped, _ := put(t, s, 8); len(dropped) != 0 { // 0004, 0007, 0008: at the bound
		t.Fatalf("at the bound nothing is dropped yet: %v", dropped)
	}
	if dropped, _ := put(t, s, 9); len(dropped) != 1 || dropped[0] != "gw-x-0004" {
		t.Fatalf("after the backlog the normal bound applies again: dropped %v", dropped)
	}
}

func TestWriteKeyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.key")
	if k, err := ReadKeyFile(path); k != "" || err != nil {
		t.Fatalf("no file, no key: %q %v", k, err)
	}
	if err := WriteKeyFile(path, "cbk_one"); err != nil {
		t.Fatal(err)
	}
	if err := WriteKeyFile(path, "cbk_two"); err != nil {
		t.Fatal(err)
	}
	if k, _ := ReadKeyFile(path); k != "cbk_two" {
		t.Fatalf("key = %q", k)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v, want 0600", fi.Mode().Perm())
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatalf("no temporary files left: %v", entries)
	}
}

// A restart while the spool holds more than its normal bound (a backlog from
// an unauthorised spell, then a fresh enroll and restart) drops nothing.
func TestRestartWithBacklogDropsNothing(t *testing.T) {
	s := spoolOf(t, 3, 0)
	s.SetHold(true)
	for i := 1; i <= 5; i++ {
		put(t, s, i)
	}
	again, err := NewSpool(s.Dir, 3) // a new process: no hold, no memory
	if err != nil {
		t.Fatal(err)
	}
	if dropped, err := put(t, again, 6); err != nil || len(dropped) != 0 {
		t.Fatalf("the backlog must survive a restart: dropped %v err %v", dropped, err)
	}
}

// While held, the spool stops recording before the disk drops under its
// free-space floor, and never deletes a stored batch for it. A fake small
// filesystem stands in for the real one.
func TestHeldSpoolStopsBeforeTheDiskFills(t *testing.T) {
	const total = 1_000_000
	free := uint64(120_000) // 12 %
	s := spoolOf(t, 3, 100)
	s.FreeSpace = func() (uint64, uint64, error) { return free, total, nil }
	s.SetHold(true)
	if _, err := put(t, s, 1); err != nil {
		t.Fatalf("12%% free is above the 10%% floor: %v", err)
	}
	free = 99_000 // under 10 %
	_, err := put(t, s, 2)
	if !errors.Is(err, ErrSpoolHeld) || !strings.Contains(err.Error(), "disk") {
		t.Fatalf("under the floor the batch must be refused with the reason: %v", err)
	}
	if ids, _ := s.Pending(); len(ids) != 1 || ids[0] != "gw-x-0001" {
		t.Fatalf("nothing written, nothing deleted: %v", ids)
	}
	// the batch itself counts: 100,010 free would leave under 10 % after it
	free = 100_010
	if _, err := s.Put(Batch{ID: "gw-x-0003", Readings: make([]Reading, 200)}); !errors.Is(err, ErrSpoolHeld) {
		t.Fatalf("a batch that would cross the floor is refused: %v", err)
	}
	// a configured floor applies
	free = 150_000 // 15 %
	s.MinFreePercent = 20
	if _, err := put(t, s, 4); !errors.Is(err, ErrSpoolHeld) {
		t.Fatalf("15%% free is under a 20%% floor: %v", err)
	}
	// not held: the normal bound rules, the disk floor does not apply
	s.MinFreePercent = 0
	free = 10_000
	ns := spoolOf(t, 3, 0)
	ns.FreeSpace = s.FreeSpace
	if _, err := put(t, ns, 1); err != nil {
		t.Fatalf("an unheld spool writes as before: %v", err)
	}
	// an unreadable disk leaves the batch-count bound only
	s.FreeSpace = func() (uint64, uint64, error) { return 0, 0, errors.New("no statfs") }
	if _, err := put(t, s, 5); err != nil {
		t.Fatalf("unknown free space must not stop recording: %v", err)
	}
	if problems := (Config{SpoolMinFreePercent: 95}).EdgeProblems(false); len(problems) != 1 {
		t.Fatalf("a floor above 90%% is a config problem: %v", problems)
	}
}
