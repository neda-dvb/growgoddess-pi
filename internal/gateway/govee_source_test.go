package gateway

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// roundTripFunc lets a test stand in for the Govee HTTP endpoint.
type roundTripFunc func(*http.Request) *http.Response

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r), nil }

func goveeClient(body string) *http.Client {
	return &http.Client{Transport: roundTripFunc(func(*http.Request) *http.Response {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}
	})}
}

func TestGoveeSourcePollFahrenheit(t *testing.T) {
	// H5075-style state: bare numbers, temperature in Fahrenheit.
	body := `{"payload":{"capabilities":[
		{"type":"devices.capabilities.online","instance":"online","state":{"value":true}},
		{"type":"devices.capabilities.property","instance":"sensorTemperature","state":{"value":83.48}},
		{"type":"devices.capabilities.property","instance":"sensorHumidity","state":{"value":34.6}}
	]}}`
	s := &GoveeSource{
		Zone: "room-1", ZoneID: "zone-a", DeviceID: "dev-1",
		SKU: "H5075", Device: "AA:BB", SourceUnit: "f", Client: goveeClient(body),
	}
	rs, err := s.Poll(time.Now())
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if len(rs) != 2 {
		t.Fatalf("want 2 readings (temp+rh), got %d: %+v", len(rs), rs)
	}
	got := map[string]Reading{}
	for _, r := range rs {
		got[r.Type] = r
	}
	// 83.48°F -> 28.6°C (rounded to 0.1)
	if got["air_temp"].Value != 28.6 {
		t.Errorf("air_temp = %v, want 28.6 (F->C)", got["air_temp"].Value)
	}
	if got["rh"].Value != 34.6 {
		t.Errorf("rh = %v, want 34.6", got["rh"].Value)
	}
	// tagged with the zone + device, room-prefixed sensor id, measured
	if got["air_temp"].SensorID != "room-1:air_temp" || got["air_temp"].ZoneID != "zone-a" ||
		got["air_temp"].DeviceID != "dev-1" || got["air_temp"].ValueOrigin != "measured" {
		t.Errorf("air_temp tagging wrong: %+v", got["air_temp"])
	}
}

func TestGoveeSourceOfflineIsGapNotError(t *testing.T) {
	// State returned but without temp/humidity (sensor offline) => no readings,
	// no error (a gap, never a fabricated value or a logged failure).
	body := `{"payload":{"capabilities":[
		{"type":"devices.capabilities.online","instance":"online","state":{"value":false}}
	]}}`
	s := &GoveeSource{Zone: "room-1", SKU: "H5075", SourceUnit: "f", Client: goveeClient(body)}
	rs, err := s.Poll(time.Now())
	if err != nil {
		t.Fatalf("offline must not error: %v", err)
	}
	if len(rs) != 0 {
		t.Fatalf("offline must yield no readings, got %+v", rs)
	}
}

func TestGoveeSourceCelsiusPassthrough(t *testing.T) {
	body := `{"payload":{"capabilities":[
		{"instance":"sensorTemperature","state":{"value":22.4}}]}}`
	s := &GoveeSource{Zone: "room-1", SKU: "H5179", SourceUnit: "c", Client: goveeClient(body)}
	rs, _ := s.Poll(time.Now())
	if len(rs) != 1 || rs[0].Value != 22.4 {
		t.Fatalf("celsius device must pass through, got %+v", rs)
	}
}
