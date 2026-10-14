package main

import (
	"bytes"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/risadams/janus/internal/anthropic"
	"github.com/risadams/janus/internal/guard"
	"github.com/risadams/janus/internal/ledger"
	"github.com/risadams/janus/internal/proxy"
	"github.com/risadams/janus/internal/transform"
)

// testConsole is a plain console with a fixed clock, as record's tests
// need one.
func testConsole(w io.Writer) *Console {
	c := NewConsole(w, false, ledger.DefaultPrices)
	c.now = func() time.Time { return time.Date(2026, 10, 13, 16, 35, 50, 0, time.Local) }
	return c
}

// consoleExchange is a Messages request from Claude Code's main loop, as
// it reaches record.
func consoleExchange(edit func(*proxy.Exchange)) proxy.Exchange {
	header := http.Header{}
	header.Set("X-Claude-Code-Session-Id", "s1")
	ex := proxy.Exchange{
		ID: "x", Start: time.Date(2026, 10, 13, 20, 35, 30, 0, time.UTC), Method: "POST", Path: "/v1/messages",
		Header: header, Status: 200, Duration: 20554 * time.Millisecond, Backend: "upstream",
		RequestBody: []byte(`{"model":"claude-opus-5-5","messages":[{"role":"user","content":"hi"}]}`),
	}
	if edit != nil {
		edit(&ex)
	}
	return ex
}

func n(i int) *int { return &i }

func TestConsoleLines(t *testing.T) {
	cases := map[string]struct {
		ex   proxy.Exchange
		want string
	}{
		// The first line of the issue's example: a main-loop turn, nearly
		// all of it from the cache.
		"main turn": {
			consoleExchange(func(ex *proxy.Exchange) {
				ex.Summary = &anthropic.Summary{Model: "claude-opus-5-5", StopReason: "tool_use", Complete: true,
					Usage: anthropic.Usage{Input: 2, Output: 2324, CacheWrite: n(1912), CacheRead: n(148855), CacheWrite1h: n(1912), CacheWrite5m: n(0)}}
			}),
			"main       Opus 5.5                ✓ 20.6s   tool call   151k in · 98% cached    2.3k out   $0.092",
		},
		"subagent": {
			consoleExchange(func(ex *proxy.Exchange) {
				ex.Header.Set("X-Claude-Code-Agent-Id", "a1")
				ex.Duration = 967 * time.Millisecond
				ex.Summary = &anthropic.Summary{Model: "claude-sonnet-5", StopReason: "end_turn", Complete: true,
					Usage: anthropic.Usage{Input: 90, Output: 9, CacheWrite: n(2374), CacheRead: n(81340)}}
			}),
			"subagent   Sonnet 5                ✓ 967ms   answered    84k in · 97% cached     9 out      $0.022",
		},
		"answered locally": {
			consoleExchange(func(ex *proxy.Exchange) {
				ex.Backend, ex.BackendModel, ex.Duration = "local", "qwen/qwen3.8-27b", 3180*time.Millisecond
				ex.Summary = &anthropic.Summary{Model: "qwen/qwen3.8-27b", StopReason: "end_turn", Complete: true, Usage: anthropic.Usage{Input: 22, Output: 37}}
			}),
			"main       qwen3.8-27b (local)     ✓ 3.2s    answered    22 in                   37 out     free",
		},
		"stood in for Claude": {
			consoleExchange(func(ex *proxy.Exchange) {
				ex.Backend, ex.BackendModel, ex.Duration = "local", "qwen/qwen3.8-27b", 3180*time.Millisecond
				ex.Attempt = &proxy.Attempt{Backend: "upstream", Why: "upstream answered HTTP 429", Duration: 300 * time.Millisecond}
				ex.Summary = &anthropic.Summary{Model: "qwen/qwen3.8-27b", StopReason: "end_turn", Complete: true, Usage: anthropic.Usage{Input: 22, Output: 37}}
			}),
			"main       qwen3.8-27b (local)     ✓ 3.2s    answered    22 in                   37 out     free\n" +
				"          ↳ Claude could not answer (upstream answered HTTP 429), so the local model stood in",
		},
		"stood in on part of a long conversation": {
			consoleExchange(func(ex *proxy.Exchange) {
				ex.Backend, ex.BackendModel, ex.Duration = "local", "qwen/qwen3.8-27b", 3180*time.Millisecond
				ex.Stages = []string{"set_model", "leave_out_messages"}
				ex.Attempt = &proxy.Attempt{Backend: "upstream", Why: "upstream answered HTTP 429", Duration: 300 * time.Millisecond}
				ex.Summary = &anthropic.Summary{Model: "qwen/qwen3.8-27b", StopReason: "end_turn", Complete: true, Usage: anthropic.Usage{Input: 22, Output: 37}}
			}),
			"main       qwen3.8-27b (local)     ✓ 3.2s    answered    22 in                   37 out     free\n" +
				"          ↳ Claude could not answer (upstream answered HTTP 429), so the local model stood in, with older turns left out to fit its context",
		},
		"escalated": {
			consoleExchange(func(ex *proxy.Exchange) {
				ex.Header.Set("X-Claude-Code-Agent-Id", "a1")
				ex.Attempt = &proxy.Attempt{Backend: "local", Why: "the local answer has no text and no tool call", Duration: 9340 * time.Millisecond}
				ex.Summary = &anthropic.Summary{Model: "claude-opus-5-5", StopReason: "end_turn", Complete: true, Usage: anthropic.Usage{Input: 2, Output: 9}}
			}),
			"subagent   Opus 5.5                ✓ 20.6s   answered    2 in                    9 out      <$0.001\n" +
				"          ↳ local tried for 9.3s first, then it went to Claude: the local answer has no text and no tool call",
		},
		"rate limited": {
			consoleExchange(func(ex *proxy.Exchange) {
				ex.Status, ex.Duration = 429, 505*time.Millisecond
				ex.Summary = &anthropic.Summary{Complete: true, Error: &anthropic.APIError{Type: "rate_limit_error"}}
			}),
			"main       Opus 5.5                ✗ 429 505ms  rate limited",
		},
		"Janus could not reach Claude": {
			consoleExchange(func(ex *proxy.Exchange) {
				ex.Status, ex.Duration = 502, 223*time.Millisecond
				ex.Fault = "could not reach upstream: dial tcp: lookup api.anthropic.com: no such host"
			}),
			"main       Opus 5.5                ✗ 502 223ms\n" +
				"          ↳ fault: could not reach upstream: dial tcp: lookup api.anthropic.com: no such host",
		},
		"no stand-in": {
			consoleExchange(func(ex *proxy.Exchange) {
				ex.Status, ex.Duration = 429, 505*time.Millisecond
				ex.NoFallback = `no local stand-in: this request is about 281000 tokens, more than the 32768-token context of local model "qwen/qwen3.8-27b"`
				ex.Summary = &anthropic.Summary{Complete: true, Error: &anthropic.APIError{Type: "rate_limit_error"}}
			}),
			"main       Opus 5.5                ✗ 429 505ms  rate limited\n" +
				`          ↳ no local stand-in: this request is about 281000 tokens, more than the 32768-token context of local model "qwen/qwen3.8-27b"`,
		},
		"refused": {
			consoleExchange(func(ex *proxy.Exchange) {
				ex.Backend, ex.Status, ex.Duration, ex.Reason = "none", 403, 2*time.Millisecond, "local-only content and no local model"
			}),
			"main       Opus 5.5                ✗ refused 2ms\n" +
				"          ↳ refused: local-only content and no local model",
		},
		"cut off": {
			consoleExchange(func(ex *proxy.Exchange) {
				ex.Summary = &anthropic.Summary{Model: "claude-opus-5-5", Usage: anthropic.Usage{Input: 2, Output: 4}}
			}),
			"main       Opus 5.5                ✓ 20.6s   cut off     2 in                    4 out      <$0.001",
		},
		"digested": {
			consoleExchange(func(ex *proxy.Exchange) {
				ex.Transforms = []transform.Effect{{Name: "digest", Blocks: 2, Rewritten: 1, Tokens: 3373}}
				ex.Summary = &anthropic.Summary{Model: "claude-opus-5-5", StopReason: "end_turn", Complete: true, Usage: anthropic.Usage{Input: 2, Output: 9}}
			}),
			"main       Opus 5.5                ✓ 20.6s   answered    2 in                    9 out      <$0.001\n" +
				"          ↳ digest: 1 rewritten, 0 revised, about 3.4k tokens fewer",
		},
		"carried, not decided": {
			consoleExchange(func(ex *proxy.Exchange) {
				ex.Transforms = []transform.Effect{{Name: "digest", Blocks: 1, Tokens: 3000}}
				ex.Summary = &anthropic.Summary{Model: "claude-opus-5-5", StopReason: "end_turn", Complete: true, Usage: anthropic.Usage{Input: 2, Output: 9}}
			}),
			"main       Opus 5.5                ✓ 20.6s   answered    2 in                    9 out      <$0.001",
		},
		"unreadable encoding": {
			consoleExchange(func(ex *proxy.Exchange) { ex.Unread = "content-encoding br" }),
			"main       Opus 5.5                ✓ 20.6s\n" +
				"          ↳ response not read: content-encoding br",
		},
		"not a Messages request": {
			proxy.Exchange{Method: "HEAD", Path: "/api/hello", Status: 200, Duration: 154 * time.Millisecond, Header: http.Header{}},
			"ping       HEAD /api/hello         ✓ 154ms",
		},
		"a long path is cut to fit": {
			proxy.Exchange{Method: "POST", Path: "/v1/messages/count_tokens", Status: 200, Duration: 80 * time.Millisecond, Header: http.Header{}},
			"count      POST /v1/messages/cou…  ✓ 80ms",
		},
	}
	for name, c := range cases {
		rec := ledger.FromExchange(c.ex)
		got := formatRequest(c.ex, rec, exchangeNotes(c.ex), ledger.DefaultPrices, func(_ style, s string) string { return s })
		if got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", name, got, c.want)
		}
	}
}

func TestConsoleStampsTimeAndPrintsTheDateWhenItChanges(t *testing.T) {
	var out bytes.Buffer
	c := testConsole(&out)
	c.Printf("first")
	c.Logger().Printf("second")
	c.now = func() time.Time { return time.Date(2026, 10, 14, 0, 0, 1, 0, time.Local) }
	c.Printf("after midnight")

	want := "── Tuesday 13 October 2026 ──\n16:35:50  first\n16:35:50  second\n── Wednesday 14 October 2026 ──\n00:00:01  after midnight\n"
	if out.String() != want {
		t.Errorf("got\n%s\nwant\n%s", out.String(), want)
	}
}

func TestConsoleColor(t *testing.T) {
	failed := consoleExchange(func(ex *proxy.Exchange) {
		ex.Status = 429
		ex.Summary = &anthropic.Summary{Complete: true, Error: &anthropic.APIError{Type: "rate_limit_error"}}
	})
	cached := consoleExchange(func(ex *proxy.Exchange) {
		ex.Summary = &anthropic.Summary{Model: "claude-opus-5-5", StopReason: "tool_use", Complete: true,
			Usage: anthropic.Usage{Input: 2, Output: 2324, CacheWrite: n(1912), CacheRead: n(148855)}}
	})
	local := consoleExchange(func(ex *proxy.Exchange) {
		ex.Backend, ex.BackendModel = "local", "qwen/qwen3.8-27b"
		ex.Attempt = &proxy.Attempt{Backend: "upstream", Why: "upstream answered HTTP 429"}
		ex.Summary = &anthropic.Summary{Model: "qwen/qwen3.8-27b", StopReason: "end_turn", Complete: true, Usage: anthropic.Usage{Input: 22, Output: 37}}
	})
	var plain, colored bytes.Buffer
	p := testConsole(&plain)
	c := testConsole(&colored)
	c.color = true
	for _, ex := range []proxy.Exchange{failed, cached, local} {
		p.Request(ex, ledger.FromExchange(ex), exchangeNotes(ex))
		c.Request(ex, ledger.FromExchange(ex), exchangeNotes(ex))
	}

	if strings.Contains(plain.String(), "\x1b[") {
		t.Errorf("plain output has color codes: %q", plain.String())
	}
	for _, want := range []string{"\x1b[31m✗ 429\x1b[0m", "\x1b[1mmain", "\x1b[32m98% cached\x1b[0m", "\x1b[35mqwen3.8-27b (local)", "\x1b[33m↳ Claude could not answer"} {
		if !strings.Contains(colored.String(), want) {
			t.Errorf("colored output lacks %q:\n%q", want, colored.String())
		}
	}
	// Taking the codes out leaves the plain lines: color never moves a column.
	if strip(colored.String()) != plain.String() {
		t.Errorf("colored, without codes:\n%s\nplain:\n%s", strip(colored.String()), plain.String())
	}
}

// strip removes color codes.
func strip(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b {
			for i < len(s) && s[i] != 'm' {
				i++
			}
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func TestCacheShareIsColoredOnlyWhenItMatters(t *testing.T) {
	paint := func(s style, text string) string { return "<" + string(s) + ">" + text }
	cases := map[string]struct {
		u    ledger.Usage
		want string
	}{
		"large and cached":    {ledger.Usage{Input: 2, CacheRead: n(148855), CacheWrite: n(1912)}, "<32>98% cached"},
		"large and rewritten": {ledger.Usage{Input: 2, CacheRead: n(1000), CacheWrite: n(150000)}, "<33>0% cached"},
		"small":               {ledger.Usage{Input: 2, CacheRead: n(10), CacheWrite: n(900)}, "<2>1% cached"},
	}
	for name, c := range cases {
		if got := tokens(c.u, paint); !strings.Contains(got, c.want) {
			t.Errorf("%s: %q, want %q", name, got, c.want)
		}
	}
}

func TestAnExpensiveTurnStandsOut(t *testing.T) {
	paint := func(s style, text string) string { return "<" + string(s) + ">" + text }
	rec := ledger.Record{Backend: "upstream", Response: &ledger.Response{Model: "claude-opus-5-5"},
		Usage: &ledger.Usage{Input: 2, CacheWrite: n(150000), CacheWrite1h: n(150000), CacheWrite5m: n(0), Output: 500}}
	if got := cost(rec, ledger.DefaultPrices, paint); got != "  <33>$1.21" {
		t.Errorf("cost = %q", got)
	}
}

func TestClaudeName(t *testing.T) {
	for in, want := range map[string]string{
		"claude-opus-5-5":           "Opus 5.5",
		"claude-sonnet-5":           "Sonnet 5",
		"claude-haiku-4-5-20251001": "Haiku 4.5",
		"claude-fable-5-1":          "Fable 5.1",
		"claude-future":             "claude-future",
		"gpt-x":                     "gpt-x",
		"":                          "",
	} {
		if got := claudeName(in); got != want {
			t.Errorf("claudeName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHumanNumbers(t *testing.T) {
	for in, want := range map[int]string{0: "0", 950: "950", 1912: "1.9k", 81340: "81k", 148855: "149k", 1_234_567: "1.2M"} {
		if got := humanCount(in); got != want {
			t.Errorf("humanCount(%d) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[time.Duration]string{967 * time.Millisecond: "967ms", 20554 * time.Millisecond: "20.6s", 125 * time.Second: "2m05s"} {
		if got := humanDuration(in); got != want {
			t.Errorf("humanDuration(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestWantColor(t *testing.T) {
	env := func(kv ...string) func(string) string {
		m := map[string]string{}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return func(k string) string { return m[k] }
	}
	// A file is not a terminal.
	f, err := os.CreateTemp(t.TempDir(), "out")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if wantColor(f, env()) {
		t.Error("color into a file")
	}
	if !wantColor(f, env("FORCE_COLOR", "1")) {
		t.Error("FORCE_COLOR was ignored")
	}
	if wantColor(f, env("FORCE_COLOR", "1", "NO_COLOR", "1")) {
		t.Error("NO_COLOR did not win")
	}
}

// A secret in the history is replaced on every turn; the console says so
// the first time in a conversation, then only when something changes.
func TestGuardNotesAreSaidOncePerConversation(t *testing.T) {
	var out bytes.Buffer
	c := testConsole(&out)
	dir := t.TempDir()
	led := ledger.Open(filepath.Join(dir, "ledger"))
	why := []string{`Bash read /app/conf/db.yaml, which "conf/**" makes local-only`}
	turn := func(session string, edit func(*proxy.Guard)) string {
		out.Reset()
		ex := exchangeCarrying("x")
		ex.Header.Set("X-Claude-Code-Session-Id", session)
		g := &proxy.Guard{Scanned: true, Report: guard.Report{Findings: 1, Kinds: map[string]int{"aws_access_key": 1}}, Replaced: 1,
			LocalOnly: why, Withheld: 1}
		if edit != nil {
			edit(g)
		}
		ex.Guard = g
		record(ex, led, nil, nil, c)
		return out.String()
	}
	replaced := "↳ guard: 1 kept from going upstream (aws_access_key)\n"
	withheld := "↳ local-only: 1 withheld from upstream"

	first := turn("s1", nil)
	if !strings.Contains(first, replaced) || !strings.Contains(first, withheld) || strings.Contains(first, "put back") {
		t.Errorf("first turn:\n%s", first)
	}
	if again := turn("s1", nil); strings.Contains(again, "↳") {
		t.Errorf("the same notes were repeated:\n%s", again)
	}
	// Something new in the same conversation is said.
	more := turn("s1", func(g *proxy.Guard) {
		g.Report.Findings, g.Report.Kinds, g.Replaced = 2, map[string]int{"aws_access_key": 1, "github_token": 1}, 2
	})
	if !strings.Contains(more, "↳ guard: 2 kept from going upstream") || strings.Contains(more, withheld) {
		t.Errorf("a new secret:\n%s", more)
	}
	// Putting a secret back in an answer is news every time.
	for range 2 {
		if back := turn("s1", func(g *proxy.Guard) { g.Restored = 1 }); !strings.Contains(back, "1 put back in the answer") {
			t.Errorf("a secret put back was not noted:\n%s", back)
		}
	}
	// Another conversation is told for itself.
	if other := turn("s2", nil); !strings.Contains(other, replaced) || !strings.Contains(other, withheld) {
		t.Errorf("another session:\n%s", other)
	}
	// A conversation idle past forgetAfter is told again.
	start := c.now()
	c.now = func() time.Time { return start.Add(forgetAfter + time.Minute) }
	if back := turn("s1", nil); !strings.Contains(back, replaced) {
		t.Errorf("after an idle hour:\n%s", back)
	}
	if len(c.said) != 1 {
		t.Errorf("idle conversations were kept: %d", len(c.said))
	}
}

// A secret that went upstream, with redaction off, is said every time:
// it was sent.
func TestDetectOnlyFindingsAreAlwaysSaid(t *testing.T) {
	var out bytes.Buffer
	c := testConsole(&out)
	led := ledger.Open(filepath.Join(t.TempDir(), "ledger"))
	for i := range 2 {
		out.Reset()
		record(exchangeCarrying(fakeAWSKey), led, nil, &guard.Detector{}, c)
		if !strings.Contains(out.String(), "↳ guard: 1 found going upstream") {
			t.Errorf("turn %d:\n%s", i+1, out.String())
		}
	}
}

// In auto mode, Claude Code asks Claude whether each tool call is safe,
// and those checks never go to the local model (#162). At a session limit
// with a stand-in carrying the conversation, every tool call is then
// refused; the console says what to do, once per conversation, and only
// when a stand-in is configured (#173).
func TestASafetyCheckThatCannotReachClaudeSaysToLeaveAutoMode(t *testing.T) {
	var out bytes.Buffer
	led := ledger.Open(filepath.Join(t.TempDir(), "ledger"))
	check := `{"model":"claude-sonnet-5","max_tokens":64,"system":[{"type":"text","text":"You are a security monitor for autonomous AI coding agents."}],"messages":[{"role":"user","content":"Respond with <severity>N</severity> ONLY."}]}`
	turn := func(c *Console, status int) string {
		out.Reset()
		ex := consoleExchange(func(ex *proxy.Exchange) {
			ex.Status, ex.Duration = status, 300*time.Millisecond
			ex.RequestBody = []byte(check)
			ex.Summary = &anthropic.Summary{Complete: true, Error: &anthropic.APIError{Type: "rate_limit_error"}}
		})
		record(ex, led, nil, nil, c)
		return out.String()
	}
	hint := "↳ auto mode's safety check could not reach Claude, so Claude Code will refuse tool calls; press Shift+Tab to leave auto mode"

	c := testConsole(&out)
	c.standIns = true
	if first := turn(c, 429); !strings.Contains(first, "safety") || !strings.Contains(first, hint) {
		t.Errorf("first refused check:\n%s", first)
	}
	if again := turn(c, 429); strings.Contains(again, "↳") {
		t.Errorf("the hint was repeated:\n%s", again)
	}

	// Without a stand-in the whole session is stopped, and there is
	// nothing to say about auto mode in particular.
	plain := testConsole(&out)
	if got := turn(plain, 429); strings.Contains(got, "auto mode") {
		t.Errorf("hint with no stand-in configured:\n%s", got)
	}
	// Nor when Claude answered.
	fresh := testConsole(&out)
	fresh.standIns = true
	if got := turn(fresh, 200); strings.Contains(got, "auto mode") {
		t.Errorf("hint when Claude answered:\n%s", got)
	}
}
