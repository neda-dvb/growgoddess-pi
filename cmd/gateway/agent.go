// Facility-agent mode for the Cannabits Edge Gateway.
//
// Instead of a static per-zone config, the agent pulls its room/controller
// bindings from the backend (GET /api/pro/plans/:id/gateway/config, X-Api-Key)
// every ConfigPollSeconds, and maintains one read-only source per connected
// room — each streaming to its OWN zone. One Pi at the facility serves every
// room on the LAN; adding/removing a controller in the UI takes effect on the
// next poll, no restart. The spool, client, batch and telemetry POST are the
// exact same proven path as static mode. Read-only: it never controls.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/neda-dvb/growgoddess-pi/internal/gateway"
)

type agentController struct {
	Vendor  string `json:"vendor"`
	Host    string `json:"host"`
	Port    int    `json:"port"`
	Address int    `json:"address"`
}

type agentRoom struct {
	ZoneID     string           `json:"zoneId"`
	Controller *agentController `json:"controller"`
}

// agentDevice is one zone-scoped cloud sensor (e.g. Govee) the agent polls.
type agentDevice struct {
	DeviceID   string `json:"deviceId"`
	ZoneID     string `json:"zoneId"`
	RoomID     string `json:"roomId"`
	Vendor     string `json:"vendor"`
	SKU        string `json:"sku"`
	ExternalID string `json:"externalId"`
	SourceUnit string `json:"sourceUnit"`
}

type agentConfigResp struct {
	PlanID      string        `json:"planId"`
	Rooms       []agentRoom   `json:"rooms"`
	Devices     []agentDevice `json:"devices"`
	GoveeAPIKey string        `json:"goveeApiKey"`
}

func fetchGatewayConfig(api, planID, key string) (agentConfigResp, error) {
	url := fmt.Sprintf("%s/api/pro/plans/%s/gateway/config", strings.TrimRight(api, "/"), planID)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return agentConfigResp{}, err
	}
	req.Header.Set("X-Api-Key", key)
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return agentConfigResp{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return agentConfigResp{}, fmt.Errorf("config HTTP %d", resp.StatusCode)
	}
	var out agentConfigResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return agentConfigResp{}, err
	}
	return out, nil
}

func runAgent(cfg gateway.Config, key string) {
	if cfg.API == "" || cfg.PlanID == "" || key == "" {
		log.Fatalf("agent mode needs api, planId and a key (or keyFile)")
	}
	spool, err := gateway.NewSpool(cfg.SpoolDir, 0)
	if err != nil {
		log.Fatalf("spool: %v", err)
	}
	client := gateway.NewClient(cfg.API, cfg.PlanID, key, cfg.GatewayLabel)

	flushEvery := time.Duration(cfg.FlushSeconds) * time.Second
	if flushEvery <= 0 {
		flushEvery = time.Minute
	}
	pollEvery := time.Duration(cfg.ConfigPollSeconds) * time.Second
	if pollEvery <= 0 {
		pollEvery = 15 * time.Second
	}
	const sampleEvery = 60 * time.Second // per-controller read cadence

	log.Printf("gateway agent %s: plan %s, config poll %s, flush %s, spool %s",
		cfg.GatewayLabel, cfg.PlanID, pollEvery, flushEvery, cfg.SpoolDir)

	// Govee (and future cloud vendors) need a vendor API key. The agent holds
	// it in its own environment; the backend never serves it in the config.
	goveeKey := strings.TrimSpace(os.Getenv("GOVEE_API_KEY"))

	readings := make(chan []gateway.Reading, 64)
	type running struct {
		ctrl agentController
		stop chan struct{}
	}
	active := map[string]*running{}
	type runningDevice struct {
		dev  agentDevice
		stop chan struct{}
	}
	activeDevices := map[string]*runningDevice{}

	reconcile := func() {
		conf, err := fetchGatewayConfig(cfg.API, cfg.PlanID, key)
		if err != nil {
			log.Printf("agent config poll: %v", err)
			return
		}
		want := map[string]agentController{}
		for _, r := range conf.Rooms {
			if r.Controller != nil && strings.ToLower(r.Controller.Vendor) == "opticlimate" && r.Controller.Host != "" {
				want[r.ZoneID] = *r.Controller
			}
		}
		// stop sources that were removed or whose controller changed
		for zone, run := range active {
			if w, ok := want[zone]; !ok || w != run.ctrl {
				close(run.stop)
				delete(active, zone)
				log.Printf("agent: stopped source for zone %s", zone)
			}
		}
		// start sources for newly connected rooms
		for zone, w := range want {
			if _, ok := active[zone]; ok {
				continue
			}
			port := w.Port
			if port == 0 {
				port = 4001
			}
			src := &gateway.OptiClimateSource{
				Zone:      zone,
				URL:       fmt.Sprintf("http://%s:%d", w.Host, port),
				Address:   w.Address,
				Registers: gateway.OptiClimateDefaultRegisters(),
				Every:     sampleEvery,
			}
			stop := make(chan struct{})
			active[zone] = &running{ctrl: w, stop: stop}
			go runSourceLoop(src, readings, stop)
			log.Printf("agent: streaming %s -> zone %s", src.URL, zone)
		}

		// ── zone sensors (Govee) ──
		// Prefer the facility's connected key (served in the config); fall back
		// to the gateway's own GOVEE_API_KEY environment variable.
		effGoveeKey := goveeKey
		if conf.GoveeAPIKey != "" {
			effGoveeKey = conf.GoveeAPIKey
		}
		wantDev := map[string]agentDevice{}
		for _, d := range conf.Devices {
			if strings.ToLower(d.Vendor) == "govee" && d.ExternalID != "" && d.SKU != "" {
				wantDev[d.DeviceID] = d
			}
		}
		for id, run := range activeDevices {
			if w, ok := wantDev[id]; !ok || w != run.dev {
				close(run.stop)
				delete(activeDevices, id)
				log.Printf("agent: stopped device %s", id)
			}
		}
		for id, d := range wantDev {
			if _, ok := activeDevices[id]; ok {
				continue
			}
			if effGoveeKey == "" {
				log.Printf("agent: skipping Govee %s (%s) — no Govee key (connect in-app or set GOVEE_API_KEY)", d.SKU, d.ExternalID)
				continue
			}
			src := &gateway.GoveeSource{
				Zone: d.RoomID, ZoneID: d.ZoneID, DeviceID: d.DeviceID,
				APIKey: effGoveeKey, SKU: d.SKU, Device: d.ExternalID,
				SourceUnit: d.SourceUnit, Every: sampleEvery,
			}
			stop := make(chan struct{})
			activeDevices[id] = &runningDevice{dev: d, stop: stop}
			go runSourceLoop(src, readings, stop)
			log.Printf("agent: streaming Govee %s -> room %s / zone %s", d.SKU, d.RoomID, d.ZoneID)
		}
	}

	reconcile()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	flush := time.NewTicker(flushEvery)
	defer flush.Stop()
	poll := time.NewTicker(pollEvery)
	defer poll.Stop()
	// Test Connection requests are polled more often so the UI feels responsive.
	cmds := time.NewTicker(5 * time.Second)
	defer cmds.Stop()
	var pending []gateway.Reading

	flushNow := func() {
		if len(pending) > 0 {
			batch := gateway.Batch{
				ID: gateway.BatchID(cfg.GatewayLabel, time.Now()), DataMode: "live",
				Created: time.Now().UTC(), Readings: pending,
			}
			pending = nil
			if dropped, err := spool.Put(batch); err != nil {
				log.Printf("spool write: %v", err)
			} else if len(dropped) > 0 {
				log.Printf("SPOOL FULL: dropped %d oldest batch(es): %v", len(dropped), dropped)
			}
		}
		drain(spool, client)
	}

	for {
		select {
		case rs := <-readings:
			pending = append(pending, rs...)
		case <-poll.C:
			reconcile()
		case <-cmds.C:
			pollCommands(cfg.API, cfg.PlanID, key)
		case <-flush.C:
			flushNow()
		case <-sig:
			log.Printf("agent shutting down: final flush")
			flushNow()
			return
		}
	}
}

// ── Test Connection: read-only controller probe on demand ──

type agentTest struct {
	TestID     string          `json:"testId"`
	ZoneID     string          `json:"zoneId"`
	Controller agentController `json:"controller"`
}

func fetchGatewayCommands(api, planID, key string) ([]agentTest, error) {
	url := fmt.Sprintf("%s/api/pro/plans/%s/gateway/commands", strings.TrimRight(api, "/"), planID)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Api-Key", key)
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("commands HTTP %d", resp.StatusCode)
	}
	var out struct {
		Tests []agentTest `json:"tests"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out.Tests, nil
}

// performControllerTest does the existing read-only OptiClimate read and
// returns the canonical metrics that came back numeric. It never writes.
func performControllerTest(ctrl agentController) ([]string, error) {
	port := ctrl.Port
	if port == 0 {
		port = 4001
	}
	src := &gateway.OptiClimateSource{
		Zone: "test", URL: fmt.Sprintf("http://%s:%d", ctrl.Host, port),
		Address: ctrl.Address, Registers: gateway.OptiClimateDefaultRegisters(), Every: time.Minute,
	}
	rs, err := src.Poll(time.Now())
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	metrics := []string{}
	for _, r := range rs {
		if !seen[r.Type] {
			seen[r.Type] = true
			metrics = append(metrics, r.Type)
		}
	}
	return metrics, nil
}

func postGatewayResult(api, planID, key, testID string, ok bool, metrics []string, errMsg string) {
	url := fmt.Sprintf("%s/api/pro/plans/%s/gateway/commands/%s/result", strings.TrimRight(api, "/"), planID, testID)
	body, _ := json.Marshal(map[string]any{"ok": ok, "metrics": metrics, "error": errMsg})
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Key", key)
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		log.Printf("agent: post test result: %v", err)
		return
	}
	resp.Body.Close()
}

func pollCommands(api, planID, key string) {
	tests, err := fetchGatewayCommands(api, planID, key)
	if err != nil {
		return // quiet: commands poll is best-effort
	}
	for _, t := range tests {
		if strings.ToLower(t.Controller.Vendor) != "opticlimate" {
			postGatewayResult(api, planID, key, t.TestID, false, nil, "unsupported controller")
			continue
		}
		metrics, err := performControllerTest(t.Controller)
		if err != nil {
			log.Printf("agent: test %s (%s) failed: %v", t.TestID, t.Controller.Host, err)
			postGatewayResult(api, planID, key, t.TestID, false, nil, "could not reach the controller")
			continue
		}
		log.Printf("agent: test %s (%s) ok: %v", t.TestID, t.Controller.Host, metrics)
		postGatewayResult(api, planID, key, t.TestID, true, metrics, "")
	}
}

// runSourceLoop samples one source on its cadence into the shared channel
// until told to stop (when its room's controller is removed or changed).
func runSourceLoop(s gateway.Source, out chan<- []gateway.Reading, stop <-chan struct{}) {
	tick := time.NewTicker(s.Interval())
	defer tick.Stop()
	poll := func() {
		rs, err := s.Poll(time.Now())
		if err != nil {
			log.Printf("%s: %v", s.Describe(), err)
			return
		}
		select {
		case out <- rs:
		case <-stop:
		}
	}
	poll()
	for {
		select {
		case <-tick.C:
			poll()
		case <-stop:
			return
		}
	}
}
