package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

	var stdout, stderr bytes.Buffer
	code = run([]string{"doctor", "-config", path}, &stdout, &stderr)
	return stdout.String() + stderr.String(), code, dir
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
	if strings.Contains(out, "ANTHROPIC_BASE_URL") {
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

func TestDoctorPortInUseIsAWarning(t *testing.T) {
	up := serverSaying(200, "ok")
	defer up.Close()
	lms := serverSaying(200, lmStudioModels)
	defer lms.Close()
	busy, _ := net.Listen("tcp", "127.0.0.1:0")
	defer busy.Close()

	out, code, _ := doctor(t, up.URL, lms.URL, map[string]any{"listen": busy.Addr().String()})
	if code != 0 {
		t.Errorf("exit code = %d, want 0: the port is in use whenever janus is running", code)
	}
	wantLines(t, out, "warn listen", "is in use. That is expected if janus is already running")
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

	// With routing off, configured models are not checked: they are not used.
	out, _, _ := doctor(t, up.URL, lms.URL, map[string]any{"local": map[string]any{"baseUrl": lms.URL, "models": map[string]any{"x": map[string]any{"model": "no-such-model"}}}})
	if strings.Contains(out, "route") && strings.Contains(out, "no-such-model") {
		t.Errorf("a model was checked with routing off:\n%s", out)
	}
}
