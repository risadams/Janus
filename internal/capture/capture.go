package capture

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/risadams/janus/internal/proxy"
)

// Writer saves each exchange as three or four files under
// <dir>/<day>/<id>_<path>:
//
//	.md            the readable render; start here
//	.request.txt   the request body exactly as the client sent it
//	.response.txt  the response body, decompressed
//	.sent.txt      the request as janus sent it on, if it changed it
//
// Captures hold prompts, code, and tool output in full, so every file is
// created readable by its owner only, and old ones are pruned.
type Writer struct {
	dir string
	// retain is how long captures are kept; maxBytes is how much disk they
	// may use. Zero means no limit.
	retain   time.Duration
	maxBytes int64

	mu     sync.Mutex
	writes int
}

// pruneEvery is how many captures are written between prunes.
const pruneEvery = 50

// New returns a Writer and prunes what is already there.
func New(dir string, retainDays, maxMegabytes int) *Writer {
	w := &Writer{
		dir:      dir,
		retain:   time.Duration(retainDays) * 24 * time.Hour,
		maxBytes: int64(maxMegabytes) << 20,
	}
	w.Prune(time.Now())
	return w
}

// Write saves one exchange and returns its name: the path under the
// captures directory, without extension, which the ledger records.
func (w *Writer) Write(ex proxy.Exchange) (name string, err error) {
	day := ex.Start.UTC().Format("2006-01-02")
	name = filepath.Join(day, ex.ID+"_"+sanitize(ex.Path))
	base := filepath.Join(w.dir, name)

	if err := os.MkdirAll(filepath.Dir(base), 0o700); err != nil {
		return "", fmt.Errorf("capture: %w", err)
	}
	files := map[string][]byte{
		".request.txt":  ex.RequestBody,
		".response.txt": ex.ResponseBody,
		".md":           []byte(Render(ex)),
	}
	// What was actually sent, when janus changed the request on the way:
	// this is the file that shows what left the machine.
	if ex.SentBody != nil {
		files[".sent.txt"] = ex.SentBody
	}
	for ext, content := range files {
		if err := os.WriteFile(base+ext, content, 0o600); err != nil {
			return "", fmt.Errorf("capture: %w", err)
		}
	}

	w.mu.Lock()
	w.writes++
	due := w.writes%pruneEvery == 0
	w.mu.Unlock()
	if due {
		w.Prune(time.Now())
	}
	return name, nil
}

// WriteShadow saves a shadow run beside the capture it shadows, as
// <name>.shadow.md, .shadow.request.txt (what was sent to the local model),
// and .shadow.response.txt. name is what Write returned for the real
// request.
func (w *Writer) WriteShadow(name string, ex proxy.Exchange) error {
	base := filepath.Join(w.dir, name)
	if err := os.MkdirAll(filepath.Dir(base), 0o700); err != nil {
		return fmt.Errorf("capture: %w", err)
	}
	for ext, content := range map[string][]byte{
		".shadow.request.txt":  ex.RequestBody,
		".shadow.response.txt": ex.ResponseBody,
		".shadow.md":           []byte(Render(ex)),
	} {
		if err := os.WriteFile(base+ext, content, 0o600); err != nil {
			return fmt.Errorf("capture: %w", err)
		}
	}
	return nil
}

// WriteAttempt saves a local answer that was rejected in favour of
// Claude's, beside the capture of the request, as
// <name>.attempt.response.txt. It is the evidence for why janus escalated.
func (w *Writer) WriteAttempt(name string, response []byte) error {
	base := filepath.Join(w.dir, name)
	if err := os.MkdirAll(filepath.Dir(base), 0o700); err != nil {
		return fmt.Errorf("capture: %w", err)
	}
	if err := os.WriteFile(base+".attempt.response.txt", response, 0o600); err != nil {
		return fmt.Errorf("capture: %w", err)
	}
	return nil
}

// sanitize turns a request path into something safe in a file name:
// "/v1/messages" becomes "v1-messages".
func sanitize(path string) string {
	var b strings.Builder
	for _, r := range strings.Trim(path, "/") {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	if b.Len() == 0 {
		return "root"
	}
	if b.Len() > 80 {
		return b.String()[:80]
	}
	return b.String()
}

// Prune deletes captures older than the retention period, then the oldest
// remaining ones until the total fits the size limit. It only ever touches
// day directories and the capture files inside them.
func (w *Writer) Prune(now time.Time) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	days, err := os.ReadDir(w.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("capture: %w", err)
	}

	type file struct {
		path string
		size int64
	}
	var files []file
	var total int64
	var dayDirs []string
	for _, d := range days {
		day, err := time.Parse("2006-01-02", d.Name())
		if err != nil || !d.IsDir() {
			continue // not something janus made
		}
		dayDir := filepath.Join(w.dir, d.Name())
		// A day directory covers 24 hours, so it is only wholly past
		// retention once its last moment is.
		if w.retain > 0 && now.Sub(day.Add(24*time.Hour)) > w.retain {
			os.RemoveAll(dayDir)
			continue
		}
		dayDirs = append(dayDirs, dayDir)
		filepath.WalkDir(dayDir, func(path string, entry fs.DirEntry, err error) error {
			if err != nil || entry.IsDir() {
				return nil
			}
			if info, err := entry.Info(); err == nil {
				files = append(files, file{path, info.Size()})
				total += info.Size()
			}
			return nil
		})
	}

	if w.maxBytes > 0 && total > w.maxBytes {
		// Names begin with the day and then a timestamped ID, so sorting by
		// path is sorting by age.
		sort.Slice(files, func(i, j int) bool { return files[i].path < files[j].path })
		removing := ""
		for _, f := range files {
			// Finish removing a capture once started, so that none is left
			// with only some of its files.
			if total <= w.maxBytes && stem(f.path) != removing {
				break
			}
			if os.Remove(f.path) == nil {
				total -= f.size
				removing = stem(f.path)
			}
		}
		for _, dir := range dayDirs {
			os.Remove(dir) // succeeds only if the directory is now empty
		}
	}
	return nil
}

// stem is a capture file's path without its extension, which every file of
// one capture shares, its shadow, replay, and attempt files included.
func stem(path string) string {
	for _, ext := range []string{
		".shadow.request.txt", ".shadow.response.txt", ".shadow.md",
		".replay.response.txt", ".replay.md",
		".attempt.response.txt",
		".sent.txt",
		".request.txt", ".response.txt", ".md",
	} {
		if s, ok := strings.CutSuffix(path, ext); ok {
			return s
		}
	}
	return path
}
