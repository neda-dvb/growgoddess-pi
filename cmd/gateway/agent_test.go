package main

import (
	"testing"
	"time"
)

// TestPerformCommandGates pins the two gates that need no controller: a
// write without allowControl is refused, and an expired command is never
// performed, whatever its kind.
func TestPerformCommandGates(t *testing.T) {
	now := time.Date(2026, 9, 10, 16, 0, 0, 0, time.UTC)
	ctrl := agentController{Vendor: "opticlimate", Host: "192.0.2.1", Port: 4001}

	ok, _, msg := performCommand(agentCommand{Kind: "set_setpoint", Controller: ctrl,
		Payload:   map[string]any{"setpoint": "temp_day", "value": 28.0},
		ExpiresAt: now.Add(time.Minute).Format(time.RFC3339)}, false, now)
	if ok || msg == "" || msg[:len("control is disabled")] != "control is disabled" {
		t.Fatalf("write without allowControl must be refused, got ok=%v msg=%q", ok, msg)
	}

	ok, _, msg = performCommand(agentCommand{Kind: "set_setpoint", Controller: ctrl,
		Payload:   map[string]any{"setpoint": "temp_day", "value": 28.0},
		ExpiresAt: now.Add(-time.Second).Format(time.RFC3339)}, true, now)
	if ok || msg != "the command expired before the gateway performed it" {
		t.Fatalf("expired write must not run, got ok=%v msg=%q", ok, msg)
	}

	ok, _, msg = performCommand(agentCommand{Kind: "test", Controller: agentController{Vendor: "trolmaster", Host: "x"}}, true, now)
	if ok || msg != "unsupported controller" {
		t.Fatalf("unknown vendor must be refused, got ok=%v msg=%q", ok, msg)
	}

	ok, _, msg = performCommand(agentCommand{Kind: "set_setpoint", Controller: ctrl, Payload: map[string]any{"setpoint": "temp_day"}}, true, now)
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
	if ok, _, msg := performCommand(agentCommand{Kind: "read_settings", Controller: agentController{Vendor: "opticlimate"}}, true, now); ok || msg != "no controller address" {
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
