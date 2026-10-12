package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/risadams/janus/internal/backend"
	"github.com/risadams/janus/internal/guard"
)

// A key assembled at run time, so that nothing credential-shaped sits in
// the repository.
var fakeKey = strings.Join([]string{"AKIA", "Q7RZ2LP9XVB4NKD8"}, "")

var placeholderShape = regexp.MustCompile(`JANUS_REDACTED_[0-9a-f]{12}`)

func withSecret(model string) string {
	body, _ := json.Marshal(map[string]any{
		"model": model, "stream": true,
		"messages": []map[string]any{{"role": "user", "content": "deploy using aws_access_key_id = " + fakeKey + " please"}},
	})
	return string(body)
}

// guarded runs janus with a redactor in front of an upstream, sends one
// request, and returns what the upstream saw, what the client got, and the
// exchange.
type guardedResult struct {
	UpstreamBody   string
	UpstreamHeader http.Header
	UpstreamHits   int
	Status         int
	Body           string
	Exchange       Exchange
}

func guarded(t *testing.T, path, body string, respond func(w http.ResponseWriter, r *http.Request, seen string)) guardedResult {
	t.Helper()
	var res guardedResult
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		res.UpstreamBody, res.UpstreamHeader = string(b), r.Header.Clone()
		res.UpstreamHits++
		respond(w, r, string(b))
	}))
	defer up.Close()
	u, _ := url.Parse(up.URL)
	reported := make(chan Exchange, 1)
	janus := httptest.NewServer(New(Options{
		Upstream: backend.NewUpstream(u, nil), Redactor: guard.NewRedactor(&guard.Detector{}), KeepBodies: true,
		Done: func(ex Exchange) { reported <- ex },
	}))
	defer janus.Close()

	req, _ := http.NewRequest("POST", janus.URL+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret-token")
	req.Header.Set("Accept-Encoding", "gzip, deflate, br, zstd")
	resp, err := plainClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	res.Status, res.Body = resp.StatusCode, string(b)
	select {
	case res.Exchange = <-reported:
	case <-time.After(3 * time.Second):
		t.Fatal("no exchange reported")
	}
	return res
}

// streamUsing answers with a stream whose text uses whatever placeholder
// was in the request, cut in two so that it straddles two events.
func streamUsing(w http.ResponseWriter, r *http.Request, seen string) {
	token := placeholderShape.FindString(seen)
	text := "Deploying with " + token + " now."
	cut := len("Deploying with ") + 7
	w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprint(w, "event: message_start\n"+`data: {"type":"message_start","message":{"id":"m","model":"claude-opus-5-5","usage":{"input_tokens":2}}}`+"\n\n")
	fmt.Fprint(w, "event: content_block_start\n"+`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`+"\n\n")
	for _, piece := range []string{text[:cut], text[cut:]} {
		enc, _ := json.Marshal(piece)
		fmt.Fprintf(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":%s}}\n\n", enc)
		w.(http.Flusher).Flush()
	}
	fmt.Fprint(w, "event: content_block_stop\n"+`data: {"type":"content_block_stop","index":0}`+"\n\n")
	fmt.Fprint(w, "event: message_delta\n"+`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":9}}`+"\n\n")
	fmt.Fprint(w, "event: message_stop\n"+`data: {"type":"message_stop"}`+"\n\n")
}

// unissued has the shape of a placeholder and is not one janus made.
const unissued = "JANUS_" + "REDACTED_0123456789ab"

// streamAs answers with a stream of one block, a delta per piece.
func streamAs(w http.ResponseWriter, deltaType, field string, pieces ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprint(w, "event: message_start\n"+`data: {"type":"message_start","message":{"id":"m","model":"claude-opus-5-5","usage":{"input_tokens":2}}}`+"\n\n")
	fmt.Fprint(w, "event: content_block_start\n"+`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`+"\n\n")
	for _, piece := range pieces {
		enc, _ := json.Marshal(piece)
		fmt.Fprintf(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":%q,%q:%s}}\n\n", deltaType, field, enc)
		w.(http.Flusher).Flush()
	}
	fmt.Fprint(w, "event: content_block_stop\n"+`data: {"type":"content_block_stop","index":0}`+"\n\n")
	fmt.Fprint(w, "event: message_delta\n"+`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":9}}`+"\n\n")
	fmt.Fprint(w, "event: message_stop\n"+`data: {"type":"message_stop"}`+"\n\n")
}

func textOfStream(stream string) (text string, sawError bool) {
	for _, line := range strings.Split(stream, "\n") {
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		var ev struct {
			Type  string
			Delta struct{ Text string }
		}
		json.Unmarshal([]byte(data), &ev)
		text += ev.Delta.Text
		sawError = sawError || ev.Type == "error"
	}
	return text, sawError
}

func TestSecretsAreRedactedGoingUpAndRestoredComingDown(t *testing.T) {
	res := guarded(t, "/v1/messages", withSecret("claude-opus-5-5"), streamUsing)

	// Up: the upstream never sees the key, and sees a placeholder in its
	// place with the rest of the request intact.
	if strings.Contains(res.UpstreamBody, fakeKey) {
		t.Fatal("the secret reached the upstream")
	}
	if !placeholderShape.MatchString(res.UpstreamBody) || !strings.Contains(res.UpstreamBody, "deploy using aws_access_key_id = ") || !strings.Contains(res.UpstreamBody, " please") {
		t.Errorf("upstream saw %s", res.UpstreamBody)
	}
	if res.UpstreamHeader.Get("Authorization") != "Bearer secret-token" {
		t.Error("the client's own credential must still reach the upstream")
	}
	// The response has to be rewritten, so it is asked for uncompressed.
	if got := res.UpstreamHeader.Get("Accept-Encoding"); got != "identity" {
		t.Errorf("upstream Accept-Encoding = %q, want identity", got)
	}

	// Down: the client gets the key back, and never sees a placeholder.
	text, sawError := textOfStream(res.Body)
	if text != "Deploying with "+fakeKey+" now." || sawError || strings.Contains(res.Body, "JANUS_REDACTED") {
		t.Errorf("client received %q (error event %v)\n%s", text, sawError, res.Body)
	}

	// Recorded: what happened, the original request as the client sent it,
	// and the response as the upstream sent it, which holds no secret.
	g := res.Exchange.Guard
	if g == nil || g.Replaced != 1 || g.Restored != 1 || g.Blocked != "" || g.Report.Kinds["aws_access_key"] != 1 {
		t.Fatalf("guard = %+v", g)
	}
	if !strings.Contains(string(res.Exchange.RequestBody), fakeKey) {
		t.Error("the kept request should be the client's original")
	}
	// SentBody is the evidence of what left: exactly what the upstream got.
	if string(res.Exchange.SentBody) != res.UpstreamBody || strings.Contains(string(res.Exchange.SentBody), fakeKey) {
		t.Error("SentBody should be the redacted request exactly as the upstream received it")
	}
	// (The upstream cut the placeholder across two events, so only its
	// first half can be looked for here.)
	if strings.Contains(string(res.Exchange.ResponseBody), fakeKey) || !strings.Contains(string(res.Exchange.ResponseBody), "JANUS_R") {
		t.Error("the kept response should be what the upstream sent, placeholders and all")
	}
	if res.Exchange.Summary == nil || !res.Exchange.Summary.Complete {
		t.Errorf("summary = %+v", res.Exchange.Summary)
	}
}

func TestRedactionRestoresANonStreamingAnswer(t *testing.T) {
	res := guarded(t, "/v1/messages", withSecret("claude-opus-5-5"), func(w http.ResponseWriter, r *http.Request, seen string) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"type":"message","model":"claude-opus-5-5","content":[{"type":"tool_use","id":"t","name":"Bash","input":{"command":"aws configure set aws_access_key_id %s"}}],"stop_reason":"tool_use","usage":{"input_tokens":2,"output_tokens":9}}`, placeholderShape.FindString(seen))
	})
	var msg struct {
		Content []struct{ Input struct{ Command string } }
	}
	if err := json.Unmarshal([]byte(res.Body), &msg); err != nil || res.Status != 200 {
		t.Fatalf("status %d, body %s (%v)", res.Status, res.Body, err)
	}
	if msg.Content[0].Input.Command != "aws configure set aws_access_key_id "+fakeKey {
		t.Errorf("command = %q", msg.Content[0].Input.Command)
	}
	if res.Exchange.Guard.Restored != 1 {
		t.Errorf("guard = %+v", res.Exchange.Guard)
	}
}

// A request with nothing secret in it goes exactly as it came, compressed
// response and all: the redactor changes nothing it does not have to.
func TestRedactorLeavesCleanRequestsAlone(t *testing.T) {
	clean := `{"model":"claude-opus-5-5","stream":true,"messages":[{"role":"user","content":"count the lines"}]}`
	res := guarded(t, "/v1/messages", clean, streamUsing)

	if res.UpstreamBody != clean {
		t.Errorf("upstream saw %q", res.UpstreamBody)
	}
	if got := res.UpstreamHeader.Get("Accept-Encoding"); got != "gzip" {
		t.Errorf("Accept-Encoding = %q, want the usual gzip", got)
	}
	if g := res.Exchange.Guard; g == nil || g.Replaced != 0 || g.Report.Findings != 0 {
		t.Errorf("guard = %+v", g)
	}
	if res.Exchange.SentBody != nil {
		t.Error("a request sent as received should have no separate SentBody")
	}
}

// A request that cannot be checked is not sent.
func TestRedactorFailsClosed(t *testing.T) {
	for name, body := range map[string]string{"not json": `{"model":`, "not a request": `["a","b"]`} {
		res := guarded(t, "/v1/messages", body, func(http.ResponseWriter, *http.Request, string) {})
		if res.UpstreamHits != 0 {
			t.Errorf("%s: a request that could not be checked was sent upstream", name)
		}
		if res.Status != http.StatusInternalServerError || !strings.Contains(res.Body, "The request was not sent upstream") {
			t.Errorf("%s: status %d, body %s", name, res.Status, res.Body)
		}
		if g := res.Exchange.Guard; g == nil || g.Blocked == "" || res.Exchange.Backend != "none" {
			t.Errorf("%s: guard = %+v, backend %q", name, g, res.Exchange.Backend)
		}
	}
}

func TestCountTokensIsRedactedToo(t *testing.T) {
	res := guarded(t, "/v1/messages/count_tokens", withSecret("claude-opus-5-5"), func(w http.ResponseWriter, r *http.Request, seen string) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"input_tokens":42}`)
	})
	if strings.Contains(res.UpstreamBody, fakeKey) || !placeholderShape.MatchString(res.UpstreamBody) {
		t.Errorf("count_tokens sent the secret upstream: %s", res.UpstreamBody)
	}
	if res.Status != 200 || res.Body != `{"input_tokens":42}` {
		t.Errorf("status %d, body %s", res.Status, res.Body)
	}
}

func TestUnrestorableAnswerIsStoppedNotPassedOn(t *testing.T) {
	// A stream that uses a placeholder janus never issued.
	res := guarded(t, "/v1/messages", withSecret("claude-opus-5-5"), func(w http.ResponseWriter, r *http.Request, seen string) {
		streamAs(w, "input_json_delta", "partial_json", `{"command":"deploy `, unissued+`"}`)
	})
	_, sawError := textOfStream(res.Body)
	if !sawError || strings.Contains(res.Body, "message_stop") || strings.Contains(res.Body, fakeKey) {
		t.Errorf("the stream was not stopped with an error:\n%s", res.Body)
	}
	if g := res.Exchange.Guard; g == nil || g.Blocked == "" {
		t.Errorf("guard = %+v", g)
	}

	// In prose nothing acts on the token, so the turn goes through as the
	// model wrote it.
	res = guarded(t, "/v1/messages", withSecret("claude-opus-5-5"), func(w http.ResponseWriter, r *http.Request, seen string) {
		streamAs(w, "text_delta", "text", "The placeholder looks like ", unissued+".")
	})
	text, sawError := textOfStream(res.Body)
	if sawError || text != "The placeholder looks like "+unissued+"." || !strings.Contains(res.Body, "message_stop") || res.Exchange.Guard.Blocked != "" {
		t.Errorf("prose naming a placeholder was stopped:\n%s", res.Body)
	}

	// The same in a JSON answer: the whole answer is withheld.
	res = guarded(t, "/v1/messages", withSecret("claude-opus-5-5"), func(w http.ResponseWriter, r *http.Request, seen string) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"type":"message","content":[{"type":"text","text":"use JANUS_REDACTED_0123456789ab"}]}`)
	})
	if res.Status != http.StatusBadGateway || !strings.Contains(res.Body, "withheld") || res.Exchange.Guard.Blocked == "" {
		t.Errorf("status %d, body %s, guard %+v", res.Status, res.Body, res.Exchange.Guard)
	}
}

// An upstream error is passed on as it is; there is nothing in it to
// restore and the status must not be disturbed.
func TestUpstreamErrorPassesThroughUnderRedaction(t *testing.T) {
	errBody := `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`
	res := guarded(t, "/v1/messages", withSecret("claude-opus-5-5"), func(w http.ResponseWriter, r *http.Request, seen string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(429)
		io.WriteString(w, errBody)
	})
	if res.Status != 429 || res.Body != errBody {
		t.Errorf("status %d, body %s", res.Status, res.Body)
	}
}
