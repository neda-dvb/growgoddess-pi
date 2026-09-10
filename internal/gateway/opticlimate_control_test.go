package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeOptiClimate is a controller with a register table: GET returns it, a
// well-formed POST updates it (unless refuse is set), exactly like the real
// HMI backend behaves.
type fakeOptiClimate struct {
	mu     sync.Mutex
	regs   map[string]any
	refuse string // non-empty: every write answers requestError
	ignore bool   // accept the write but do not change the register
	// applyAfter: the unit applies the write only once this many GETs have
	// happened since the POST (the real controller answers the first GET with
	// the old value). pendingRegs holds the value until then.
	applyAfter  int
	getsSince   int
	pendingRegs map[string]float64
	posts       int
	lastBody    string
}

func (f *fakeOptiClimate) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/backend/getRegisterValues"):
			if r.Method != http.MethodGet {
				t.Errorf("getRegisterValues must be GET, got %s", r.Method)
			}
			var ids []string
			_ = json.Unmarshal([]byte(r.URL.Query().Get("ids")), &ids)
			if len(f.pendingRegs) > 0 {
				f.getsSince++
				if f.getsSince >= f.applyAfter {
					for k, v := range f.pendingRegs {
						f.regs[k] = v
					}
					f.pendingRegs = nil
				}
			}
			values := map[string]map[string]any{}
			for _, id := range ids {
				if v, ok := f.regs[id]; ok {
					values[id] = map[string]any{"value": v}
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"getRegisterValues": map[string]any{"address": 0, "values": values}})
		case strings.HasSuffix(r.URL.Path, "/backend/setRegisterValues"):
			f.posts++
			if r.Method != http.MethodPost {
				t.Errorf("setRegisterValues must be POST, got %s", r.Method)
			}
			if ct := r.Header.Get("Content-Type"); ct != "application/json" {
				t.Errorf("write Content-Type = %q, want application/json", ct)
			}
			var body struct {
				Address int                `json:"address"`
				Values  map[string]float64 `json:"values"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("write body: %v", err)
			}
			raw, _ := json.Marshal(body)
			f.lastBody = string(raw)
			if f.refuse != "" {
				_ = json.NewEncoder(w).Encode(map[string]any{"setRegisterValues": map[string]any{"requestError": []string{f.refuse}}})
				return
			}
			if !f.ignore && f.applyAfter > 0 {
				f.pendingRegs = map[string]float64{}
				for k, v := range body.Values {
					f.pendingRegs[k] = v
				}
				f.getsSince = 0
			} else if !f.ignore {
				for k, v := range body.Values {
					f.regs[k] = v
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"setRegisterValues": map[string]any{"values": body.Values}})
		default:
			http.NotFound(w, r)
		}
	}
}

func newFake() *fakeOptiClimate {
	return &fakeOptiClimate{regs: map[string]any{
		"Room1TempWntdDay":   29.0,
		"Room1TempWntdNight": 25.0,
		"HumiSetPointDay":    64.0,
		"HumiSetPointNight":  64.0,
		"RoomDayTempMin":     16.0,
		"RoomDayTempMax":     30.0,
		"RoomNightTempMin":   16.0,
		"RoomNightTempMax":   30.0,
		"ContrStatus":        "Day",
		"TimeMode":           "Auto",
	}}
}

func TestOptiClimateControlSetSetpoint(t *testing.T) {
	f := newFake()
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	ctl := &OptiClimateControl{URL: srv.URL, Client: srv.Client()}

	res, err := ctl.SetSetpoint("temp_day", 28)
	if err != nil {
		t.Fatalf("write failed: %v", err)
	}
	if res.Register != "Room1TempWntdDay" || res.Previous != 29 || res.Readback != 28 || res.Value != 28 {
		t.Fatalf("result = %+v", res)
	}
	if f.posts != 1 {
		t.Fatalf("posts = %d, want exactly one write", f.posts)
	}
	want := `{"address":0,"values":{"Room1TempWntdDay":28}}`
	if f.lastBody != want {
		t.Fatalf("write body = %s, want %s", f.lastBody, want)
	}
	if f.regs["Room1TempWntdDay"] != 28.0 {
		t.Fatalf("controller register = %v, want 28", f.regs["Room1TempWntdDay"])
	}
}

// A no-op write (the current value written back) is the safe end-to-end
// probe: one POST, read-back equal, previous == value.
func TestOptiClimateControlNoOpWrite(t *testing.T) {
	f := newFake()
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	ctl := &OptiClimateControl{URL: srv.URL, Client: srv.Client()}
	res, err := ctl.SetSetpoint("rh_day", 64)
	if err != nil {
		t.Fatalf("no-op write failed: %v", err)
	}
	if res.Previous != 64 || res.Readback != 64 || f.posts != 1 {
		t.Fatalf("result = %+v posts=%d", res, f.posts)
	}
}

func TestOptiClimateControlRefusesOutsideAlarmLimits(t *testing.T) {
	f := newFake()
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	ctl := &OptiClimateControl{URL: srv.URL, Client: srv.Client()}
	_, err := ctl.SetSetpoint("temp_day", 31) // RoomDayTempMax is 30
	if !errors.Is(err, ErrOutsideControllerLimits) {
		t.Fatalf("err = %v, want ErrOutsideControllerLimits", err)
	}
	if f.posts != 0 {
		t.Fatalf("nothing may be written when the limits refuse: posts = %d", f.posts)
	}
	if _, err := ctl.SetSetpoint("temp_night", 15); !errors.Is(err, ErrOutsideControllerLimits) {
		t.Fatalf("night below RoomNightTempMin must be refused, got %v", err)
	}
	// humidity has no alarm registers on this controller: no limit check
	if _, err := ctl.SetSetpoint("rh_night", 70); err != nil {
		t.Fatalf("humidity write must not be blocked by temperature limits: %v", err)
	}
}

func TestOptiClimateControlControllerRefusal(t *testing.T) {
	f := newFake()
	f.refuse = "value out of range"
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	ctl := &OptiClimateControl{URL: srv.URL, Client: srv.Client()}
	_, err := ctl.SetSetpoint("temp_day", 28)
	if err == nil || !strings.Contains(err.Error(), "value out of range") {
		t.Fatalf("controller refusal must surface its message, got %v", err)
	}
	if f.regs["Room1TempWntdDay"] != 29.0 {
		t.Fatalf("a refused write must leave the register untouched")
	}
}

// The real unit applies a write asynchronously: the GET right after the POST
// still shows the old value. The read-back must wait for it.
func TestOptiClimateControlAsyncApply(t *testing.T) {
	f := newFake()
	f.applyAfter = 3 // old value on the first two read-backs, new on the third
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	ctl := &OptiClimateControl{URL: srv.URL, Client: srv.Client(), ReadbackAttempts: 5, ReadbackDelay: 5 * time.Millisecond}
	res, err := ctl.SetSetpoint("temp_day", 27)
	if err != nil {
		t.Fatalf("a write the unit applies a moment later must be confirmed, got %v", err)
	}
	if res.Previous != 29 || res.Readback != 27 || f.posts != 1 {
		t.Fatalf("result = %+v posts=%d", res, f.posts)
	}
	// and when it never applies within the attempts, the last value is reported
	f2 := newFake()
	f2.applyAfter = 10
	srv2 := httptest.NewServer(f2.handler(t))
	defer srv2.Close()
	ctl2 := &OptiClimateControl{URL: srv2.URL, Client: srv2.Client(), ReadbackAttempts: 3, ReadbackDelay: 5 * time.Millisecond}
	if _, err := ctl2.SetSetpoint("temp_day", 27); err == nil || !strings.Contains(err.Error(), "still reports 29 after writing 27") {
		t.Fatalf("exhausted read-back must report the last value, got %v", err)
	}
}

func TestOptiClimateControlReadbackMismatch(t *testing.T) {
	f := newFake()
	f.ignore = true // controller says ok but keeps the old value
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	ctl := &OptiClimateControl{URL: srv.URL, Client: srv.Client(), ReadbackAttempts: 2, ReadbackDelay: time.Millisecond}
	_, err := ctl.SetSetpoint("temp_day", 28)
	if err == nil || !strings.Contains(err.Error(), "still reports 29 after writing 28") {
		t.Fatalf("read-back mismatch must be an error, got %v", err)
	}
}

func TestOptiClimateControlUnknownSetpoint(t *testing.T) {
	f := newFake()
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	ctl := &OptiClimateControl{URL: srv.URL, Client: srv.Client()}
	if _, err := ctl.SetSetpoint("RoomDayTempMax", 40); err == nil {
		t.Fatal("only the four canonical setpoints may be written")
	}
	if f.posts != 0 {
		t.Fatalf("posts = %d, want 0", f.posts)
	}
}

func TestOptiClimateControlReadSettings(t *testing.T) {
	f := newFake()
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	ctl := &OptiClimateControl{URL: srv.URL, Client: srv.Client()}
	now := time.Date(2026, 9, 10, 16, 0, 0, 0, time.UTC)
	s, err := ctl.ReadSettings(now)
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]float64{"temp_day": 29, "temp_night": 25, "rh_day": 64, "rh_night": 64} {
		if s.Setpoints[key] != want {
			t.Errorf("%s = %g, want %g", key, s.Setpoints[key], want)
		}
	}
	if s.Program != "Day" || s.TimeMode != "Auto" {
		t.Errorf("program/timeMode = %q/%q", s.Program, s.TimeMode)
	}
	if s.AlarmLimits["temp_day"] != [2]float64{16, 30} {
		t.Errorf("day alarm limits = %v", s.AlarmLimits["temp_day"])
	}
	if s.ReadAt != "2026-09-10T16:00:00Z" {
		t.Errorf("readAt = %s", s.ReadAt)
	}
	if f.posts != 0 {
		t.Fatalf("reading settings must never write: posts = %d", f.posts)
	}
}

func TestRequestErrorText(t *testing.T) {
	cases := map[string]string{
		``:                "",
		`null`:            "",
		`["a","b"]`:       "a; b",
		`"single"`:        "single",
		`{"code":1}`:      `{"code":1}`,
		fmt.Sprintf(`[]`): "",
	}
	for raw, want := range cases {
		if got := requestErrorText(json.RawMessage(raw)); got != want {
			t.Errorf("%s: got %q want %q", raw, got, want)
		}
	}
}
