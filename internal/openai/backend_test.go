package openai

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/risadams/janus/internal/anthropic"
	"github.com/risadams/janus/internal/backend"
	"github.com/risadams/janus/internal/config"
	"github.com/risadams/janus/internal/guard"
	"github.com/risadams/janus/internal/openai/openaitest"
	"github.com/risadams/janus/internal/proxy"
)

// throughJanus is a janus whose every Messages request goes to the fake
// OpenAI, as a fallback list naming only openai will send it (#427).
type throughJanus struct {
	*httptest.Server
	exchanges chan proxy.Exchange
}

func newThroughJanus(t *testing.T, srv *openaitest.Server, redactor *guard.Redactor) *throughJanus {
	t.Helper()
	b, err := NewBackend(config.OpenAI{BaseURL: srv.URL, Key: "env:OPENAI_API_KEY"}, loadTestKey(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	unused, _ := url.Parse("http://127.0.0.1:1")
	j := &throughJanus{exchanges: make(chan proxy.Exchange, 1)}
	j.Server = httptest.NewServer(proxy.New(proxy.Options{
		Upstream: backend.NewUpstream(unused, nil),
		Redactor: redactor,
		Plan: func(ctx context.Context, header http.Header, req *anthropic.Request) (proxy.Plan, *proxy.Refusal) {
			choice, translate, err := b.Translation(header, req)
			if err != nil {
				t.Fatal(err)
			}
			return proxy.Plan{Backend: b, Route: "openai", Reason: "test", BackendModel: choice.Model, ResponseModel: req.Model(), Translate: translate}, nil
		},
		KeepBodies: true,
		Done:       func(ex proxy.Exchange) { j.exchanges <- ex },
	}))
	t.Cleanup(j.Close)
	return j
}

// claudeCodeHeaders are what Claude Code sends with each request: its
// Anthropic credential among them.
var claudeCodeHeaders = http.Header{
	"Authorization":            {"Bearer sk-ant-oat01-claude-code-credential"},
	"X-Api-Key":                {"sk-ant-api03-claude-code-credential"},
	"Anthropic-Version":        {"2023-06-01"},
	"Anthropic-Beta":           {"claude-code-20250219"},
	"X-Claude-Code-Session-Id": {"session-1"},
	"Cookie":                   {"session=claude"},
	"Accept-Encoding":          {"gzip, br"},
	"User-Agent":               {"claude-cli/2.3.0"},
}

func (j *throughJanus) send(t *testing.T, body string) (*http.Response, string, proxy.Exchange) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, j.URL+"/v1/messages", strings.NewReader(body))
	req.Header = claudeCodeHeaders.Clone()
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Transport: &http.Transport{DisableCompression: true}}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	select {
	case ex := <-j.exchanges:
		return resp, string(got), ex
	case <-time.After(5 * time.Second):
		t.Fatal("no exchange reported")
		return nil, "", proxy.Exchange{}
	}
}

const hello = `{"model":"claude-sonnet-5-5","max_tokens":1000,"stream":true,"messages":[{"role":"user","content":"Say hi."}]}`

func TestOpenAIIsSentTheKeyAndNoneOfClaudeCodesHeaders(t *testing.T) {
	srv := openaitest.New(t, openaitest.Answer(openaitest.Text{Text: "hi"}))
	srv.Key = testKey
	j := newThroughJanus(t, srv, nil)

	resp, _, _ := j.send(t, hello)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	got := srv.Requests()[0]
	if got.Path != "/v1/responses" {
		t.Errorf("sent to %s, want /v1/responses", got.Path)
	}
	for name := range got.Header {
		switch name {
		case "Authorization", "Content-Type", "Content-Length":
		default:
			t.Errorf("OpenAI was sent %s: %q", name, got.Header.Values(name))
		}
	}
	if got.Header.Get("Authorization") != "Bearer "+testKey {
		t.Error("OpenAI was not sent the key")
	}
	if strings.Contains(string(got.Body), "claude-code-credential") {
		t.Error("Claude Code's credential reached OpenAI in the body")
	}
}

func TestAStreamedAnswerReachesClaudeCodeNamingTheModelItAskedFor(t *testing.T) {
	srv := openaitest.New(t, openaitest.Answer(openaitest.Text{Text: "Hello there."}))
	j := newThroughJanus(t, srv, nil)

	resp, got, ex := j.send(t, hello)
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("status %d, content type %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	answer, ok := anthropic.Assemble([]byte(got), resp.Header.Get("Content-Type"))
	if !ok || textOf(answer) != "Hello there." || !namesOnly(got, "claude-sonnet-5-5") {
		t.Fatalf("Claude Code received %+v from\n%s", answer, got)
	}
	if ex.Backend != "openai" || ex.BackendModel != "gpt-6.1-sol" {
		t.Errorf("exchange went to %s, model %s", ex.Backend, ex.BackendModel)
	}
	if ex.Summary == nil || ex.Summary.Model != "gpt-6.1-sol" {
		t.Errorf("the ledger would record the answer as from %+v, want OpenAI's model", ex.Summary)
	}
	if !strings.Contains(string(ex.SentBody), `"model":"gpt-6.1-sol"`) {
		t.Errorf("the exchange does not record what OpenAI was sent: %s", ex.SentBody)
	}
}

func TestAWholeAnswerReachesClaudeCodeAsAMessage(t *testing.T) {
	srv := openaitest.New(t, openaitest.Answer(openaitest.Text{Text: "Hello there."}))
	j := newThroughJanus(t, srv, nil)

	resp, got, _ := j.send(t, strings.Replace(hello, `"stream":true`, `"stream":false`, 1))
	answer, ok := anthropic.Assemble([]byte(got), resp.Header.Get("Content-Type"))
	if resp.StatusCode != http.StatusOK || !ok || textOf(answer) != "Hello there." || !namesOnly(got, "claude-sonnet-5-5") {
		t.Fatalf("status %d, Claude Code received %s", resp.StatusCode, got)
	}
}

func TestOpenAIErrorsReachClaudeCodeInAnthropicsShape(t *testing.T) {
	for _, c := range []struct {
		name   string
		reply  openaitest.Reply
		status int
		says   string
	}{
		{"rate limit", openaitest.RateLimited("rate_limit_exceeded", "7"), http.StatusTooManyRequests, "per-minute rate limit"},
		{"refused key", openaitest.Fail(http.StatusUnauthorized, "invalid_request_error", "invalid_api_key", "Incorrect API key provided: janus-te****5d85."), http.StatusInternalServerError, "environment variable OPENAI_API_KEY"},
		{"overloaded", openaitest.Fail(http.StatusServiceUnavailable, "server_error", "server_is_overloaded", "busy"), 529, "overloaded"},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv := openaitest.New(t, c.reply)
			j := newThroughJanus(t, srv, nil)
			resp, got, _ := j.send(t, hello)
			if resp.StatusCode != c.status || !strings.Contains(got, `"type":"error"`) || !strings.Contains(got, "janus: ") || !strings.Contains(got, c.says) {
				t.Errorf("status %d, body %s", resp.StatusCode, got)
			}
			if strings.Contains(got, "5d85") {
				t.Error("OpenAI's message, which quotes part of the key, reached Claude Code")
			}
		})
	}
}

// The guard covers OpenAI as it covers Claude: a secret goes as its
// placeholder, and comes back as itself.
func TestSecretsAreRedactedOnTheWayToOpenAIAndRestoredOnTheWayBack(t *testing.T) {
	secret := strings.Join([]string{"AKIA", "Q7RZ2LP9XVB4NKD8"}, "")
	body := strings.Replace(hello, "Say hi.", "deploy with aws_access_key_id = "+secret, 1)
	redactor := guard.NewRedactor(&guard.Detector{})
	rd, err := redactor.Redact([]byte(body))
	if err != nil || !rd.Active() {
		t.Fatalf("the secret was not found: %v", err)
	}
	placeholder := placeholderIn(string(rd.Body))

	srv := openaitest.New(t, openaitest.Answer(openaitest.Text{Text: "Using " + placeholder + "."}))
	j := newThroughJanus(t, srv, redactor)
	resp, got, ex := j.send(t, body)

	if sent := string(srv.Requests()[0].Body); strings.Contains(sent, secret) || !strings.Contains(sent, placeholder) {
		t.Errorf("OpenAI was sent %s", sent)
	}
	answer, _ := anthropic.Assemble([]byte(got), resp.Header.Get("Content-Type"))
	if textOf(answer) != "Using "+secret+"." {
		t.Errorf("Claude Code received %q", textOf(answer))
	}
	if ex.Guard == nil || ex.Guard.Replaced != 1 || ex.Guard.Restored != 1 {
		t.Errorf("guard = %+v", ex.Guard)
	}
}

func textOf(a anthropic.Answer) string {
	var s string
	for _, b := range a.Blocks {
		if b.Type == "text" {
			s += b.Text
		}
	}
	return s
}

// namesOnly reports whether an answer names model, and no OpenAI model.
func namesOnly(answer, model string) bool {
	return strings.Contains(answer, `"model":"`+model+`"`) && !strings.Contains(answer, `"model":"gpt-`)
}

func placeholderIn(s string) string {
	i := strings.Index(s, "JANUS_REDACTED_")
	if i < 0 {
		return ""
	}
	return s[i : i+len("JANUS_REDACTED_")+12]
}

// A stream that stops before the response does reaches Claude Code as a
// broken stream, never as a finished answer.
func TestABrokenOpenAIStreamIsNotPassedOffAsFinished(t *testing.T) {
	srv := openaitest.New(t, openaitest.Reply{Output: []openaitest.Item{openaitest.Text{Text: "Half"}}, Break: true})
	j := newThroughJanus(t, srv, nil)
	_, got, ex := j.send(t, hello)
	if strings.Contains(got, "message_stop") {
		t.Errorf("Claude Code was told the answer finished:\n%s", got)
	}
	if ex.Summary != nil && ex.Summary.StopReason != "" {
		t.Errorf("the exchange records a stop reason, %q", ex.Summary.StopReason)
	}
}
