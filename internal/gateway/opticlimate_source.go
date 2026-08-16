package gateway

// OptiClimateSource: an OptiClimate / Airsupplies controller as a gateway
// source, read over the controller's local HTTP API. The controller already
// exposes its live register values in physical units, so unlike the passive
// Modbus tap this adapter simply asks and maps.
//
// READ-ONLY BY DESIGN: this adapter only ever issues HTTP GET against
// getRegisterValues. It never calls setRegisterValues or any other write
// endpoint - the gateway measures, it never controls. Do not add a write
// path here; control is a separate contract with separate consent.
//
// The wire call is one GET per poll:
//
//	GET {URL}/backend/getRegisterValues?address={Address}&ids=<json array of names>
//
// where ids is a URL-encoded JSON array of register NAMES, e.g.
// ["Room1Temp","Humidity"]. The response carries each requested value:
//
//	{"getRegisterValues":{"address":0,"values":{
//	    "Room1Temp":{"value":27.7},"Humidity":{"value":71.5},
//	    "CO2In":{"value":"Disconnected"}}}}
//
// Values are already in physical units. Crucially, some values are non-numeric
// ("Disconnected", or null) when a probe is absent: each value is decoded
// loosely and any value that is not a JSON number is skipped, so one bad probe
// never fails the whole poll.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// OptiClimateSource reads named registers from one controller over HTTP.
// OptiClimateDefaultRegisters is the known OptiClimate/Revomax register→metric
// mapping. The adapter owns it, so the operator never enters register names:
// binding an OptiClimate controller to a room is enough. Read-only.
func OptiClimateDefaultRegisters() []ModbusRegisterMap {
	return []ModbusRegisterMap{
		{Name: "Room1Temp", Metric: "air_temp"},
		{Name: "Humidity", Metric: "rh"},
		{Name: "Room1TempWntdDay", Metric: "temp_setpoint"},
		{Name: "HumiSetPointDay", Metric: "rh_setpoint"},
	}
}

type OptiClimateSource struct {
	Zone      string
	URL       string              // controller base URL, e.g. http://192.168.2.110:4001
	Address   int                 // Modbus unit address behind the controller
	Registers []ModbusRegisterMap // Name -> Metric; Factor/Slave/Table unused here
	Every     time.Duration
	// Client is injectable for tests; nil means a fresh client with a ~10s
	// timeout is used per poll.
	Client *http.Client
}

func (s *OptiClimateSource) Describe() string        { return "opticlimate" }
func (s *OptiClimateSource) Simulated() bool         { return false }
func (s *OptiClimateSource) Interval() time.Duration { return s.Every }

// Poll issues ONE GET, decodes the values loosely, and emits a Reading for
// every configured register whose value came back as a JSON number. Transport
// or HTTP failures return an error (so the caller logs and skips this tick);
// individual non-numeric values never produce an error.
func (s *OptiClimateSource) Poll(now time.Time) ([]Reading, error) {
	names := make([]string, 0, len(s.Registers))
	for _, r := range s.Registers {
		if r.Name != "" {
			names = append(names, r.Name)
		}
	}
	idsJSON, err := json.Marshal(names)
	if err != nil {
		return nil, fmt.Errorf("opticlimate: encode ids: %w", err)
	}
	endpoint := fmt.Sprintf("%s/backend/getRegisterValues?address=%d&ids=%s",
		s.URL, s.Address, url.QueryEscape(string(idsJSON)))

	client := s.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := client.Get(endpoint) // GET only - never a write
	if err != nil {
		return nil, fmt.Errorf("opticlimate get: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("opticlimate: HTTP %d from %s", resp.StatusCode, s.URL)
	}

	var payload struct {
		GetRegisterValues struct {
			Address int `json:"address"`
			Values  map[string]struct {
				Value json.RawMessage `json:"value"`
			} `json:"values"`
		} `json:"getRegisterValues"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("opticlimate decode: %w", err)
	}

	ts := now.UTC().Format(time.RFC3339)
	var out []Reading
	for _, r := range s.Registers {
		if r.Name == "" || r.Metric == "" {
			continue
		}
		v, ok := payload.GetRegisterValues.Values[r.Name]
		if !ok {
			continue // controller did not return this register
		}
		f, ok := numericValue(v.Value)
		if !ok {
			continue // "Disconnected", null, or any non-number: skip, no error
		}
		out = append(out, Reading{
			SensorID:    s.Zone + ":" + r.Metric,
			Type:        r.Metric,
			Ts:          ts,
			Value:       round1(f),
			ValueOrigin: "measured",
		})
	}
	return out, nil
}

// numericValue reports whether a raw register value is a JSON number and, if
// so, its float. A string ("Disconnected"), null, or bool decodes to a
// non-float64 interface and is rejected - this is the guard that keeps one
// absent probe from poisoning the whole poll.
func numericValue(raw json.RawMessage) (float64, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	var v interface{}
	if err := json.Unmarshal(raw, &v); err != nil {
		return 0, false
	}
	f, ok := v.(float64)
	return f, ok
}
