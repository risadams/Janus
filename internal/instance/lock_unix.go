//go:build unix

package instance

import (
	"errors"
	"os"
	"syscall"
)

// lock opens path and takes an exclusive flock on it without waiting.
func lock(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errHeld
		}
		return nil, err
	}
	return f, nil
}

// AddrInUse reports whether err from net.Listen means the address is taken.
func AddrInUse(err error) bool {
	return errors.Is(err, syscall.EADDRINUSE)
}
