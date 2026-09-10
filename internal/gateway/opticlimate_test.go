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
