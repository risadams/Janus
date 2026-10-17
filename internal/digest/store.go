package digest

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Store keeps the originals of digested tool results, one file each, for
// Claude to fetch through janus mcp's tool_output tool. They hold tool
// output in full, so like captures they are readable by their owner only
// and are deleted after the retention period. Unlike captures they are
// kept whether or not capture is on: a digest promises the original.
type Store struct {
	dir    string
	retain time.Duration
	now    func() time.Time
	logf   func(format string, args ...any)

	mu     sync.Mutex
	pruned time.Time // when Prune last ran
}

// pruneEvery is how often Put looks for originals past the retention
// period. Janus can run for weeks, so pruning only at start would keep
// them all until the next restart.
const pruneEvery = time.Hour

// NewStore returns a store in dir that keeps originals for retainDays, or
// for good if that is zero, and deletes what is already older. Originals
// it cannot delete then or later are reported through logf, which may be
// nil.
func NewStore(dir string, retainDays int, logf func(format string, args ...any)) *Store {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	s := &Store{dir: dir, retain: time.Duration(retainDays) * 24 * time.Hour, now: time.Now, logf: logf}
	s.pruned = s.now()
	if err := s.Prune(s.pruned); err != nil {
		s.logf("%v", err)
	}
	return s
}

// pruneIfDue runs Prune if it has not run for pruneEvery.
func (s *Store) pruneIfDue() {
	now := s.now()
	s.mu.Lock()
	due := now.Sub(s.pruned) >= pruneEvery
	if due {
		s.pruned = now
	}
	s.mu.Unlock()
	if due {
		if err := s.Prune(now); err != nil {
			s.logf("%v", err)
		}
	}
}

// chtimes is os.Chtimes, replaceable so that tests can make it fail.
var chtimes = os.Chtimes

// idPattern is what Put returns; anything else is not an id, and Get
// refuses it before it can name a path.
var idPattern = regexp.MustCompile(`^[0-9a-f]{16}$`)

// Put saves text and returns its id: a hash of the text, so the same
// output saved twice is one file. Now and then it also deletes originals
// past the retention period.
func (s *Store) Put(text string) (id string, err error) {
	defer s.pruneIfDue()
	sum := sha256.Sum256([]byte(text))
	id = hex.EncodeToString(sum[:8])
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return "", fmt.Errorf("digest store: %w", err)
	}
	path := filepath.Join(s.dir, id+".txt")
	if _, err := os.Stat(path); err == nil {
		// Kept as long as it is in use. If it cannot be marked as in use
		// it may be pruned while a digest still points at it, so the
		// result is not digested.
		if err := chtimes(path, s.now(), s.now()); err != nil {
			return "", fmt.Errorf("digest store: %s could not be marked as in use, so it might be deleted before the digest is done with: %w", path, err)
		}
		return id, nil
	}
	tmp, err := os.CreateTemp(s.dir, "."+id+"-*")
	if err != nil {
		return "", fmt.Errorf("digest store: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(text); err != nil {
		_ = tmp.Close() // the write's error is the one to report
		return "", fmt.Errorf("digest store: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("digest store: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return "", fmt.Errorf("digest store: %w", err)
	}
	return id, nil
}

// ErrNotFound is returned for an id the store does not hold: never saved,
// or deleted after the retention period.
var ErrNotFound = errors.New("no original with that id: it was never stored here, or has been deleted after the retention period")

// Get returns the text saved under id.
func (s *Store) Get(id string) (string, error) {
	id = strings.TrimSpace(id)
	if !idPattern.MatchString(id) {
		return "", fmt.Errorf("%q is not an id; ids are 16 hex digits", id)
	}
	data, err := os.ReadFile(filepath.Join(s.dir, id+".txt")) // #nosec G304 -- id is 16 hex digits, checked above, so it cannot leave the store
	if os.IsNotExist(err) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("digest store: %w", err)
	}
	return string(data), nil
}

// Prune deletes originals older than the retention period. It only ever
// touches files named as Put names them. Originals hold tool output in
// full, so one it cannot delete is reported rather than skipped quietly:
// the error names each, and Prune carries on with the rest.
func (s *Store) Prune(now time.Time) error {
	if s.retain <= 0 {
		return nil
	}
	entries, err := os.ReadDir(s.dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("digest store: originals past logging.retainDays could not be looked for: %w", err)
	}
	var failed []error
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".txt") || !idPattern.MatchString(strings.TrimSuffix(name, ".txt")) {
			continue
		}
		if info, err := e.Info(); err == nil && now.Sub(info.ModTime()) > s.retain {
			path := filepath.Join(s.dir, name)
			if err := os.Remove(path); err != nil {
				failed = append(failed, fmt.Errorf("%s is past logging.retainDays but could not be deleted: %w", path, err))
			}
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("digest store: delete these by hand, since originals hold tool output in full: %w", errors.Join(failed...))
	}
	return nil
}
