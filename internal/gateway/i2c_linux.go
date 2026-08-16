//go:build linux

package gateway

// Linux I²C transport via /dev/i2c-N and the I2C_SLAVE ioctl. Standard
// library only - the gateway stays a dependency-free static binary.

import (
	"fmt"
	"os"
	"syscall"
)

const i2cSlave = 0x0703

type linuxI2C struct {
	f *os.File
}

// OpenI2C opens bus (e.g. /dev/i2c-1) and selects the 7-bit address.
func OpenI2C(bus string, addr int) (I2CDevice, error) {
	f, err := os.OpenFile(bus, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), i2cSlave, uintptr(addr)); errno != 0 {
		f.Close()
		return nil, fmt.Errorf("i2c slave select 0x%02x: %v", addr, errno)
	}
	return &linuxI2C{f: f}, nil
}

func (d *linuxI2C) Write(b []byte) error {
	_, err := d.f.Write(b)
	return err
}

func (d *linuxI2C) Read(b []byte) error {
	_, err := d.f.Read(b)
	return err
}

func (d *linuxI2C) Close() error { return d.f.Close() }
