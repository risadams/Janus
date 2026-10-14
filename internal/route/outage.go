package route

import (
	"net/http"
	"strconv"
	"sync"
	"time"
)

// When Claude fails, an ifDown stand-in should answer only if Claude is
// down for a while: a session limit, or an outage. A brief failure is
// better relayed to Claude Code, which retries with backoff and usually
// reaches Claude a few seconds later; a local model's turn would stay in
// the conversation for no reason (#150).
//
// The ledger's rate-limit headers on real failures (docs/research/
// standin.md) show a session limit as a 429 whose retry-after is the time
// to its reset, hours away, and anthropic-ratelimit-unified-reset with the
// same. x-should-retry was "true" on every one of them, so it says nothing.
const (
	// longWait is the wait, by the response's own headers, past which a
	// failure is a limit to stand in for at once rather than to retry.
	longWait = time.Minute
	// outageWindow and outageFailures are what counts as an outage when
	// the headers say nothing: this many failures within the window,
	// without Claude answering in between. Claude Code's backoff puts
	// three failures within about ten seconds.
	outageWindow   = 2 * time.Minute
	outageFailures = 3
)

// outage decides, from Claude's failures, when it has been down long enough
// for the local model to stand in. One per router: Claude is one service.
type outage struct {
	mu       sync.Mutex
	failures []time.Time
	now      func() time.Time
}

// standIn is a plan's FallBackOn for Claude: whether this failure, with
// those before it, means the local model should answer. Status 0 is no
// response at all. A status that is not Claude being unable to answer (a
// bad request, say) is never a reason.
func (o *outage) standIn(status int, header http.Header) bool {
	if status != 0 && !UpstreamDown(status) {
		return false
	}
	now := o.clock()
	if retryWait(header, now) >= longWait {
		return true
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	kept := o.failures[:0]
	for _, t := range o.failures {
		if now.Sub(t) < outageWindow {
			kept = append(kept, t)
		}
	}
	o.failures = append(kept, now)
	return len(o.failures) >= outageFailures
}

// recovered is told when Claude answers: the failures before it were not
// an outage, or it is over.
func (o *outage) recovered() {
	o.mu.Lock()
	o.failures = nil
	o.mu.Unlock()
}

func (o *outage) clock() time.Time {
	if o.now != nil {
		return o.now()
	}
	return time.Now()
}

// retryWait is how long a failed response says to wait before trying
// again: the longer of retry-after (seconds, or an HTTP date) and the time
// to anthropic-ratelimit-unified-reset (a Unix time) when the unified limit
// is rejected. Zero if it says nothing.
func retryWait(h http.Header, now time.Time) time.Duration {
	if h == nil {
		return 0
	}
	var wait time.Duration
	if v := h.Get("Retry-After"); v != "" {
		if secs, err := strconv.Atoi(v); err == nil {
			wait = time.Duration(secs) * time.Second
		} else if at, err := http.ParseTime(v); err == nil {
			wait = at.Sub(now)
		}
	}
	if h.Get("Anthropic-Ratelimit-Unified-Status") == "rejected" {
		if secs, err := strconv.ParseInt(h.Get("Anthropic-Ratelimit-Unified-Reset"), 10, 64); err == nil {
			wait = max(wait, time.Unix(secs, 0).Sub(now))
		}
	}
	return max(wait, 0)
}
