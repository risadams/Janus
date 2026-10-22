// Package openaitest is a fake OpenAI Responses API for the offline test
// suite, as the fake upstream and the fake LM Studio are for Anthropic and
// LM Studio (#369).
//
// There is no OpenAI account to record real traffic from (charter D7), so
// every shape here is built by hand from OpenAI's published reference, as
// docs/research/openai.md describes it: the streaming events and their
// order, reasoning items whose encrypted_content is complete only when the
// item is done, obfuscation padding on deltas, sequence numbers, errors in
// OpenAI's shape, and rate-limit headers whose reset values are durations.
// Where the docs are silent, as on whether a stream carries SSE "event:"
// lines, the fake can do it either way, so a test can show Janus does not
// depend on it.
package openaitest

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
)

// Server is a fake OpenAI API. Each request to /v1/responses takes the
// next scripted Reply; /v1/responses/input_tokens answers InputTokens.
// Every request is recorded, so a test can check what Janus sent.
type Server struct {
	*httptest.Server

	// Key, if set, is the API key every request must carry as
	// "Authorization: Bearer <key>"; any other is refused with OpenAI's
	// 401. Set it before the first request.
	Key string
	// OmitEventLines leaves the SSE "event:" line out of each streamed
	// event, so only the type inside its data says what it is. OpenAI's
	// reference does not show which it sends (openai.md, Response stream).
	OmitEventLines bool
	// InputTokens is the count /v1/responses/input_tokens answers.
	InputTokens int
	// Forget keeps no request after answering it, for a long run, such
	// as a soak, that would otherwise hold every body in memory.
	Forget bool

	t        testing.TB
	mu       sync.Mutex
	script   []Reply
	requests []Request
	seen     int
}

// A Request is one request the fake received.
type Request struct {
	Method string
	Path   string
	Header http.Header
	Body   []byte
}

// JSON decodes the request's body, failing the test if it is not JSON.
func (r Request) JSON(t testing.TB) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal(r.Body, &v); err != nil {
		t.Fatalf("openaitest: request to %s is not JSON: %v", r.Path, err)
	}
	return v
}

// New starts a fake that answers with replies, in order, and stops it when
// the test ends. A request with no reply left fails the test.
func New(t testing.TB, replies ...Reply) *Server {
	t.Helper()
	s := &Server{t: t, script: append([]Reply(nil), replies...)}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/responses", s.responses)
	mux.HandleFunc("POST /v1/responses/input_tokens", s.inputTokens)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		s.record(r)
		writeError(w, Error(http.StatusNotFound, "invalid_request_error", "unknown_url",
			fmt.Sprintf("Invalid URL (%s %s)", r.Method, r.URL.Path)))
	})
	s.Server = httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

// Script adds replies after those already scripted.
func (s *Server) Script(replies ...Reply) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.script = append(s.script, replies...)
}

// Requests returns every request received so far, in order.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.requests...)
}

// record keeps the request and returns its number, counting from 1.
func (s *Server) record(r *http.Request) (Request, int) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		s.t.Errorf("openaitest: reading request body: %v", err)
	}
	req := Request{Method: r.Method, Path: r.URL.Path, Header: r.Header.Clone(), Body: body}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seen++
	if !s.Forget {
		s.requests = append(s.requests, req)
	}
	return req, s.seen
}

// authorized answers OpenAI's 401 and returns false if Key is set and the
// request does not carry it.
func (s *Server) authorized(w http.ResponseWriter, r *http.Request) bool {
	if s.Key == "" || r.Header.Get("Authorization") == "Bearer "+s.Key {
		return true
	}
	writeError(w, Error(http.StatusUnauthorized, "invalid_request_error", "invalid_api_key",
		"Incorrect API key provided. You can find your API key at https://platform.openai.com/account/api-keys."))
	return false
}

func (s *Server) inputTokens(w http.ResponseWriter, r *http.Request) {
	s.record(r)
	if !s.authorized(w, r) {
		return
	}
	writeJSON(w, http.StatusOK, obj{"object": "response.input_tokens", "input_tokens": s.InputTokens})
}

func (s *Server) responses(w http.ResponseWriter, r *http.Request) {
	req, n := s.record(r)
	if !s.authorized(w, r) {
		return
	}
	s.mu.Lock()
	if len(s.script) == 0 {
		s.mu.Unlock()
		s.t.Errorf("openaitest: request %d to /v1/responses has no reply scripted", n)
		writeError(w, Error(http.StatusInternalServerError, "server_error", "", "openaitest: no reply scripted"))
		return
	}
	reply := s.script[0]
	s.script = s.script[1:]
	s.mu.Unlock()

	var params struct {
		Model      string `json:"model"`
		Stream     bool   `json:"stream"`
		Store      *bool  `json:"store"`
		Truncation string `json:"truncation"`
	}
	if err := json.Unmarshal(req.Body, &params); err != nil {
		writeError(w, Error(http.StatusBadRequest, "invalid_request_error", "",
			"We could not parse the JSON body of your request."))
		return
	}

	for name, values := range reply.Header {
		w.Header()[name] = values
	}
	if reply.HTTP != nil {
		writeError(w, *reply.HTTP)
		return
	}
	for name, value := range defaultLimits {
		if w.Header().Get(name) == "" {
			w.Header().Set(name, value)
		}
	}

	resp := newResponse(n, params.Model, params.Store, params.Truncation)
	if !params.Stream {
		resp.finish(reply)
		writeJSON(w, http.StatusOK, resp.object())
		return
	}
	s.stream(w, resp, reply)
}

// defaultLimits are the rate-limit headers on every answer that does not
// set its own: the values in OpenAI's rate-limit guide, whose resets are
// durations from now, not times.
var defaultLimits = map[string]string{
	"X-Ratelimit-Limit-Requests":     "60",
	"X-Ratelimit-Limit-Tokens":       "150000",
	"X-Ratelimit-Remaining-Requests": "59",
	"X-Ratelimit-Remaining-Tokens":   "149984",
	"X-Ratelimit-Reset-Requests":     "1s",
	"X-Ratelimit-Reset-Tokens":       "6m0s",
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v) // a client that hung up gets nothing either way
}

func writeError(w http.ResponseWriter, e HTTPError) {
	writeJSON(w, e.Status, obj{"error": obj{
		"message": e.Message,
		"type":    e.Type,
		"param":   nil,
		"code":    nullable(e.Code),
	}})
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// obj is a JSON object, as the fake writes it.
type obj = map[string]any

// itoa is strconv.Itoa, shortened for building ids.
func itoa(n int) string { return strconv.Itoa(n) }
