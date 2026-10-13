//go:build windows

package instance

import (
	"errors"
	"os"
	"syscall"
)

const (
	errorSharingViolation = syscall.Errno(32)    // ERROR_SHARING_VIOLATION
	wsaeAddrInUse         = syscall.Errno(10048) // WSAEADDRINUSE
)

// lock opens path for writing while letting others only read it, so a
// second open for writing fails until this handle is closed. Reading stays
// allowed so that a second Janus can say who holds the lock.
func lock(path string) (*os.File, error) {
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := syscall.CreateFile(name, syscall.GENERIC_READ|syscall.GENERIC_WRITE, syscall.FILE_SHARE_READ,
		nil, syscall.OPEN_ALWAYS, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		if errors.Is(err, errorSharingViolation) {
			return nil, errHeld
		}
		return nil, err
	}
	return os.NewFile(uintptr(h), path), nil
}

// AddrInUse reports whether err from net.Listen means the address is taken.
func AddrInUse(err error) bool {
	return errors.Is(err, wsaeAddrInUse) || errors.Is(err, syscall.EADDRINUSE)
}
