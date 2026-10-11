package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// explain runs `janus route` on a request body, against a config with one
// local model and the given rules, with LM Studio played by lms.
func explain(t *testing.T, lmsURL string, routeOn bool, rules []map[string]any, body string, extraArgs ...string) (string, int) {
	t.Helper()
	dir := t.TempDir()
	cfg := map[string]any{
		"local":    map[string]any{"baseUrl": lmsURL, "models": map[string]any{"janus-local": map[string]any{"model": "qwen/qwen3.8-27b"}}},
		"features": map[string]any{"route": routeOn},
		"routing":  rules,
	}
	data, _ := json.Marshal(cfg)
	cfgPath := filepath.Join(dir, "janus.config.json")
	os.WriteFile(cfgPath, data, 0o600)
	reqPath := filepath.Join(dir, "request.json")
	os.WriteFile(reqPath, []byte(body), 0o600)

	var stdout, stderr bytes.Buffer
	args := append([]string{"route", "-config", cfgPath}, extraArgs...)
	code := run(append(args, reqPath), &stdout, &stderr)
	return stdout.String() + stderr.String(), code
}

const subagentBody = `{"model":"claude-opus-5-5","system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1; cc_entrypoint=sdk-cli; cc_is_subagent=1;"}],"tools":[{"name":"Read"},{"name":"Bash"}],"messages":[{"role":"user","content":"count lines"}]}`
const mainBody = `{"model":"claude-opus-5-5","system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1; cc_entrypoint=sdk-cli;"}],"messages":[{"role":"user","content":"hi"}]}`

var subagentsLocal = []map[string]any{
	{"name": "haiku", "model": "claude-haiku-5-5", "route": "local", "local": "janus-local"},
	{"name": "subagents", "class": "subagent", "route": "local", "local": "janus-local"},
}

func TestRouteExplainsALocalDecision(t *testing.T) {
	lms := serverSaying(200, lmStudioModels)
	defer lms.Close()

	out, code := explain(t, lms.URL, true, subagentsLocal, subagentBody)
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	wantLines(t, out,
		"Request claude-opus-5-5, class subagent (from the request's first system block), about",
		"tokens, 2 tools",
		"rule haiku: no match (model is claude-opus-5-5, not claude-haiku-5-5)",
		"rule subagents: local, to qwen/qwen3.8-27b",
		"Decision local: qwen/qwen3.8-27b answers, reported to the client as claude-opus-5-5",
		"changes on the way: set_model",
	)
}

func TestRouteExplainsAnUpstreamDecision(t *testing.T) {
	lms := serverSaying(200, lmStudioModels)
	defer lms.Close()

	out, _ := explain(t, lms.URL, true, subagentsLocal, mainBody)
	wantLines(t, out,
		"class main",
		"no tools",
		"rule subagents: no match (class is main, not subagent)",
		"no rule matched: upstream",
		"Decision upstream, untouched",
	)

	// -class overrides what the body says.
	out, _ = explain(t, lms.URL, true, subagentsLocal, mainBody, "-class", "subagent")
	wantLines(t, out, "class subagent (as given)", "Decision local")
}

func TestRouteExplainsAFallbackAndARefusal(t *testing.T) {
	// LM Studio is not there: a rule falls back upstream, an alias refuses.
	down := "http://" + freeAddr(t)

	out, code := explain(t, down, true, subagentsLocal, subagentBody)
	if code != 0 {
		t.Errorf("exit %d", code)
	}
	wantLines(t, out, "rule subagents: could not reach LM Studio", "; sent upstream", "Decision upstream, untouched")

	out, _ = explain(t, down, true, nil, `{"model":"janus-local","messages":[{"role":"user","content":"hi"}]}`)
	wantLines(t, out, "class unknown (it does not look like a Claude Code request)", "model alias janus-local: refused", "Decision refused with HTTP 502: janus: could not reach LM Studio")
}

func TestRouteSaysWhenRoutingIsOff(t *testing.T) {
	lms := serverSaying(200, lmStudioModels)
	defer lms.Close()

	out, _ := explain(t, lms.URL, false, subagentsLocal, subagentBody)
	wantLines(t, out, "Routing is off (features.route), so this request goes upstream untouched. With it on:", "rule subagents: local")
}

func TestRouteBadInput(t *testing.T) {
	lms := serverSaying(200, lmStudioModels)
	defer lms.Close()

	if out, code := explain(t, lms.URL, true, nil, "not json"); code != 1 || !strings.Contains(out, "is not a Messages request body") {
		t.Errorf("exit %d: %s", code, out)
	}
	if out, code := explain(t, lms.URL, true, nil, mainBody, "-class", "sidekick"); code != 2 || !strings.Contains(out, `-class "sidekick"`) {
		t.Errorf("exit %d: %s", code, out)
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"route"}, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), "usage: janus route") {
		t.Errorf("exit %d: %s", code, stderr.String())
	}
	if code := run([]string{"route", filepath.Join(t.TempDir(), "missing.json")}, &stdout, &stderr); code != 1 {
		t.Errorf("missing file: exit %d", code)
	}
}

func TestBannerListsRoutes(t *testing.T) {
	lines := routingSummary(testConfigWithRules())
	want := []string{
		`model "janus-local" -> local qwen/qwen3.8-27b`,
		"rule small-subagents: class subagent, up to 40000 tokens, with tools -> local qwen/qwen3.8-27b",
		"rule 2: model claude-haiku-5-5 -> local qwen/qwen3.8-27b first, Claude if its answer fails the checks or takes over 2m0s",
		"rule rest: everything else -> upstream",
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Errorf("summary:\n%s\nwant:\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
}

func TestRouteExplainsLocalFirstAndFallback(t *testing.T) {
	lms := serverSaying(200, lmStudioModels)
	defer lms.Close()

	out, _ := explain(t, lms.URL, true, []map[string]any{{"name": "careful", "class": "subagent", "route": "local-then-escalate", "local": "janus-local"}}, subagentBody)
	wantLines(t, out,
		"rule careful: local first, on qwen/qwen3.8-27b; its answer is checked, and Claude answers instead if it fails",
		"Decision local first: qwen/qwen3.8-27b answers, and its whole answer is checked before the client",
		"If it passes it is reported as claude-opus-5-5; if not, Claude answers.",
	)

	out, _ = explain(t, lms.URL, true, subagentsLocal, subagentBody)
	wantLines(t, out, "Decision local: qwen/qwen3.8-27b answers", "if it cannot be reached or answers with an error, Claude answers instead")
}
