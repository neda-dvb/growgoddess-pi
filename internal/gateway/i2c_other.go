//go:build !linux

package gateway

import "fmt"

// OpenI2C exists on non-Linux only so the gateway builds everywhere; real
// probes are a Linux (Raspberry Pi) concern.
func OpenI2C(bus string, addr int) (I2CDevice, error) {
	return nil, fmt.Errorf("i2c is only supported on linux (bus %s)", bus)
}
