package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// TestFakeClaude is not a test: it is the stand-in for Claude Code that
// janus claude runs in the tests below. It reports the arguments and the
// two variables janus sets, and exits with the code it is asked to.
func TestFakeClaude(t *testing.T) {
	if os.Getenv("JANUS_FAKE_CLAUDE") == "" {
		t.Skip("run by the janus claude tests")
	}
	args := os.Args[slices.Index(os.Args, "--")+1:]
	fmt.Printf("args=%q base=%q toolsearch=%q\n", args, os.Getenv("ANTHROPIC_BASE_URL"), os.Getenv("ENABLE_TOOL_SEARCH"))
	fmt.Printf("headers=%q\n", os.Getenv("ANTHROPIC_CUSTOM_HEADERS"))
	code, _ := strconv.Atoi(os.Getenv("JANUS_FAKE_EXIT"))
	os.Exit(code)
}

// launch runs janus claude with the fake Claude Code, a config listening
// on listen, and the guard on or off.
func launch(t *testing.T, listen string, guard bool, args ...string) (out string, code int, ran bool) {
	t.Helper()
	return launchAs(t, "claude", listen, guard, args...)
}

// launchAs is launch for the launcher command named, "claude" or "local".
func launchAs(t *testing.T, command, listen string, guard bool, args ...string) (out string, code int, ran bool) {
	t.Helper()
	saved := claudeProgram
	claudeProgram = []string{os.Args[0], "-test.run=^TestFakeClaude$", "--"}
	t.Cleanup(func() { claudeProgram = saved })
	t.Setenv("JANUS_FAKE_CLAUDE", "1")

	dir := t.TempDir()
	cfg := map[string]any{"listen": listen, "logging": map[string]any{"dir": filepath.Join(dir, "logs")},
		"features": map[string]any{"guard": guard}}
	data, _ := json.Marshal(cfg)
	path := filepath.Join(dir, "janus.config.json")
	os.WriteFile(path, data, 0o600)

	var stdout, stderr bytes.Buffer
	code = run(append([]string{command, "-config", path}, args...), &stdout, &stderr)
	return stdout.String() + stderr.String(), code, strings.Contains(stdout.String(), "args=")
}

// listening returns an address something accepts connections on.
func listening(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	return ln.Addr().String()
}

// refused returns a loopback address nothing is listening on.
func refused() string { return "127.0.0.1:1" }

func TestJanusClaudeGoesThroughJanusWhenItIsUp(t *testing.T) {
	addr := listening(t)
	t.Setenv("JANUS_FAKE_EXIT", "3")
	out, code, ran := launch(t, addr, true, "--resume", "abc")
	if !ran || code != 3 {
		t.Fatalf("exit %d, ran %v; want Claude Code run and its exit code 3 passed on:\n%s", code, ran, out)
	}
	want := fmt.Sprintf(`args=["--resume" "abc"] base="http://%s" toolsearch="true"`, addr)
	if !strings.Contains(out, want) {
		t.Errorf("got %s\nwant %s", out, want)
	}
}

func TestJanusClaudeGoesDirectWhenJanusIsDown(t *testing.T) {
	// A variable naming the stopped janus would leave Claude Code with
	// nowhere to go, so it is removed.
	t.Setenv("ANTHROPIC_BASE_URL", "http://"+refused())
	t.Setenv("ENABLE_TOOL_SEARCH", "")
	out, code, ran := launch(t, refused(), false, "-p", "hi")
	if !ran || code != 0 {
		t.Fatalf("exit %d, ran %v:\n%s", code, ran, out)
	}
	for _, want := range []string{`args=["-p" "hi"] base="" toolsearch=""`, "going straight\nto Anthropic"} {
		if !strings.Contains(out, want) {
			t.Errorf("output is missing %q:\n%s", want, out)
		}
	}
}

func TestJanusClaudeRefusesWhenJanusIsDownAndTheGuardIsOn(t *testing.T) {
	out, code, ran := launch(t, refused(), true)
	if ran || code != 1 || !strings.Contains(out, "the guard is on") {
		t.Errorf("exit %d, ran %v; want a refusal naming the guard:\n%s", code, ran, out)
	}
}

func TestJanusClaudeRequireRefusesWhenJanusIsDown(t *testing.T) {
	out, code, ran := launch(t, refused(), false, "-require")
	if ran || code != 1 || !strings.Contains(out, "-require was given") {
		t.Errorf("exit %d, ran %v:\n%s", code, ran, out)
	}
	if _, code, ran := launch(t, listening(t), false, "-require"); !ran || code != 0 {
		t.Errorf("with janus up, -require: exit %d, ran %v", code, ran)
	}
}

func TestSplitClaudeArgs(t *testing.T) {
	cases := []struct {
		args, ours, theirs []string
	}{
		{nil, nil, nil},
		{[]string{"-config", "j.json", "--resume"}, []string{"-config", "j.json"}, []string{"--resume"}},
		{[]string{"--config=j.json", "-require", "-p", "hi"}, []string{"--config=j.json", "-require"}, []string{"-p", "hi"}},
		{[]string{"-require", "--", "-require"}, []string{"-require"}, []string{"-require"}},
		{[]string{"chat", "-require"}, []string{}, []string{"chat", "-require"}},
		{[]string{"-h"}, []string{"-h"}, nil},
	}
	for _, c := range cases {
		ours, theirs := splitClaudeArgs(c.args)
		if !slices.Equal(ours, c.ours) || !slices.Equal(theirs, c.theirs) {
			t.Errorf("splitClaudeArgs(%q) = %q, %q; want %q, %q", c.args, ours, theirs, c.ours, c.theirs)
		}
	}
}

func TestDialAddrReachesEveryInterfaceOnLoopback(t *testing.T) {
	for listen, want := range map[string]string{
		"127.0.0.1:8787": "127.0.0.1:8787",
		":8787":          "127.0.0.1:8787",
		"0.0.0.0:8787":   "127.0.0.1:8787",
		"[::]:8787":      "127.0.0.1:8787",
		"[::1]:8787":     "[::1]:8787",
		"mac.local:8787": "mac.local:8787",
	} {
		if got := dialAddr(listen); got != want {
			t.Errorf("dialAddr(%q) = %q, want %q", listen, got, want)
		}
	}
}

// A session's choice of provider reaches Janus in the route header, added
// to any headers already set, and Claude Code's own arguments pass on.
func TestTheLaunchersSayWhichProviderTheSessionWants(t *testing.T) {
	addr := listening(t)
	t.Setenv("ANTHROPIC_CUSTOM_HEADERS", "X-Team: core")
	for _, c := range []struct {
		command string
		args    []string
		want    string
	}{
		{"claude", []string{"-provider", "codex", "--resume"}, `headers="X-Team: core\nX-Janus-Route: openai"`},
		{"claude", []string{"-provider=claude"}, `headers="X-Team: core\nX-Janus-Route: upstream"`},
		{"local", []string{"--resume"}, `headers="X-Team: core\nX-Janus-Route: local"`},
		{"claude", []string{"--resume"}, `headers="X-Team: core"`},
	} {
		out, code, ran := launchAs(t, c.command, addr, false, c.args...)
		if !ran || code != 0 || !strings.Contains(out, c.want) || !strings.Contains(out, `"--resume"`) && slices.Contains(c.args, "--resume") {
			t.Errorf("janus %s %q: exit %d:\n%s\nwant %s", c.command, c.args, code, out, c.want)
		}
	}
	if out, code, ran := launch(t, addr, false, "-provider", "gemini"); ran || code != 2 || !strings.Contains(out, "is not claude, openai, codex, or local") {
		t.Errorf("an unknown provider: exit %d, ran %v:\n%s", code, ran, out)
	}
	// Only janus can send a session to a provider, so with it down the
	// session does not start.
	if out, code, ran := launchAs(t, "local", refused(), false); ran || code != 1 || !strings.Contains(out, "only janus can send this session") {
		t.Errorf("janus down: exit %d, ran %v:\n%s", code, ran, out)
	}
}
