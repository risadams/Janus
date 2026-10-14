package proxy

import "strings"

// Why a local model did not answer. Every local attempt that was not
// accepted has one (Attempt.Cause), and so has every decision to send a
// request upstream that a rule or stand-in meant for the local model
// (Plan.LocalCause). Each points to a different next step (CauseHint), and
// they are recorded in the ledger under these names (#202).
const (
	// CauseUnreachable: no connection to LM Studio at all.
	CauseUnreachable = "unreachable"
	// CauseSkipped: the local backend failed repeatedly, and is being
	// skipped for a while without being asked.
	CauseSkipped = "skipped"
	// CauseModelMissing: LM Studio does not have the model.
	CauseModelMissing = "model_missing"
	// CauseLoadFailed: the model was not loaded, and loading it failed.
	CauseLoadFailed = "load_failed"
	// CauseTooLarge: the request does not fit the model's context.
	CauseTooLarge = "too_large"
	// CauseBusy: the model already had as many requests as it may take.
	CauseBusy = "busy"
	// CauseSlow: the model ran out of time before it had answered.
	CauseSlow = "slow"
	// CauseServerError: LM Studio answered with an error.
	CauseServerError = "server_error"
	// CauseBrokeOff: the answer stopped part way, not by a timeout.
	CauseBrokeOff = "broke_off"
	// CauseRejected: a whole answer came back and failed a check.
	CauseRejected = "rejected"
	// CauseUnsupported: the request could not be adapted for the local
	// model at all.
	CauseUnsupported = "unsupported"
)

// CauseHint is the next step for a cause, in a few words.
func CauseHint(cause string) string {
	switch cause {
	case CauseUnreachable:
		return "start LM Studio's server, or check local.baseUrl"
	case CauseSkipped:
		return "it failed repeatedly; fix that and it resumes by itself"
	case CauseModelMissing:
		return "correct the model id; janus doctor lists the ones LM Studio has"
	case CauseLoadFailed:
		return "free memory, or lower contextTokens"
	case CauseTooLarge:
		return "raise contextTokens, if the machine has the memory"
	case CauseBusy:
		return "raise maxConcurrent, if the machine has the memory"
	case CauseSlow:
		return "raise maxSeconds or timeoutSeconds, or use a faster model"
	case CauseServerError:
		return "see LM Studio's log"
	case CauseBrokeOff:
		return "LM Studio stopped mid-answer; see its log"
	case CauseRejected:
		return "the model is not up to this work"
	case CauseUnsupported:
		return "the request uses something the local model can't be sent"
	}
	return ""
}

// overflowWords are how LM Studio and the engines under it say a request
// did not fit the model's context.
var overflowWords = []string{
	"context length", "context window", "context size", "context overflow",
	"exceeds the context", "exceed the context", "n_ctx", "too many tokens",
	"maximum context", "context_length_exceeded",
}

// Overflowed reports whether an error from the local model says the
// request was too large for its context. Janus's own size estimate can pass
// a request the model's tokenizer finds too long.
func Overflowed(message string) bool {
	m := strings.ToLower(message)
	for _, w := range overflowWords {
		if strings.Contains(m, w) {
			return true
		}
	}
	return false
}
