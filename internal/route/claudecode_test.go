package route

import (
	"net/http"
	"strconv"
	"strings"
	"testing"
)

func TestClaudeCodeVersionIsReadFromTheUserAgent(t *testing.T) {
	for ua, want := range map[string]string{
		"claude-cli/2.1.301 (external, cli)":    "2.1.301",
		"claude-cli/2.1.296":                    "2.1.296",
		"claude-cli/":                           "",
		"claude-cli/2.1.301-beta (external)":    "",
		"claude-cli/2.1.301;rm -rf (external)":  "",
		"Mozilla/5.0 claude-cli/2.1.301":        "",
		"anthropic-sdk-typescript/0.60.0 (cli)": "",
		"":                                      "",
	} {
		h := http.Header{}
		h.Set("User-Agent", ua)
		if got := ClaudeCodeVersion(h); got != want {
			t.Errorf("ClaudeCodeVersion(%q) = %q, want %q", ua, got, want)
		}
	}
}

func TestTheTestedRangeIsInOrder(t *testing.T) {
	if newerThanTested(TestedClaudeCode.Oldest) || !atLeast(TestedClaudeCode.Newest, TestedClaudeCode.Oldest) {
		t.Errorf("TestedClaudeCode %+v is not oldest then newest", TestedClaudeCode)
	}
}

// newerThanTested reports whether Claude Code release v is newer than
// TestedClaudeCode.Newest.
func newerThanTested(v string) bool { return !atLeast(TestedClaudeCode.Newest, v) }

// atLeast reports whether dotted version a is at least b.
func atLeast(a, b string) bool {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) || i < len(bs); i++ {
		var x, y int
		if i < len(as) {
			x, _ = strconv.Atoi(as[i])
		}
		if i < len(bs) {
			y, _ = strconv.Atoi(bs[i])
		}
		if x != y {
			return x > y
		}
	}
	return true
}
