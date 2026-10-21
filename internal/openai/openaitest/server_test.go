package openaitest_test

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/risadams/janus/internal/openai/openaitest"
)

// event is one server-sent event: the type its "event:" line named, if
// there was one, and its data.
type event struct {
	name string
	data map[string]any
}

func post(t *testing.T, url, body string) *http.Response {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// readEvents reads a stream to its end, returning its events and the
// error, if any, that ended it.
func readEvents(t *testing.T, r io.Reader) ([]event, error) {
	t.Helper()
	var events []event
	var name string
	sc := bufio.NewScanner(r)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			var data map[string]any
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &data); err != nil {
				t.Fatalf("event data is not JSON: %v: %s", err, line)
			}
			events = append(events, event{name, data})
			name = ""
		}
	}
	return events, sc.Err()
}

func types(events []event) []string {
	var out []string
	for _, e := range events {
		out = append(out, e.data["type"].(string))
	}
	return out
}

func TestStreamFollowsTheDocumentedEventOrder(t *testing.T) {
	s := openaitest.New(t, openaitest.Reply{
		Output: []openaitest.Item{
			openaitest.Reasoning{Summary: "Thinking it over", Encrypted: "gAAAA-envelope"},
			openaitest.Text{Text: "Here is the plan."},
			openaitest.Call{CallID: "call_1", Name: "Bash", Arguments: `{"command":"ls -la"}`},
		},
		Usage: openaitest.Usage{Input: 1200, Cached: 1000, Write30m: 100, Output: 50, Reasoning: 20},
	})
	resp := post(t, s.URL+"/v1/responses", `{"model":"gpt-6.1-sol","stream":true,"store":false}`)
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q", ct)
	}
	events, err := readEvents(t, resp.Body)
	if err != nil {
		t.Fatal(err)
	}

	got := strings.Join(types(events), "\n")
	want := strings.Join([]string{
		"response.created", "response.in_progress",
		"response.output_item.added",
		"response.reasoning_summary_part.added",
		"response.reasoning_summary_text.delta", "response.reasoning_summary_text.delta",
		"response.reasoning_summary_text.done", "response.reasoning_summary_part.done",
		"response.output_item.done",
		"response.output_item.added", "response.content_part.added",
		"response.output_text.delta", "response.output_text.delta", "response.output_text.delta",
		"response.output_text.done", "response.content_part.done", "response.output_item.done",
		"response.output_item.added",
		"response.function_call_arguments.delta", "response.function_call_arguments.delta", "response.function_call_arguments.delta",
		"response.function_call_arguments.done", "response.output_item.done",
		"response.completed",
	}, "\n")
	if got != want {
		t.Fatalf("events:\n%s\nwant:\n%s", got, want)
	}

	for i, e := range events {
		if e.name != e.data["type"] {
			t.Errorf("event %d: event line %q, data type %q", i, e.name, e.data["type"])
		}
		if n := e.data["sequence_number"].(float64); int(n) != i {
			t.Errorf("event %d: sequence_number %v", i, n)
		}
		if strings.HasSuffix(e.data["type"].(string), ".delta") && e.data["obfuscation"] == nil {
			t.Errorf("event %d (%s) has no obfuscation", i, e.data["type"])
		}
	}

	final := events[len(events)-1].data["response"].(map[string]any)
	if final["status"] != "completed" || final["store"] != false || final["model"] != "gpt-6.1-sol" {
		t.Errorf("final response = %v", final)
	}
	usage := final["usage"].(map[string]any)
	details := usage["input_tokens_details"].(map[string]any)
	if usage["input_tokens"] != 1200.0 || details["cached_tokens"] != 1000.0 || details["cache_write_tokens"] != 100.0 {
		t.Errorf("usage = %v", usage)
	}
	if len(final["output"].([]any)) != 3 {
		t.Errorf("final output has %d items, want 3", len(final["output"].([]any)))
	}
}

func TestEncryptedReasoningArrivesOnlyWhenTheItemIsDone(t *testing.T) {
	s := openaitest.New(t, openaitest.Answer(openaitest.Reasoning{Encrypted: "gAAAA-envelope"}))
	events, err := readEvents(t, post(t, s.URL+"/v1/responses", `{"stream":true}`).Body)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		item, _ := e.data["item"].(map[string]any)
		if item == nil {
			continue
		}
		enc, has := item["encrypted_content"]
		switch e.data["type"] {
		case "response.output_item.added":
			if has {
				t.Errorf("added item carries encrypted_content %v", enc)
			}
		case "response.output_item.done":
			if enc != "gAAAA-envelope" {
				t.Errorf("done item's encrypted_content = %v", enc)
			}
		}
	}
}

func TestDeltasAreJoinedIntoTheWholeText(t *testing.T) {
	text := "Ünïcödé text, split into deltas — never inside a character."
	s := openaitest.New(t, openaitest.Answer(openaitest.Text{Text: text}))
	events, err := readEvents(t, post(t, s.URL+"/v1/responses", `{"stream":true}`).Body)
	if err != nil {
		t.Fatal(err)
	}
	var joined strings.Builder
	for _, e := range events {
		if e.data["type"] == "response.output_text.delta" {
			joined.WriteString(e.data["delta"].(string))
		}
	}
	if joined.String() != text {
		t.Fatalf("deltas join to %q", joined.String())
	}
}

func TestEventLinesCanBeLeftOut(t *testing.T) {
	s := openaitest.New(t, openaitest.Answer(openaitest.Text{Text: "hi"}))
	s.OmitEventLines = true
	events, err := readEvents(t, post(t, s.URL+"/v1/responses", `{"stream":true}`).Body)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if e.name != "" {
			t.Fatalf("event line %q sent", e.name)
		}
	}
	if len(events) == 0 || events[len(events)-1].data["type"] != "response.completed" {
		t.Fatalf("events = %v", types(events))
	}
}

func TestNonStreamingAnswerIsOneResponseObject(t *testing.T) {
	s := openaitest.New(t, openaitest.Answer(
		openaitest.Call{CallID: "call_9", Name: "Read", Arguments: `{"path":"a.go"}`}))
	resp := post(t, s.URL+"/v1/responses", `{"model":"gpt-6-luna"}`)
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["object"] != "response" || body["status"] != "completed" || body["model"] != "gpt-6-luna" {
		t.Fatalf("body = %v", body)
	}
	item := body["output"].([]any)[0].(map[string]any)
	if item["type"] != "function_call" || item["call_id"] != "call_9" || item["arguments"] != `{"path":"a.go"}` {
		t.Fatalf("item = %v", item)
	}
}

func TestIncompleteAndFailedResponsesEndTheStream(t *testing.T) {
	s := openaitest.New(t,
		openaitest.Reply{Output: []openaitest.Item{openaitest.Text{Text: "cut"}}, Status: "incomplete", Reason: "max_output_tokens"},
		openaitest.Reply{Status: "failed", Reason: "server_error", Message: "The model failed to generate a response."},
	)
	for _, want := range []struct{ event, field, value string }{
		{"response.incomplete", "incomplete_details", "max_output_tokens"},
		{"response.failed", "error", "server_error"},
	} {
		events, err := readEvents(t, post(t, s.URL+"/v1/responses", `{"stream":true}`).Body)
		if err != nil {
			t.Fatal(err)
		}
		last := events[len(events)-1].data
		if last["type"] != want.event {
			t.Fatalf("last event %v, want %s", last["type"], want.event)
		}
		detail := last["response"].(map[string]any)[want.field].(map[string]any)
		if detail["reason"] != want.value && detail["code"] != want.value {
			t.Errorf("%s = %v", want.field, detail)
		}
	}
}

func TestMidStreamErrorEndsWithAnErrorEvent(t *testing.T) {
	s := openaitest.New(t, openaitest.Reply{
		Output:      []openaitest.Item{openaitest.Text{Text: "partial"}},
		StreamError: &openaitest.StreamError{Code: "server_error", Message: "boom"},
	})
	events, err := readEvents(t, post(t, s.URL+"/v1/responses", `{"stream":true}`).Body)
	if err != nil {
		t.Fatal(err)
	}
	last := events[len(events)-1].data
	if last["type"] != "error" || last["code"] != "server_error" || last["message"] != "boom" {
		t.Fatalf("last event = %v", last)
	}
}

func TestBrokenStreamEndsWithoutAFinalEvent(t *testing.T) {
	s := openaitest.New(t, openaitest.Reply{Output: []openaitest.Item{openaitest.Text{Text: "partial answer"}}, Break: true})
	events, err := readEvents(t, post(t, s.URL+"/v1/responses", `{"stream":true}`).Body)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("stream ended with %v, want an unexpected EOF", err)
	}
	for _, e := range events {
		if strings.HasPrefix(e.data["type"].(string), "response.completed") {
			t.Fatal("a broken stream sent response.completed")
		}
	}
}

func TestHTTPErrorsAreInOpenAIsShape(t *testing.T) {
	s := openaitest.New(t,
		openaitest.Fail(http.StatusServiceUnavailable, "server_error", "server_is_overloaded", "Overloaded"),
		openaitest.RateLimited("rate_limit_exceeded", "6"),
		openaitest.RateLimited("insufficient_quota", ""),
	)
	cases := []struct {
		status     int
		code       string
		retryAfter string
	}{
		{503, "server_is_overloaded", ""},
		{429, "rate_limit_exceeded", "6"},
		{429, "insufficient_quota", ""},
	}
	for _, c := range cases {
		resp := post(t, s.URL+"/v1/responses", `{"stream":true}`)
		if resp.StatusCode != c.status {
			t.Errorf("%s: status %d, want %d", c.code, resp.StatusCode, c.status)
		}
		if got := resp.Header.Get("Retry-After"); got != c.retryAfter {
			t.Errorf("%s: Retry-After %q, want %q", c.code, got, c.retryAfter)
		}
		var body struct {
			Error struct {
				Message, Type string
				Code          *string
				Param         any
			}
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.Error.Code == nil || *body.Error.Code != c.code || body.Error.Message == "" || body.Error.Type == "" {
			t.Errorf("%s: error body = %+v", c.code, body.Error)
		}
	}
}

func TestRateLimitHeadersResetAsDurations(t *testing.T) {
	s := openaitest.New(t, openaitest.Answer(openaitest.Text{Text: "ok"}))
	resp := post(t, s.URL+"/v1/responses", `{}`)
	for name, want := range map[string]string{
		"X-Ratelimit-Reset-Requests": "1s",
		"X-Ratelimit-Reset-Tokens":   "6m0s",
		"X-Ratelimit-Limit-Tokens":   "150000",
	} {
		if got := resp.Header.Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}

func TestInputTokensAnswersTheConfiguredCount(t *testing.T) {
	s := openaitest.New(t)
	s.InputTokens = 4321
	var body map[string]any
	if err := json.NewDecoder(post(t, s.URL+"/v1/responses/input_tokens", `{"input":"hi"}`).Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["object"] != "response.input_tokens" || body["input_tokens"] != 4321.0 {
		t.Fatalf("body = %v", body)
	}
}

func TestWrongKeyIsRefusedWithA401(t *testing.T) {
	s := openaitest.New(t, openaitest.Answer(openaitest.Text{Text: "never sent"}))
	s.Key = "sk-test-right"
	req, _ := http.NewRequest(http.MethodPost, s.URL+"/v1/responses", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer sk-test-wrong")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", resp.StatusCode)
	}

	req, _ = http.NewRequest(http.MethodPost, s.URL+"/v1/responses", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer sk-test-right")
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp2.Body.Close() }()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("with the right key: status %d", resp2.StatusCode)
	}
}

func TestRequestsAreRecorded(t *testing.T) {
	s := openaitest.New(t, openaitest.Answer(openaitest.Text{Text: "ok"}))
	req, _ := http.NewRequest(http.MethodPost, s.URL+"/v1/responses", strings.NewReader(`{"model":"gpt-6-luna","store":false}`))
	req.Header.Set("Authorization", "Bearer sk-test")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	got := s.Requests()
	if len(got) != 1 || got[0].Path != "/v1/responses" || got[0].Header.Get("Authorization") != "Bearer sk-test" {
		t.Fatalf("requests = %+v", got)
	}
	if body := got[0].JSON(t); body["store"] != false {
		t.Fatalf("recorded body = %v", body)
	}
}

// failRecorder stands in for a test, to see whether the fake fails it.
type failRecorder struct {
	testing.TB
	failed bool
}

func (f *failRecorder) Errorf(string, ...any) { f.failed = true }

func TestUnscriptedRequestFailsTheTest(t *testing.T) {
	rec := &failRecorder{TB: t}
	s := openaitest.New(rec)
	resp := post(t, s.URL+"/v1/responses", `{}`)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status %d, want 500", resp.StatusCode)
	}
	if !rec.failed {
		t.Fatal("an unscripted request did not fail the test")
	}
}
