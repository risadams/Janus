package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/risadams/janus/internal/anthropic"
	"github.com/risadams/janus/internal/backend"
	"github.com/risadams/janus/internal/capture"
	"github.com/risadams/janus/internal/config"
	"github.com/risadams/janus/internal/guard"
	"github.com/risadams/janus/internal/ledger"
	"github.com/risadams/janus/internal/openai"
	"github.com/risadams/janus/internal/openai/openaitest"
	"github.com/risadams/janus/internal/proxy"
)

// The OpenAI key goes to OpenAI and nowhere else: not into a capture, a
// ledger record, or a console line, whatever OpenAI answers (#371, #426).
func TestTheOpenAIKeyIsFoundInNoCaptureLedgerRecordOrLogLine(t *testing.T) {
	// Made up, and distinctive enough that finding it can only mean a leak.
	key := strings.Join([]string{"sk-proj-", "janusLeakCheck", "7d1f0c9e2b"}, "")
	t.Setenv("JANUS_TEST_OPENAI_KEY", key)
	cfg := config.OpenAI{Key: "env:JANUS_TEST_OPENAI_KEY"}
	loaded, err := openai.LoadKey(cfg, os.LookupEnv)
	if err != nil {
		t.Fatal(err)
	}

	srv := openaitest.New(t,
		openaitest.Answer(openaitest.Text{Text: "Hello."}),
		// OpenAI's 401 quotes part of the key; this one quotes all of it.
		openaitest.Fail(http.StatusUnauthorized, "invalid_request_error", "invalid_api_key", "Incorrect API key provided: "+key+"."),
		openaitest.Fail(http.StatusBadRequest, "invalid_request_error", "", "Bad request from "+key+"."),
		openaitest.Reply{Output: []openaitest.Item{openaitest.Text{Text: "Hel"}}, StreamError: &openaitest.StreamError{Code: "server_error", Message: "Failed for " + key + "."}},
	)
	cfg.BaseURL = srv.URL
	b, err := openai.NewBackend(cfg, loaded, nil)
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	var console bytes.Buffer
	led := ledger.Open(filepath.Join(dir, "ledger"))
	captures := capture.New(filepath.Join(dir, "captures"), 7, 1024)
	detector := &guard.Detector{}
	unused, _ := url.Parse("http://127.0.0.1:1")
	done := make(chan struct{}, 1)
	janus := httptest.NewServer(proxy.New(proxy.Options{
		Upstream: backend.NewUpstream(unused, nil),
		Redactor: guard.NewRedactor(detector),
		Plan: func(ctx context.Context, header http.Header, req *anthropic.Request) (proxy.Plan, *proxy.Refusal) {
			tr, err := b.Translation(header, req)
			if err != nil {
				t.Fatal(err)
			}
			return proxy.Plan{Backend: b, Route: "openai", Reason: "test", BackendModel: tr.Choice.Model, ResponseModel: req.Model(), Translate: tr.Translate}, nil
		},
		KeepBodies: true,
		Done: func(ex proxy.Exchange) {
			record(ex, led, captures, detector, testConsole(&console))
			done <- struct{}{}
		},
	}))
	defer janus.Close()

	var answers bytes.Buffer
	for _, stream := range []string{"true", "false", "true", "true"} {
		req, _ := http.NewRequest(http.MethodPost, janus.URL+"/v1/messages", strings.NewReader(
			`{"model":"claude-sonnet-5-5","max_tokens":100,"stream":`+stream+`,"messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Api-Key", "sk-ant-api03-claude-code")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(&answers, resp.Body)
		resp.Body.Close()
		<-done
	}

	for i, r := range srv.Requests() {
		if r.Header.Get("Authorization") != "Bearer "+key {
			t.Errorf("request %d did not carry the key to OpenAI", i+1)
		}
	}
	if bytes.Contains(answers.Bytes(), []byte(key)) {
		t.Error("the key reached Claude Code in an answer")
	}
	if bytes.Contains(console.Bytes(), []byte(key)) {
		t.Errorf("the key is in a console line:\n%s", console.String())
	}
	files := 0
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		files++
		data, _ := os.ReadFile(path) // #nosec G304 -- a file this test wrote
		if bytes.Contains(data, []byte(key)) {
			t.Errorf("the key is in %s", strings.TrimPrefix(path, dir))
		}
		return nil
	})
	if files < 5 {
		t.Errorf("found %d files; want a ledger and four captures to search", files)
	}
}

// What the ledger keeps of a translation: the fields OpenAI could not be
// sent, and the reasoning tokens OpenAI reports (#381).
func TestTheLedgerRecordsWhatWasDroppedAndWhatWasSpentReasoning(t *testing.T) {
	for _, stream := range []bool{true, false} {
		srv := openaitest.New(t, openaitest.Reply{Output: []openaitest.Item{openaitest.Text{Text: "Hi."}},
			Usage: openaitest.Usage{Input: 100, Output: 40, Reasoning: 30}})
		cfg := config.Default()
		cfg.OpenAI = config.OpenAI{BaseURL: srv.URL, Key: "env:OPENAI_API_KEY"}
		cfg.Features.Route = true
		cfg.Routing = []config.Rule{{Fallback: []string{"openai"}}}
		key, _ := openai.LoadKey(cfg.OpenAI, func(string) (string, bool) { return "janus-test-key", true })
		oa, err := openai.NewBackend(cfg.OpenAI, key, nil)
		if err != nil {
			t.Fatal(err)
		}
		unused, _ := url.Parse("http://127.0.0.1:1")
		router := newRouter(cfg, backend.NewUpstream(unused, nil), backend.NewLocal(unused, nil), oa)
		reported := make(chan proxy.Exchange, 1)
		janus := httptest.NewServer(proxy.New(proxy.Options{Upstream: router.Upstream, Plan: router.Plan, Done: func(ex proxy.Exchange) { reported <- ex }}))
		body := fmt.Sprintf(`{"model":"claude-sonnet-5-5","max_tokens":1000,"stream":%v,"stop_sequences":["END"],"temperature":0.2,"messages":[{"role":"user","content":"Say hi."}]}`, stream)
		resp, err := http.Post(janus.URL+"/v1/messages", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		rec := ledger.FromExchange(<-reported)
		janus.Close()
		if rec.Backend != "openai" || rec.Request == nil || strings.Join(rec.Request.Dropped, ",") != "stop_sequences,temperature" {
			t.Errorf("stream=%v: backend %s, request %+v", stream, rec.Backend, rec.Request)
		}
		if rec.Usage == nil || rec.Usage.Thinking == nil || *rec.Usage.Thinking != 30 || rec.Usage.Output != 40 {
			t.Errorf("stream=%v: usage %+v", stream, rec.Usage)
		}
		if usd, unpriced := ledger.DefaultPrices.Cost(rec.Response.Model, *rec.Usage); unpriced != "" || usd <= 0 {
			t.Errorf("stream=%v: the answer from %s is unpriced: %q", stream, rec.Response.Model, unpriced)
		}
	}
}

// An answer that stopped at the output cap with only reasoning in it is
// recorded as such, not as an ordinary max_tokens stop (#452).
func TestTheLedgerSaysWhenReasoningUsedTheWholeCap(t *testing.T) {
	for _, stream := range []bool{true, false} {
		srv := openaitest.New(t,
			openaitest.Reply{Output: []openaitest.Item{openaitest.Reasoning{Encrypted: "gAAAA"}}, Status: "incomplete", Reason: "max_output_tokens",
				Usage: openaitest.Usage{Input: 100, Output: 64, Reasoning: 64}},
			openaitest.Reply{Output: []openaitest.Item{openaitest.Text{Text: "cut off mid"}}, Status: "incomplete", Reason: "max_output_tokens",
				Usage: openaitest.Usage{Input: 100, Output: 64, Reasoning: 10}})
		cfg := config.Default()
		cfg.OpenAI = config.OpenAI{BaseURL: srv.URL, Key: "env:OPENAI_API_KEY"}
		cfg.Features.Route = true
		cfg.Routing = []config.Rule{{Fallback: []string{"openai"}}}
		key, _ := openai.LoadKey(cfg.OpenAI, func(string) (string, bool) { return "janus-test-key", true })
		oa, err := openai.NewBackend(cfg.OpenAI, key, nil)
		if err != nil {
			t.Fatal(err)
		}
		unused, _ := url.Parse("http://127.0.0.1:1")
		router := newRouter(cfg, backend.NewUpstream(unused, nil), backend.NewLocal(unused, nil), oa)
		reported := make(chan proxy.Exchange, 1)
		janus := httptest.NewServer(proxy.New(proxy.Options{Upstream: router.Upstream, Plan: router.Plan, Done: func(ex proxy.Exchange) { reported <- ex }}))
		for i, wantFlag := range []bool{true, false} {
			body := fmt.Sprintf(`{"model":"claude-sonnet-5-5","max_tokens":64,"stream":%v,"messages":[{"role":"user","content":"Is it safe?"}]}`, stream)
			resp, err := http.Post(janus.URL+"/v1/messages", "application/json", strings.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			rec := ledger.FromExchange(<-reported)
			if rec.Response == nil || rec.Response.StopReason != "max_tokens" || rec.Response.ReasoningUsedTheCap != wantFlag {
				t.Errorf("stream=%v answer %d: response %+v, want reasoning_used_the_cap %v", stream, i+1, rec.Response, wantFlag)
			}
		}
		if sent := string(srv.Requests()[0].Body); !strings.Contains(sent, `"effort":"low"`) {
			t.Errorf("stream=%v: a 64-token request did not go with the lowest effort: %s", stream, sent)
		}
		janus.Close()
	}
}
