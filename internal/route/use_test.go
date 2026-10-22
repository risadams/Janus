package route

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/risadams/janus/internal/backend"
	"github.com/risadams/janus/internal/openai/openaitest"
	"github.com/risadams/janus/internal/proxy"
)

// janus use moves a session to a provider by hand; the choice holds until
// cleared or until the provider cannot answer, and one session's choice
// leaves the others to the rules (#379).
func TestAProviderChosenByHandHoldsUntilItCannotAnswer(t *testing.T) {
	oa, srv := fakeOpenAI(t, openaitest.Answer(openaitest.Text{Text: "1"}), openaitest.RateLimited("insufficient_quota", ""))
	var sent []string
	up := httptest.NewServer(claudeAnswers(&sent))
	defer up.Close()
	u, _ := url.Parse(up.URL)
	r := &Router{Upstream: backend.NewUpstream(u, nil), OpenAI: oa,
		Rules: []Rule{{Name: "main", Route: RouteUpstream, Fallback: []Provider{{Name: RouteUpstream}}}}}
	reported := make(chan proxy.Exchange, 1)
	mux := http.NewServeMux()
	mux.Handle(ControlPath, r.Control())
	mux.Handle("/", proxy.New(proxy.Options{Upstream: r.Upstream, Plan: r.Plan, Done: func(ex proxy.Exchange) { r.Report(ex); reported <- ex }}))
	janus := httptest.NewServer(mux)
	defer janus.Close()
	use := func(body string) int {
		resp, err := http.Post(janus.URL+ControlPath, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	send := func(session string) proxy.Exchange {
		req, _ := http.NewRequest(http.MethodPost, janus.URL+"/v1/messages", strings.NewReader(opusRequest))
		req.Header.Set("X-Claude-Code-Session-Id", session)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return <-reported
	}

	if code := use(`{"provider":"codex","session":"s1"}`); code != http.StatusOK {
		t.Fatalf("janus use: %d", code)
	}
	if ex := send("s1"); ex.Backend != "openai" || !strings.HasPrefix(ex.Reason, "janus use openai") {
		t.Errorf("the chosen session went to %s: %q", ex.Backend, ex.Reason)
	}
	if ex := send("s2"); ex.Backend != "upstream" {
		t.Errorf("another session went to %s", ex.Backend)
	}
	// OpenAI's quota runs out: the turn moves on, and the choice ends.
	if ex := send("s1"); ex.Backend != "upstream" {
		t.Errorf("with OpenAI out, the chosen session went to %s", ex.Backend)
	}
	if got := r.manual.get("s1"); got != "" {
		t.Errorf("the choice holds after its provider could not answer: %q", got)
	}

	// For every session, then cleared.
	use(`{"provider":"claude"}`)
	if got := r.manual.get("any"); got != RouteUpstream {
		t.Errorf("every session's choice = %q", got)
	}
	use(`{"clear":true}`)
	if got := r.manual.get("any"); got != "" {
		t.Errorf("cleared, the choice is %q", got)
	}
	if code := use(`{"provider":"gemini"}`); code != http.StatusBadRequest {
		t.Errorf("an unknown provider: %d", code)
	}
	if len(srv.Requests()) != 2 {
		t.Errorf("OpenAI asked %d times", len(srv.Requests()))
	}
}

// A web page in a browser on this machine cannot move a session.
func TestJanusUseRefusesWebPages(t *testing.T) {
	r := &Router{}
	srv := httptest.NewServer(r.Control())
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(`{"provider":"claude"}`))
	req.Header.Set("Origin", "https://example.com")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || r.manual.get("") != "" {
		t.Errorf("status %d, choice %q", resp.StatusCode, r.manual.get(""))
	}
}
