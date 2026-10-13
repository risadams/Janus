package transform

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/risadams/janus/internal/anthropic"
)

// shorten replaces any tool result over a size with a note, and numbers
// each note: asked twice about one block it answers differently, as a
// local model would. Only the memo can keep its output stable.
type shorten struct {
	over  int
	calls int
}

func (s *shorten) Name() string { return "shorten" }

func (s *shorten) Rewrite(at Position, b anthropic.Block) (anthropic.Block, bool) {
	if b.Type != "tool_result" || len(b.Raw()) <= s.over {
		return b, false
	}
	s.calls++
	note, _ := json.Marshal(fmt.Sprintf("[%d bytes, shortened; note %d]", len(b.Raw()), s.calls))
	return b.With("content", note), true
}

// clip cuts text tool results to 20 characters, to show transforms
// running in order, each on what the one before made.
type clip struct{}

func (clip) Name() string { return "clip" }

func (clip) Rewrite(at Position, b anthropic.Block) (anthropic.Block, bool) {
	if b.Type != "tool_result" {
		return b, false
	}
	var content string
	if raw, ok := b.Field("content"); !ok || json.Unmarshal(raw, &content) != nil || len(content) <= 20 {
		return b, false
	}
	short, _ := json.Marshal(content[:20])
	return b.With("content", short), true
}

func claudeCode(session, agent string) http.Header {
	h := http.Header{}
	h.Set("X-Claude-Code-Session-Id", session)
	if agent != "" {
		h.Set("X-Claude-Code-Agent-Id", agent)
	}
	return h
}

// conversation builds the messages of a Claude Code tool loop: a typed
// question, then assistant turns of signed thinking and a tool call, each
// answered by a tool result. Results are random in size, and some repeat
// an earlier one exactly, as rereading a file does.
func conversation(rng *rand.Rand, turns int) []json.RawMessage {
	messages := []json.RawMessage{json.RawMessage(`{"role":"user","content":"fix the failing test"}`)}
	var results []string
	for i := range turns {
		messages = append(messages, json.RawMessage(fmt.Sprintf(
			`{"role":"assistant","content":[{"type":"thinking","thinking":"","signature":"sig%d"},{"type":"tool_use","id":"toolu_%d","name":"Bash","input":{"command":"step %d"}}]}`, i, i, i)))
		result := strings.Repeat(fmt.Sprintf("line %d of output\n", i), rng.Intn(40))
		if len(results) > 0 && rng.Intn(4) == 0 {
			result = results[rng.Intn(len(results))]
		}
		results = append(results, result)
		text, _ := json.Marshal(result)
		messages = append(messages, json.RawMessage(fmt.Sprintf(
			`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_%d","content":%s}]}`, i, text)))
	}
	return messages
}

// request is the first n messages as Claude Code sends them, with the
// moving breakpoint on the last block of the last message.
func request(t *testing.T, messages []json.RawMessage, n int) *anthropic.Request {
	t.Helper()
	sent := append([]json.RawMessage(nil), messages[:n]...)
	last := sent[n-1]
	if i := bytes.LastIndex(last, []byte(`}]}`)); i >= 0 {
		last = append(append(append([]byte(nil), last[:i]...), `,"cache_control":{"type":"ephemeral","ttl":"1h"}`...), last[i:]...)
	}
	sent[n-1] = last
	body, _ := json.Marshal(map[string]any{"model": "claude-opus-5-5", "max_tokens": 32000, "messages": sent})
	req, err := anthropic.ParseRequest(body)
	if err != nil {
		t.Fatalf("request of %d messages: %v", n, err)
	}
	return req
}

// bare is each message as the API compares it for caching: without the
// breakpoint, which moves on every turn.
func bare(t *testing.T, req *anthropic.Request) []string {
	t.Helper()
	var out []string
	for _, m := range req.Messages() {
		blocks := m.Blocks()
		stripped := make([]anthropic.Block, len(blocks))
		for i, b := range blocks {
			stripped[i] = b.Without("cache_control")
		}
		out = append(out, string(m.WithBlocks(stripped).Raw()))
	}
	return out
}

func TestGrowingConversationKeepsAStablePrefix(t *testing.T) {
	for seed := int64(1); seed <= 25; seed++ {
		rng := rand.New(rand.NewSource(seed))
		messages := conversation(rng, 30)
		s := &shorten{over: 300}
		p := New(s, clip{})
		header := claudeCode("sess", "")

		var before []string
		shortened := 0
		for n := 1; n <= len(messages); n += 2 {
			req := request(t, messages, n)
			p.Apply(header, req)
			now := bare(t, req)
			for i := range before {
				if now[i] != before[i] {
					t.Fatalf("seed %d, request of %d messages: message %d changed after it was sent\nbefore %s\n   now %s", seed, n, i, before[i], now[i])
				}
			}
			before = now
			shortened = strings.Count(strings.Join(now, ""), " bytes, short")
		}
		if s.calls == 0 || shortened == 0 {
			t.Fatalf("seed %d: nothing was shortened, so the test proves nothing", seed)
		}
	}
}

func TestEachDistinctBlockIsRewrittenOnce(t *testing.T) {
	result := strings.Repeat("x", 500)
	text, _ := json.Marshal(result)
	messages := []json.RawMessage{
		json.RawMessage(`{"role":"user","content":"go"}`),
		json.RawMessage(`{"role":"assistant","content":[{"type":"tool_use","id":"a","name":"Read","input":{}}]}`),
		json.RawMessage(fmt.Sprintf(`{"role":"user","content":[{"type":"tool_result","tool_use_id":"a","content":%s}]}`, text)),
		json.RawMessage(`{"role":"assistant","content":[{"type":"tool_use","id":"b","name":"Read","input":{}}]}`),
		json.RawMessage(fmt.Sprintf(`{"role":"user","content":[{"type":"tool_result","tool_use_id":"a","content":%s}]}`, text)),
	}
	s := &shorten{over: 300}
	p := New(s)
	header := claudeCode("sess", "")
	p.Apply(header, request(t, messages, 1))
	p.Apply(header, request(t, messages, 3))
	req := request(t, messages, 5)
	p.Apply(header, req)

	if s.calls != 1 {
		t.Errorf("the transform was asked %d times about one block, want once", s.calls)
	}
	got := bare(t, req)
	if got[2] != got[4] {
		t.Errorf("the same block was sent in two forms:\n%s\n%s", got[2], got[4])
	}
}

func TestHistoryOfAConversationFirstSeenMidwayIsLeftAlone(t *testing.T) {
	messages := conversation(rand.New(rand.NewSource(7)), 6)
	s := &shorten{over: 0}
	p := New(s)
	header := claudeCode("sess", "")

	req := request(t, messages, 9)
	original := string(req.Bytes())
	if stages := p.Apply(header, req); stages != nil || string(req.Bytes()) != original {
		t.Fatalf("first request seen of a conversation under way was changed (stages %v)", stages)
	}

	req = request(t, messages, 11)
	if stages := Stages(p.Apply(header, req)); len(stages) != 1 || stages[0] != "shorten" {
		t.Errorf("stages = %v, want [shorten] for the new tool result", stages)
	}
	got := bare(t, req)
	for i, m := range got[:9] {
		if strings.Contains(m, "shortened") {
			t.Errorf("message %d, sent before as it was, is now shortened: %s", i, m)
		}
	}
	if !strings.Contains(got[10], "shortened") {
		t.Errorf("the new tool result was not shortened: %s", got[10])
	}
}

func TestFirstRequestOfAConversationIsRewritten(t *testing.T) {
	big := strings.Repeat("y", 400)
	text, _ := json.Marshal(big)
	body := fmt.Sprintf(`{"model":"m","messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":%s}]}]}`, text)
	req, _ := anthropic.ParseRequest([]byte(body))
	if stages := Stages(New(&shorten{over: 300}).Apply(claudeCode("new", ""), req)); len(stages) != 1 {
		t.Errorf("stages = %v, want the first request's block shortened", stages)
	}
}

func TestThinkingIsNeverGivenToTransforms(t *testing.T) {
	seen := &recorder{}
	p := New(seen)
	messages := conversation(rand.New(rand.NewSource(3)), 3)
	header := claudeCode("sess", "")
	p.Apply(header, request(t, messages, 1))
	p.Apply(header, request(t, messages, 7))
	for _, kind := range seen.types {
		if kind == "thinking" || kind == "redacted_thinking" {
			t.Fatalf("a %s block was given to a transform", kind)
		}
	}
	if len(seen.types) == 0 {
		t.Fatal("no blocks were given to the transform")
	}
}

type recorder struct{ types []string }

func (r *recorder) Name() string { return "recorder" }
func (r *recorder) Rewrite(at Position, b anthropic.Block) (anthropic.Block, bool) {
	r.types = append(r.types, b.Type)
	if b.HasCacheControl() {
		panic("a block was given to a transform with its cache_control")
	}
	return b, false
}

func TestConversationsAreKeptApart(t *testing.T) {
	result := strings.Repeat("z", 500)
	text, _ := json.Marshal(result)
	body := fmt.Sprintf(`{"model":"m","messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":%s}]}]}`, text)
	s := &shorten{over: 300}
	p := New(s)
	forms := map[string]bool{}
	for _, h := range []http.Header{claudeCode("one", ""), claudeCode("one", "agent-1"), claudeCode("two", ""), {}} {
		req, _ := anthropic.ParseRequest([]byte(body))
		p.Apply(h, req)
		forms[string(req.Bytes())] = true
	}
	if s.calls != 4 || len(forms) != 4 {
		t.Errorf("%d rewrites giving %d forms, want each conversation to decide for itself (4 and 4)", s.calls, len(forms))
	}
}

func TestIdleConversationsAreForgotten(t *testing.T) {
	now := time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)
	p := New(&shorten{over: 0})
	p.now = func() time.Time { return now }
	messages := conversation(rand.New(rand.NewSource(5)), 2)
	p.Apply(claudeCode("old", ""), request(t, messages, 1))

	now = now.Add(DefaultIdle + time.Minute)
	p.Apply(claudeCode("new", ""), request(t, messages, 1))
	if _, ok := p.conversations["old/"]; ok {
		t.Error("a conversation idle for over an hour is still remembered")
	}
	if _, ok := p.conversations["new/"]; !ok {
		t.Error("the conversation just used is not remembered")
	}
}

func TestNothingToDo(t *testing.T) {
	body := []byte(`{"model":"m", "messages":[{"role":"user","content":"hi"}]}`)
	for name, p := range map[string]*Pipeline{"nil": nil, "no transforms": New(), "nothing matches": New(&shorten{over: 1 << 20})} {
		req, _ := anthropic.ParseRequest(body)
		if stages := p.Apply(claudeCode("s", ""), req); stages != nil || string(req.Bytes()) != string(body) {
			t.Errorf("%s: stages %v, body %s; want nothing changed", name, stages, req.Bytes())
		}
	}
}

func TestStagesAreInPipelineOrderAndReplayedFromTheMemo(t *testing.T) {
	text, _ := json.Marshal(strings.Repeat("output\n", 100))
	messages := []json.RawMessage{
		json.RawMessage(`{"role":"user","content":"go"}`),
		json.RawMessage(`{"role":"assistant","content":[{"type":"tool_use","id":"a","name":"Bash","input":{}}]}`),
		json.RawMessage(fmt.Sprintf(`{"role":"user","content":[{"type":"tool_result","tool_use_id":"a","content":%s}]}`, text)),
	}
	p := New(&shorten{over: 0}, clip{})
	header := claudeCode("sess", "")
	p.Apply(header, request(t, messages, 1))
	p.Apply(header, request(t, messages, 3))
	// Nothing new in the last message would need rewriting if the memo
	// were not replaying earlier rewrites; the stages must still say so.
	stages := Stages(p.Apply(header, request(t, messages, 3)))
	if strings.Join(stages, ",") != "shorten,clip" {
		t.Errorf("stages = %v, want [shorten clip]", stages)
	}
}

// TestRealCapturesKeepAStablePrefix replays captured Claude Code traffic
// through a pipeline that shortens large tool results. Wherever the client
// sent a message again unchanged, janus must send it again unchanged too.
func TestRealCapturesKeepAStablePrefix(t *testing.T) {
	dir := os.Getenv("JANUS_CAPTURES")
	if dir == "" {
		t.Skip("JANUS_CAPTURES is not set")
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*", "*v1-messages.request.txt"))
	sort.Strings(files) // names start with the time
	if len(files) == 0 {
		t.Fatalf("no *v1-messages.request.txt files under %s", dir)
	}

	s := &shorten{over: 2048}
	p := New(s)
	type turn struct{ arrived, sent []string }
	last := map[string]turn{}
	requests, compared := 0, 0
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		req, err := anthropic.ParseRequest(raw)
		if err != nil || len(req.Messages()) == 0 {
			continue
		}
		header := capturedHeader(t, strings.TrimSuffix(f, ".request.txt")+".md")
		arrived := bare(t, req)
		p.Apply(header, req)
		sent := bare(t, req)
		requests++

		id := Conversation(header, req)
		if prev, ok := last[id]; ok {
			for i := 0; i < len(prev.arrived) && i < len(arrived); i++ {
				if arrived[i] != prev.arrived[i] {
					continue // the client changed this message itself
				}
				compared++
				if sent[i] != prev.sent[i] {
					t.Fatalf("%s: message %d arrived as before but was sent in another form", filepath.Base(f), i)
				}
			}
		}
		last[id] = turn{arrived, sent}
	}
	t.Logf("%d requests in %d conversations; %d repeated messages sent as before; %d tool results shortened",
		requests, len(last), compared, s.calls)
	if s.calls == 0 {
		t.Error("no tool result was shortened, so the replay proves nothing")
	}
}

// capturedHeader reads the request headers a capture recorded.
func capturedHeader(t *testing.T, md string) http.Header {
	t.Helper()
	h := http.Header{}
	data, err := os.ReadFile(md)
	if err != nil {
		return h
	}
	for _, line := range strings.Split(string(data), "\n") {
		if k, v, ok := strings.Cut(line, ": "); ok && strings.HasPrefix(k, "X-Claude-Code-") {
			h.Set(k, v)
		}
	}
	return h
}
