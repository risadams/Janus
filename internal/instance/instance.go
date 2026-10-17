// Package instance keeps to one Janus per log folder.
//
// Two proxies writing the same folder would append to the same ledger,
// prune the same captures, and double every count in janus stats. The lock
// is per folder rather than per machine so that a scratch Janus with its own
// config and log folder can run beside the everyday one.
//
// The lock is the operating system's: an flock on macOS and Linux, a file
// opened without write sharing on Windows. The system lets go of it when the
// process ends, however it ends, so a crash never leaves a stale lock to
// clear by hand.
package instance

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// FileName is the lock file Janus keeps in its log folder.
const FileName = "janus.lock"

// Lock is a held lock on a log folder.
type Lock struct {
	f *os.File
}

// HeldError is returned by Acquire when another process holds the lock.
type HeldError struct {
	// Dir is the log folder.
	Dir string
	// Holder is what the running Janus wrote about itself, such as its pid
	// and address, or "" if it has not written anything yet.
	Holder string
}

func (e *HeldError) Error() string {
	who := "another janus"
	if e.Holder != "" {
		who += " (" + e.Holder + ")"
	}
	return fmt.Sprintf("%s is already using the log folder %s", who, e.Dir)
}

// errHeld is what the platform code returns when the lock is taken.
var errHeld = errors.New("lock held")

// Acquire takes the lock on dir, creating dir if it does not exist. If
// another process holds it, the error is a *HeldError.
func Acquire(dir string) (*Lock, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, FileName)
	f, err := lock(path)
	if errors.Is(err, errHeld) {
		data, _ := os.ReadFile(path) // #nosec G304 -- the lock file, to say who holds it
		return nil, &HeldError{Dir: dir, Holder: strings.TrimSpace(string(data))}
	}
	if err != nil {
		return nil, fmt.Errorf("could not lock %s: %w", path, err)
	}
	return &Lock{f: f}, nil
}

// Describe records who holds the lock, for a second Janus to report. It
// replaces anything written before.
func (l *Lock) Describe(holder string) error {
	if err := l.f.Truncate(0); err != nil {
		return err
	}
	_, err := l.f.WriteAt([]byte(holder+"\n"), 0)
	return err
}

// Release lets go of the lock. The file stays: removing it would let a
// process still waiting on the old file believe it held the lock while
// another took a new one.
func (l *Lock) Release() error {
	return l.f.Close()
}
