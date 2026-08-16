package gateway

// Passive Modbus RTU decoding for the OptiClimate interlink bus (and any
// other RS485 Modbus installation). PASSIVE is the design center: the
// gateway never transmits a single byte on the bus - it decodes the traffic
// the installation's own master (the Smart Remote) already exchanges with
// its units. The monitor keeps full control; we are a read-only wiretap on
// data the operator owns.
//
// RTU frames are delimited by bus silence, which a USB serial tap cannot
// observe reliably, so the decoder scans the byte stream: it attempts to
// parse a valid frame (structure + CRC) at the current offset and advances
// one byte on failure. Reads are attributed to register addresses by
// pairing each response with the master request that preceded it (the
// response alone does not name its registers). Observed writes (a setpoint
// changed on the touchscreen) are decoded too - they carry their own
// addresses.
//
// Everything here is a pure function of bytes and state: fully testable
// without hardware.

import (
	"encoding/binary"
	"fmt"
	"time"
)

// crc16Modbus is the standard Modbus CRC (poly 0xA001 reflected, init
// 0xFFFF), transmitted little-endian after the frame.
func crc16Modbus(data []byte) uint16 {
	crc := uint16(0xFFFF)
	for _, b := range data {
		crc ^= uint16(b)
		for i := 0; i < 8; i++ {
			if crc&1 != 0 {
				crc = crc>>1 ^ 0xA001
			} else {
				crc >>= 1
			}
		}
	}
	return crc
}

func crcOK(frame []byte) bool {
	n := len(frame)
	if n < 4 {
		return false
	}
	want := binary.LittleEndian.Uint16(frame[n-2:])
	return crc16Modbus(frame[:n-2]) == want
}

// RegisterTable distinguishes Modbus input registers (function 0x04,
// read-only measurements) from holding registers (0x03/0x06/0x10,
// settings). The Revomax log's I:/S: prefixes mirror exactly this split.
type RegisterTable string

const (
	TableInput   RegisterTable = "input"
	TableHolding RegisterTable = "holding"
)

// Observation is one register value seen on the bus.
type Observation struct {
	Slave   byte
	Table   RegisterTable
	Address uint16 // register address as used on the wire
	Raw     uint16
	// Written marks values observed in a WRITE (an operator changed a
	// setting) rather than in the master's routine polling.
	Written bool
	At      time.Time
}

// pendingRequest remembers a master read so the following response can be
// attributed to its registers.
type pendingRequest struct {
	slave    byte
	function byte
	start    uint16
	count    uint16
	at       time.Time
}

// Decoder is the streaming frame scanner. Feed it raw bytes as they arrive;
// it emits observations. Single-goroutine use.
type Decoder struct {
	buf     []byte
	pending *pendingRequest
	// FramesDecoded / BytesSkipped are diagnostics: a healthy tap decodes
	// steadily with few skips; wrong baud shows as pure skipping.
	FramesDecoded int
	BytesSkipped  int
	now           func() time.Time
}

func NewDecoder() *Decoder {
	return &Decoder{now: time.Now}
}

// maxFrame bounds scanning: a full 125-register response is 3+250+2 bytes.
const maxFrame = 256

// Feed consumes raw bytes and returns every observation completed by them.
func (d *Decoder) Feed(data []byte) []Observation {
	d.buf = append(d.buf, data...)
	var out []Observation
	for {
		frame, consumed, obs := d.scanOne()
		if consumed == 0 {
			break
		}
		d.buf = d.buf[consumed:]
		if frame {
			d.FramesDecoded++
			out = append(out, obs...)
		} else {
			d.BytesSkipped++
		}
	}
	return out
}

// scanOne tries to parse one frame at the buffer start. Returns whether a
// frame was found, how many bytes to consume (0 = wait for more data), and
// any observations.
func (d *Decoder) scanOne() (frame bool, consumed int, obs []Observation) {
	if len(d.buf) < 4 {
		return false, 0, nil
	}
	fn := d.buf[1]

	// A pending request expires quickly: attributing a much later response
	// to an old request would misname its registers.
	if d.pending != nil && d.now().Sub(d.pending.at) > time.Second {
		d.pending = nil
	}

	// A response to the pending read takes priority: its shape (slave,
	// function, byte count) is fully predicted by the request.
	if p := d.pending; p != nil && d.buf[0] == p.slave && fn == p.function {
		byteCount := int(d.buf[2])
		if byteCount == int(p.count)*2 {
			n := 3 + byteCount + 2
			if len(d.buf) < n {
				return false, 0, nil // incomplete; wait
			}
			if crcOK(d.buf[:n]) {
				table := TableHolding
				if p.function == 0x04 {
					table = TableInput
				}
				at := d.now()
				for i := 0; i < int(p.count); i++ {
					obs = append(obs, Observation{
						Slave: p.slave, Table: table,
						Address: p.start + uint16(i),
						Raw:     binary.BigEndian.Uint16(d.buf[3+2*i:]),
						At:      at,
					})
				}
				d.pending = nil
				return true, n, obs
			}
		}
	}

	switch fn {
	case 0x03, 0x04: // read holding / read input: an 8-byte master request
		if len(d.buf) < 8 {
			return false, 0, nil
		}
		if crcOK(d.buf[:8]) {
			count := binary.BigEndian.Uint16(d.buf[4:])
			if count >= 1 && count <= 125 {
				d.pending = &pendingRequest{
					slave: d.buf[0], function: fn,
					start: binary.BigEndian.Uint16(d.buf[2:]),
					count: count, at: d.now(),
				}
				return true, 8, nil
			}
		}
	case 0x06: // write single holding register: request and echo, same shape
		if len(d.buf) < 8 {
			return false, 0, nil
		}
		if crcOK(d.buf[:8]) {
			return true, 8, []Observation{{
				Slave: d.buf[0], Table: TableHolding,
				Address: binary.BigEndian.Uint16(d.buf[2:]),
				Raw:     binary.BigEndian.Uint16(d.buf[4:]),
				Written: true, At: d.now(),
			}}
		}
	case 0x10: // write multiple holding registers: master request carries values
		if len(d.buf) < 9 {
			return false, 0, nil
		}
		byteCount := int(d.buf[6])
		count := int(binary.BigEndian.Uint16(d.buf[4:]))
		if byteCount == count*2 && count >= 1 && count <= 123 {
			n := 7 + byteCount + 2
			if len(d.buf) < n {
				return false, 0, nil
			}
			if crcOK(d.buf[:n]) {
				start := binary.BigEndian.Uint16(d.buf[2:])
				at := d.now()
				for i := 0; i < count; i++ {
					obs = append(obs, Observation{
						Slave: d.buf[0], Table: TableHolding,
						Address: start + uint16(i),
						Raw:     binary.BigEndian.Uint16(d.buf[7+2*i:]),
						Written: true, At: at,
					})
				}
				return true, n, obs
			}
		}
	}

	// nothing valid at this offset: skip one byte and rescan
	return false, 1, nil
}

// ── register map: bus address to canonical metric, a human decision ──

// ModbusRegisterMap binds one observed register to a canonical metric. The
// Factor is explicit (physical = raw x Factor) - scaling is pinned by a
// human against the values the monitor displays, never guessed.
type ModbusRegisterMap struct {
	Slave   int           `json:"slave"`
	Table   RegisterTable `json:"table"` // input | holding
	Address int           `json:"address"`
	Metric  string        `json:"metric"`
	Factor  float64       `json:"factor"`
	// Name is the controller-side register name used by HTTP sources such as
	// opticlimate (e.g. "Room1Temp"). The passive Modbus decoder ignores it;
	// this field is additive so one register map serves both worlds.
	Name string `json:"name,omitempty"`
	// Signed interprets the 16-bit raw as two's complement (temperatures
	// below zero).
	Signed bool   `json:"signed,omitempty"`
	Probe  string `json:"probe,omitempty"`
}

// Physical converts an observed raw value.
func (m ModbusRegisterMap) Physical(raw uint16) float64 {
	f := m.Factor
	if f == 0 {
		f = 1
	}
	if m.Signed {
		return float64(int16(raw)) * f
	}
	return float64(raw) * f
}

// Matches reports whether an observation is this mapping's register.
func (m ModbusRegisterMap) Matches(o Observation) bool {
	return int(o.Slave) == m.Slave && o.Table == m.Table && int(o.Address) == m.Address
}

// ObservationsToReadings converts mapped observations into gateway readings;
// unmapped observations are counted per register so the inspect mode can
// show the whole bus honestly.
func ObservationsToReadings(obs []Observation, maps []ModbusRegisterMap, zone string) (readings []Reading, unmapped map[string]int) {
	unmapped = map[string]int{}
	for _, o := range obs {
		mapped := false
		for _, m := range maps {
			if m.Matches(o) {
				readings = append(readings, Reading{
					SensorID:    zone + ":" + m.Metric,
					Type:        m.Metric,
					Ts:          o.At.UTC().Format("2006-01-02T15:04:05.000Z"),
					Value:       m.Physical(o.Raw),
					Probe:       m.Probe,
					ValueOrigin: "measured",
				})
				mapped = true
				break
			}
		}
		if !mapped {
			unmapped[fmt.Sprintf("%d/%s/%d", o.Slave, o.Table, o.Address)]++
		}
	}
	return readings, unmapped
}

// ── baud auto-detection ──

// CommonBauds are tried during auto-detection, most likely first.
var CommonBauds = []int{19200, 9600, 38400, 57600, 115200, 4800}

// ScoreStream counts how many valid frames a byte capture yields - the
// auto-baud scorer: the correct rate decodes steadily, a wrong one decodes
// nothing. Pure; the serial layer captures, this judges.
func ScoreStream(capture []byte) int {
	d := NewDecoder()
	d.Feed(capture)
	return d.FramesDecoded
}
