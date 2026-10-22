package route

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"
)

// When a provider reaches a limit that says when it resets, every session
// moves down its list until then, and back on the first turn after (#378).
// Asking again before the reset would cost each turn a refused request and
// a slower answer; staying away long after it would leave the conversation
// on a weaker or dearer model for no reason.
//
// The state is per provider, not per session: a usage limit belongs to the
// account, so every session on janus meets it at once and is let back at
// once. It is kept in a file under logging.dir, so that a restart of janus
// does not send every session back to a provider that is still refusing.
//
// A provider that fails with no reset time, an outage, is left for a
// backoff that starts at outageBackoff and doubles while the failures go
// on, up to maxOutageBackoff; its first answer ends it.
const (
	outageBackoff    = 30 * time.Second
	maxOutageBackoff = 10 * time.Minute
)

// resets is when each provider can be asked again.
type resets struct {
	mu      sync.Mutex
	limits  map[string]reset
	outages map[string]*backoff
	file    string // empty keeps the limits in memory only
	now     func() time.Time
}

// reset is a limit with a reset time.
type reset struct {
	Until time.Time `json:"until"`
	Why   string    `json:"why"`
}

type backoff struct {
	until time.Time
	wait  time.Duration
}

func (r *resets) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

// out says why provider should not be asked now, or "" if it may be.
func (r *resets) out(provider string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.clock()
	if l, ok := r.limits[provider]; ok {
		if now.Before(l.Until) {
			return fmt.Sprintf("%s; it resets at %s, and is passed over until then", l.Why, l.Until.Local().Format("15:04"))
		}
		delete(r.limits, provider)
		r.saveLocked()
	}
	if b := r.outages[provider]; b != nil && now.Before(b.until) {
		return fmt.Sprintf("it has been failing, and is left alone until %s", b.until.Local().Format("15:04:05"))
	}
	return ""
}

// limit records that provider reached a limit lifting at until.
func (r *resets) limit(provider string, until time.Time, why string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.limits == nil {
		r.limits = map[string]reset{}
	}
	if l, ok := r.limits[provider]; ok && !until.After(l.Until) {
		return
	}
	r.limits[provider] = reset{Until: until, Why: why}
	r.saveLocked()
}

// outage records that provider is down with no reset time in sight.
func (r *resets) outage(provider string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.outages == nil {
		r.outages = map[string]*backoff{}
	}
	b := r.outages[provider]
	switch {
	case b == nil:
		b = &backoff{wait: outageBackoff}
		r.outages[provider] = b
	default:
		b.wait = min(2*b.wait, maxOutageBackoff)
	}
	b.until = r.clock().Add(b.wait)
}

// answered records that provider answered: whatever kept it away is over.
func (r *resets) answered(provider string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.outages, provider)
	if _, ok := r.limits[provider]; ok {
		delete(r.limits, provider)
		r.saveLocked()
	}
}

// saveLocked writes the limits to the file, if there is one. Losing them
// costs only the refused requests that would find them again, so a write
// that fails is not an error. Call it with r.mu held.
func (r *resets) saveLocked() {
	if r.file == "" {
		return
	}
	data, err := json.Marshal(r.limits)
	if err != nil {
		return
	}
	_ = os.WriteFile(r.file, data, 0o600) // #nosec G703 -- a file under logging.dir; see above
}

// load reads the limits from file, keeping the ones still in force, and
// writes there from now on. A missing file is no limits.
func (r *resets) load(file string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.file = file
	data, err := os.ReadFile(file) // #nosec G304 -- a file under logging.dir
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var limits map[string]reset
	if err := json.Unmarshal(data, &limits); err != nil {
		return fmt.Errorf("%s is not janus's record of provider limits: %w; delete it to start afresh", file, err)
	}
	now := r.clock()
	for provider, l := range limits {
		if now.Before(l.Until) {
			if r.limits == nil {
				r.limits = map[string]reset{}
			}
			r.limits[provider] = l
		}
	}
	return nil
}

// LimitsFile is where, under logging.dir, the router keeps the providers'
// limits across restarts.
const LimitsFile = "limits.json"

// Remember keeps the providers' limits in file, reading any already there.
func (r *Router) Remember(file string) error { return r.resets.load(file) }

// openaiWait is how long OpenAI's rate-limit answer says to wait: the
// longest of Retry-After and the reset of each limit it reports as used
// up, x-ratelimit-reset-requests or -tokens, which are durations such as
// "6m0s" (docs/research/openai.md, Errors). A limit with some left says
// nothing about this refusal.
func openaiWait(h http.Header) time.Duration {
	if h == nil {
		return 0
	}
	var wait time.Duration
	if secs, err := strconv.Atoi(h.Get("Retry-After")); err == nil {
		wait = time.Duration(secs) * time.Second
	}
	for _, limit := range []string{"Requests", "Tokens"} {
		if h.Get("X-Ratelimit-Remaining-"+limit) != "0" {
			continue
		}
		if d, err := time.ParseDuration(h.Get("X-Ratelimit-Reset-" + limit)); err == nil {
			wait = max(wait, d)
		}
	}
	return max(wait, 0)
}
