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
	"log"
	"net"
	"net/http"
	"net/url"
	"sort"
	"time"
)

// OptiClimateSource reads named registers from one controller over HTTP.
// OptiClimateDefaultRegisters is the known OptiClimate/Revomax register→metric
// mapping. The adapter owns it, so the operator never enters register names:
// binding an OptiClimate controller to a room is enough. Read-only.
//
// CO2: the controller has a CO2 input (CO2In), a CO2 setpoint and a dosing
// output, but only when a CO2 sensor is connected AND the CO2 function is
// enabled do they mean anything. CO2In reads "Disconnected" without a sensor
// (skipped like any non-number); the setpoint and the dosing state are
// emitted only while CO2Enable is true, so a facility never sees a factory
// default setpoint of 400 ppm or a "dosing" flag for a function that is off.
func OptiClimateDefaultRegisters() []ModbusRegisterMap {
	return []ModbusRegisterMap{
		{Name: "Room1Temp", Metric: "air_temp"},
		{Name: "Humidity", Metric: "rh"},
		// the ACTIVE setpoints: the day pair while the controller runs its day
		// program, the night pair at night. During its transitions (Pre-heat,
		// Cool-down) neither is emitted and the platform's step line carries
		// the last known value. ContrStatus is requested as the guard only.
		{Name: "ContrStatus"},
		{Name: "Room1TempWntdDay", Metric: "temp_setpoint", OnlyWhenRegister: "ContrStatus", OnlyWhenEquals: "Day"},
		{Name: "Room1TempWntdNight", Metric: "temp_setpoint", OnlyWhenRegister: "ContrStatus", OnlyWhenEquals: "Night"},
		{Name: "HumiSetPointDay", Metric: "rh_setpoint", OnlyWhenRegister: "ContrStatus", OnlyWhenEquals: "Day"},
		{Name: "HumiSetPointNight", Metric: "rh_setpoint", OnlyWhenRegister: "ContrStatus", OnlyWhenEquals: "Night"},
		{Name: "CO2In", Metric: "co2"},
		{Name: "CO2Enable"}, // guard only, never emitted
		{Name: "CO2Setpoint", Metric: "co2_setpoint", OnlyWhenTrue: "CO2Enable"},
		{Name: "CO2OutDig", Metric: "co2_dosing_state", Boolean: true, OnlyWhenTrue: "CO2Enable"},
		// the light cell: on/off as the controller judges it, and its relative
		// level in percent (not PPFD, not lux; the cell's own scale)
		{Name: "LightCell", Metric: "light_state", Boolean: true},
		{Name: "LightSensor", Metric: "light_level"},
		// the equipment: what the unit is doing right now
		{Name: "PwrRelays", Metric: "cooling_state", Boolean: true},       // compressor on/off
		{Name: "Compressor", Metric: "cooling_output"},                    // compressor percentage
		{Name: "Heater", Metric: "heating_state", NonZeroAsState: true},   // heater percentage > 0
		{Name: "dehumidify", Metric: "dehumidifier_state", Boolean: true}, // de-humidify active
		{Name: "AirSpeed", Metric: "fan_output"},                          // air speed flow percentage
	}
}

// optiClimateLinkRegister is the network box's own report of its connection
// to the air unit: "Ok", or one of "Lost", "LostNotify", "Missing",
// "MissingNotify". The box that answers on the LAN is a display computer; the
// air unit hangs behind it on a cable. When that link is down the box keeps
// answering, with defaults: Room1Temp 0.0, Humidity 50.0, every other
// temperature 20.0, all versions 0 (Cannaru Room 8, 2026-09-21). Those are
// not measurements, and Humidity 50.0 even sits inside the valid range, so
// while the link is not "Ok" the source emits nothing at all.
const optiClimateLinkRegister = "AircoConnectionStatus"

// optiClimateLinkReminder is how often a STANDING loss is reported again, so
// a chart window opened days later still holds the reason the room is empty.
const optiClimateLinkReminder = 6 * time.Hour

type OptiClimateSource struct {
	Zone      string
	URL       string              // controller base URL, e.g. http://192.168.2.110:4001
	Address   int                 // Modbus unit address behind the controller
	Registers []ModbusRegisterMap // Name -> Metric; Factor/Slave/Table unused here
	Every     time.Duration
	// Client is injectable for tests; nil means a fresh client with a ~10s
	// timeout is used per poll.
	Client *http.Client
	// alarms remembers which controller alarms were active at the last event
	// poll, so only transitions become events. nil until the first poll.
	alarms map[string]bool
	// link is the last link status the EVENT poll saw ("" = none yet), and
	// linkReported when a loss was last reported. Touched only by PollEvents.
	link         string
	linkReported time.Time
	// linkLogged is the last link status the READ poll logged, so the journal
	// gets one line per change, not one per minute. Touched only by Poll.
	linkLogged string
}

// optiClimateLink asks the box for its link to the air unit. known is false
// when the box does not answer, returns no string, or does not have the
// register: a box answers a requestError for the WHOLE call when one name is
// unknown, which is why this is its own small call and never rides on the
// readings request. Unknown never blocks a room's readings.
func optiClimateLink(client *http.Client, baseURL string, address int) (status string, known bool) {
	values, err := optiClimateGetValues(client, baseURL, address, []string{optiClimateLinkRegister})
	if err != nil {
		return "", false
	}
	status = stringValue(values[optiClimateLinkRegister])
	return status, status != ""
}

// optiClimateAlarm is one entry of getAlarms.
type optiClimateAlarm struct {
	Occurrences    int   `json:"occurrences"`
	Active         bool  `json:"active"`
	FirstOccurence int64 `json:"firstOccurrence"`
	LastOccurrence int64 `json:"lastOccurrence"`
}

// PollEvents reads the controller's alarm table and turns every change of an
// alarm's active flag into one chart event (kind "note", payload
// {event: "controller_alarm", alarm, active, occurrences}). The first poll
// reports the alarms that are active at that moment, so a restart never
// hides a standing alarm; a cleared alarm is reported once, when it clears.
func (s *OptiClimateSource) PollEvents(now time.Time) ([]Event, error) {
	client := s.Client
	if client == nil {
		client = optiClimateHTTPClient()
	}
	resp, err := client.Get(fmt.Sprintf("%s/backend/getAlarms?address=%d", s.URL, s.Address))
	if err != nil {
		return nil, fmt.Errorf("opticlimate alarms: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("opticlimate alarms: HTTP %d", resp.StatusCode)
	}
	var payload struct {
		GetAlarms struct {
			Alarms map[string]map[string]optiClimateAlarm `json:"alarms"`
		} `json:"getAlarms"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("opticlimate alarms decode: %w", err)
	}
	current := map[string]bool{}
	details := map[string]optiClimateAlarm{}
	for _, byName := range payload.GetAlarms.Alarms { // keyed by unit address
		for name, a := range byName {
			current[name] = a.Active
			details[name] = a
		}
	}
	first := s.alarms == nil
	var out []Event
	names := make([]string, 0, len(current))
	for n := range current {
		names = append(names, n)
	}
	sort.Strings(names)
	ts := now.UTC().Format(time.RFC3339)
	for _, name := range names {
		active := current[name]
		was, known := s.alarms[name]
		if (first && active) || (!first && (!known || was != active)) {
			a := details[name]
			out = append(out, Event{
				Kind: "note", ZoneID: s.Zone, OccurredAt: ts,
				Payload: map[string]any{
					"event": "controller_alarm", "alarm": name, "active": active,
					"occurrences": a.Occurrences, "lastOccurrence": a.LastOccurrence,
				},
				SourceRef: "opticlimate-alarm:" + name,
			})
		}
	}
	s.alarms = current
	if e, ok := s.linkEvent(client, now); ok {
		out = append(out, e)
	}
	return out, nil
}

// linkEvent turns the box's link to its air unit into chart events (kind
// "sensor_status", payload {event: "controller_link", status, ok}): a loss
// when it happens or is found standing on the first poll, again every
// optiClimateLinkReminder while it stands, and the recovery once. A link that
// is healthy from the start is silent, and so is an unknown status.
func (s *OptiClimateSource) linkEvent(client *http.Client, now time.Time) (Event, bool) {
	status, known := optiClimateLink(client, s.URL, s.Address)
	if !known {
		return Event{}, false
	}
	ok, wasOK := status == "Ok", s.link == "" || s.link == "Ok"
	s.link = status
	switch {
	case !ok && (wasOK || now.Sub(s.linkReported) >= optiClimateLinkReminder):
		s.linkReported = now
	case ok && !wasOK:
	default:
		return Event{}, false
	}
	return Event{
		Kind: "sensor_status", ZoneID: s.Zone, OccurredAt: now.UTC().Format(time.RFC3339),
		Payload:   map[string]any{"event": "controller_link", "status": status, "ok": ok},
		SourceRef: "opticlimate-link",
	}, true
}

func (s *OptiClimateSource) Describe() string        { return "opticlimate" }
func (s *OptiClimateSource) Simulated() bool         { return false }
func (s *OptiClimateSource) Interval() time.Duration { return s.Every }

// Poll issues ONE GET, decodes the values loosely, and emits a Reading for
// every configured register whose value came back as a JSON number. Transport
// or HTTP failures return an error (so the caller logs and skips this tick);
// individual non-numeric values never produce an error.
func (s *OptiClimateSource) Poll(now time.Time) ([]Reading, error) {
	// the box's link to its air unit first: without it every value below is
	// a default, so nothing is asked for and nothing is emitted. Not an error:
	// the box answers, so the agent must not start looking for it by MAC.
	if status, known := optiClimateLink(s.Client, s.URL, s.Address); known {
		if status != s.linkLogged {
			if status != "Ok" {
				log.Printf("opticlimate %s: the box reports %s %q: no link to the air unit, its values are defaults, nothing is recorded", s.Zone, optiClimateLinkRegister, status)
			} else if s.linkLogged != "" {
				log.Printf("opticlimate %s: link to the air unit is back, streaming again", s.Zone)
			}
			s.linkLogged = status
		}
		if status != "Ok" {
			return nil, nil
		}
	}
	names := make([]string, 0, len(s.Registers))
	for _, r := range s.Registers {
		if r.Name != "" {
			names = append(names, r.Name)
		}
	}
	values, err := optiClimateGetValues(s.Client, s.URL, s.Address, names) // GET only - never a write
	if err != nil {
		return nil, err
	}

	ts := now.UTC().Format(time.RFC3339)
	var out []Reading
	for _, r := range s.Registers {
		if r.Name == "" || r.Metric == "" {
			continue
		}
		if r.OnlyWhenTrue != "" && !boolValue(values[r.OnlyWhenTrue]) {
			continue // the function this register belongs to is disabled
		}
		if r.OnlyWhenRegister != "" && stringValue(values[r.OnlyWhenRegister]) != r.OnlyWhenEquals {
			continue // not the program this register applies to
		}
		v, ok := values[r.Name]
		if !ok {
			continue // controller did not return this register
		}
		var f float64
		if r.Boolean {
			if !isBool(v) {
				continue
			}
			f = 0
			if boolValue(v) {
				f = 1
			}
		} else if f, ok = numericValue(v); !ok {
			continue // "Disconnected", null, or any non-number: skip, no error
		} else if r.NonZeroAsState {
			if f > 0 {
				f = 1
			} else {
				f = 0
			}
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

// isBool reports whether a raw register value is JSON true or false.
func isBool(raw json.RawMessage) bool {
	var b bool
	return json.Unmarshal(raw, &b) == nil
}

// boolValue is true only for JSON true; anything else (false, null, a
// number, a string, absent) is false.
func boolValue(raw json.RawMessage) bool {
	var b bool
	return json.Unmarshal(raw, &b) == nil && b
}

// optiClimateGetValues is the one read call both the source and the control
// adapter use: GET getRegisterValues for the named registers, returning each
// raw value (a JSON number, a string such as "Disconnected", or null).
func optiClimateGetValues(client *http.Client, baseURL string, address int, names []string) (map[string]json.RawMessage, error) {
	idsJSON, err := json.Marshal(names)
	if err != nil {
		return nil, fmt.Errorf("opticlimate: encode ids: %w", err)
	}
	endpoint := fmt.Sprintf("%s/backend/getRegisterValues?address=%d&ids=%s",
		baseURL, address, url.QueryEscape(string(idsJSON)))
	if client == nil {
		client = optiClimateHTTPClient()
	}
	resp, err := client.Get(endpoint)
	if err != nil {
		return nil, fmt.Errorf("opticlimate get: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("opticlimate: HTTP %d from %s", resp.StatusCode, baseURL)
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
	out := make(map[string]json.RawMessage, len(payload.GetRegisterValues.Values))
	for name, v := range payload.GetRegisterValues.Values {
		out[name] = v.Value
	}
	return out, nil
}

// optiClimateHTTPClient is the client every controller call uses when none is
// injected. Keep-alives are OFF on purpose: the boxes sit on Wi-Fi and drop
// idle connections without a reset, and on 2026-09-13 a pooled half-dead
// connection made every poll time out for eight minutes while a fresh
// connection answered in a tenth of a second. One connection per call costs
// nothing at one poll a minute and can never go stale. Dial 3 s, headers 8 s,
// whole call 10 s.
func optiClimateHTTPClient() *http.Client {
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			DisableKeepAlives:     true,
			DialContext:           (&net.Dialer{Timeout: 3 * time.Second}).DialContext,
			ResponseHeaderTimeout: 8 * time.Second,
		},
	}
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
