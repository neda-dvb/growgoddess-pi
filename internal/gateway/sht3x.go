package gateway

// SHT3x (Sensirion SHT30/31/35) adapter: the reference real-sensor source.
// A single-shot high-repeatability measurement returns six bytes
// (temperature word + CRC, humidity word + CRC); the conversion formulas
// and the CRC are straight from the datasheet and live here as pure,
// tested functions. The I²C transport is an interface so the adapter logic
// is testable without hardware; the Linux implementation sits behind a
// build tag in i2c_linux.go.
//
// STATUS: the conversion path is unit-tested against datasheet vectors;
// the physical I²C path compiles for Linux but awaits verification on real
// hardware - see CANNABITS_GATEWAY.md.

import (
	"fmt"
	"time"
)

// I2CDevice is the minimal transport the adapter needs.
type I2CDevice interface {
	Write(b []byte) error
	Read(b []byte) error
	Close() error
}

// sht3xCRC is the datasheet CRC-8: polynomial 0x31, init 0xFF, no
// reflection, no final XOR.
func sht3xCRC(data []byte) byte {
	crc := byte(0xFF)
	for _, b := range data {
		crc ^= b
		for i := 0; i < 8; i++ {
			if crc&0x80 != 0 {
				crc = crc<<1 ^ 0x31
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}

// ConvertSHT3x validates and converts the sensor's six raw bytes.
// temp °C = -45 + 175·raw/65535, rh % = 100·raw/65535.
func ConvertSHT3x(raw []byte) (tempC, rhPct float64, err error) {
	if len(raw) != 6 {
		return 0, 0, fmt.Errorf("sht3x: expected 6 bytes, got %d", len(raw))
	}
	if sht3xCRC(raw[0:2]) != raw[2] {
		return 0, 0, fmt.Errorf("sht3x: temperature CRC mismatch")
	}
	if sht3xCRC(raw[3:5]) != raw[5] {
		return 0, 0, fmt.Errorf("sht3x: humidity CRC mismatch")
	}
	tRaw := float64(uint16(raw[0])<<8 | uint16(raw[1]))
	hRaw := float64(uint16(raw[3])<<8 | uint16(raw[4]))
	tempC = -45 + 175*tRaw/65535
	rhPct = 100 * hRaw / 65535
	return tempC, rhPct, nil
}

// SHT31Source samples one SHT3x probe and emits air_temp + rh.
type SHT31Source struct {
	Zone  string
	Probe string
	Every time.Duration
	// Open connects to the bus lazily so a temporarily absent sensor is a
	// polling error, not a startup crash.
	Open func() (I2CDevice, error)
}

func (s *SHT31Source) Describe() string        { return "sht31" }
func (s *SHT31Source) Simulated() bool         { return false }
func (s *SHT31Source) Interval() time.Duration { return s.Every }

// measureCmd is single-shot, high repeatability, no clock stretching
// (0x2400); the sensor needs up to 15 ms before the result is readable.
var measureCmd = []byte{0x24, 0x00}

const measureDelay = 20 * time.Millisecond

func (s *SHT31Source) Poll(now time.Time) ([]Reading, error) {
	dev, err := s.Open()
	if err != nil {
		return nil, fmt.Errorf("sht31 open: %w", err)
	}
	defer dev.Close()
	if err := dev.Write(measureCmd); err != nil {
		return nil, fmt.Errorf("sht31 command: %w", err)
	}
	time.Sleep(measureDelay)
	raw := make([]byte, 6)
	if err := dev.Read(raw); err != nil {
		return nil, fmt.Errorf("sht31 read: %w", err)
	}
	temp, rh, err := ConvertSHT3x(raw)
	if err != nil {
		return nil, err
	}
	ts := now.UTC().Format(time.RFC3339)
	return []Reading{
		{SensorID: s.Zone + ":air_temp", Type: "air_temp", Ts: ts, Value: round1(temp), Probe: s.Probe, ValueOrigin: "measured"},
		{SensorID: s.Zone + ":rh", Type: "rh", Ts: ts, Value: round1(rh), Probe: s.Probe, ValueOrigin: "measured"},
	}, nil
}
