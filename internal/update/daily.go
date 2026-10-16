package update

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// CacheFile is the name, in the log folder, of the file that keeps the
// last daily check's answer.
const CacheFile = "update-check.json"

// checkEvery is how often Daily asks GitHub at most.
const checkEvery = 24 * time.Hour

// cached is the last check, as kept in the cache file.
type cached struct {
	Checked    time.Time `json:"checked"`
	Prerelease bool      `json:"prerelease"`
	Tag        string    `json:"tag,omitempty"`
	URL        string    `json:"url,omitempty"`
}

// Daily is Latest asked at most once a day: within a day of the last
// check, with the same choice about pre-releases, it answers from the file
// at path and sends nothing. A check that fails is remembered too, so a
// machine that is offline does not ask again on every start; the failure
// is returned only the first time. The file is only a cache: if it cannot
// be read or written, Daily asks GitHub as Latest does.
func Daily(ctx context.Context, client *http.Client, api, userAgent string, pre bool, path string, now time.Time) (Release, bool, error) {
	if b, err := os.ReadFile(path); err == nil {
		var c cached
		if json.Unmarshal(b, &c) == nil && c.Prerelease == pre {
			// A check stamped in the future, from a clock set wrong, is
			// not trusted to hold off the next one.
			if age := now.Sub(c.Checked); age >= 0 && age < checkEvery {
				if v, ok := Parse(c.Tag); ok {
					return Release{Version: v, Tag: c.Tag, URL: c.URL}, true, nil
				}
				return Release{}, false, nil
			}
		}
	}

	r, found, err := Latest(ctx, client, api, userAgent, pre)
	if ctx.Err() != nil {
		// Janus is stopping; that is not an answer worth keeping.
		return r, found, err
	}
	c := cached{Checked: now, Prerelease: pre}
	if found && err == nil {
		c.Tag, c.URL = r.Tag, r.URL
	}
	writeCache(path, c)
	return r, found, err
}

// writeCache replaces the cache file whole, so that a Janus stopped while
// writing it leaves the old one or the new one, never half of either.
func writeCache(path string, c cached) {
	b, err := json.Marshal(c)
	if err != nil {
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), CacheFile+".*")
	if err != nil {
		return
	}
	_, werr := tmp.Write(b)
	cerr := tmp.Close()
	if werr != nil || cerr != nil || os.Rename(tmp.Name(), path) != nil {
		os.Remove(tmp.Name())
	}
}
