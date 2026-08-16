//go:build linux

package gateway

// Linux serial transport for the RS485 tap: standard library only, raw
// termios via ioctl. The port is opened read-only at the TTY level and the
// gateway never writes to it - passive means passive.

import (
	"fmt"
	"io"
	"os"
	"syscall"
	"unsafe"
)

var baudConstants = map[int]uint32{
	4800:   syscall.B4800,
	9600:   syscall.B9600,
	19200:  syscall.B19200,
	38400:  syscall.B38400,
	57600:  syscall.B57600,
	115200: syscall.B115200,
}

// OpenSerial opens a serial device in raw mode: 8 data bits, one stop bit,
// parity "N", "E" or "O", VMIN=0/VTIME=1 so reads poll with a 100 ms
// timeout.
func OpenSerial(device string, baud int, parity string) (io.ReadCloser, error) {
	b, ok := baudConstants[baud]
	if !ok {
		return nil, fmt.Errorf("unsupported baud %d", baud)
	}
	f, err := os.OpenFile(device, os.O_RDONLY|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, err
	}
	t := syscall.Termios{
		Iflag: syscall.IGNPAR,
		Cflag: syscall.CREAD | syscall.CLOCAL | syscall.CS8 | b,
	}
	switch parity {
	case "", "N":
	case "E":
		t.Cflag |= syscall.PARENB
	case "O":
		t.Cflag |= syscall.PARENB | syscall.PARODD
	default:
		f.Close()
		return nil, fmt.Errorf("parity must be N, E or O")
	}
	t.Ispeed = b
	t.Ospeed = b
	t.Cc[syscall.VMIN] = 0
	t.Cc[syscall.VTIME] = 1 // tenths of a second
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), syscall.TCSETS, uintptr(unsafe.Pointer(&t))); errno != 0 {
		f.Close()
		return nil, fmt.Errorf("termios: %v", errno)
	}
	return f, nil
}
