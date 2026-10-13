package main

import (
	"strings"
	"testing"
	"time"

	"github.com/neda-dvb/growgoddess-pi/internal/gateway"
)

// testGates lets every controller address through, so a test reaches the
// gate it is about.
func testGates(allowControl bool) commandGates {
	return commandGates{AllowControl: allowControl, HostAllowed: func(string) bool { return true }}
}

// TestPerformCommandGates pins the two gates that need no controller: a
// write without allowControl is refused, and an expired command is never
// performed, whatever its kind.
func TestPerformCommandGates(t *testing.T) {
	now := time.Date(2026, 9, 10, 16, 0, 0, 0, time.UTC)
	ctrl := agentController{Vendor: "opticlimate", Host: "192.0.2.1", Port: 4001}

	ok, _, msg := performCommand(agentCommand{Kind: "set_setpoint", Controller: ctrl,
		Payload:   map[string]any{"setpoint": "temp_day", "value": 28.0},
		ExpiresAt: now.Add(time.Minute).Format(time.RFC3339)}, testGates(false), now)
	if ok || msg == "" || msg[:len("control is disabled")] != "control is disabled" {
		t.Fatalf("write without allowControl must be refused, got ok=%v msg=%q", ok, msg)
	}

	ok, _, msg = performCommand(agentCommand{Kind: "set_setpoint", Controller: ctrl,
		Payload:   map[string]any{"setpoint": "temp_day", "value": 28.0},
		ExpiresAt: now.Add(-time.Second).Format(time.RFC3339)}, testGates(true), now)
	if ok || msg != "the command expired before the gateway performed it" {
		t.Fatalf("expired write must not run, got ok=%v msg=%q", ok, msg)
	}

	ok, _, msg = performCommand(agentCommand{Kind: "test", Controller: agentController{Vendor: "trolmaster", Host: "x"}}, testGates(true), now)
	if ok || msg != "unsupported controller" {
		t.Fatalf("unknown vendor must be refused, got ok=%v msg=%q", ok, msg)
	}

	ok, _, msg = performCommand(agentCommand{Kind: "set_setpoint", Controller: ctrl, Payload: map[string]any{"setpoint": "temp_day"}}, testGates(true), now)
	if ok || msg != "malformed setpoint command" {
		t.Fatalf("missing value must be refused before any network call, got ok=%v msg=%q", ok, msg)
	}
}

// TestSameTarget: a binding that only gained its MAC is the same running
// source; a changed host is not.
func TestSameTarget(t *testing.T) {
	a := agentController{Vendor: "opticlimate", Host: "192.168.2.110", Port: 4001}
	b := a
	b.MAC = "24:18:c6:20:89:91"
	if !sameTarget(a, b) {
		t.Error("a learned MAC must not restart the source")
	}
	c := a
	c.Host = "192.168.2.114"
	if sameTarget(a, c) {
		t.Error("a new host is a new target")
	}
	// a discover command needs no host; every other kind does
	now := time.Now()
	if ok, _, msg := performCommand(agentCommand{Kind: "read_settings", Controller: agentController{Vendor: "opticlimate"}}, testGates(true), now); ok || msg != "no controller address" {
		t.Errorf("read_settings without a host must be refused, got %v %q", ok, msg)
	}
}

func TestCommandExpired(t *testing.T) {
	now := time.Date(2026, 9, 10, 16, 0, 0, 0, time.UTC)
	if commandExpired(agentCommand{}, now) {
		t.Error("no expiry means not expired")
	}
	if commandExpired(agentCommand{ExpiresAt: "garbage"}, now) {
		t.Error("an unparsable expiry must not block (the backend already enforces it)")
	}
	if !commandExpired(agentCommand{ExpiresAt: "2026-09-10T15:59:59Z"}, now) {
		t.Error("a past expiry is expired")
	}
	if commandExpired(agentCommand{ExpiresAt: "2026-09-10T16:00:01Z"}, now) {
		t.Error("a future expiry is not expired")
	}
}

// TestObservedCapabilities: reads are what answered, writes exist only with
// allowControl, the CO2 setpoint stays unknown either way.
func TestObservedCapabilities(t *testing.T) {
	rs := []gateway.Reading{{Type: "rh"}, {Type: "air_temp"}, {Type: "rh"}, {Type: "temp_setpoint"}}
	off := observedCapabilities(rs, false)
	if strings.Join(off.Read, ",") != "air_temp,rh,temp_setpoint" || len(off.Write) != 0 || off.Control {
		t.Fatalf("control off: %+v", off)
	}
	on := observedCapabilities(rs, true)
	if strings.Join(on.Write, ",") != "rh_day,rh_night,temp_day,temp_night" || !on.Control {
		t.Fatalf("control on: %+v", on)
	}
	if strings.Join(on.Unknown, ",") != "co2_setpoint" {
		t.Fatalf("unknown = %v", on.Unknown)
	}
}

// TestCommandHostValidation: a command reaches only an address inside the
// gateway's controller networks or one its own scan found, whatever the
// backend sends; a refused address is refused before any network call.
func TestCommandHostValidation(t *testing.T) {
	now := time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC)
	nets, err := gateway.ParseControllerNetworks([]string{"192.168.2.0/24"})
	if err != nil {
		t.Fatal(err)
	}
	found := &hostSet{}
	g := commandGates{AllowControl: false, Discovered: found,
		HostAllowed: func(h string) bool { return gateway.HostInNetworks(h, nets) || found.has(h) }}
	refused := "the controller address is outside this gateway's controller networks"
	for _, host := range []string{"8.8.8.8", "127.0.0.1", "192.168.3.7", "evil.example.com", "169.254.169.254"} {
		for _, kind := range []string{"test", "read_settings", "set_setpoint"} {
			ok, _, msg := performCommand(agentCommand{Kind: kind, Controller: agentController{Vendor: "opticlimate", Host: host},
				Payload: map[string]any{"setpoint": "temp_day", "value": 25.0}}, g, now)
			if ok || msg != refused {
				t.Errorf("%s to %s: ok=%v %q, want refused", kind, host, ok, msg)
			}
		}
	}
	// inside the network: the host gate passes and the next gate speaks
	ok, _, msg := performCommand(agentCommand{Kind: "set_setpoint", Controller: agentController{Vendor: "opticlimate", Host: "192.168.2.110"},
		Payload: map[string]any{"setpoint": "temp_day", "value": 25.0}}, g, now)
	if ok || msg != "control is disabled on this gateway (allowControl is off)" {
		t.Fatalf("a host in the network must pass the host gate, got %v %q", ok, msg)
	}
	// a box the gateway found itself is reachable even outside the list
	found.add("192.168.3.7")
	_, _, msg = performCommand(agentCommand{Kind: "set_setpoint", Controller: agentController{Vendor: "opticlimate", Host: "192.168.3.7"},
		Payload: map[string]any{"setpoint": "temp_day", "value": 25.0}}, g, now)
	if msg == refused {
		t.Fatal("a discovered box must pass the host gate")
	}
	// no host function: every address is refused
	if _, _, msg := performCommand(agentCommand{Kind: "test", Controller: agentController{Vendor: "opticlimate", Host: "192.168.2.110"}}, commandGates{}, now); msg != refused {
		t.Fatalf("missing host gate must refuse, got %q", msg)
	}
}

// TestGuardBandRefusalOnTheGateway: with control on, a setpoint outside the
// gateway's own band is refused even though the backend asked for it.
func TestGuardBandRefusalOnTheGateway(t *testing.T) {
	now := time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC)
	g := testGates(true)
	// 192.0.2.1 (TEST-NET) answers nothing: a refusal here can only come from the band
	for _, c := range []struct {
		key   string
		value float64
	}{{"temp_day", 35}, {"temp_night", 10}, {"rh_day", 95}, {"rh_night", 20}} {
		ok, _, msg := performCommand(agentCommand{Kind: "set_setpoint", Controller: agentController{Vendor: "opticlimate", Host: "192.0.2.1"},
			Payload: map[string]any{"setpoint": c.key, "value": c.value}}, g, now)
		if ok || !strings.Contains(msg, "outside the gateway's guard band") {
			t.Errorf("%s=%g: ok=%v %q, want a guard band refusal", c.key, c.value, ok, msg)
		}
	}
	g.Band = gateway.GuardBand{TempMin: 20, TempMax: 26}
	if _, _, msg := performCommand(agentCommand{Kind: "set_setpoint", Controller: agentController{Vendor: "opticlimate", Host: "192.0.2.1"},
		Payload: map[string]any{"setpoint": "temp_day", "value": 27.0}}, g, now); !strings.Contains(msg, "guard band") {
		t.Errorf("a configured band applies, got %q", msg)
	}
}
