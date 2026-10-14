package proxy

import (
	"strings"
	"testing"
)

// CauseHint must point at something concrete for every cause, so that
// whoever reads a stats row or an audit line knows where to look.
func TestCauseHint(t *testing.T) {
	cases := map[string]string{
		CauseUnreachable:  "LM Studio",
		CauseSkipped:      "failed repeatedly",
		CauseModelMissing: "model id",
		CauseLoadFailed:   "memory",
		CauseTooLarge:     "contextTokens",
		CauseBusy:         "maxConcurrent",
		CauseSlow:         "faster",
		CauseServerError:  "log",
		CauseBrokeOff:     "log",
		CauseRejected:     "not up to this work",
	}
	for cause, want := range cases {
		if got := CauseHint(cause); !strings.Contains(got, want) {
			t.Errorf("CauseHint(%q) = %q, want it to mention %q", cause, got, want)
		}
	}
	if got := CauseHint("no-such-cause"); got != "" {
		t.Errorf("CauseHint for an unknown cause = %q, want empty", got)
	}
}

func TestOverflowed(t *testing.T) {
	for _, m := range []string{
		"This model's maximum context length is 8192 tokens",
		"prompt is too long: exceeds the context",
		"maximum context length exceeded (n_ctx)",
		"Too many tokens for the context window",
	} {
		if !Overflowed(m) {
			t.Errorf("Overflowed(%q) = false, want true", m)
		}
	}
	for _, m := range []string{
		"could not reach LM Studio: connection refused",
		"the answer broke off",
		"internal error",
	} {
		if Overflowed(m) {
			t.Errorf("Overflowed(%q) = true, want false", m)
		}
	}
}
