package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// ── SHT3x: datasheet truth ──

func TestSHT3xCRCDatasheetVector(t *testing.T) {
	// the vector printed in the Sensirion datasheet: 0xBEEF -> 0x92
	if got := sht3xCRC([]byte{0xBE, 0xEF}); got != 0x92 {
		t.Fatalf("CRC(0xBEEF) = 0x%02X, want 0x92", got)
	}
}

func TestConvertSHT3x(t *testing.T) {
	// build a frame for known raw words and verify the datasheet formulas
	frame := func(tRaw, hRaw uint16) []byte {
		b := []byte{byte(tRaw >> 8), byte(tRaw & 0xFF), 0, byte(hRaw >> 8), byte(hRaw & 0xFF), 0}
		b[2] = sht3xCRC(b[0:2])
		b[5] = sht3xCRC(b[3:5])
		return b
	}
	// raw 0x0000: -45 °C, 0 %; raw 0xFFFF: 130 °C, 100 %
	temp, rh, err := ConvertSHT3x(frame(0x0000, 0xFFFF))
	if err != nil {
		t.Fatal(err)
	}
	if temp != -45 || rh != 100 {
		t.Errorf("extremes: %g °C %g %%, want -45 and 100", temp, rh)
	}
	// mid-scale: 0x8000 -> about 42.5 °C and 50 %
	temp, rh, _ = ConvertSHT3x(frame(0x8000, 0x8000))
	if temp < 42.4 || temp > 42.6 || rh < 49.9 || rh > 50.1 {
		t.Errorf("mid-scale: %g °C %g %%", temp, rh)
	}
	// a flipped bit must fail the CRC, never produce a value
	bad := frame(0x6666, 0x6666)
	bad[1] ^= 0x01
	if _, _, err := ConvertSHT3x(bad); err == nil {
		t.Errorf("corrupted frame must be rejected")
	}
}

type fakeI2C struct {
	frame  []byte
	wrote  [][]byte
	closed bool
}

func (f *fakeI2C) Write(b []byte) error {
	f.wrote = append(f.wrote, append([]byte{}, b...))
	return nil
}
func (f *fakeI2C) Read(b []byte) error { copy(b, f.frame); return nil }
func (f *fakeI2C) Close() error        { f.closed = true; return nil }

func TestSHT31SourcePoll(t *testing.T) {
	frame := []byte{0x80, 0x00, 0, 0x80, 0x00, 0}
	frame[2] = sht3xCRC(frame[0:2])
	frame[5] = sht3xCRC(frame[3:5])
	dev := &fakeI2C{frame: frame}
	src := &SHT31Source{Zone: "cycle-01", Probe: "probe-a", Every: time.Minute,
		Open: func() (I2CDevice, error) { return dev, nil }}
	rs, err := src.Poll(time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 2 || rs[0].SensorID != "cycle-01:air_temp" || rs[1].SensorID != "cycle-01:rh" {
		t.Fatalf("readings: %+v", rs)
	}
	if rs[0].ValueOrigin != "measured" || rs[0].Probe != "probe-a" {
		t.Errorf("a real probe reports measured with its probe id: %+v", rs[0])
	}
	if len(dev.wrote) != 1 || dev.wrote[0][0] != 0x24 {
		t.Errorf("measure command not sent: %v", dev.wrote)
	}
	if !dev.closed {
		t.Errorf("device must be closed after each poll")
	}
}

// ── spool: durability and bounds ──

func TestSpoolRoundTripAndOrder(t *testing.T) {
	spool, err := NewSpool(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		b := Batch{ID: BatchID("tent-pi", base.Add(time.Duration(i)*time.Minute)),
			DataMode: "live", Created: base,
			Readings: []Reading{{SensorID: "cycle-01:rh", Type: "rh", Ts: "2026-07-30T12:00:00Z", Value: 50}}}
		if _, err := spool.Put(b); err != nil {
			t.Fatal(err)
		}
	}
	ids, err := spool.Pending()
	if err != nil || len(ids) != 3 {
		t.Fatalf("pending: %v %v", ids, err)
	}
	for i := 1; i < len(ids); i++ {
		if ids[i] < ids[i-1] {
			t.Errorf("spool must list oldest first: %v", ids)
		}
	}
	got, err := spool.Get(ids[0])
	if err != nil || got.DataMode != "live" || len(got.Readings) != 1 {
		t.Fatalf("round trip: %+v %v", got, err)
	}
	if err := spool.Delete(ids[0]); err != nil {
		t.Fatal(err)
	}
	if ids, _ = spool.Pending(); len(ids) != 2 {
		t.Errorf("delete did not shrink the spool: %v", ids)
	}
}

func TestSpoolBoundDropsOldestLoudly(t *testing.T) {
	spool, _ := NewSpool(t.TempDir(), 2)
	base := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	var lastDropped []string
	for i := 0; i < 3; i++ {
		d, err := spool.Put(Batch{ID: BatchID("x", base.Add(time.Duration(i)*time.Minute)), DataMode: "live"})
		if err != nil {
			t.Fatal(err)
		}
		lastDropped = d
	}
	if len(lastDropped) != 1 {
		t.Fatalf("the third batch must report one dropped id, got %v", lastDropped)
	}
	ids, _ := spool.Pending()
	if len(ids) != 2 || ids[0] == lastDropped[0] {
		t.Errorf("the OLDEST batch must be the dropped one: kept %v dropped %v", ids, lastDropped)
	}
}

// ── config: the honesty rule ──

func TestConfigValidation(t *testing.T) {
	good := Config{API: "http://x", PlanID: "p", Zone: "cycle-01", GatewayLabel: "tent",
		SpoolDir: "/tmp/s", Sources: []SourceConfig{{Type: "sht31", Bus: "/dev/i2c-1"}}}
	if p := good.Validate(); len(p) != 0 {
		t.Fatalf("valid config rejected: %v", p)
	}
	if good.DataMode() != "live" {
		t.Errorf("real sources mean live mode")
	}

	simOnly := good
	simOnly.Sources = []SourceConfig{{Type: "sim"}}
	if p := simOnly.Validate(); len(p) != 0 {
		t.Fatalf("sim-only config rejected: %v", p)
	}
	if simOnly.DataMode() != "fixture" {
		t.Errorf("a simulated source makes the gateway a fixture instrument")
	}

	mixed := good
	mixed.Sources = []SourceConfig{{Type: "sim"}, {Type: "sht31", Bus: "/dev/i2c-1"}}
	problems := mixed.Validate()
	found := false
	for _, p := range problems {
		if p == "a gateway is either a live instrument or a fixture source: simulated and real sources cannot mix" {
			found = true
		}
	}
	if !found {
		t.Errorf("mixing simulated and real sources must be a configuration error: %v", problems)
	}
}

func TestSimSourceDeterministic(t *testing.T) {
	src := SimSource{Zone: "cycle-01", Every: time.Minute}
	at := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	a, _ := src.Poll(at)
	b, _ := src.Poll(at)
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	if string(ja) != string(jb) {
		t.Errorf("sim source must be deterministic")
	}
	if a[0].ValueOrigin != "simulated" {
		t.Errorf("sim readings must declare their origin: %+v", a[0])
	}
}

// ── client + spool: the outage story ──

func TestClientSendAndOutageRecovery(t *testing.T) {
	var healthy atomic.Bool
	var received atomic.Int32
	var lastBatchID atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !healthy.Load() {
			http.Error(w, `{"error":"down"}`, http.StatusInternalServerError)
			return
		}
		var env struct {
			BatchID  string `json:"batchId"`
			DataMode string `json:"dataMode"`
			Readings []any  `json:"readings"`
		}
		_ = json.NewDecoder(r.Body).Decode(&env)
		received.Add(1)
		lastBatchID.Store(env.BatchID)
		fmt.Fprintf(w, `{"stored":%d,"rejected":[],"receivedAt":"2026-07-30T12:00:00Z"}`, len(env.Readings))
	}))
	defer server.Close()

	spool, _ := NewSpool(t.TempDir(), 0)
	client := NewClient(server.URL, "plan-1", "cbk_test", "tent-pi")
	base := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	put := func(i int) Batch {
		b := Batch{ID: BatchID("tent-pi", base.Add(time.Duration(i)*time.Minute)), DataMode: "live",
			Readings: []Reading{{SensorID: "cycle-01:rh", Type: "rh", Ts: "2026-07-30T12:00:00Z", Value: 50}}}
		if _, err := spool.Put(b); err != nil {
			t.Fatal(err)
		}
		return b
	}

	// outage: sends fail, spool keeps everything
	healthy.Store(false)
	b0 := put(0)
	if _, err := client.Send(b0); err == nil {
		t.Fatal("a 500 must be an error so the spool keeps the batch")
	}
	put(1)
	if ids, _ := spool.Pending(); len(ids) != 2 {
		t.Fatalf("outage must accumulate the spool: %v", ids)
	}

	// recovery: drain oldest first, delete on confirmation
	healthy.Store(true)
	ids, _ := spool.Pending()
	for _, id := range ids {
		b, _ := spool.Get(id)
		res, err := client.Send(b)
		if err != nil {
			t.Fatal(err)
		}
		if res.Stored != 1 {
			t.Errorf("stored = %d", res.Stored)
		}
		_ = spool.Delete(id)
	}
	if ids, _ := spool.Pending(); len(ids) != 0 {
		t.Errorf("spool must be empty after recovery: %v", ids)
	}
	if received.Load() != 2 {
		t.Errorf("server must have received both batches, got %d", received.Load())
	}
	if lastBatchID.Load().(string) == "" {
		t.Errorf("batch id must travel in the envelope")
	}
}
