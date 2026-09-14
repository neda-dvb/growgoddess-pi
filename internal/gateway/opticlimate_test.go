package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// opticlimatePayload is one controller response: two live probes, one
// disconnected (string), one absent (null). The string and the null must be
// skipped without failing the poll.
const opticlimatePayload = `{"getRegisterValues":{"address":0,"values":{
	"Room1Temp":{"value":27.7},
	"Humidity":{"value":71.5},
	"CO2In":{"value":"Disconnected"},
	"LeafTemp":{"value":null}
}}}`

func TestOptiClimateSourcePoll(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// READ-ONLY contract: the adapter must only ever GET.
		if r.Method != http.MethodGet {
			t.Errorf("opticlimate must only GET, got %s", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(opticlimatePayload))
	}))
	defer srv.Close()

	src := &OptiClimateSource{
		Zone:    "room-1",
		URL:     srv.URL,
		Address: 0,
		Every:   time.Minute,
		Client:  srv.Client(),
		Registers: []ModbusRegisterMap{
			{Name: "Room1Temp", Metric: "air_temp"},
			{Name: "Humidity", Metric: "rh"},
			{Name: "CO2In", Metric: "co2"},     // comes back "Disconnected": skip
			{Name: "LeafTemp", Metric: "leaf"}, // comes back null: skip
		},
	}

	if src.Simulated() {
		t.Error("opticlimate is a live source, Simulated() must be false")
	}
	if src.Describe() != "opticlimate" {
		t.Errorf("Describe() = %q, want opticlimate", src.Describe())
	}

	now := time.Date(2026, 8, 11, 10, 0, 0, 0, time.UTC)
	readings, err := src.Poll(now)
	if err != nil {
		t.Fatalf("a string/null value must not fail the poll: %v", err)
	}
	if len(readings) != 2 {
		t.Fatalf("readings = %d, want 2 (the two numeric registers): %+v", len(readings), readings)
	}

	byMetric := map[string]Reading{}
	for _, r := range readings {
		byMetric[r.Type] = r
	}

	cases := []struct {
		metric   string
		sensorID string
		value    float64
	}{
		{"air_temp", "room-1:air_temp", 27.7},
		{"rh", "room-1:rh", 71.5},
	}
	for _, c := range cases {
		r, ok := byMetric[c.metric]
		if !ok {
			t.Errorf("missing reading for metric %q", c.metric)
			continue
		}
		if r.SensorID != c.sensorID {
			t.Errorf("%s: SensorID = %q, want %q", c.metric, r.SensorID, c.sensorID)
		}
		if r.Value != c.value {
			t.Errorf("%s: Value = %g, want %g", c.metric, r.Value, c.value)
		}
		if r.ValueOrigin != "measured" {
			t.Errorf("%s: ValueOrigin = %q, want measured", c.metric, r.ValueOrigin)
		}
		if r.Ts != now.UTC().Format(time.RFC3339) {
			t.Errorf("%s: Ts = %q, want %q", c.metric, r.Ts, now.UTC().Format(time.RFC3339))
		}
	}

	// the non-numeric registers must be absent, not zero-valued readings
	if _, ok := byMetric["co2"]; ok {
		t.Error(`"Disconnected" string must be skipped, not emitted`)
	}
	if _, ok := byMetric["leaf"]; ok {
		t.Error("null value must be skipped, not emitted")
	}
}

// TestOptiClimateSourceHTTPError proves transport/HTTP failures surface as
// errors (so the poller logs and retries next tick).
func TestOptiClimateSourceHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer srv.Close()

	src := &OptiClimateSource{
		Zone: "room-1", URL: srv.URL, Every: time.Minute, Client: srv.Client(),
		Registers: []ModbusRegisterMap{{Name: "Room1Temp", Metric: "air_temp"}},
	}
	if _, err := src.Poll(time.Now()); err == nil {
		t.Error("an HTTP 500 must be returned as an error")
	}
}

// TestOptiClimateCO2Gating pins the CO2 honesty rules: a disconnected sensor
// emits nothing; with the CO2 function disabled neither the factory setpoint
// nor the dosing flag is emitted; with a sensor and CO2 enabled all three
// flow, the dosing output as 1/0.
func TestOptiClimateCO2Gating(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		want    map[string]float64 // metric -> value
	}{
		{
			"no sensor, CO2 disabled (every Cannaru box today)",
			`{"getRegisterValues":{"address":0,"values":{"Room1Temp":{"value":29.1},"Humidity":{"value":64.5},
			  "CO2In":{"value":"Disconnected"},"CO2Enable":{"value":false},"CO2Setpoint":{"value":400},"CO2OutDig":{"value":true}}}}`,
			map[string]float64{"air_temp": 29.1, "rh": 64.5},
		},
		{
			"sensor connected, CO2 disabled: the measurement is real, the control is not",
			`{"getRegisterValues":{"address":0,"values":{"Room1Temp":{"value":29.1},"Humidity":{"value":64.5},
			  "CO2In":{"value":712.0},"CO2Enable":{"value":false},"CO2Setpoint":{"value":400},"CO2OutDig":{"value":false}}}}`,
			map[string]float64{"air_temp": 29.1, "rh": 64.5, "co2": 712},
		},
		{
			"sensor connected, CO2 enabled, dosing on",
			`{"getRegisterValues":{"address":0,"values":{"Room1Temp":{"value":29.1},"Humidity":{"value":64.5},
			  "CO2In":{"value":705.0},"CO2Enable":{"value":true},"CO2Setpoint":{"value":1000},"CO2OutDig":{"value":true}}}}`,
			map[string]float64{"air_temp": 29.1, "rh": 64.5, "co2": 705, "co2_setpoint": 1000, "co2_dosing_state": 1},
		},
		{
			"sensor connected, CO2 enabled, dosing off",
			`{"getRegisterValues":{"address":0,"values":{"CO2In":{"value":1210.0},"CO2Enable":{"value":true},"CO2Setpoint":{"value":1200},"CO2OutDig":{"value":false}}}}`,
			map[string]float64{"co2": 1210, "co2_setpoint": 1200, "co2_dosing_state": 0},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Errorf("must only GET, got %s", r.Method)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.payload))
			}))
			defer srv.Close()
			src := &OptiClimateSource{Zone: "room-1", URL: srv.URL, Every: time.Minute, Client: srv.Client(), Registers: OptiClimateDefaultRegisters()}
			readings, err := src.Poll(time.Now())
			if err != nil {
				t.Fatal(err)
			}
			got := map[string]float64{}
			for _, r := range readings {
				got[r.Type] = r.Value
				if r.SensorID != "room-1:"+r.Type {
					t.Errorf("sensor id %q for %s", r.SensorID, r.Type)
				}
			}
			if len(got) != len(tc.want) {
				t.Fatalf("emitted %v, want %v", got, tc.want)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Errorf("%s = %g, want %g (all: %v)", k, got[k], v, got)
				}
			}
		})
	}
}

// TestOptiClimateLightCell: the light cell streams on/off as 1/0 and its
// relative level; a dark room reads 0 and off, honestly, not "absent".
func TestOptiClimateLightCell(t *testing.T) {
	for _, tc := range []struct {
		payload string
		state   float64
		level   float64
	}{
		{`{"getRegisterValues":{"address":0,"values":{"LightCell":{"value":true},"LightSensor":{"value":66.27}}}}`, 1, 66.3},
		{`{"getRegisterValues":{"address":0,"values":{"LightCell":{"value":false},"LightSensor":{"value":0.0}}}}`, 0, 0},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(tc.payload))
		}))
		src := &OptiClimateSource{Zone: "room-1", URL: srv.URL, Every: time.Minute, Client: srv.Client(), Registers: OptiClimateDefaultRegisters()}
		readings, err := src.Poll(time.Now())
		srv.Close()
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]float64{}
		for _, r := range readings {
			got[r.Type] = r.Value
		}
		if len(got) != 2 || got["light_state"] != tc.state || got["light_level"] != tc.level {
			t.Fatalf("emitted %v, want light_state=%g light_level=%g", got, tc.state, tc.level)
		}
	}
}

// TestOptiClimateActiveSetpoint: the streamed setpoint is the one the
// controller is actually holding: the day pair by day, the night pair at
// night, nothing during a transition (the step line carries the last value).
func TestOptiClimateActiveSetpoint(t *testing.T) {
	regs := `"Room1TempWntdDay":{"value":29},"Room1TempWntdNight":{"value":25},"HumiSetPointDay":{"value":64},"HumiSetPointNight":{"value":60}`
	for _, tc := range []struct {
		program string
		temp    float64
		rh      float64
		emitted bool
	}{
		{"Day", 29, 64, true},
		{"Night", 25, 60, true},
		{"Cool-down", 0, 0, false},
		{"Pre-heat", 0, 0, false},
	} {
		payload := `{"getRegisterValues":{"address":0,"values":{"ContrStatus":{"value":"` + tc.program + `"},` + regs + `}}}`
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(payload))
		}))
		src := &OptiClimateSource{Zone: "room-1", URL: srv.URL, Every: time.Minute, Client: srv.Client(), Registers: OptiClimateDefaultRegisters()}
		readings, err := src.Poll(time.Now())
		srv.Close()
		if err != nil {
			t.Fatal(err)
		}
		got := map[string][]float64{}
		for _, r := range readings {
			got[r.Type] = append(got[r.Type], r.Value)
		}
		if !tc.emitted {
			if len(got["temp_setpoint"]) != 0 || len(got["rh_setpoint"]) != 0 {
				t.Errorf("%s: no setpoint may be emitted during a transition, got %v", tc.program, got)
			}
			continue
		}
		if len(got["temp_setpoint"]) != 1 || got["temp_setpoint"][0] != tc.temp || len(got["rh_setpoint"]) != 1 || got["rh_setpoint"][0] != tc.rh {
			t.Errorf("%s: setpoints = %v, want temp %g rh %g exactly once", tc.program, got, tc.temp, tc.rh)
		}
	}
}

// TestOptiClimateEquipment: the compressor relay and percentage, the heater
// percentage as a state, de-humidify and the air speed map to the
// platform's equipment metrics.
func TestOptiClimateEquipment(t *testing.T) {
	payload := `{"getRegisterValues":{"address":0,"values":{"PwrRelays":{"value":true},"Compressor":{"value":100.0},"Heater":{"value":0.0},"dehumidify":{"value":false},"AirSpeed":{"value":86.0}}}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(payload))
	}))
	defer srv.Close()
	src := &OptiClimateSource{Zone: "room-1", URL: srv.URL, Every: time.Minute, Client: srv.Client(), Registers: OptiClimateDefaultRegisters()}
	readings, err := src.Poll(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]float64{}
	for _, r := range readings {
		got[r.Type] = r.Value
	}
	want := map[string]float64{"cooling_state": 1, "cooling_output": 100, "heating_state": 0, "dehumidifier_state": 0, "fan_output": 86}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %g, want %g (all: %v)", k, got[k], v, got)
		}
	}
	if len(got) != len(want) {
		t.Errorf("emitted %v, want exactly %v", got, want)
	}
	// a heater at 35 % is heating
	payload = `{"getRegisterValues":{"address":0,"values":{"Heater":{"value":35.0}}}}`
	if rs, _ := src.Poll(time.Now()); len(rs) != 1 || rs[0].Type != "heating_state" || rs[0].Value != 1 {
		t.Errorf("heater 35 %% must be heating_state 1, got %+v", rs)
	}
}

// TestOptiClimateAlarmEvents: the first poll reports standing alarms, later
// polls report only transitions, a cleared alarm once.
func TestOptiClimateAlarmEvents(t *testing.T) {
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	src := &OptiClimateSource{Zone: "room-1", URL: srv.URL, Every: time.Minute, Client: srv.Client()}
	now := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)

	body = `{"getAlarms":{"alarms":{"0":{"Room1TempOver":{"occurrences":3,"active":true,"firstOccurrence":1,"lastOccurrence":2},"PowerFailure":{"occurrences":1,"active":false,"firstOccurrence":1,"lastOccurrence":1}}}}}`
	evs, err := src.PollEvents(now)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].Payload["alarm"] != "Room1TempOver" || evs[0].Payload["active"] != true || evs[0].Kind != "note" || evs[0].ZoneID != "room-1" {
		t.Fatalf("first poll must report only the standing alarm, got %+v", evs)
	}
	// nothing changed: no events
	if evs, _ := src.PollEvents(now.Add(time.Minute)); len(evs) != 0 {
		t.Fatalf("unchanged alarms must not repeat, got %+v", evs)
	}
	// the alarm clears, another one raises
	body = `{"getAlarms":{"alarms":{"0":{"Room1TempOver":{"occurrences":3,"active":false,"firstOccurrence":1,"lastOccurrence":2},"PowerFailure":{"occurrences":2,"active":true,"firstOccurrence":1,"lastOccurrence":3}}}}}`
	evs, _ = src.PollEvents(now.Add(2 * time.Minute))
	if len(evs) != 2 {
		t.Fatalf("one cleared and one raised alarm must give two events, got %+v", evs)
	}
	if evs[0].Payload["alarm"] != "PowerFailure" || evs[0].Payload["active"] != true || evs[1].Payload["alarm"] != "Room1TempOver" || evs[1].Payload["active"] != false {
		t.Fatalf("events = %+v", evs)
	}
	// an empty table after a standing alarm: nothing to report (the alarm simply vanished from the table)
	body = `{"getAlarms":{"alarms":{}}}`
	if evs, _ := src.PollEvents(now.Add(3 * time.Minute)); len(evs) != 0 {
		t.Fatalf("an alarm dropped from the table is not a transition, got %+v", evs)
	}
}
