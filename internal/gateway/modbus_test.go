package gateway

import (
	"encoding/binary"
	"io"
	"testing"
	"time"
)

// frame appends the Modbus CRC to a payload.
func frame(payload ...byte) []byte {
	crc := crc16Modbus(payload)
	return append(payload, byte(crc&0xFF), byte(crc>>8))
}

func TestCRC16KnownVector(t *testing.T) {
	// the canonical example frame: 01 03 00 00 00 01 -> CRC 0x0A84,
	// transmitted 84 0A
	if got := crc16Modbus([]byte{0x01, 0x03, 0x00, 0x00, 0x00, 0x01}); got != 0x0A84 {
		t.Fatalf("CRC = 0x%04X, want 0x0A84", got)
	}
	if !crcOK([]byte{0x01, 0x03, 0x00, 0x00, 0x00, 0x01, 0x84, 0x0A}) {
		t.Fatal("the canonical frame must verify")
	}
}

// readInputRequest builds a master read of input registers.
func readInputRequest(slave byte, start, count uint16) []byte {
	p := []byte{slave, 0x04, 0, 0, 0, 0}
	binary.BigEndian.PutUint16(p[2:], start)
	binary.BigEndian.PutUint16(p[4:], count)
	return frame(p...)
}

// readResponse builds the matching slave response.
func readResponse(slave byte, fn byte, values ...uint16) []byte {
	p := []byte{slave, fn, byte(len(values) * 2)}
	for _, v := range values {
		var b [2]byte
		binary.BigEndian.PutUint16(b[:], v)
		p = append(p, b[0], b[1])
	}
	return frame(p...)
}

func TestDecoderPairsRequestAndResponse(t *testing.T) {
	d := NewDecoder()
	// the master reads input registers 22..23 of slave 0; the unit answers
	// humidity 430 and air speed 78 - the OptiClimate story in miniature
	stream := append(readInputRequest(0, 22, 2), readResponse(0, 0x04, 430, 78)...)
	obs := d.Feed(stream)
	if len(obs) != 2 {
		t.Fatalf("observations = %d, want 2: %+v", len(obs), obs)
	}
	if obs[0].Table != TableInput || obs[0].Address != 22 || obs[0].Raw != 430 {
		t.Errorf("first observation wrong: %+v", obs[0])
	}
	if obs[1].Address != 23 || obs[1].Raw != 78 {
		t.Errorf("second observation wrong: %+v", obs[1])
	}
	if d.FramesDecoded != 2 {
		t.Errorf("frames = %d, want 2", d.FramesDecoded)
	}
}

func TestDecoderSurvivesFragmentationAndGarbage(t *testing.T) {
	d := NewDecoder()
	stream := []byte{0xDE, 0xAD, 0xBE} // line noise before the tap synced
	stream = append(stream, readInputRequest(1, 6, 1)...)
	stream = append(stream, readResponse(1, 0x04, 223)...)
	// feed ONE BYTE AT A TIME: serial reads fragment arbitrarily
	var obs []Observation
	for _, b := range stream {
		obs = append(obs, d.Feed([]byte{b})...)
	}
	if len(obs) != 1 || obs[0].Raw != 223 || obs[0].Address != 6 {
		t.Fatalf("fragmented decode failed: %+v", obs)
	}
	if d.BytesSkipped == 0 {
		t.Errorf("the noise bytes must be counted as skipped")
	}
}

func TestDecoderObservesWrites(t *testing.T) {
	d := NewDecoder()
	// the operator sets holding register 37 (day temperature) to 23
	p := []byte{0, 0x06, 0, 37, 0, 23}
	obs := d.Feed(frame(p...))
	if len(obs) != 1 {
		t.Fatalf("observations: %+v", obs)
	}
	if !obs[0].Written || obs[0].Table != TableHolding || obs[0].Address != 37 || obs[0].Raw != 23 {
		t.Errorf("write observation wrong: %+v", obs[0])
	}

	// write-multiple: registers 71..72 to 42 and 55
	p = []byte{0, 0x10, 0, 71, 0, 2, 4, 0, 42, 0, 55}
	obs = d.Feed(frame(p...))
	if len(obs) != 2 || !obs[0].Written || obs[0].Address != 71 || obs[1].Raw != 55 {
		t.Errorf("multi-write observation wrong: %+v", obs)
	}
}

func TestDecoderPendingExpires(t *testing.T) {
	d := NewDecoder()
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	d.now = func() time.Time { return now }
	d.Feed(readInputRequest(0, 22, 1))
	// the response never arrives; much later a DIFFERENT response-shaped
	// blob of the same slave and function must not inherit the addresses
	now = now.Add(5 * time.Second)
	obs := d.Feed(readResponse(0, 0x04, 999))
	if len(obs) != 0 {
		t.Errorf("a stale request must not name a late response: %+v", obs)
	}
}

func TestModbusRegisterMapPhysical(t *testing.T) {
	m := ModbusRegisterMap{Factor: 0.1, Signed: true}
	if v := m.Physical(223); v != 22.3 {
		t.Errorf("223 x0.1 = %g, want 22.3", v)
	}
	if v := m.Physical(0xFFF6); v != -1 {
		t.Errorf("signed 0xFFF6 x0.1 = %g, want -1", v)
	}
	unsigned := ModbusRegisterMap{Factor: 1}
	if v := unsigned.Physical(0xFFF6); v != 65526 {
		t.Errorf("unsigned: %g", v)
	}
}

func TestObservationsToReadings(t *testing.T) {
	at := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	obs := []Observation{
		{Slave: 0, Table: TableInput, Address: 22, Raw: 430, At: at},
		{Slave: 0, Table: TableInput, Address: 18, Raw: 620, At: at}, // unmapped pressure
	}
	maps := []ModbusRegisterMap{{Slave: 0, Table: TableInput, Address: 22, Metric: "rh", Factor: 0.1}}
	readings, unmapped := ObservationsToReadings(obs, maps, "cycle-01")
	if len(readings) != 1 || readings[0].SensorID != "cycle-01:rh" || readings[0].Value != 43 {
		t.Fatalf("readings: %+v", readings)
	}
	if readings[0].ValueOrigin != "measured" {
		t.Errorf("bus observations are measured: %+v", readings[0])
	}
	if unmapped["0/input/18"] != 1 {
		t.Errorf("the unmapped register must be counted: %v", unmapped)
	}
}

func TestScoreStream(t *testing.T) {
	valid := append(readInputRequest(0, 22, 1), readResponse(0, 0x04, 430)...)
	valid = append(valid, readInputRequest(0, 6, 1)...)
	valid = append(valid, readResponse(0, 0x04, 223)...)
	if s := ScoreStream(valid); s < 4 {
		t.Errorf("a clean capture must score its frames, got %d", s)
	}
	noise := make([]byte, 400)
	for i := range noise {
		noise[i] = byte(i*7 + 13)
	}
	if s := ScoreStream(noise); s != 0 {
		t.Errorf("wrong-baud noise must score 0, got %d", s)
	}
}

// fakeSerial replays a canned byte stream in chunks.
type fakeSerial struct {
	data []byte
	pos  int
}

func (f *fakeSerial) Read(b []byte) (int, error) {
	if f.pos >= len(f.data) {
		time.Sleep(5 * time.Millisecond) // idle bus
		return 0, nil
	}
	n := copy(b, f.data[f.pos:min(f.pos+7, len(f.data))]) // ragged chunks
	f.pos += n
	return n, nil
}
func (f *fakeSerial) Close() error { return nil }

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func TestModbusListenSourceEndToEnd(t *testing.T) {
	stream := append(readInputRequest(0, 22, 1), readResponse(0, 0x04, 430)...)
	stream = append(stream, readInputRequest(0, 6, 1)...)
	stream = append(stream, readResponse(0, 0x04, 223)...)
	src := &ModbusListenSource{
		Zone: "cycle-01", Every: time.Minute, Baud: 19200,
		Registers: []ModbusRegisterMap{
			{Slave: 0, Table: TableInput, Address: 22, Metric: "rh", Factor: 0.1},
			{Slave: 0, Table: TableInput, Address: 6, Metric: "air_temp", Factor: 0.1},
		},
		Open: func(device string, baud int, parity string) (io.ReadCloser, error) {
			return &fakeSerial{data: stream}, nil
		},
	}
	if _, err := src.Poll(time.Now()); err != nil { // starts the reader
		t.Fatal(err)
	}
	var readings []Reading
	for i := 0; i < 50 && len(readings) < 2; i++ {
		time.Sleep(10 * time.Millisecond)
		rs, _ := src.Poll(time.Now())
		readings = append(readings, rs...)
	}
	if len(readings) != 2 {
		t.Fatalf("readings = %d, want 2: %+v", len(readings), readings)
	}
	byMetric := map[string]float64{}
	for _, r := range readings {
		byMetric[r.Type] = r.Value
	}
	if byMetric["rh"] != 43 || byMetric["air_temp"] != 22.3 {
		t.Errorf("values: %+v", byMetric)
	}
	stats := src.Observed()
	if stats["input/0/22"].Count != 1 {
		t.Errorf("stats must record the observation: %+v", stats)
	}
}
