package ledger

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// Ledger appends records to one JSON Lines file per UTC day,
// <dir>/2026-10-11.jsonl, so files rotate on their own and a day can be
// read, archived, or deleted without touching the rest.
type Ledger struct {
	dir string
	mu  sync.Mutex
}

// Open returns a ledger that writes under dir. Nothing is created until the
// first record.
func Open(dir string) *Ledger {
	return &Ledger{dir: dir}
}

// Append writes one record. The file is opened and closed each time: at the
// rate requests arrive this costs nothing, and it means a record is on disk
// when Append returns and the file can be moved or removed between writes.
func (l *Ledger) Append(rec Record) error {
	line, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("ledger: %w", err)
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	if err := os.MkdirAll(l.dir, 0o700); err != nil {
		return fmt.Errorf("ledger: %w", err)
	}
	path := filepath.Join(l.dir, rec.Time.UTC().Format("2006-01-02")+".jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) // #nosec G304 -- a day file in the ledger directory the config names
	if err != nil {
		return fmt.Errorf("ledger: %w", err)
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		_ = f.Close() // the write's error is the one to report
		return fmt.Errorf("ledger: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("ledger: %w", err)
	}
	return nil
}
