//go:build !linux

package gateway

import (
	"fmt"
	"io"
)

// OpenSerial exists on non-Linux only so the gateway builds everywhere; the
// RS485 tap is a Linux (Raspberry Pi) concern.
func OpenSerial(device string, baud int, parity string) (io.ReadCloser, error) {
	return nil, fmt.Errorf("serial is only supported on linux (device %s)", device)
}
