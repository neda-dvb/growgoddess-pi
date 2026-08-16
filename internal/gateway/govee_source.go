package gateway

// GoveeSource: a Govee temperature/humidity sensor as a gateway source, read
// over the official Govee cloud API. Unlike OptiClimate (a LAN controller bound
// to a ROOM), a Govee sensor is placed in a ZONE inside a room, and the cloud
// API is reachable from anywhere — so a Govee sensor keeps reporting even when
// the facility LAN (and its controller) is unreachable.
//
// READ-ONLY: this adapter only ever POSTs to device/state to READ. It never
// calls device/control. The gateway measures, it never controls.
//
// One call per poll:
//
//	POST https://openapi.api.govee.com/router/api/v1/device/state
//	     {"requestId": "...", "payload": {"sku": "H5075", "device": "..."}}
//
// The state returns capabilities; a temp/humidity sensor carries
// instance "sensorTemperature" and "sensorHumidity" with a bare numeric value
// and NO unit field. The unit is therefore configured per device (SourceUnit):
// the H5075, for example, reports Fahrenheit. Temperature is converted to the
// canonical Celsius here; humidity is a plain percent.

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const goveeStateURL = "https://openapi.api.govee.com/router/api/v1/device/state"

// GoveeSource reads one Govee sensor and emits its readings tagged with the
// sub-zone and device they came from. The API key is held only to set the
// request header and is never logged.
type GoveeSource struct {
	Zone       string // the ROOM id (sensor_id prefix), e.g. "room-1"
	ZoneID     string // the sub-zone id (a pro_zones id)
	DeviceID   string // the device id (a pro_devices id)
	APIKey     string // Govee-API-Key
	SKU        string // e.g. "H5075"
	Device     string // Govee device id (MAC-like)
	SourceUnit string // "f" | "c": how THIS device reports temperature
	Every      time.Duration
	// Client is injectable for tests; nil means a fresh client with a ~15s
	// timeout is used per poll.
	Client *http.Client
}

func (s *GoveeSource) Describe() string        { return "govee:" + s.SKU }
func (s *GoveeSource) Simulated() bool         { return false }
func (s *GoveeSource) Interval() time.Duration { return s.Every }

// Poll queries device state once and emits air_temp (converted to °C) and rh
// when present. A returned-but-incomplete state (sensor offline / not yet
// reported) yields no readings and NO error — a brief gap, not a logged
// failure. Transport, HTTP and rate-limit failures return an error so the
// caller logs and skips this tick.
func (s *GoveeSource) Poll(now time.Time) ([]Reading, error) {
	body, _ := json.Marshal(map[string]any{
		"requestId": goveeReqID(),
		"payload":   map[string]string{"sku": s.SKU, "device": s.Device},
	})
	req, _ := http.NewRequest(http.MethodPost, goveeStateURL, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Govee-API-Key", s.APIKey) // only use of the key; never logged
	client := s.Client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("govee state: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("govee: 429 rate limited")
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("govee: HTTP %d", resp.StatusCode)
	}

	var payload struct {
		Payload struct {
			Capabilities []struct {
				Instance string `json:"instance"`
				State    struct {
					Value json.RawMessage `json:"value"`
				} `json:"state"`
			} `json:"capabilities"`
		} `json:"payload"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("govee decode: %w", err)
	}

	ts := now.UTC().Format(time.RFC3339)
	emit := func(out []Reading, metric string, v float64) []Reading {
		return append(out, Reading{
			SensorID: s.Zone + ":" + metric, Type: metric, Ts: ts,
			Value: round1(v), ValueOrigin: "measured",
			ZoneID: s.ZoneID, DeviceID: s.DeviceID,
		})
	}
	var out []Reading
	for _, c := range payload.Payload.Capabilities {
		f, ok := numericValue(c.State.Value)
		if !ok {
			continue
		}
		switch strings.ToLower(c.Instance) {
		case "sensortemperature":
			out = emit(out, "air_temp", s.toCelsius(f))
		case "sensorhumidity":
			out = emit(out, "rh", f)
		}
	}
	return out, nil
}

// toCelsius converts a raw temperature to the canonical Celsius per the
// device's configured source unit. Default (unset) is treated as Celsius.
func (s *GoveeSource) toCelsius(v float64) float64 {
	if strings.ToLower(s.SourceUnit) == "f" {
		return (v - 32) * 5 / 9
	}
	return v
}

func goveeReqID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
