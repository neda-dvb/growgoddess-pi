package gateway

// OptiClimateControl: the WRITE path to an OptiClimate / Airsupplies
// controller. Deliberately a separate type from OptiClimateSource, which
// stays read-only: control is a separate contract with separate consent
// (the gateway's allowControl flag, the platform's guard band, the owner's
// explicit request).
//
// The controller exposes writes through its own HMI backend:
//
//	POST {URL}/backend/setRegisterValues?address={Address}
//	{"address":0,"values":{"Room1TempWntdDay":28}}
//
// and answers {"setRegisterValues":{"values":{...}}} or, when it refuses,
// {"setRegisterValues":{"requestError":["..."]}}. This adapter writes ONE
// register per call, then reads it back with the same GET the source uses;
// a write is confirmed only when the read-back equals the requested value.
//
// Before writing a temperature setpoint it also reads the controller's own
// alarm limits for that period (Room{Day,Night}TempMin/Max) and refuses a
// value outside them: those limits were set on the controller by the
// operator and a setpoint the controller would alarm on makes no sense.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

// OptiClimateSetpointRegisters maps the platform's canonical setpoint keys to
// the controller's register names. The platform never sees register names.
var OptiClimateSetpointRegisters = map[string]string{
	"temp_day":   "Room1TempWntdDay",
	"temp_night": "Room1TempWntdNight",
	"rh_day":     "HumiSetPointDay",
	"rh_night":   "HumiSetPointNight",
}

// optiClimateAlarmLimits names the controller's own [min, max] alarm registers
// that bound a setpoint. Humidity setpoints have none on this controller.
var optiClimateAlarmLimits = map[string][2]string{
	"temp_day":   {"RoomDayTempMin", "RoomDayTempMax"},
	"temp_night": {"RoomNightTempMin", "RoomNightTempMax"},
}

// ErrOutsideControllerLimits: the value violates the controller's own alarm
// limits; nothing was written.
var ErrOutsideControllerLimits = errors.New("outside the controller's own alarm limits")

// OptiClimateControl performs writes against one controller.
type OptiClimateControl struct {
	URL     string // controller base URL, e.g. http://192.168.2.110:4001
	Address int
	Client  *http.Client // nil: a fresh client with a 10s timeout
	// The controller applies a write asynchronously: the web box accepts the
	// POST at once and forwards it to the unit, and a GET issued right after
	// still returns the old value (observed live: 29 read 97 ms after 27 was
	// written, 27 from about a second later). The read-back therefore polls
	// ReadbackAttempts times, ReadbackDelay apart, until the value matches.
	// Zero means the defaults: 8 attempts, 1.5 s apart (12 s in total).
	ReadbackAttempts int
	ReadbackDelay    time.Duration
}

func (c *OptiClimateControl) readbackPlan() (int, time.Duration) {
	attempts, delay := c.ReadbackAttempts, c.ReadbackDelay
	if attempts <= 0 {
		attempts = 8
	}
	if delay <= 0 {
		delay = 1500 * time.Millisecond
	}
	return attempts, delay
}

func (c *OptiClimateControl) client() *http.Client {
	if c.Client != nil {
		return c.Client
	}
	return &http.Client{Timeout: 10 * time.Second}
}

// OptiClimateSettings is what the controller holds right now: the four
// setpoints, the control program it is running (Day, Night, Pre-heat,
// Cool-down), its time mode, and the alarm limits that bound temperature.
type OptiClimateSettings struct {
	Setpoints   map[string]float64    `json:"setpoints"`
	Program     string                `json:"program"`
	TimeMode    string                `json:"timeMode"`
	AlarmLimits map[string][2]float64 `json:"alarmLimits,omitempty"`
	ReadAt      string                `json:"readAt"`
}

// ReadSettings reads every setpoint, the current program and the alarm
// limits in one GET. A non-numeric setpoint is simply absent.
func (c *OptiClimateControl) ReadSettings(now time.Time) (OptiClimateSettings, error) {
	names := []string{"ContrStatus", "TimeMode"}
	for _, reg := range OptiClimateSetpointRegisters {
		names = append(names, reg)
	}
	for _, pair := range optiClimateAlarmLimits {
		names = append(names, pair[0], pair[1])
	}
	sort.Strings(names)
	values, err := optiClimateGetValues(c.client(), c.URL, c.Address, names)
	if err != nil {
		return OptiClimateSettings{}, err
	}
	out := OptiClimateSettings{
		Setpoints:   map[string]float64{},
		AlarmLimits: map[string][2]float64{},
		ReadAt:      now.UTC().Format(time.RFC3339),
	}
	for key, reg := range OptiClimateSetpointRegisters {
		if f, ok := numericValue(values[reg]); ok {
			out.Setpoints[key] = f
		}
	}
	for key, pair := range optiClimateAlarmLimits {
		lo, okLo := numericValue(values[pair[0]])
		hi, okHi := numericValue(values[pair[1]])
		if okLo && okHi {
			out.AlarmLimits[key] = [2]float64{lo, hi}
		}
	}
	out.Program = stringValue(values["ContrStatus"])
	out.TimeMode = stringValue(values["TimeMode"])
	return out, nil
}

// WriteResult is a confirmed setpoint write.
type WriteResult struct {
	Setpoint string  `json:"setpoint"`
	Register string  `json:"register"`
	Previous float64 `json:"previous"`
	Value    float64 `json:"value"`
	Readback float64 `json:"readback"`
}

// SetSetpoint writes one canonical setpoint and confirms it by read-back.
// Order: read previous + alarm limits, check limits, POST, GET read-back.
// Any error before the POST means nothing was written.
func (c *OptiClimateControl) SetSetpoint(key string, value float64) (WriteResult, error) {
	reg, ok := OptiClimateSetpointRegisters[key]
	if !ok {
		return WriteResult{}, fmt.Errorf("opticlimate control: unknown setpoint %q", key)
	}
	names := []string{reg}
	limits, hasLimits := optiClimateAlarmLimits[key]
	if hasLimits {
		names = append(names, limits[0], limits[1])
	}
	before, err := optiClimateGetValues(c.client(), c.URL, c.Address, names)
	if err != nil {
		return WriteResult{}, err
	}
	previous, ok := numericValue(before[reg])
	if !ok {
		return WriteResult{}, fmt.Errorf("opticlimate control: %s is not readable on this controller", reg)
	}
	if hasLimits {
		lo, okLo := numericValue(before[limits[0]])
		hi, okHi := numericValue(before[limits[1]])
		if okLo && okHi && (value < lo || value > hi) {
			return WriteResult{}, fmt.Errorf("%w (%g to %g)", ErrOutsideControllerLimits, lo, hi)
		}
	}

	body, _ := json.Marshal(map[string]any{"address": c.Address, "values": map[string]float64{reg: value}})
	endpoint := fmt.Sprintf("%s/backend/setRegisterValues?address=%d", c.URL, c.Address)
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return WriteResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client().Do(req)
	if err != nil {
		return WriteResult{}, fmt.Errorf("opticlimate write: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return WriteResult{}, fmt.Errorf("opticlimate write: HTTP %d", resp.StatusCode)
	}
	var reply struct {
		SetRegisterValues struct {
			RequestError json.RawMessage `json:"requestError"`
		} `json:"setRegisterValues"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&reply); err != nil {
		return WriteResult{}, fmt.Errorf("opticlimate write decode: %w", err)
	}
	if msg := requestErrorText(reply.SetRegisterValues.RequestError); msg != "" {
		return WriteResult{}, fmt.Errorf("controller refused the write: %s", msg)
	}

	// read-back: the unit applies the write asynchronously, so poll until it
	// reports the new value or the attempts run out
	attempts, delay := c.readbackPlan()
	var last string
	for i := 0; i < attempts; i++ {
		if i > 0 {
			time.Sleep(delay)
		}
		after, err := optiClimateGetValues(c.client(), c.URL, c.Address, []string{reg})
		if err != nil {
			last = "unreadable (" + err.Error() + ")"
			continue
		}
		last = strings.TrimSpace(string(after[reg]))
		if readback, ok := numericValue(after[reg]); ok && readback == value {
			return WriteResult{Setpoint: key, Register: reg, Previous: previous, Value: value, Readback: readback}, nil
		}
	}
	return WriteResult{}, fmt.Errorf("controller still reports %s after writing %g", last, value)
}

// requestErrorText flattens the controller's requestError (an array of
// strings, a string, or absent) into one message; "" means no error.
func requestErrorText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var list []string
	if err := json.Unmarshal(raw, &list); err == nil {
		return strings.Join(list, "; ")
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return string(raw)
}

// stringValue returns a string register value ("Day"), or "" for anything
// else.
func stringValue(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return s
}
