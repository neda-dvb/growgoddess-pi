// gateway — the Cannabits Edge Gateway.
//
// Reads sensors, spools batches to disk, and streams them to the ingestion
// API once a minute. A network outage loses nothing: batches wait in the
// spool and drain in order when the platform is reachable again, and the
// server's idempotency keys make every retry safe. Read-only by design:
// this program measures, it never controls.
//
//	go run ./cmd/gateway -config gateway.json
//
// Enroll a new gateway once, with the one-time code from Grow Goddess Pro
// (stores the gateway's own key in the env file, the plan id in the config):
//
//	sudo cannabits-gateway enroll [-config f] [-env-file f] <code>
//
// Cross-compile for a Raspberry Pi:
//
//	GOOS=linux GOARCH=arm64 go build -o cannabits-gateway ./cmd/gateway
//
// See CANNABITS_GATEWAY.md for the config format and a systemd unit.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/neda-dvb/growgoddess-pi/internal/gateway"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "enroll" {
		os.Exit(runEnroll(os.Args[2:], os.Stdout, os.Stderr))
	}
	configPath := flag.String("config", "gateway.json", "gateway configuration file")
	insecureLocalhost := flag.Bool("insecure-localhost", false, "allow a plain http API URL for a backend on localhost (development only)")
	modbusInspect := flag.Bool("modbus-inspect", false, "listen on the modbus source and print every observed register; nothing is sent anywhere")
	agentMode := flag.Bool("agent", false, "facility-agent mode: pull room/controller config from the backend and stream every connected room")
	flag.Parse()

	raw, err := os.ReadFile(*configPath)
	if err != nil {
		log.Fatalf("read config: %v", err)
	}
	var cfg gateway.Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		log.Fatalf("parse config: %v", err)
	}
	key := cfg.Key
	if cfg.KeyFile != "" {
		kb, err := os.ReadFile(cfg.KeyFile)
		if err != nil {
			log.Fatalf("read key file: %v", err)
		}
		key = strings.TrimSpace(string(kb))
	}

	// Environment overrides win over the config file, so the deploy can keep the
	// public API host and the secret out of gateway.json entirely (set them in
	// the systemd unit's EnvironmentFile). Applies to both static and agent mode.
	if v := strings.TrimSpace(os.Getenv("CANNABITS_API_URL")); v != "" {
		cfg.API = v
	}
	if v := strings.TrimSpace(os.Getenv("CANNABITS_API_KEY")); v != "" {
		key = v
	}
	// the gateway's own key (enroll, rotation) wins over every other source
	if cfg.SpoolDir != "" {
		own, err := gateway.ReadKeyFile(cfg.StateKeyPath())
		if err != nil {
			log.Fatalf("read %s: %v", cfg.StateKeyPath(), err)
		}
		if own != "" {
			key = own
			log.Printf("key: this gateway's own key %s... from %s", own[:min(12, len(own))], cfg.StateKeyPath())
		}
	}
	if problems := cfg.EdgeProblems(*insecureLocalhost); len(problems) > 0 {
		log.Fatalf("config problems:\n  %s", strings.Join(problems, "\n  "))
	}

	// Agent mode: the gateway pulls its room/controller config from the
	// backend and streams every connected room. No static zone/sources.
	if *agentMode {
		runAgent(cfg, key)
		return
	}

	if problems := cfg.Validate(); len(problems) > 0 {
		log.Fatalf("config problems:\n  %s", strings.Join(problems, "\n  "))
	}

	sources := buildSources(cfg)

	// The on-site mapping tool: watch the bus, name nothing, send nothing.
	// Compare the printed values against what the monitor displays, then
	// pin the register map in the config.
	if *modbusInspect {
		runModbusInspect(sources)
		return
	}

	spool, err := gateway.NewSpool(cfg.SpoolDir, 0)
	if err != nil {
		log.Fatalf("spool: %v", err)
	}
	spool.MinFreePercent = cfg.SpoolMinFreePercent
	client := gateway.NewClient(cfg.API, cfg.PlanID, key, cfg.GatewayLabel)
	syncFilter := &gateway.SyncFilter{Policy: cfg.SyncPolicy, Local: cfg.LocalStore()}
	auth := &gateway.AuthState{Logf: log.Printf}
	mode := cfg.DataMode()
	flushEvery := time.Duration(cfg.FlushSeconds) * time.Second
	if flushEvery <= 0 {
		flushEvery = time.Minute
	}
	log.Printf("gateway %s: %d source(s), dataMode %s, flush every %s, spool %s, sync %s",
		cfg.GatewayLabel, len(sources), mode, flushEvery, cfg.SpoolDir, gateway.StricterPolicy(cfg.SyncPolicy, ""))
	if pending, _ := spool.Pending(); len(pending) > 0 {
		log.Printf("spool holds %d batch(es) from a previous run; they drain first", len(pending))
	}

	// pollers: each source samples on its own cadence into the accumulator
	readings := make(chan []gateway.Reading, 16)
	stop := make(chan struct{})
	for _, src := range sources {
		go func(s gateway.Source) {
			tick := time.NewTicker(s.Interval())
			defer tick.Stop()
			poll := func() {
				rs, err := s.Poll(time.Now())
				if err != nil {
					log.Printf("%s: %v", s.Describe(), err)
					return
				}
				readings <- rs
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
		}(src)
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	flush := time.NewTicker(flushEvery)
	defer flush.Stop()
	var pending []gateway.Reading

	flushNow := func() {
		send, err := syncFilter.Filter(pending, time.Now())
		if err != nil {
			log.Printf("sync policy: %v", err)
		}
		pending = nil
		if len(send) > 0 {
			batch := gateway.Batch{
				ID: gateway.BatchID(cfg.GatewayLabel, time.Now()), DataMode: mode,
				Created: time.Now().UTC(), Readings: send,
			}
			spoolPut(spool, auth, batch)
		}
		drain(spool, client, auth)
	}

	for {
		select {
		case rs := <-readings:
			pending = append(pending, rs...)
		case <-flush.C:
			flushNow()
		case <-sig:
			log.Printf("shutting down: final flush")
			close(stop)
			flushNow()
			return
		}
	}
}

// spoolPut stores a batch before any send. While the platform refuses the
// key the spool is held: nothing is dropped, and once even the raised bound
// is reached the new batch is not recorded, loudly, so what is stored stays.
func spoolPut(spool *gateway.Spool, auth *gateway.AuthState, batch gateway.Batch) {
	spool.SetHold(auth.Unauthorised())
	dropped, err := spool.Put(batch)
	switch {
	case errors.Is(err, gateway.ErrSpoolHeld):
		log.Printf("UNAUTHORISED AND SPOOL FULL: batch %s NOT RECORDED (%d readings): %v; every stored batch is kept. Enroll again: cannabits-gateway enroll <code>", batch.ID, len(batch.Readings), err)
	case err != nil:
		log.Printf("spool write: %v", err)
	case len(dropped) > 0:
		log.Printf("SPOOL FULL: dropped %d oldest batch(es) - data lost: %v", len(dropped), dropped)
	}
}

// drain sends spooled batches oldest first, stopping at the first failure
// (order is preserved; the rest waits for the next tick). A refused key
// enters the unauthorised state; an accepted batch leaves it.
func drain(spool *gateway.Spool, client *gateway.Client, auth *gateway.AuthState) {
	ids, err := spool.Pending()
	if err != nil {
		log.Printf("spool list: %v", err)
		return
	}
	defer func() { auth.Tick(time.Now(), len(ids)) }()
	for _, id := range ids {
		b, err := spool.Get(id)
		if err != nil {
			log.Printf("spool read %s: %v", id, err)
			continue
		}
		res, err := client.Send(b)
		auth.Observe(err, time.Now())
		if err != nil {
			if !auth.Unauthorised() {
				log.Printf("send %s: %v (kept in spool)", id, err)
			}
			return
		}
		note := ""
		if res.DuplicateBatch {
			note = " (already known)"
		}
		log.Printf("sent %s: %d stored, %d rejected%s, drift %.1fs", id, res.Stored, res.Rejected, note, res.ClockDriftSec)
		if err := spool.Delete(id); err != nil {
			log.Printf("spool delete %s: %v", id, err)
		}
	}
}

// runModbusInspect drives the modbus source without any transmission: a
// 5-second summary of every register the bus reveals, with the raw value
// and its most plausible physical interpretations, until interrupted.
func runModbusInspect(sources []gateway.Source) {
	var src *gateway.ModbusListenSource
	for _, s := range sources {
		if m, ok := s.(*gateway.ModbusListenSource); ok {
			src = m
		}
	}
	if src == nil {
		log.Fatal("modbus-inspect needs a modbus-listen source in the config")
	}
	log.Printf("modbus-inspect: watching %s (baud %d, 0 = auto). Nothing is sent anywhere. Ctrl-C to stop.", src.Device, src.Baud)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
			if _, err := src.Poll(time.Now()); err != nil {
				log.Printf("poll: %v", err)
			}
			stats := src.Observed()
			if len(stats) == 0 {
				log.Printf("no registers observed yet")
				continue
			}
			keys := make([]string, 0, len(stats))
			for k := range stats {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			log.Printf("observed registers (table/slave/address · raw last [min..max] · x0.1 hint · writes):")
			for _, k := range keys {
				st := stats[k]
				w := ""
				if st.Written > 0 {
					w = fmt.Sprintf("  %d writes", st.Written)
				}
				log.Printf("  %-22s %6d [%d..%d]  x0.1=%.1f  (%d obs)%s",
					k, st.Last, st.Min, st.Max, float64(int16(st.Last))*0.1, st.Count, w)
			}
		case <-sig:
			return
		}
	}
}

func buildSources(cfg gateway.Config) []gateway.Source {
	var out []gateway.Source
	for _, sc := range cfg.Sources {
		every := time.Duration(sc.IntervalSeconds) * time.Second
		if every <= 0 {
			every = time.Minute
		}
		switch sc.Type {
		case "sim":
			out = append(out, gateway.SimSource{Zone: cfg.Zone, Probe: sc.Probe, Every: every})
		case "sht31":
			bus, addr := sc.Bus, sc.Address
			if addr == 0 {
				addr = 0x44 // the SHT3x default address
			}
			out = append(out, &gateway.SHT31Source{
				Zone: cfg.Zone, Probe: sc.Probe, Every: every,
				Open: func() (gateway.I2CDevice, error) { return gateway.OpenI2C(bus, addr) },
			})
		case "modbus-listen":
			out = append(out, &gateway.ModbusListenSource{
				Zone: cfg.Zone, Every: every, Registers: sc.Registers,
				Device: sc.Device, Baud: sc.Baud, Parity: sc.Parity,
				Open: gateway.OpenSerial,
			})
		case "opticlimate":
			out = append(out, &gateway.OptiClimateSource{
				Zone: cfg.Zone, URL: sc.URL, Address: sc.Address,
				Registers: sc.Registers, Every: every,
			})
		}
	}
	return out
}
