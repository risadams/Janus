package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/risadams/janus/internal/anthropic"
	"github.com/risadams/janus/internal/config"
	"github.com/risadams/janus/internal/proxy"
)

func TestVersionLine(t *testing.T) {
	if got, want := versionLine(), "janus dev"; got != want {
		t.Errorf("versionLine() = %q, want %q", got, want)
	}
}

func TestRunVersion(t *testing.T) {
	for _, arg := range []string{"version", "-version", "--version"} {
		var out, errOut bytes.Buffer
		if code := run([]string{arg}, &out, &errOut); code != 0 || out.String() != "janus dev\n" {
			t.Errorf("run(%q) = %d, stdout %q, stderr %q", arg, code, out.String(), errOut.String())
		}
	}
}

func TestRunRejectsUnknownCommand(t *testing.T) {
	var out, errOut bytes.Buffer
	cfgPath := filepath.Join(t.TempDir(), "c.json")
	if code := run([]string{"-config", cfgPath, "bogus"}, &out, &errOut); code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	if !strings.Contains(errOut.String(), `unknown command "bogus"`) {
		t.Errorf("stderr = %q", errOut.String())
	}
	if _, err := os.Stat(cfgPath); err == nil {
		t.Error("a starter config was written for a command that was rejected")
	}
}

func TestRunReportsInvalidConfig(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "c.json")
	os.WriteFile(cfgPath, []byte(`{"listen":"0.0.0.0:8787","features":{"route":true}}`), 0o600)

	var out, errOut bytes.Buffer
	if code := run([]string{"-config", cfgPath}, &out, &errOut); code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	for _, want := range []string{cfgPath, "allowRemote", "features.route"} {
		if !strings.Contains(errOut.String(), want) {
			t.Errorf("stderr = %q, missing %q", errOut.String(), want)
		}
	}
}

func TestBannerGivesTheClaudeCodeCommand(t *testing.T) {
	var out bytes.Buffer
	printBanner(&out, config.Default(), "127.0.0.1:8787")
	for _, want := range []string{
		"ANTHROPIC_BASE_URL=http://127.0.0.1:8787 ENABLE_TOOL_SEARCH=true claude",
		`$env:ANTHROPIC_BASE_URL="http://127.0.0.1:8787"`,
		"https://api.anthropic.com",
		"http://studio.tuesday.local:1234",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("banner is missing %q:\n%s", want, out.String())
		}
	}
}

func TestFormatExchange(t *testing.T) {
	n := func(i int) *int { return &i }
	base := proxy.Exchange{Method: "POST", Path: "/v1/messages", Status: 200, Duration: 2065 * time.Millisecond}
	with := func(edit func(*proxy.Exchange)) proxy.Exchange { ex := base; edit(&ex); return ex }

	cases := map[string]struct {
		ex   proxy.Exchange
		want string
	}{
		"not a Messages response": {
			proxy.Exchange{Method: "HEAD", Path: "/api/hello", Status: 200, Duration: 154 * time.Millisecond},
			"HEAD /api/hello -> 200 (154ms)",
		},
		"upstream usage": {
			with(func(ex *proxy.Exchange) {
				ex.Summary = &anthropic.Summary{Model: "claude-opus-5-5", StopReason: "tool_use", Complete: true,
					Usage: anthropic.Usage{Input: 2, Output: 364, CacheWrite: n(876), CacheRead: n(28730)}}
			}),
			"POST /v1/messages -> 200 (2.065s) claude-opus-5-5 tool_use in=2 cache_write=876 cache_read=28730 out=364",
		},
		"usage fields the server did not send are left out": {
			with(func(ex *proxy.Exchange) {
				ex.Summary = &anthropic.Summary{Model: "qwen/qwen3.8-27b", StopReason: "end_turn", Complete: true,
					Usage: anthropic.Usage{Input: 17, Output: 3}}
			}),
			"POST /v1/messages -> 200 (2.065s) qwen/qwen3.8-27b end_turn in=17 out=3",
		},
		"cut off": {
			with(func(ex *proxy.Exchange) {
				ex.Summary = &anthropic.Summary{Model: "claude-opus-5-5", Usage: anthropic.Usage{Input: 2, Output: 4}}
			}),
			"POST /v1/messages -> 200 (2.065s) claude-opus-5-5 in=2 out=4 (incomplete)",
		},
		"api error": {
			with(func(ex *proxy.Exchange) {
				ex.Status = 429
				ex.Summary = &anthropic.Summary{Complete: true, Error: &anthropic.APIError{Type: "rate_limit_error"}}
			}),
			"POST /v1/messages -> 429 (2.065s) error=rate_limit_error",
		},
		"unreadable encoding": {
			with(func(ex *proxy.Exchange) { ex.Unread = "content-encoding br" }),
			"POST /v1/messages -> 200 (2.065s) response not read: content-encoding br",
		},
	}
	for name, c := range cases {
		if got := formatExchange(c.ex); got != c.want {
			t.Errorf("%s:\n got %s\nwant %s", name, got, c.want)
		}
	}
}
