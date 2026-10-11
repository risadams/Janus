package route

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/risadams/janus/internal/backend"
	"github.com/risadams/janus/internal/guard/guardtest"
)

// The leak tests: requests seeded with every kind of secret and with
// local-only content are sent down every route janus has, and nothing
// protected may appear in any byte the fake Anthropic receives.

// localOnlyContent is what the policy protects in the seeded request. None
// of it is shaped like a secret: only the policy can keep it back.
var localOnlyContent = []string{
	"the merger closes on the ninth",   // read from a protected path
	"the board meets in Lisbon",        // quoted by a search
	"margin on the Halden account",     // context line of that search
	"layoffs are planned for the west", // in a document carrying a marker
	"our bid will be four million",     // in a message carrying a marker
}

// forbidden is everything that must not reach the upstream: each secret in
// the corpus, and the local-only content.
func forbidden() []string {
	out := append([]string{}, localOnlyContent...)
	seen := map[string]bool{}
	for _, s := range guardtest.Secrets() {
		if !seen[s.Want] {
			seen[s.Want] = true
			out = append(out, s.Want)
		}
	}
	return out
}

// leaksIn returns which forbidden items are in a body sent upstream. It
// looks for each as it is, as JSON would escape it, and line by line, so
// that a secret spanning lines is found however it was written out.
func leaksIn(body []byte) []string {
	var found []string
	for _, item := range forbidden() {
		forms := []string{item}
		if escaped, err := json.Marshal(item); err == nil {
			forms = append(forms, strings.Trim(string(escaped), `"`))
		}
		for _, line := range strings.Split(item, "\n") {
			if len(line) >= 20 && !strings.HasPrefix(line, "-----") {
				forms = append(forms, line)
			}
		}
		for _, form := range forms {
			if bytes.Contains(body, []byte(form)) {
				found = append(found, fmt.Sprintf("%.12s… (%d bytes)", item, len(item)))
				break
			}
		}
	}
	return found
}

// seeded is a conversation with every secret in the corpus spread over
// every part of a request that carries text, and local-only content in
// each of the forms the policy knows.
func seeded(stream bool) string {
	secrets := guardtest.Secrets()
	// part deals the secrets out in turn, so every one lands somewhere.
	part := func(n int) string {
		var texts []string
		for i, s := range secrets {
			if i%6 == n {
				texts = append(texts, s.Text)
			}
		}
		return strings.Join(texts, "\n")
	}
	text := func(s string) map[string]any { return map[string]any{"type": "text", "text": s} }
	use := func(id, name string, input map[string]any) map[string]any {
		return map[string]any{"role": "assistant", "content": []map[string]any{{"type": "tool_use", "id": id, "name": name, "input": input}}}
	}
	result := func(id string, content any) map[string]any {
		return map[string]any{"role": "user", "content": []map[string]any{{"type": "tool_result", "tool_use_id": id, "content": content}}}
	}
	body, _ := json.Marshal(map[string]any{
		"model": "claude-opus-5-5", "stream": stream, "max_tokens": 1024,
		"system": []map[string]any{text("You are an agent."), text("Project notes:\n" + part(0))},
		"tools":  []map[string]any{{"name": "Read", "description": "Reads a file. " + part(2), "input_schema": map[string]any{"type": "object"}}},
		"messages": []map[string]any{
			{"role": "user", "content": "set this up for me:\n" + part(1)},
			use("t1", "Read", map[string]any{"file_path": "/app/deploy/settings.py"}),
			result("t1", part(2)),
			use("t2", "Bash", map[string]any{"command": "deploy --with '" + part(3) + "'", "env": map[string]any{"note": part(3)}}),
			result("t2", []map[string]any{text("deployed"), text(part(4))}),
			{"role": "assistant", "content": []map[string]any{text("I used these:\n" + part(5))}},
			// Local-only, by path.
			use("t3", "Read", map[string]any{"file_path": "/app/secrets/plan.md"}),
			result("t3", "the merger closes on the ninth"),
			// Local-only, quoted by a search that never named the file.
			use("t4", "Grep", map[string]any{"pattern": "meets|margin", "path": "/app"}),
			result("t4", "src/cal.go:4:// when the team meets\nsecrets/notes.txt:3:the board meets in Lisbon\nsecrets/notes.txt-4-margin on the Halden account"),
			// Local-only, by marker.
			use("t5", "Read", map[string]any{"file_path": "/app/docs/memo.md"}),
			result("t5", []map[string]any{text("COMPANY CONFIDENTIAL\nlayoffs are planned for the west")}),
			{"role": "user", "content": []map[string]any{text("COMPANY CONFIDENTIAL: our bid will be four million"), text("now carry on")}},
		},
	})
	return string(body)
}

// TestTheLeakCheckCanFail shows the check is not vacuous: with the guard
// off, the same request sends every forbidden item upstream, and leaksIn
// says so.
func TestTheLeakCheckCanFail(t *testing.T) {
	s := newSite(t, siteOptions{noGuard: true})
	if status, answer, _ := s.post("/v1/messages", seeded(true)); status != 200 {
		t.Fatalf("status %d: %s", status, answer)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.sent) != 1 {
		t.Fatalf("upstream received %d requests", len(s.sent))
	}
	if got, want := len(leaksIn(s.sent[0])), len(forbidden()); got != want {
		t.Errorf("with the guard off the check found %d of %d forbidden items; it would miss a leak", got, want)
	}
	if len(forbidden()) < 18 {
		t.Errorf("the corpus has only %d items", len(forbidden()))
	}
}

func TestNothingProtectedReachesTheUpstream(t *testing.T) {
	local := qwen["janus-local"]
	broken := func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "model crashed", http.StatusInternalServerError)
	}
	empty := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, strings.ReplaceAll(localStream, "Two lines.", ""))
	}
	slow := func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}
	overloaded := func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(529)
		io.WriteString(w, `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`)
	}
	mainLoop := []string{"X-Claude-Code-Agent-Id", ""}

	routes := []struct {
		name   string
		opts   siteOptions
		path   string
		header []string
		// upstream is how many requests the route sends to Anthropic
		// when nothing makes the request local-only.
		upstream int
	}{
		{name: "no routing at all", upstream: 1},
		{name: "token count", path: "/v1/messages/count_tokens", upstream: 1},
		{name: "override to upstream", header: []string{backend.RouteHeader, "upstream"}, upstream: 1},
		{name: "override to local", header: []string{backend.RouteHeader, "local"}, upstream: 0},
		{name: "rule: upstream", opts: siteOptions{rules: []Rule{{Name: "all", Route: RouteUpstream}}}, upstream: 1},
		{name: "rule: shadow", opts: siteOptions{rules: []Rule{{Name: "watch", Route: RouteShadow, Local: local}}}, upstream: 1},
		{name: "rule: local", opts: siteOptions{rules: subagentsLocal}, upstream: 0},
		{name: "rule: local, main conversation unmatched", opts: siteOptions{rules: subagentsLocal}, header: mainLoop, upstream: 1},
		{name: "rule: local, LM Studio absent, falls back", opts: siteOptions{rules: subagentsLocal, noLocal: true}, upstream: 1},
		{name: "rule: local, LM Studio failing, falls back", opts: siteOptions{rules: subagentsLocal, lmStudio: broken}, upstream: 1},
		{name: "rule: escalate, local answer accepted", opts: siteOptions{
			rules: []Rule{{Name: "try", Route: RouteEscalate, Local: local, TrialTimeout: time.Minute}}}, upstream: 0},
		{name: "rule: escalate, local answer empty", opts: siteOptions{lmStudio: empty,
			rules: []Rule{{Name: "try", Route: RouteEscalate, Local: local, TrialTimeout: time.Minute}}}, upstream: 1},
		{name: "rule: escalate, local model failing", opts: siteOptions{lmStudio: broken,
			rules: []Rule{{Name: "try", Route: RouteEscalate, Local: local, TrialTimeout: time.Minute}}}, upstream: 1},
		{name: "rule: escalate, local model too slow", opts: siteOptions{lmStudio: slow,
			rules: []Rule{{Name: "try", Route: RouteEscalate, Local: local, TrialTimeout: 100 * time.Millisecond}}}, upstream: 1},
		{name: "rule: upstream, Anthropic overloaded, local stands in", opts: siteOptions{upstream: overloaded,
			rules: []Rule{{Name: "all", Route: RouteUpstream, IfDown: &local}}}, upstream: 1},
		{name: "Anthropic overloaded, nothing stands in", opts: siteOptions{upstream: overloaded}, upstream: 1},
	}

	total := 0
	for _, action := range []string{"withhold", "local"} {
		for _, stream := range []bool{true, false} {
			for _, route := range routes {
				name := fmt.Sprintf("%s/stream=%v/%s", action, stream, route.name)
				t.Run(name, func(t *testing.T) {
					opts := route.opts
					opts.action, opts.redact = action, true
					s := newSite(t, opts)
					path := route.path
					if path == "" {
						path = "/v1/messages"
					}
					// Twice, as a conversation does: the second finds
					// the breaker and any cached state as the first
					// left them.
					for turn := 1; turn <= 2; turn++ {
						s.post(path, seeded(stream), route.header...)
					}

					s.mu.Lock()
					sent := s.sent
					s.mu.Unlock()
					total += len(sent)
					for i, body := range sent {
						if leaks := leaksIn(body); len(leaks) > 0 {
							t.Errorf("upstream request %d of %d holds %d protected items: %s", i+1, len(sent), len(leaks), strings.Join(leaks, ", "))
						}
					}
					// Under "local" a request holding local-only content
					// goes nowhere near Anthropic, except to be counted.
					want := 2 * route.upstream
					if action == "local" && path == "/v1/messages" {
						want = 0
					}
					if len(sent) != want {
						t.Errorf("upstream received %d requests, want %d", len(sent), want)
					}
				})
			}
		}
	}
	if total < 40 {
		t.Errorf("only %d requests reached the upstream across all routes; the test is not exercising them", total)
	}
}
