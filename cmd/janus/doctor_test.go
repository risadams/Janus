package main

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/risadams/janus/internal/instance"
	"github.com/risadams/janus/internal/ledger"
	"github.com/risadams/janus/internal/route"
)

const lmStudioModels = `{"data":[
	{"id":"qwen/qwen3.8-27b","state":"loaded","loaded_context_length":61696},
	{"id":"gemma-4-e4b","state":"not-loaded"}]}`

func serverSaying(status int, body string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		io.WriteString(w, body)
	}))
}

// freeAddr is a loopback address nothing is listening on.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

// doctor runs the command against a config in a fresh directory, with
// upstream and LM Studio pointed at the given URLs.
func doctor(t *testing.T, upstream, local string, extra map[string]any) (out string, code int, dir string) {
	t.Helper()
	dir = t.TempDir()
	out, code = doctorIn(t, dir, upstream, local, extra)
	return out, code, dir
}

// doctorIn is doctor with the config, and the log folder it names, in dir.
func doctorIn(t *testing.T, dir, upstream, local string, extra map[string]any) (out string, code int) {
	t.Helper()
	cfg := map[string]any{
		"listen":   freeAddr(t),
		"upstream": map[string]any{"baseUrl": upstream},
		"local":    map[string]any{"baseUrl": local},
		"logging":  map[string]any{"dir": filepath.Join(dir, "logs")},
	}
	for k, v := range extra {
		cfg[k] = v
	}
	data, _ := json.Marshal(cfg)
	path := filepath.Join(dir, "janus.config.json")
	os.WriteFile(path, data, 0o600)
	// Keep the real Claude Code settings out of it, and point the
	// environment at janus as a working setup would.
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	if listen, ok := cfg["listen"].(string); ok {
		t.Setenv("ANTHROPIC_BASE_URL", "http://"+listen)
	}

	var stdout, stderr bytes.Buffer
	code = run([]string{"doctor", "-config", path}, &stdout, &stderr)
	return stdout.String() + stderr.String(), code
}

func TestDiagnoseUpstreamNamesTheFault(t *testing.T) {
	u, _ := url.Parse("https://api.anthropic.com")
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"no such host", &net.DNSError{Err: "no such host", Name: "api.anthropic.com", IsNotFound: true}, "no such host"},
		{"temporary dns", &net.DNSError{Err: "server misbehaving", Name: "x", IsTemporary: true}, "temporary"},
		{"not trusted", x509.UnknownAuthorityError{}, "not trusted"},
		{"bad cert", &x509.CertificateInvalidError{Reason: x509.Expired}, "certificate"},
		{"timeout", context.DeadlineExceeded, "did not answer in time"},
		{"refused", &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}, "nothing is listening"},
		{"plain", errors.New("some socket weirdness"), "some socket weirdness"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := diagnoseUpstream(u, c.err); !strings.Contains(got, c.want) {
				t.Errorf("diagnoseUpstream = %q, want it to contain %q", got, c.want)
			}
		})
	}
	if got := diagnoseUpstream(nil, errors.New("unsupported protocol scheme")); !strings.Contains(got, "unsupported protocol scheme") {
		t.Errorf("a nil URL should fall back to the raw error, got %q", got)
	}
}

func wantLines(t *testing.T, out string, wants ...string) {
	t.Helper()
	squeezed := strings.Join(strings.Fields(out), " ")
	for _, want := range wants {
		if !strings.Contains(squeezed, want) {
			t.Errorf("output is missing %q", want)
		}
	}
	if t.Failed() {
		t.Logf("output was:\n%s", out)
	}
}

func TestDoctorAllGood(t *testing.T) {
	up := serverSaying(200, "Anthropic API")
	defer up.Close()
	lms := serverSaying(200, lmStudioModels)
	defer lms.Close()

	out, code, dir := doctor(t, up.URL, lms.URL, nil)
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	wantLines(t, out,
		"ok config",
		"ok listen 127.0.0.1:",
		"is free",
		"ok upstream "+up.URL+" answered in",
		"ok local "+lms.URL+", loaded: qwen/qwen3.8-27b (context 61,696)",
		"ok logs "+filepath.Join(dir, "logs")+" will be created when janus runs",
		"ANTHROPIC_BASE_URL=http://127.0.0.1:",
	)
	if strings.Contains(out, "warn") || strings.Contains(out, "fail") {
		t.Errorf("a clean setup produced a warning or failure:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "logs")); err == nil {
		t.Error("doctor created the logs directory; it should only look")
	}
}

// An upstream that answers with an error status is still reachable: doctor
// sends no credentials, so a 401 or 404 says nothing bad.
func TestDoctorTreatsAnyUpstreamAnswerAsReachable(t *testing.T) {
	up := serverSaying(401, `{"type":"error"}`)
	defer up.Close()
	lms := serverSaying(200, lmStudioModels)
	defer lms.Close()

	out, code, _ := doctor(t, up.URL, lms.URL, nil)
	if code != 0 {
		t.Errorf("exit code = %d, want 0\n%s", code, out)
	}
	wantLines(t, out, "ok upstream")
}

func TestDoctorUpstreamDownFails(t *testing.T) {
	lms := serverSaying(200, lmStudioModels)
	defer lms.Close()

	out, code, _ := doctor(t, "http://"+freeAddr(t), lms.URL, nil)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	wantLines(t, out, "fail upstream", "did not answer", "Something above needs fixing")
	if strings.Contains(out, "ENABLE_TOOL_SEARCH=true claude") {
		t.Error("the Claude Code command was printed for a setup that will not work")
	}
}

func TestDoctorLocalProblemsAreWarnings(t *testing.T) {
	up := serverSaying(200, "ok")
	defer up.Close()
	noneLoaded := serverSaying(200, `{"data":[{"id":"a","state":"not-loaded"},{"id":"b","state":"not-loaded"}]}`)
	defer noneLoaded.Close()
	// What LM Studio really says for a path it does not know: HTTP 200.
	notLMStudio := serverSaying(200, `{"error":"Unexpected endpoint or method. (GET /api/v0/models)"}`)
	defer notLMStudio.Close()

	cases := map[string]struct {
		local string
		wants []string
	}{
		"not running":     {"http://" + freeAddr(t), []string{"warn local", "did not answer", "serve on the local network"}},
		"no model loaded": {noneLoaded.URL, []string{"warn local", "has 2 models and none loaded"}},
		"something else":  {notLMStudio.URL, []string{"warn local", "the server answered but not like LM Studio (HTTP 200)"}},
	}
	for name, c := range cases {
		out, code, _ := doctor(t, up.URL, c.local, nil)
		if code != 0 {
			t.Errorf("%s: exit code = %d, want 0: janus runs without the local model", name, code)
		}
		for _, want := range c.wants {
			if !strings.Contains(strings.Join(strings.Fields(out), " "), want) {
				t.Errorf("%s: output is missing %q\n%s", name, want, out)
			}
		}
		if strings.Contains(out, "lms server") {
			t.Errorf("%s: doctor suggests the lms CLI, which stops the Bionic app's server", name)
		}
	}
}

func TestDoctorPortInUseByAnotherConfigIsAWarning(t *testing.T) {
	up := serverSaying(200, "ok")
	defer up.Close()
	lms := serverSaying(200, lmStudioModels)
	defer lms.Close()
	busy, _ := net.Listen("tcp", "127.0.0.1:0")
	defer busy.Close()

	out, code, dir := doctor(t, up.URL, lms.URL, map[string]any{"listen": busy.Addr().String()})
	if code != 0 {
		t.Errorf("exit code = %d, want 0: the port is in use whenever janus is running", code)
	}
	wantLines(t, out, "warn listen", "is in use, but not by a janus using this config",
		filepath.Join(dir, "logs"), "run janus doctor -config with the config it was started with")
}

func TestDoctorPortInUseByThisConfigsJanusIsFine(t *testing.T) {
	up := serverSaying(200, "ok")
	defer up.Close()
	lms := serverSaying(200, lmStudioModels)
	defer lms.Close()
	busy, _ := net.Listen("tcp", "127.0.0.1:0")
	defer busy.Close()
	// The janus that would have the port holds the lock on the log folder
	// the config names; doctor's helper puts that under its own directory.
	dir := t.TempDir()
	lock, err := instance.Acquire(filepath.Join(dir, "logs"))
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	lock.Describe("pid 4242, listening on http://" + busy.Addr().String())

	out, code := doctorIn(t, dir, up.URL, lms.URL, map[string]any{"listen": busy.Addr().String()})
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	wantLines(t, out, "ok listen", "is in use by the janus using this config (pid 4242")
	if strings.Contains(out, "warn listen") {
		t.Errorf("doctor warned about the port its own janus has:\n%s", out)
	}
}

// Doctor run where a stale config sits, while the janus on the port runs
// with another, read a ledger nobody writes and said Claude Code was not
// going through janus (#365).
func TestDoctorDoesNotBlameClaudeCodeForAnotherConfigsLedger(t *testing.T) {
	up := serverSaying(200, "ok")
	defer up.Close()
	lms := serverSaying(200, lmStudioModels)
	defer lms.Close()
	busy, _ := net.Listen("tcp", "127.0.0.1:0")
	defer busy.Close()
	dir := t.TempDir()
	ledger.Open(filepath.Join(dir, "logs", "ledger")).Append(ledger.Record{V: 1, ID: "a", Time: time.Now().Add(-5 * 24 * time.Hour), Class: "main"})
	t.Setenv("ANTHROPIC_BASE_URL", "")

	out, _ := doctorIn(t, dir, up.URL, lms.URL, map[string]any{"listen": busy.Addr().String()})
	wantLines(t, out, "warn listen", "ok ledger", "5 days ago", "but the janus running now does not write there", "ok claude")
	for _, wrong := range []string{"not going through janus", "goes straight to Anthropic"} {
		if strings.Contains(strings.Join(strings.Fields(out), " "), wrong) {
			t.Errorf("doctor says %q of a ledger the running janus does not write:\n%s", wrong, out)
		}
	}
}

func TestDoctorNamesTheConfigByItsFullPath(t *testing.T) {
	up := serverSaying(200, "ok")
	defer up.Close()
	lms := serverSaying(200, lmStudioModels)
	defer lms.Close()
	dir := t.TempDir()
	t.Chdir(dir)
	data, _ := json.Marshal(map[string]any{
		"listen":   freeAddr(t),
		"upstream": map[string]any{"baseUrl": up.URL},
		"local":    map[string]any{"baseUrl": lms.URL},
	})
	os.WriteFile("janus.config.json", data, 0o600)
	t.Setenv("CLAUDE_CONFIG_DIR", dir)

	var stdout, stderr bytes.Buffer
	run([]string{"doctor"}, &stdout, &stderr)
	want, _ := filepath.Abs("janus.config.json")
	wantLines(t, stdout.String(), "ok config "+want)
}

func TestDoctorInvalidConfigStopsThere(t *testing.T) {
	out, code, _ := doctor(t, "not a url", "http://127.0.0.1:1", map[string]any{"features": map[string]any{"route": true}})
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	wantLines(t, out, "fail config", "upstream.baseUrl", "features.route is on but local.models is empty")
	for _, later := range []string{"listen", "upstream ", "local ", "logs "} {
		if strings.Contains(out, "ok    "+later) || strings.Contains(out, "warn  "+later) {
			t.Errorf("checks ran against a config that did not validate:\n%s", out)
		}
	}
}

func TestDoctorUnwritableLogs(t *testing.T) {
	up := serverSaying(200, "ok")
	defer up.Close()
	lms := serverSaying(200, lmStudioModels)
	defer lms.Close()
	blocker := filepath.Join(t.TempDir(), "file")
	os.WriteFile(blocker, nil, 0o600)

	out, code, _ := doctor(t, up.URL, lms.URL, map[string]any{"logging": map[string]any{"dir": filepath.Join(blocker, "logs")}})
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	wantLines(t, out, "fail logs", "is a file")
}

func TestDoctorWithoutAConfigFileUsesDefaultsAndWritesNothing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "janus.config.json")
	up := serverSaying(200, "ok")
	defer up.Close()
	lms := serverSaying(200, lmStudioModels)
	defer lms.Close()

	var stdout, stderr bytes.Buffer
	code := run([]string{"doctor", "-config", path, "-listen", freeAddr(t), "-upstream", up.URL, "-local", lms.URL}, &stdout, &stderr)
	out := stdout.String()

	if code != 0 {
		t.Errorf("exit code = %d\n%s%s", code, out, stderr.String())
	}
	wantLines(t, out, "does not exist yet; using the defaults", "ok upstream "+up.URL, "ok local "+lms.URL)
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("doctor wrote files: %v", entries)
	}
}

func TestDoctorBadArguments(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"doctor", "extra"}, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), "unexpected argument") {
		t.Errorf("exit %d, stderr %q", code, stderr.String())
	}
}

func TestDoctorChecksRoutedModels(t *testing.T) {
	up := serverSaying(200, "ok")
	defer up.Close()
	lms := serverSaying(200, lmStudioModels) // qwen loaded with 61,696; gemma-4-e4b not loaded
	defer lms.Close()

	routed := func(models map[string]any) map[string]any {
		return map[string]any{"features": map[string]any{"route": true}, "local": map[string]any{"baseUrl": lms.URL, "models": models}}
	}
	cases := map[string]struct {
		models   map[string]any
		wantCode int
		wants    []string
	}{
		"loaded": {map[string]any{"janus-local": map[string]any{"model": "qwen/qwen3.8-27b"}}, 0,
			[]string{`ok route "janus-local" goes to qwen/qwen3.8-27b, loaded with a context of 61,696 tokens`}},
		"not loaded": {map[string]any{"janus-small": map[string]any{"model": "gemma-4-e4b"}}, 0,
			[]string{`warn route "janus-small" goes to gemma-4-e4b, which is not loaded`}},
		"not in LM Studio": {map[string]any{"janus-local": map[string]any{"model": "no-such-model"}}, 1,
			[]string{`fail route "janus-local" goes to no-such-model, which LM Studio does not have`, "qwen/qwen3.8-27b, gemma-4-e4b"}},
		"limit above what is loaded": {map[string]any{"janus-local": map[string]any{"model": "qwen/qwen3.8-27b", "contextTokens": 200000}}, 0,
			[]string{`warn route`, "contextTokens is 200,000 but the model is loaded with 61,696"}},
	}
	for name, c := range cases {
		out, code, _ := doctor(t, up.URL, lms.URL, routed(c.models))
		if code != c.wantCode {
			t.Errorf("%s: exit code = %d, want %d\n%s", name, code, c.wantCode, out)
		}
		squeezed := strings.Join(strings.Fields(out), " ")
		for _, want := range c.wants {
			if !strings.Contains(squeezed, want) {
				t.Errorf("%s: output is missing %q\n%s", name, want, out)
			}
		}
	}

	// With compression on, the digest model is checked whether or not
	// routing is.
	for name, c := range map[string]struct {
		model    string
		wantCode int
		want     string
	}{
		"loaded":           {"qwen/qwen3.8-27b", 0, `ok compress digests are written by "janus-local", qwen/qwen3.8-27b, loaded`},
		"not loaded":       {"gemma-4-e4b", 0, `warn compress digests are written by "janus-local", gemma-4-e4b, which is not loaded`},
		"not in LM Studio": {"no-such-model", 1, `fail compress digests are written by "janus-local", no-such-model, which LM Studio does not have`},
	} {
		out, code, _ := doctor(t, up.URL, lms.URL, map[string]any{"features": map[string]any{"compress": true},
			"local": map[string]any{"baseUrl": lms.URL, "models": map[string]any{"janus-local": map[string]any{"model": c.model}}}})
		if code != c.wantCode || !strings.Contains(strings.Join(strings.Fields(out), " "), c.want) {
			t.Errorf("compress, %s: exit %d (want %d), output missing %q\n%s", name, code, c.wantCode, c.want, out)
		}
	}

	// With routing off, configured models are not checked: they are not used.
	out, _, _ := doctor(t, up.URL, lms.URL, map[string]any{"local": map[string]any{"baseUrl": lms.URL, "models": map[string]any{"x": map[string]any{"model": "no-such-model"}}}})
	if strings.Contains(out, "route") && strings.Contains(out, "no-such-model") {
		t.Errorf("a model was checked with routing off:\n%s", out)
	}
}

func TestDoctorSaysWhenTheLedgerWentQuiet(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	if f, seen := checkLedger(dir, now, false); f.level != "ok" || seen != trafficQuiet || !strings.Contains(f.detail, "nothing recorded yet") {
		t.Errorf("empty ledger: %+v, seen %v", f, seen)
	}
	if f, seen := checkLedger(dir, now, true); f.level != "ok" || seen != trafficUnknown || !strings.Contains(f.detail, "does not write") {
		t.Errorf("empty ledger of another config: %+v, seen %v", f, seen)
	}

	ledger.Open(dir).Append(ledger.Record{V: 1, ID: "a", Time: now.Add(-3 * time.Hour), Class: "main"})
	if f, seen := checkLedger(dir, now, false); f.level != "ok" || seen != trafficRecent || !strings.Contains(f.detail, "3 hours ago") {
		t.Errorf("recent ledger: %+v, seen %v", f, seen)
	}
	if f, seen := checkLedger(dir, now.Add(3*24*time.Hour), false); f.level != "warn" || seen != trafficQuiet ||
		!strings.Contains(f.detail, "3 days ago") || !strings.Contains(f.detail, "not going through janus") {
		t.Errorf("quiet ledger: %+v, seen %v", f, seen)
	}
	if f, seen := checkLedger(dir, now.Add(3*24*time.Hour), true); f.level != "ok" || seen != trafficUnknown ||
		!strings.Contains(f.detail, "3 days ago") || strings.Contains(f.detail, "not going through janus") {
		t.Errorf("quiet ledger of another config: %+v, seen %v", f, seen)
	}
}

func TestDoctorSaysWhetherClaudeCodeIsPointedAtJanus(t *testing.T) {
	const listen = "127.0.0.1:8787"
	settings := func(t *testing.T, body string) string {
		path := filepath.Join(t.TempDir(), "settings.json")
		if body != "" {
			os.WriteFile(path, []byte(body), 0o600)
		}
		return path
	}
	env := func(v string) func(string) string {
		return func(k string) string {
			if k == "ANTHROPIC_BASE_URL" {
				return v
			}
			return ""
		}
	}
	cases := []struct {
		name, env, settings string
		seen                traffic
		level, says         string
	}{
		{"shell points at janus", "http://localhost:8787", "", trafficQuiet, "ok", "in this shell points at janus"},
		{"settings point at janus", "", `{"env":{"ANTHROPIC_BASE_URL":"http://127.0.0.1:8787"}}`, trafficQuiet, "ok", "settings.json points at janus"},
		{"settings point elsewhere", "http://127.0.0.1:8787", `{"env":{"ANTHROPIC_BASE_URL":"http://127.0.0.1:9999"}}`, trafficRecent, "warn", "is http://127.0.0.1:9999, which is not janus"},
		{"unset, but traffic arrives", "", `{"model":"opus"}`, trafficRecent, "ok", "some other way, such as an alias"},
		{"unset and quiet", "", "", trafficQuiet, "warn", "goes straight to Anthropic"},
		{"unset, and the ledger is another janus's", "", "", trafficUnknown, "ok", "the ledger\nthat would show it is the running janus's"},
		{"settings unreadable", "", "{", trafficQuiet, "warn", "is not valid JSON"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := checkClaude(listen, env(c.env), settings(t, c.settings), c.seen)
			if f.level != c.level || !strings.Contains(f.detail, c.says) {
				t.Errorf("got %s %q; want %s saying %q", f.level, f.detail, c.level, c.says)
			}
		})
	}
}

// doctor names the Claude Code release in the newest request that names
// one, and warns, never fails, when it is outside the tested range.
func TestDoctorWarnsOfAnUntestedClaudeCode(t *testing.T) {
	saved := route.TestedClaudeCode
	t.Cleanup(func() { route.TestedClaudeCode = saved })
	route.TestedClaudeCode.Oldest, route.TestedClaudeCode.Newest = "2.1.296", "2.1.301"
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)

	for _, c := range []struct {
		versions []string // oldest request first
		level    string
		says     string
	}{
		{nil, "", ""},
		{[]string{"2.1.301", ""}, "ok", "Claude Code 2.1.301, which janus was tested with (2.1.296 to 2.1.301)"},
		{[]string{"2.1.296"}, "ok", "2.1.296, which janus was tested with"},
		{[]string{"2.1.296", "2.1.310"}, "warn", "2.1.310 is newer than any this janus was tested with (up to 2.1.301)"},
		{[]string{"2.1.310", "2.1.299"}, "ok", "2.1.299"},
		{[]string{"2.2.0"}, "warn", "run janus update"},
		{[]string{"2.0.9"}, "warn", "older than any this janus was tested with (from 2.1.296)"},
	} {
		dir := t.TempDir()
		for i, v := range c.versions {
			ledger.Open(dir).Append(ledger.Record{V: 1, ID: strconv.Itoa(i), Time: now.Add(time.Duration(i-10) * time.Minute), Class: "main", ClaudeCode: v})
		}
		f, seen := checkClaudeVersion(dir, now)
		if c.level == "" {
			if seen {
				t.Errorf("%v: said %+v, want nothing", c.versions, f)
			}
			continue
		}
		if !seen || f.level != c.level || !strings.Contains(f.detail, c.says) {
			t.Errorf("%v: %+v (seen %v), want %s saying %q", c.versions, f, seen, c.level, c.says)
		}
	}

	// A week-old request says nothing about the Claude Code in use now.
	dir := t.TempDir()
	ledger.Open(dir).Append(ledger.Record{V: 1, ID: "old", Time: now.Add(-8 * 24 * time.Hour), Class: "main", ClaudeCode: "2.1.310"})
	if f, seen := checkClaudeVersion(dir, now); seen {
		t.Errorf("a request from 8 days ago gave %+v", f)
	}
}

// Doctor says where the OpenAI key comes from and whether it is there, and
// never prints it.
func TestDoctorSaysWhetherTheOpenAIKeyIsFoundButNotWhatItIs(t *testing.T) {
	up := serverSaying(200, "Anthropic API")
	defer up.Close()
	lms := serverSaying(200, lmStudioModels)
	defer lms.Close()
	openaiSection := map[string]any{"openai": map[string]any{"key": "env:JANUS_TEST_OPENAI_KEY"}}

	const key = "janus-test-key-0f3c9d"
	t.Setenv("JANUS_TEST_OPENAI_KEY", key)
	out, _, _ := doctor(t, up.URL, lms.URL, openaiSection)
	wantLines(t, out, "ok openai https://api.openai.com, key found in JANUS_TEST_OPENAI_KEY")
	if strings.Contains(out, key) {
		t.Errorf("doctor printed the key:\n%s", out)
	}

	os.Unsetenv("JANUS_TEST_OPENAI_KEY")
	out, code, _ := doctor(t, up.URL, lms.URL, openaiSection)
	wantLines(t, out, "warn openai", "JANUS_TEST_OPENAI_KEY is not set")
	if code != 0 {
		t.Errorf("exit code = %d, want 0: nothing uses the key yet", code)
	}
}

func TestDoctorSaysNothingOfOpenAIWithoutTheSection(t *testing.T) {
	up := serverSaying(200, "Anthropic API")
	defer up.Close()
	lms := serverSaying(200, lmStudioModels)
	defer lms.Close()
	out, _, _ := doctor(t, up.URL, lms.URL, nil)
	if strings.Contains(out, "openai") {
		t.Errorf("doctor mentions OpenAI for a config without it:\n%s", out)
	}
}
