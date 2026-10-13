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
}

// NewStore returns a store in dir that keeps originals for retainDays, or
// for good if that is zero, and deletes what is already older.
func NewStore(dir string, retainDays int) *Store {
	s := &Store{dir: dir, retain: time.Duration(retainDays) * 24 * time.Hour}
	s.Prune(time.Now())
	return s
}

// idPattern is what Put returns; anything else is not an id, and Get
// refuses it before it can name a path.
var idPattern = regexp.MustCompile(`^[0-9a-f]{16}$`)

// Put saves text and returns its id: a hash of the text, so the same
// output saved twice is one file.
func (s *Store) Put(text string) (id string, err error) {
	sum := sha256.Sum256([]byte(text))
	id = hex.EncodeToString(sum[:8])
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return "", fmt.Errorf("digest store: %w", err)
	}
	path := filepath.Join(s.dir, id+".txt")
	if _, err := os.Stat(path); err == nil {
		os.Chtimes(path, time.Now(), time.Now()) // kept as long as it is in use
		return id, nil
	}
	tmp, err := os.CreateTemp(s.dir, "."+id+"-*")
	if err != nil {
		return "", fmt.Errorf("digest store: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(text); err != nil {
		tmp.Close()
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
	data, err := os.ReadFile(filepath.Join(s.dir, id+".txt"))
	if os.IsNotExist(err) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("digest store: %w", err)
	}
	return string(data), nil
}

// Prune deletes originals older than the retention period. It only ever
// touches files named as Put names them.
func (s *Store) Prune(now time.Time) {
	if s.retain <= 0 {
		return
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".txt") || !idPattern.MatchString(strings.TrimSuffix(name, ".txt")) {
			continue
		}
		if info, err := e.Info(); err == nil && now.Sub(info.ModTime()) > s.retain {
			os.Remove(filepath.Join(s.dir, name))
		}
	}
}
