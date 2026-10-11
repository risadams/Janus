package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const claudeReadStream = "event: message_start\n" +
	`data: {"type":"message_start","message":{"id":"m","model":"claude-opus-5-5","usage":{"input_tokens":2}}}` + "\n\n" +
	`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","name":"Read","id":"t","input":{}}}` + "\n\n" +
	`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":9}}` + "\n\n" +
	`data: {"type":"message_stop"}` + "\n\n"

const claudeDoneJSON = `{"type":"message","model":"claude-opus-5-5","content":[{"type":"text","text":"Done."}],"stop_reason":"end_turn","usage":{"input_tokens":2,"output_tokens":3}}`

// replayFixture lays out a captures directory and a fake LM Studio that
// always answers with one Read call, and returns the config path and the
// directory.
func replayFixture(t *testing.T, lmsMessages http.HandlerFunc) (cfgPath, captures string, sent *[]string) {
	t.Helper()
	sent = &[]string{}
	lms := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v0/models" {
			io.WriteString(w, lmStudioModels)
			return
		}
		body, _ := io.ReadAll(r.Body)
		*sent = append(*sent, string(body))
		if lmsMessages != nil {
			lmsMessages(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, strings.Replace(claudeReadStream, "claude-opus-5-5", "qwen/qwen3.8-27b", 1))
	}))
	t.Cleanup(lms.Close)

	root := t.TempDir()
	cfg, _ := json.Marshal(map[string]any{
		"local": map[string]any{"baseUrl": lms.URL, "models": map[string]any{"janus-local": map[string]any{"model": "qwen/qwen3.8-27b"}}},
	})
	cfgPath = filepath.Join(root, "janus.config.json")
	os.WriteFile(cfgPath, cfg, 0o600)

	captures = filepath.Join(root, "captures")
	day := filepath.Join(captures, "2026-10-11")
	os.MkdirAll(day, 0o700)
	put := func(name, content string) { os.WriteFile(filepath.Join(day, name), []byte(content), 0o600) }
	// A subagent turn where Claude called Read; a main turn where Claude
	// finished; a turn with no saved answer.
	put("a1_v1-messages.request.txt", subagentBody)
	put("a1_v1-messages.response.txt", claudeReadStream)
	put("b2_v1-messages.request.txt", mainBody)
	put("b2_v1-messages.response.txt", claudeDoneJSON)
	put("c3_v1-messages.request.txt", mainBody)
	// Files that are not requests to replay.
	put("a1_v1-messages.shadow.request.txt", subagentBody)
	put("a1_v1-messages.md", "# render")
	put("d4_v1-messages-count_tokens.request.txt", mainBody)
	put("e5_api-hello.request.txt", "")
	return cfgPath, captures, sent
}

func replay(t *testing.T, cfgPath string, args ...string) (string, int) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(append([]string{"replay", "-config", cfgPath}, args...), &stdout, &stderr)
	return stdout.String() + stderr.String(), code
}

func TestReplayComparesEachCaptureWithClaudesAnswer(t *testing.T) {
	cfgPath, captures, sent := replayFixture(t, nil)
	out, code := replay(t, cfgPath, captures)
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	wantLines(t, out,
		"Replaying on qwen/qwen3.8-27b (janus-local)",
		"a1 subagent",
		"same: local tool_use (Read); Claude tool_use (Read)",
		"b2 main",
		"differs: local tool_use (Read); Claude end_turn",
		"c3 main",
		"local tool_use (Read) (no saved answer from Claude to compare with)",
		"class replayed skipped failed same differs not compared local time",
		// Of the two main turns, one differed and one had nothing to
		// compare with; the subagent turn matched.
		"main 2 0 0 0 1 1",
		"subagent 1 0 0 1 0 0",
	)

	// Only the three Messages requests were sent, with the local model's
	// name and nothing else changed.
	if len(*sent) != 3 {
		t.Fatalf("%d requests reached LM Studio, want 3", len(*sent))
	}
	for _, body := range *sent {
		if !strings.Contains(body, `"model":"qwen/qwen3.8-27b"`) {
			t.Errorf("LM Studio was sent %.80s", body)
		}
	}

	day := filepath.Join(captures, "2026-10-11")
	for _, id := range []string{"a1", "b2", "c3"} {
		for _, ext := range []string{".replay.md", ".replay.response.txt"} {
			path := filepath.Join(day, id+"_v1-messages"+ext)
			info, err := os.Stat(path)
			if err != nil || info.Size() == 0 || info.Mode().Perm() != 0o600 {
				t.Errorf("%s: %v", filepath.Base(path), err)
			}
		}
	}
	if _, err := os.Stat(filepath.Join(day, "d4_v1-messages-count_tokens.replay.md")); err == nil {
		t.Error("a count_tokens capture was replayed")
	}
}

func TestReplayFilters(t *testing.T) {
	cfgPath, captures, sent := replayFixture(t, nil)

	out, _ := replay(t, cfgPath, "-class", "subagent", captures)
	if len(*sent) != 1 || !strings.Contains(out, "a1") || strings.Contains(out, "b2") {
		t.Errorf("-class subagent sent %d requests:\n%s", len(*sent), out)
	}

	*sent = nil
	out, _ = replay(t, cfgPath, "-limit", "2", captures)
	if len(*sent) != 2 || strings.Contains(out, "c3") {
		t.Errorf("-limit 2 sent %d requests:\n%s", len(*sent), out)
	}

	// A single file can be named directly.
	*sent = nil
	out, _ = replay(t, cfgPath, filepath.Join(captures, "2026-10-11", "b2_v1-messages.request.txt"))
	if len(*sent) != 1 || !strings.Contains(out, "b2") {
		t.Errorf("naming one file sent %d requests:\n%s", len(*sent), out)
	}

	*sent = nil
	out, code := replay(t, cfgPath, "-class", "unknown", captures)
	if code != 0 || len(*sent) != 0 || !strings.Contains(out, "No requests matched.") {
		t.Errorf("a class with no captures: exit %d, sent %d\n%s", code, len(*sent), out)
	}
}

func TestReplayReportsWhatGoesWrong(t *testing.T) {
	// An empty answer and an error from the local model.
	cfgPath, captures, _ := replayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"type":"message","content":[],"stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":14}}`)
	})
	out, code := replay(t, cfgPath, "-limit", "1", captures)
	if code != 0 {
		t.Errorf("exit %d", code)
	}
	wantLines(t, out, "a1 subagent empty answer after", "subagent 1 0 1 0 0 0 -")

	cfgPath, captures, _ = replayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"no"}}`)
	})
	out, _ = replay(t, cfgPath, "-limit", "1", captures)
	wantLines(t, out, "a1 subagent api_error after", "subagent 1 0 1 0 0 0 -")
}

func TestReplayWhenLMStudioIsDown(t *testing.T) {
	_, captures, _ := replayFixture(t, nil)
	cfg, _ := json.Marshal(map[string]any{
		"local": map[string]any{"baseUrl": "http://" + freeAddr(t), "models": map[string]any{"janus-local": map[string]any{"model": "qwen/qwen3.8-27b"}}},
	})
	cfgPath := filepath.Join(t.TempDir(), "janus.config.json")
	os.WriteFile(cfgPath, cfg, 0o600)

	out, code := replay(t, cfgPath, captures)
	if code != 0 {
		t.Errorf("exit %d", code)
	}
	wantLines(t, out, "a1 subagent skipped: could not reach LM Studio", "subagent 1 1 0 0 0 0 -", "main 2 2 0 0 0 0 -")
}

func TestReplayNeedsALocalModel(t *testing.T) {
	_, captures, _ := replayFixture(t, nil)
	write := func(models map[string]any) string {
		cfg, _ := json.Marshal(map[string]any{"local": map[string]any{"models": models}})
		path := filepath.Join(t.TempDir(), "janus.config.json")
		os.WriteFile(path, cfg, 0o600)
		return path
	}
	two := map[string]any{"small": map[string]any{"model": "a"}, "large": map[string]any{"model": "b"}}

	cases := map[string]struct {
		cfg  string
		args []string
		want string
	}{
		"none configured":      {write(map[string]any{}), nil, "local.models is empty"},
		"several, none chosen": {write(two), nil, "choose one with -local: large, small"},
		"unknown name":         {write(two), []string{"-local", "medium"}, `-local "medium" is not an entry of local.models`},
	}
	for name, c := range cases {
		out, code := replay(t, c.cfg, append(c.args, captures)...)
		if code != 2 || !strings.Contains(out, c.want) {
			t.Errorf("%s: exit %d, want 2 and %q in:\n%s", name, code, c.want, out)
		}
	}
}

func TestReplayBadArguments(t *testing.T) {
	cfgPath, captures, _ := replayFixture(t, nil)
	if out, code := replay(t, cfgPath); code != 2 || !strings.Contains(out, "usage: janus replay") {
		t.Errorf("no paths: exit %d\n%s", code, out)
	}
	if out, code := replay(t, cfgPath, "-class", "sidekick", captures); code != 2 || !strings.Contains(out, `-class "sidekick"`) {
		t.Errorf("bad class: exit %d\n%s", code, out)
	}
	if _, code := replay(t, cfgPath, filepath.Join(captures, "missing")); code != 1 {
		t.Errorf("missing path: exit %d, want 1", code)
	}
	empty := t.TempDir()
	if out, code := replay(t, cfgPath, empty); code != 0 || !strings.Contains(out, "No captured Messages requests found") {
		t.Errorf("empty directory: exit %d\n%s", code, out)
	}
}

func TestSummariseTellsStreamsFromJSON(t *testing.T) {
	if s := summarise([]byte(claudeReadStream)); !s.Complete || s.StopReason != "tool_use" || strings.Join(s.Tools, ",") != "Read" {
		t.Errorf("stream: %+v", s)
	}
	if s := summarise([]byte(claudeDoneJSON)); !s.Complete || s.StopReason != "end_turn" || len(s.Tools) != 0 {
		t.Errorf("json: %+v", s)
	}
	if s := summarise([]byte("\n\ndata: {\"type\":\"message_stop\"}\n\n")); !s.Complete {
		t.Errorf("a stream with leading blank lines was not recognised: %+v", s)
	}
}
