package ledger

import (
	"strings"

	"github.com/risadams/janus/internal/proxy"
)

// AttemptCause is why a local attempt was not accepted: its recorded cause
// or, for a record written before causes were (#202), the one its Why
// shows. Janus has always written Why in a fixed form, so the old records
// can be read the same way. Empty for an accepted attempt, and for an
// attempt at the upstream.
func AttemptCause(a Attempt) string {
	if a.Verdict == VerdictAccepted || a.Backend != "local" {
		return ""
	}
	if a.Cause != "" {
		return a.Cause
	}
	why := strings.ToLower(a.Why)
	switch {
	case proxy.Overflowed(why):
		return proxy.CauseTooLarge
	case strings.Contains(why, "deadline exceeded"), strings.Contains(why, "did not answer within"):
		return proxy.CauseSlow
	case strings.Contains(why, "could not reach"):
		return proxy.CauseUnreachable
	case strings.Contains(why, "answered http"), strings.Contains(why, "returned an error"):
		return proxy.CauseServerError
	case strings.Contains(why, "broke off"), strings.Contains(why, "ended before it should have"):
		return proxy.CauseBrokeOff
	}
	return proxy.CauseRejected
}

// LocalCause is why a request a rule or stand-in meant for the local model
// never reached it: the record's LocalCause or, for an older record, the
// one its Reason shows. Empty if the local model was not passed over.
func LocalCause(rec Record) string {
	if rec.LocalCause != "" {
		return rec.LocalCause
	}
	reason := strings.ToLower(rec.Reason)
	if !strings.HasSuffix(reason, "; sent upstream") && !strings.Contains(reason, "no local stand-in:") {
		return ""
	}
	switch {
	case strings.Contains(reason, "times in a row and is being skipped"):
		return proxy.CauseSkipped
	case strings.Contains(reason, "requests in flight, its limit"):
		return proxy.CauseBusy
	case strings.Contains(reason, "could not reach lm studio"):
		return proxy.CauseUnreachable
	case strings.Contains(reason, "lm studio does not have local model"):
		return proxy.CauseModelMissing
	case strings.Contains(reason, "could not load local model"):
		return proxy.CauseLoadFailed
	case strings.Contains(reason, "is not loaded"), strings.Contains(reason, "tokens, more than"), strings.Contains(reason, "has room for"):
		return proxy.CauseTooLarge
	case strings.Contains(reason, "cannot read this request"), strings.Contains(reason, "cannot adapt this request"), strings.Contains(reason, "could not be read again"):
		return proxy.CauseUnsupported
	}
	// A local attempt that failed and went upstream says so in its
	// attempt, which AttemptCause reads.
	return ""
}

// firstCause is why a record shows the local model not answering: the
// failed attempt's cause when one was tried, else the record's own
// LocalCause.
func firstCause(rec Record) string {
	if rec.Attempt != nil {
		return AttemptCause(*rec.Attempt)
	}
	return LocalCause(rec)
}

// CauseRow counts one reason the local model did not answer: local
// attempts that ended that way, and requests that were never sent to it
// for that reason.
type CauseRow struct {
	Cause    string `json:"cause"`
	Attempts int    `json:"attempts"`
	NotTried int    `json:"not_tried"`
}

// Hint is the next step for the row's cause, in a few words.
func (r CauseRow) Hint() string { return proxy.CauseHint(r.Cause) }
