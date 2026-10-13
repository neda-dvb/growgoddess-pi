// Facility-agent mode for the Cannabits Edge Gateway.
//
// Instead of a static per-zone config, the agent pulls its room/controller
// bindings from the backend (GET /api/pro/plans/:id/gateway/config, X-Api-Key)
// every ConfigPollSeconds, and maintains one read-only source per connected
// room — each streaming to its OWN zone. One Pi at the facility serves every
// room on the LAN; adding/removing a controller in the UI takes effect on the
// next poll, no restart. The spool, client, batch and telemetry POST are the
// exact same proven path as static mode.
//
// Control: the agent also polls controller commands (test, read settings,
// set a setpoint). Writes run only with "allowControl": true in the config,
// and only inside the gateway's own guard band; see performCommand.
//
// The agent reaches a controller only at an address inside its controller
// networks (private ranges by default) or one its own LAN scan found, for a
// room binding from the config as for a command: the backend cannot point
// the Pi at the internet or at itself. The sync policy is the stricter of
// gateway.json's and the one the backend serves for this gateway.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/neda-dvb/growgoddess-pi/internal/gateway"
)

type agentController struct {
	Vendor  string `json:"vendor"`
	Host    string `json:"host"`
	Port    int    `json:"port"`
	Address int    `json:"address"`
	// MAC is the box's hardware address as the platform stores it (learned
	// from this gateway). It is identity, not target: a binding whose MAC
	// changes while host/port/address stay is the same running source.
	MAC string `json:"mac"`
}

// sameTarget reports whether two bindings reach the same box the same way.
func sameTarget(a, b agentController) bool {
	return a.Vendor == b.Vendor && a.Host == b.Host && a.Port == b.Port && a.Address == b.Address
}

// sourceHealth is one poll outcome of a room's controller source.
type sourceHealth struct {
	zone string
	ok   bool
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
	// set when this gateway authenticated with its own key
	GatewayID  string `json:"gatewayId"`
	SyncPolicy string `json:"syncPolicy"`
}

// hostSet is the controllers this gateway's own LAN scans found; safe for
// the scan goroutines.
type hostSet struct {
	mu    sync.Mutex
	hosts map[string]bool
}

func (h *hostSet) add(host string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.hosts == nil {
		h.hosts = map[string]bool{}
	}
	h.hosts[host] = true
}

func (h *hostSet) has(host string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.hosts[host]
}

// commandGates are the gateway's own refusals, whatever a command asks.
type commandGates struct {
	AllowControl bool
	Band         gateway.GuardBand
	// HostAllowed: may the gateway reach this controller address? nil
	// refuses every address.
	HostAllowed func(host string) bool
	// Discovered records the boxes a scan found (nil: not recorded).
	Discovered *hostSet
}

func (g commandGates) hostAllowed(host string) bool {
	return g.HostAllowed != nil && g.HostAllowed(host)
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
	nets, err := gateway.ParseControllerNetworks(cfg.ControllerNetworks)
	if err != nil {
		log.Fatalf("%v", err) // EdgeProblems already refused it; never reach any host on a bad list
	}
	discovered := &hostSet{}
	gates := commandGates{
		AllowControl: cfg.AllowControl, Band: cfg.GuardBand, Discovered: discovered,
		HostAllowed: func(host string) bool { return gateway.HostInNetworks(host, nets) || discovered.has(host) },
	}
	syncFilter := &gateway.SyncFilter{Policy: gateway.StricterPolicy(cfg.SyncPolicy, ""), Local: cfg.LocalStore()}
	refusedHost := map[string]string{} // zone -> refused host, so the log says it once

	flushEvery := time.Duration(cfg.FlushSeconds) * time.Second
	if flushEvery <= 0 {
		flushEvery = time.Minute
	}
	pollEvery := time.Duration(cfg.ConfigPollSeconds) * time.Second
	if pollEvery <= 0 {
		pollEvery = 15 * time.Second
	}
	const sampleEvery = 60 * time.Second // per-controller read cadence

	band := cfg.GuardBand.Effective()
	log.Printf("gateway agent %s: plan %s, config poll %s, flush %s, spool %s, control %s (guard band %g-%g °C, %g-%g %%RH), controller networks %v, sync %s",
		cfg.GatewayLabel, cfg.PlanID, pollEvery, flushEvery, cfg.SpoolDir, map[bool]string{true: "ENABLED", false: "disabled"}[cfg.AllowControl],
		band.TempMin, band.TempMax, band.RHMin, band.RHMax, nets, syncFilter.Policy)

	// Govee (and future cloud vendors) need a vendor API key. The agent holds
	// it in its own environment; the backend never serves it in the config.
	goveeKey := strings.TrimSpace(os.Getenv("GOVEE_API_KEY"))

	readings := make(chan []gateway.Reading, 64)
	events := make(chan []gateway.Event, 16)
	health := make(chan sourceHealth, 64)
	type running struct {
		ctrl agentController
		stop chan struct{}
	}
	active := map[string]*running{}
	// MAC learning + re-find: which zones already reported a learned MAC, how
	// many polls in a row failed per zone, and when a zone was last rescanned.
	learned := map[string]bool{}
	reportedCaps := map[string]string{} // zone -> signature of the last capabilities report
	fails := map[string]int{}
	lastRescan := map[string]time.Time{}
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
		if p := gateway.StricterPolicy(cfg.SyncPolicy, conf.SyncPolicy); p != syncFilter.Policy {
			log.Printf("agent: sync policy %s (gateway.json %q, platform %q)", p, cfg.SyncPolicy, conf.SyncPolicy)
			syncFilter.Policy = p
		}
		want := map[string]agentController{}
		for _, r := range conf.Rooms {
			if r.Controller != nil && strings.ToLower(r.Controller.Vendor) == "opticlimate" && r.Controller.Host != "" {
				if !gates.hostAllowed(r.Controller.Host) {
					if refusedHost[r.ZoneID] != r.Controller.Host {
						log.Printf("agent: REFUSED controller %s for zone %s: outside this gateway's controller networks", r.Controller.Host, r.ZoneID)
						refusedHost[r.ZoneID] = r.Controller.Host
					}
					continue
				}
				delete(refusedHost, r.ZoneID)
				want[r.ZoneID] = *r.Controller
			}
		}
		// stop sources that were removed or whose controller changed; a binding
		// that only gained a MAC keeps running and adopts it
		for zone, run := range active {
			if w, ok := want[zone]; !ok || !sameTarget(w, run.ctrl) {
				close(run.stop)
				delete(active, zone)
				delete(fails, zone)
				log.Printf("agent: stopped source for zone %s", zone)
			} else if w.MAC != run.ctrl.MAC {
				run.ctrl = w
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
			z := zone
			go runSourceLoop(src, readings, stop, func(ok bool) {
				select {
				case health <- sourceHealth{zone: z, ok: ok}:
				default:
				}
			})
			go runEventLoop(src, events, stop)
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
			go runSourceLoop(src, readings, stop, nil)
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
	var pendingEvents []gateway.Event

	flushNow := func() {
		send, err := syncFilter.Filter(pending, time.Now())
		if err != nil {
			log.Printf("sync policy: %v", err)
		}
		pending = nil
		if len(send) > 0 || len(pendingEvents) > 0 {
			batch := gateway.Batch{
				ID: gateway.BatchID(cfg.GatewayLabel, time.Now()), DataMode: "live",
				Created: time.Now().UTC(), Readings: send, Events: pendingEvents,
			}
			pendingEvents = nil
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
		case evs := <-events:
			pendingEvents = append(pendingEvents, evs...)
		case rs := <-readings:
			pending = append(pending, rs...)
			// readings from a bound box tell the platform two things once (and
			// again whenever they change): the box's MAC, and what it can do,
			// observed, never assumed (the metrics that actually answered, and
			// whether THIS gateway will write setpoints)
			if len(rs) > 0 {
				zone := strings.SplitN(rs[0].SensorID, ":", 2)[0]
				if run, ok := active[zone]; ok {
					caps := observedCapabilities(rs, cfg.AllowControl)
					sig := strings.Join(caps.Read, ",") + "|" + strings.Join(caps.Write, ",")
					mac := run.ctrl.MAC
					if mac == "" {
						mac = gateway.MACFor(run.ctrl.Host)
					}
					if (mac != "" && run.ctrl.MAC == "" && !learned[zone]) || reportedCaps[zone] != sig {
						learned[zone] = mac != ""
						reportedCaps[zone] = sig
						go postControllerLearned(cfg.API, cfg.PlanID, key, zone, run.ctrl.Host, mac, caps)
					}
				}
			}
		case h := <-health:
			if h.ok {
				fails[h.zone] = 0
				continue
			}
			fails[h.zone]++
			// three polls in a row lost: if the platform knows the box's MAC,
			// look for it on the LAN (at most every 5 minutes per room)
			run, ok := active[h.zone]
			if ok && fails[h.zone] >= 3 && run.ctrl.MAC != "" && time.Since(lastRescan[h.zone]) > 5*time.Minute {
				lastRescan[h.zone] = time.Now()
				go refindController(cfg.API, cfg.PlanID, key, h.zone, run.ctrl, discovered)
			}
		case <-poll.C:
			reconcile()
		case <-cmds.C:
			pollCommands(cfg.API, cfg.PlanID, key, gates)
		case <-flush.C:
			flushNow()
		case <-sig:
			log.Printf("agent shutting down: final flush")
			flushNow()
			return
		}
	}
}

// ── controller commands: test, read settings, set a setpoint ──
//
// The platform queues commands; the agent polls them every 5s, performs each
// on the LAN and posts the outcome. A test and a settings read are GET-only.
// A setpoint write is the one control operation, and it runs only when this
// gateway's config says allowControl: true; otherwise it is refused here with
// a clear result, whatever the backend asked.

type agentCommand struct {
	CommandID  string          `json:"commandId"`
	Kind       string          `json:"kind"`
	ZoneID     string          `json:"zoneId"`
	Controller agentController `json:"controller"`
	Payload    map[string]any  `json:"payload"`
	ExpiresAt  string          `json:"expiresAt"`
}

func fetchGatewayCommands(api, planID, key string) ([]agentCommand, error) {
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
		Commands []agentCommand `json:"commands"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out.Commands, nil
}

func controllerURL(ctrl agentController) string {
	port := ctrl.Port
	if port == 0 {
		port = 4001
	}
	return fmt.Sprintf("http://%s:%d", ctrl.Host, port)
}

// performControllerTest does the existing read-only OptiClimate read and
// returns the canonical metrics that came back numeric. It never writes.
func performControllerTest(ctrl agentController) ([]string, error) {
	src := &gateway.OptiClimateSource{
		Zone: "test", URL: controllerURL(ctrl),
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

func postGatewayResult(api, planID, key, commandID string, ok bool, result map[string]any, errMsg string) {
	url := fmt.Sprintf("%s/api/pro/plans/%s/gateway/commands/%s/result", strings.TrimRight(api, "/"), planID, commandID)
	body, _ := json.Marshal(map[string]any{"ok": ok, "result": result, "error": errMsg})
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Key", key)
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		log.Printf("agent: post result for %s: %v", commandID, err)
		return
	}
	resp.Body.Close()
	if resp.StatusCode >= 400 {
		log.Printf("agent: post result for %s: HTTP %d", commandID, resp.StatusCode)
	}
}

// controllerCapabilities is what the platform stores as observed abilities.
type controllerCapabilities struct {
	Read    []string `json:"read"`
	Write   []string `json:"write"`
	Unknown []string `json:"unknown,omitempty"`
	Control bool     `json:"control"`
}

// observedCapabilities derives a box's abilities from one poll: the metrics
// that answered are what it reads; the setpoints are writable only when this
// gateway runs with allowControl; the CO2 setpoint register is known to the
// adapter but has never been written, so it stays unknown.
func observedCapabilities(rs []gateway.Reading, allowControl bool) controllerCapabilities {
	seen := map[string]bool{}
	var read []string
	for _, r := range rs {
		if !seen[r.Type] {
			seen[r.Type] = true
			read = append(read, r.Type)
		}
	}
	sort.Strings(read)
	caps := controllerCapabilities{Read: read, Write: []string{}, Unknown: []string{"co2_setpoint"}, Control: allowControl}
	if allowControl {
		for k := range gateway.OptiClimateSetpointRegisters {
			caps.Write = append(caps.Write, k)
		}
		sort.Strings(caps.Write)
	}
	return caps
}

// postControllerLearned tells the platform the hardware address of a bound
// box (so the room can be found again if its IP changes) and what the box
// was observed to do.
func postControllerLearned(api, planID, key, zone, host, mac string, caps controllerCapabilities) {
	postControllerJSON(api, planID, key, "learned", map[string]any{"zoneId": zone, "host": host, "mac": mac, "capabilities": caps})
	log.Printf("agent: learned zone %s (%s): MAC %q, reads %v, control %v", zone, host, mac, caps.Read, caps.Control)
}

func postControllerJSON(api, planID, key, what string, body map[string]any) {
	url := fmt.Sprintf("%s/api/pro/plans/%s/gateway/controllers/%s", strings.TrimRight(api, "/"), planID, what)
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Key", key)
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		log.Printf("agent: report %s: %v", what, err)
		return
	}
	resp.Body.Close()
	if resp.StatusCode >= 400 {
		log.Printf("agent: report %s: HTTP %d", what, resp.StatusCode)
	}
}

// refindController scans the LAN for a room's box by MAC after its address
// stopped answering, and reports the new address. The platform updates the
// binding; the next config poll restarts the source at the new host.
func refindController(api, planID, key, zone string, ctrl agentController, discovered *hostSet) {
	hosts, _ := gateway.LocalIPv4Hosts()
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	res := gateway.DiscoverOptiClimate(ctx, hosts, 4001, 400*time.Millisecond, 64)
	for _, c := range res.Controllers {
		discovered.add(c.Host)
	}
	for _, c := range res.Controllers {
		if c.MAC != "" && c.MAC == ctrl.MAC && c.Host != ctrl.Host {
			log.Printf("agent: controller for zone %s moved: %s -> %s (MAC %s)", zone, ctrl.Host, c.Host, c.MAC)
			postControllerJSON(api, planID, key, "moved", map[string]any{"zoneId": zone, "mac": c.MAC, "host": c.Host, "port": c.Port})
			return
		}
	}
	log.Printf("agent: zone %s: %s not answering and no box with MAC %s found among %d controller(s)", zone, ctrl.Host, ctrl.MAC, len(res.Controllers))
}

// commandExpired reports whether the platform's expiry for a command has
// passed on this clock. A stale setpoint is never applied.
func commandExpired(c agentCommand, now time.Time) bool {
	if c.ExpiresAt == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339, c.ExpiresAt)
	if err != nil {
		return false
	}
	return now.After(t)
}

// performCommand runs one command and returns (ok, result, error message).
// It is the single dispatch point; the gateway's own gates live here: the
// controller address, allowControl and the guard band.
func performCommand(c agentCommand, g commandGates, now time.Time) (bool, map[string]any, string) {
	if strings.ToLower(c.Controller.Vendor) != "opticlimate" {
		return false, nil, "unsupported controller"
	}
	if c.Kind != "discover" && c.Controller.Host == "" {
		return false, nil, "no controller address"
	}
	if commandExpired(c, now) {
		return false, nil, "the command expired before the gateway performed it"
	}
	if c.Kind != "discover" && !g.hostAllowed(c.Controller.Host) {
		return false, nil, "the controller address is outside this gateway's controller networks"
	}
	switch c.Kind {
	case "test":
		metrics, err := performControllerTest(c.Controller)
		if err != nil {
			return false, nil, "could not reach the controller"
		}
		result := map[string]any{"metrics": metrics}
		if mac := gateway.MACFor(c.Controller.Host); mac != "" {
			result["mac"] = mac
		}
		return true, result, ""
	case "discover":
		hosts, subnets := gateway.LocalIPv4Hosts()
		ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
		defer cancel()
		res := gateway.DiscoverOptiClimate(ctx, hosts, 4001, 400*time.Millisecond, 64)
		res.Subnets = subnets
		if g.Discovered != nil {
			for _, found := range res.Controllers {
				g.Discovered.add(found.Host)
			}
		}
		log.Printf("agent: discovery scanned %d hosts on %v in %d ms, found %d controller(s)", res.ScannedHosts, subnets, res.DurationMs, len(res.Controllers))
		return true, map[string]any{"controllers": res.Controllers, "scannedHosts": res.ScannedHosts, "durationMs": res.DurationMs, "subnets": res.Subnets}, ""
	case "read_settings":
		ctl := &gateway.OptiClimateControl{URL: controllerURL(c.Controller), Address: c.Controller.Address}
		settings, err := ctl.ReadSettings(now)
		if err != nil {
			return false, nil, "could not read the controller's settings"
		}
		return true, map[string]any{"settings": settings}, ""
	case "set_setpoint":
		if !g.AllowControl {
			return false, nil, "control is disabled on this gateway (allowControl is off)"
		}
		key, _ := c.Payload["setpoint"].(string)
		value, ok := c.Payload["value"].(float64)
		if key == "" || !ok {
			return false, nil, "malformed setpoint command"
		}
		ctl := &gateway.OptiClimateControl{URL: controllerURL(c.Controller), Address: c.Controller.Address, Band: g.Band}
		res, err := ctl.SetSetpoint(key, value)
		if err != nil {
			return false, nil, err.Error()
		}
		return true, map[string]any{
			"setpoint": res.Setpoint, "register": res.Register,
			"previous": res.Previous, "value": res.Value, "readback": res.Readback,
		}, ""
	}
	return false, nil, "unknown command kind " + c.Kind
}

var lastCommandsPollError time.Time

func pollCommands(api, planID, key string, gates commandGates) {
	cmds, err := fetchGatewayCommands(api, planID, key)
	if err != nil {
		// the poll runs every 5s; log a failing backend once a minute, not 12 times
		if time.Since(lastCommandsPollError) > time.Minute {
			log.Printf("agent: commands poll: %v", err)
			lastCommandsPollError = time.Now()
		}
		return
	}
	for _, c := range cmds {
		now := time.Now()
		ok, result, errMsg := performCommand(c, gates, now)
		switch {
		case c.Kind == "set_setpoint" && ok:
			log.Printf("CONTROL: %s zone %s %s -> %s %v (was %v, read back %v) [%s]",
				c.Controller.Host, c.ZoneID, result["setpoint"], result["register"], result["value"], result["previous"], result["readback"], c.CommandID)
		case c.Kind == "set_setpoint":
			log.Printf("CONTROL REFUSED: %s zone %s %v=%v: %s [%s]",
				c.Controller.Host, c.ZoneID, c.Payload["setpoint"], c.Payload["value"], errMsg, c.CommandID)
		case ok:
			log.Printf("agent: %s %s (%s) ok [%s]", c.Kind, c.ZoneID, c.Controller.Host, c.CommandID)
		default:
			log.Printf("agent: %s %s (%s) failed: %s [%s]", c.Kind, c.ZoneID, c.Controller.Host, errMsg, c.CommandID)
		}
		postGatewayResult(api, planID, key, c.CommandID, ok, result, errMsg)
	}
}

// runEventLoop polls a source that also observes events (controller alarms)
// on the source's cadence; sources without events are left alone.
func runEventLoop(s gateway.Source, out chan<- []gateway.Event, stop <-chan struct{}) {
	es, ok := s.(gateway.EventSource)
	if !ok {
		return
	}
	tick := time.NewTicker(s.Interval())
	defer tick.Stop()
	poll := func() {
		evs, err := es.PollEvents(time.Now())
		if err != nil {
			log.Printf("%s events: %v", s.Describe(), err)
			return
		}
		if len(evs) == 0 {
			return
		}
		for _, e := range evs {
			if e.Payload["event"] == "controller_link" {
				log.Printf("LINK %s: %v ok=%v", e.ZoneID, e.Payload["status"], e.Payload["ok"])
				continue
			}
			log.Printf("ALARM %s: %v active=%v", e.ZoneID, e.Payload["alarm"], e.Payload["active"])
		}
		select {
		case out <- evs:
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

// runSourceLoop samples one source on its cadence into the shared channel
// until told to stop (when its room's controller is removed or changed).
func runSourceLoop(s gateway.Source, out chan<- []gateway.Reading, stop <-chan struct{}, report func(ok bool)) {
	tick := time.NewTicker(s.Interval())
	defer tick.Stop()
	poll := func() {
		rs, err := s.Poll(time.Now())
		if report != nil {
			report(err == nil)
		}
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
