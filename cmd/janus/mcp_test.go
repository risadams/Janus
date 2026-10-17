package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/risadams/janus/internal/digest"
	"github.com/risadams/janus/internal/ledger"
	"github.com/risadams/janus/internal/worker"
)

// mcpFixture makes a config pointing at a fake LM Studio, and a project
// directory with one file in it.
func mcpFixture(t *testing.T) (cfgPath, project string) {
	t.Helper()
	lms := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v0/models" {
			io.WriteString(w, lmStudioModels)
			return
		}
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"type":"message","model":"qwen/qwen3.8-27b","stop_reason":"end_turn","content":[{"type":"text","text":"Two lines (a.txt:1, a.txt:2)."}],"usage":{"input_tokens":120,"output_tokens":12}}`)
	}))
	t.Cleanup(lms.Close)

	home := t.TempDir()
	cfg, _ := json.Marshal(map[string]any{
		"local":   map[string]any{"baseUrl": lms.URL, "models": map[string]any{"janus-local": map[string]any{"model": "qwen/qwen3.8-27b"}}},
		"logging": map[string]any{"dir": "logs"},
	})
	cfgPath = filepath.Join(home, "janus.config.json")
	os.WriteFile(cfgPath, cfg, 0o600)

	project = t.TempDir()
	os.WriteFile(filepath.Join(project, "a.txt"), []byte("alpha\nbeta\n"), 0o644)
	return cfgPath, project
}

func TestMCPServesAskFilesEndToEnd(t *testing.T) {
	cfgPath, project := mcpFixture(t)
	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"ask_files","arguments":{"paths":["a.txt"],"question":"How many lines?"}}}`,
	}, "\n") + "\n"

	var stdout, stderr bytes.Buffer
	if code := runMCP([]string{"-config", cfgPath, "-root", project}, strings.NewReader(in), &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d\n%s", code, stderr.String())
	}

	// Standard output is the protocol and nothing else: every line is one
	// JSON-RPC message.
	replies := map[string]map[string]any{}
	sc := bufio.NewScanner(&stdout)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<20)
	for sc.Scan() {
		var msg struct {
			JSONRPC string
			ID      json.RawMessage
			Result  map[string]any
		}
		if err := json.Unmarshal(sc.Bytes(), &msg); err != nil || msg.JSONRPC != "2.0" {
			t.Fatalf("a line on stdout is not a JSON-RPC message: %q", sc.Text())
		}
		replies[string(msg.ID)] = msg.Result
	}
	if len(replies) != 3 {
		t.Fatalf("%d replies, want 3 (the notification gets none)", len(replies))
	}
	if info := replies["1"]["serverInfo"].(map[string]any); info["name"] != "janus" {
		t.Errorf("serverInfo = %+v", info)
	}
	if tools := replies["2"]["tools"].([]any); len(tools) != 2 || tools[0].(map[string]any)["name"] != "ask_files" || tools[1].(map[string]any)["name"] != "tool_output" {
		t.Errorf("tools = %+v", replies["2"]["tools"])
	}
	text := replies["3"]["content"].([]any)[0].(map[string]any)["text"].(string)
	if replies["3"]["isError"] != false || !strings.HasPrefix(text, "Two lines (a.txt:1, a.txt:2).") || !strings.Contains(text, "read 1 file, 11 bytes") {
		t.Errorf("tool result = %q, isError %v", text, replies["3"]["isError"])
	}

	// The call is in the ledger beside the config, not in the project.
	files, _ := filepath.Glob(filepath.Join(filepath.Dir(cfgPath), "logs", "ledger", "*.jsonl"))
	if len(files) != 1 {
		t.Fatalf("ledger files beside the config: %v", files)
	}
	data, _ := os.ReadFile(files[0])
	var rec ledger.Record
	if err := json.Unmarshal(bytes.TrimSpace(data), &rec); err != nil {
		t.Fatalf("ledger line: %v\n%s", err, data)
	}
	if rec.Class != "worker" || rec.Path != "ask_files" || rec.Backend != "local" || rec.Outcome != ledger.OutcomeOK || rec.Usage.Input != 120 {
		t.Errorf("record = %+v", rec)
	}
	if w := rec.Worker; w == nil || w.Tool != "ask_files" || w.Files != 1 || w.ReadBytes != 11 || w.ModelCalls != 1 || w.AnswerBytes != len(text) {
		t.Errorf("worker = %+v", rec.Worker)
	}
	if entries, _ := os.ReadDir(project); len(entries) != 1 {
		t.Errorf("janus mcp wrote into the project directory: %v", entries)
	}
	if !strings.Contains(stderr.String(), "serving tools on qwen/qwen3.8-27b (janus-local)") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

// The proxy stores originals under logging.dir; janus mcp, started by
// Claude Code in a project directory, must find them there.
func TestMCPFetchesADigestedOriginal(t *testing.T) {
	cfgPath, project := mcpFixture(t)
	store := digest.NewStore(filepath.Join(filepath.Dir(cfgPath), "logs", "originals"), 7, nil)
	id, err := store.Put("ok  pkg/a\nFAIL pkg/b: undefined: Foo\nok  pkg/c\n")
	if err != nil {
		t.Fatal(err)
	}
	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"tool_output","arguments":{"id":"` + id + `","pattern":"FAIL"}}}`,
	}, "\n") + "\n"
	var stdout, stderr bytes.Buffer
	if code := runMCP([]string{"-config", cfgPath, "-root", project}, strings.NewReader(in), &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d\n%s", code, stderr.String())
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	var reply struct {
		Result struct {
			Content []struct{ Text string }
			IsError bool
		}
	}
	json.Unmarshal([]byte(lines[len(lines)-1]), &reply)
	if reply.Result.IsError || len(reply.Result.Content) != 1 || reply.Result.Content[0].Text != "     2\tFAIL pkg/b: undefined: Foo\n" {
		t.Errorf("reply = %+v", reply.Result)
	}
}

func TestMCPStartupErrors(t *testing.T) {
	cfgPath, project := mcpFixture(t)
	empty := filepath.Join(t.TempDir(), "janus.config.json")
	os.WriteFile(empty, []byte(`{}`), 0o600)

	cases := map[string]struct {
		args     []string
		wantCode int
		wantIn   string
	}{
		"no local model configured":  {[]string{"-config", empty, "-root", project}, 2, "local.models is empty"},
		"a config that is not there": {[]string{"-config", filepath.Join(t.TempDir(), "none.json"), "-root", project}, 2, "local.models is empty"},
		"an unknown model name":      {[]string{"-config", cfgPath, "-local", "other", "-root", project}, 2, `-local "other" is not an entry`},
		"a root that is not there":   {[]string{"-config", cfgPath, "-root", filepath.Join(project, "missing")}, 2, "is not a directory"},
		"a stray argument":           {[]string{"-config", cfgPath, "extra"}, 2, "usage: janus mcp"},
	}
	for name, c := range cases {
		var stdout, stderr bytes.Buffer
		code := runMCP(c.args, strings.NewReader(""), &stdout, &stderr)
		if code != c.wantCode || !strings.Contains(stderr.String(), c.wantIn) {
			t.Errorf("%s: exit %d, stderr %q", name, code, stderr.String())
		}
		if stdout.Len() != 0 {
			t.Errorf("%s: wrote to stdout, which belongs to the protocol: %q", name, stdout.String())
		}
	}
}

func TestWorkerRecord(t *testing.T) {
	start := time.Date(2026, 10, 11, 18, 0, 0, 0, time.UTC)
	ok := workerRecord(worker.Call{Tool: "ask_files", Start: start, Duration: 12300 * time.Millisecond, Files: 3, BytesRead: 84000, ModelCalls: 1, AnswerBytes: 900}, "qwen/qwen3.8-27b")
	// 84,000 bytes at four a token is 21,000; the 900-byte answer at three
	// is 300.
	if ok.Outcome != ledger.OutcomeOK || ok.DurationMs != 12300 || ok.Worker.AvoidedTokens != 20700 || ok.Response.Model != "qwen/qwen3.8-27b" {
		t.Errorf("record = %+v worker %+v", ok, ok.Worker)
	}
	failed := workerRecord(worker.Call{Tool: "ask_files", Start: start, Failed: true, Why: "The local model could not answer", BytesRead: 84000}, "q")
	if failed.Outcome != ledger.OutcomeToolError || failed.Worker.AvoidedTokens != 0 || !strings.Contains(failed.Reason, "could not answer") {
		t.Errorf("failed record = %+v worker %+v", failed, failed.Worker)
	}
	if ok.ID == failed.ID {
		t.Error("two calls got the same id")
	}
}
