//go:build unix

package main

import (
	"os"
	"syscall"
)

// keepOwner gives tmp the owner and group of path, when path exists.
func keepOwner(path, tmp string) error {
	fi, err := os.Stat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	if int(st.Uid) == os.Getuid() && int(st.Gid) == os.Getgid() {
		return nil // already ours; chown needs no privilege
	}
	return os.Chown(tmp, int(st.Uid), int(st.Gid))
}
