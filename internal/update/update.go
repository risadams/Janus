// Package update finds out whether a newer Janus has been released. It
// asks GitHub's releases API, and only when a command asks it to or the
// config's update.check is on: Janus sends nothing to check for updates
// otherwise.
package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// DefaultAPI lists Janus's releases. GitHub's /releases/latest is not used,
// because it leaves pre-releases out and so cannot honour the option to
// include them.
const DefaultAPI = "https://api.github.com/repos/risadams/janus/releases"

// maxBody bounds how much of the API's answer is read. A page of releases
// is tens of kilobytes; anything far larger is not the API.
const maxBody = 4 << 20

// Release is one published version.
type Release struct {
	Version Version
	// Tag is the git tag, as published: "v0.9.0-rc.12".
	Tag string
	// URL is the release's page.
	URL string
	// Assets are the files published with it, by name, each with the URL
	// it is downloaded from.
	Assets map[string]string
}

// Latest returns the newest release at api, or found false if there is
// none. Pre-releases count only when pre is true; drafts and tags that are
// not semantic versions never do. Only the first page of 100 is read:
// GitHub lists the newest first, so the newest version is on it.
func Latest(ctx context.Context, client *http.Client, api, userAgent string, pre bool) (r Release, found bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, api+"?per_page=100", nil)
	if err != nil {
		return Release{}, false, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", userAgent)
	resp, err := client.Do(req)
	if err != nil {
		return Release{}, false, err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests:
		return Release{}, false, errors.New("GitHub's rate limit for this address is used up; try again in an hour")
	case resp.StatusCode != http.StatusOK:
		return Release{}, false, fmt.Errorf("GitHub answered %s", resp.Status)
	}

	var listed []struct {
		Tag        string `json:"tag_name"`
		URL        string `json:"html_url"`
		Draft      bool   `json:"draft"`
		Prerelease bool   `json:"prerelease"`
		Assets     []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(&listed); err != nil {
		return Release{}, false, fmt.Errorf("GitHub's list of releases could not be read: %w", err)
	}
	for _, l := range listed {
		v, ok := Parse(l.Tag)
		if !ok || l.Draft {
			continue
		}
		// A tag with a pre-release part is a pre-release whatever the flag
		// on the release says, and so is one marked as such.
		if (l.Prerelease || v.Prerelease()) && !pre {
			continue
		}
		if !found || v.Compare(r.Version) > 0 {
			r, found = Release{Version: v, Tag: l.Tag, URL: l.URL, Assets: map[string]string{}}, true
			for _, a := range l.Assets {
				r.Assets[a.Name] = a.URL
			}
		}
	}
	return r, found, nil
}

// Version is a semantic version: major.minor.patch, an optional
// pre-release part, and build metadata, which is ignored.
type Version struct {
	major, minor, patch int
	pre                 []string
}

// Parse reads a version such as "v1.2.3" or "0.9.0-rc.12". The leading v
// is optional.
func Parse(s string) (Version, bool) {
	s = strings.TrimPrefix(s, "v")
	s, _, _ = strings.Cut(s, "+")
	core, pre, hasPre := strings.Cut(s, "-")
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return Version{}, false
	}
	var nums [3]int
	for i, p := range parts {
		n, ok := number(p)
		if !ok {
			return Version{}, false
		}
		nums[i] = n
	}
	v := Version{major: nums[0], minor: nums[1], patch: nums[2]}
	if hasPre {
		v.pre = strings.Split(pre, ".")
		for _, id := range v.pre {
			if id == "" || strings.Trim(id, "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ-") != "" {
				return Version{}, false
			}
		}
	}
	return v, true
}

// number reads a non-negative decimal of digits only, so that "+1" and
// " 1", which strconv accepts or nearly does, are not versions.
func number(s string) (int, bool) {
	if s == "" || strings.Trim(s, "0123456789") != "" {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	return n, err == nil
}

// Prerelease reports whether v has a pre-release part, as "-rc.1" does.
func (v Version) Prerelease() bool { return len(v.pre) > 0 }

// Compare returns -1, 0, or 1 as v is older than, the same as, or newer
// than w, by the rules of semver.org §11: a pre-release is older than the
// release it leads to, and pre-release parts compare field by field,
// numbers as numbers and below words.
func (v Version) Compare(w Version) int {
	for _, d := range [3]int{v.major - w.major, v.minor - w.minor, v.patch - w.patch} {
		if d != 0 {
			return sign(d)
		}
	}
	switch {
	case !v.Prerelease() && !w.Prerelease():
		return 0
	case !v.Prerelease():
		return 1
	case !w.Prerelease():
		return -1
	}
	for i := 0; i < len(v.pre) && i < len(w.pre); i++ {
		a, aNum := number(v.pre[i])
		b, bNum := number(w.pre[i])
		switch {
		case aNum && bNum:
			if a != b {
				return sign(a - b)
			}
		case aNum:
			return -1
		case bNum:
			return 1
		default:
			if c := strings.Compare(v.pre[i], w.pre[i]); c != 0 {
				return c
			}
		}
	}
	return sign(len(v.pre) - len(w.pre))
}

func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	}
	return 0
}
