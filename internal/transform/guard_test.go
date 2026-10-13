package transform

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/risadams/janus/internal/anthropic"
)

// opus is claude-opus-5-5 at list price: a cache read is a fortieth of an
// hour's cache write.
func opus(model string) (Rates, bool) {
	if model != "claude-opus-5-5" {
		return Rates{}, false
	}
	return Rates{Read: 0.20, Write5m: 5, Write1h: 8}, true
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// result is a tool result message holding text.
func result(id, text string) json.RawMessage {
	content, _ := json.Marshal(text)
	return json.RawMessage(fmt.Sprintf(`{"role":"user","content":[{"type":"tool_result","tool_use_id":%q,"content":%s}]}`, id, content))
}

func call(id string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"role":"assistant","content":[{"type":"tool_use","id":%q,"name":"Read","input":{}}]}`, id))
}

func effectOf(effects []Effect, name string) Effect {
	for _, e := range effects {
		if e.Name == name {
			return e
		}
	}
	return Effect{}
}

func TestNewBlocksAreValuedAtTheirCacheRates(t *testing.T) {
	big := strings.Repeat("x", 3000)
	messages := []json.RawMessage{json.RawMessage(`{"role":"user","content":"go"}`), call("a"), result("a", big), call("b"), result("b", "ok")}
	s := &shorten{over: 300}
	p := New(s)
	p.Pricing = opus
	header := claudeCode("sess", "")
	p.Apply(header, request(t, messages, 1))

	first := effectOf(p.Apply(header, request(t, messages, 3)), "shorten")
	cut := first.Tokens
	if first.Rewritten != 1 || first.Blocks != 1 || cut < 900 {
		t.Fatalf("first send: %+v, want one block rewritten, about 1,000 tokens cut", first)
	}
	// New to the cache: written once at the hour rate the request asks
	// for, then expected to be read on about one more turn (3 messages).
	if !near(first.SavedUSD, float64(cut)*8/1e6) {
		t.Errorf("SavedUSD = %g, want the cut at the 1h write rate, %g", first.SavedUSD, float64(cut)*8/1e6)
	}
	if want := float64(cut)*8/1e6 + expectedTurns(3)*float64(cut)*0.2/1e6; !near(first.ExpectedUSD, want) {
		t.Errorf("ExpectedUSD = %g, want %g", first.ExpectedUSD, want)
	}

	again := effectOf(p.Apply(header, request(t, messages, 5)), "shorten")
	if again.Rewritten != 0 || again.Blocks != 1 || !near(again.SavedUSD, float64(cut)*0.2/1e6) || again.ExpectedUSD != 0 {
		t.Errorf("replay: %+v, want the same cut saved at the read rate, and nothing new expected", again)
	}
}

func TestFiveMinuteCacheIsPricedAsSuch(t *testing.T) {
	text, _ := json.Marshal(strings.Repeat("y", 3000))
	body := fmt.Sprintf(`{"model":"claude-opus-5-5","messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":%s,"cache_control":{"type":"ephemeral"}}]}]}`, text)
	req, _ := anthropic.ParseRequest([]byte(body))
	p := New(&shorten{over: 300})
	p.Pricing = opus
	e := effectOf(p.Apply(claudeCode("sub", "agent"), req), "shorten")
	if !near(e.SavedUSD, float64(e.Tokens)*5/1e6) {
		t.Errorf("SavedUSD = %g, want the cut at the 5-minute write rate, %g", e.SavedUSD, float64(e.Tokens)*5/1e6)
	}
}

func TestUnpricedModelsAreStillRewrittenButSaySo(t *testing.T) {
	text, _ := json.Marshal(strings.Repeat("z", 3000))
	body := fmt.Sprintf(`{"model":"some-other-model","messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":%s}]}]}`, text)
	req, _ := anthropic.ParseRequest([]byte(body))
	p := New(&shorten{over: 300})
	p.Pricing = opus
	e := effectOf(p.Apply(claudeCode("s", ""), req), "shorten")
	if e.Rewritten != 1 || !e.Unpriced || e.SavedUSD != 0 || e.ExpectedUSD != 0 {
		t.Errorf("effect = %+v, want rewritten, unpriced, no dollar figures", e)
	}
}

// grow "shortens" by making a block bigger.
type grow struct{}

func (grow) Name() string { return "grow" }
func (grow) Rewrite(at Position, b anthropic.Block) (anthropic.Block, bool) {
	return b.With("padding", json.RawMessage(`"`+strings.Repeat("-", 100)+`"`)), true
}

func TestARewriteThatDoesNotShrinkIsDeclined(t *testing.T) {
	body := []byte(`{"model":"claude-opus-5-5","messages":[{"role":"user","content":"hello"}]}`)
	req, _ := anthropic.ParseRequest(body)
	p := New(grow{})
	p.Pricing = opus
	effects := p.Apply(claudeCode("s", ""), req)
	if string(req.Bytes()) != string(body) {
		t.Errorf("a rewrite that made the block bigger was used: %s", req.Bytes())
	}
	if e := effectOf(effects, "grow"); e.Declined != 1 || e.Blocks != 0 {
		t.Errorf("effect = %+v, want the rewrite reported as declined", e)
	}
	if stages := Stages(effects); stages != nil {
		t.Errorf("stages = %v, want none: the request carries nothing of grow's", stages)
	}
}

// prune revises a "read <file>:" tool result that a later read of the same
// file supersedes: the kind of reviser stale-context pruning would be.
// #45 measured it and left it unbuilt, but the guard must still price one
// correctly.
type prune struct{}

func (prune) Name() string { return "prune" }
func (prune) Rewrite(at Position, b anthropic.Block) (anthropic.Block, bool) {
	return b, false
}
func (prune) Revise(at Position, b anthropic.Block) (anthropic.Block, bool) {
	file := readOf(b)
	if file == "" {
		return b, false
	}
	for _, m := range at.Request.Messages()[at.Message+1:] {
		for _, later := range m.Blocks() {
			if readOf(later) == file {
				return b.With("content", json.RawMessage(`"(superseded by a later read)"`)), true
			}
		}
	}
	return b, false
}

func readOf(b anthropic.Block) string {
	var r anthropic.ToolResultBlock
	if b.Type != "tool_result" || b.Decode(&r) != nil {
		return ""
	}
	var text string
	if json.Unmarshal(r.Content, &text) != nil || !strings.HasPrefix(text, "read ") {
		return ""
	}
	file, _, _ := strings.Cut(text, ":")
	return file
}

// play sends a conversation one turn at a time and returns the effects of
// the last request.
func play(t *testing.T, p *Pipeline, header map[string][]string, messages []json.RawMessage) ([]Effect, *anthropic.Request) {
	t.Helper()
	var effects []Effect
	var req *anthropic.Request
	for n := 1; n <= len(messages); n += 2 {
		req = request(t, messages, n)
		effects = p.Apply(header, req)
	}
	return effects, req
}

func TestARevisionIsTakenWhenItPaysForTheCacheItBreaks(t *testing.T) {
	messages := []json.RawMessage{json.RawMessage(`{"role":"user","content":"go"}`),
		call("a"), result("a", "read main.go:"+strings.Repeat("v1 ", 10000)),
		call("b"), result("b", "read main.go: v2")}
	p := New(prune{})
	p.Pricing = opus
	header := claudeCode("sess", "")
	effects, req := play(t, p, header, messages)

	e := effectOf(effects, "prune")
	if e.Revised != 1 || e.CostUSD <= 0 || e.ExpectedUSD <= 0 || e.Tokens < 9000 {
		t.Fatalf("effect = %+v, want one revision, cutting about 10,000 tokens, with a cost and a positive expectation", e)
	}
	if !strings.Contains(string(req.Messages()[2].Raw()), "superseded") {
		t.Errorf("the superseded read was not revised: %s", req.Messages()[2].Raw())
	}
	// From now on the revised form is the one sent.
	messages = append(messages, call("c"), result("c", "done"))
	next := request(t, messages, 7)
	e = effectOf(p.Apply(header, next), "prune")
	if !strings.Contains(string(next.Messages()[2].Raw()), "superseded") || e.Revised != 0 || e.Blocks != 1 {
		t.Errorf("after the revision: %+v, %s; want the revised form carried, not revised again", e, next.Messages()[2].Raw())
	}
}

func TestARevisionIsRefusedWhenTheCacheAfterItIsLarge(t *testing.T) {
	messages := []json.RawMessage{json.RawMessage(`{"role":"user","content":"go"}`),
		call("a"), result("a", "read main.go:"+strings.Repeat("v1 ", 1000)),
		call("b"), result("b", strings.Repeat("a long log ", 30000)),
		call("c"), result("c", "read main.go: v2")}
	p := New(prune{})
	p.Pricing = opus
	effects, req := play(t, p, claudeCode("sess", ""), messages)

	e := effectOf(effects, "prune")
	if e.Revised != 0 || e.Declined != 1 {
		t.Errorf("effect = %+v, want the revision declined: 1,000 tokens cut cannot pay for 110,000 written again", e)
	}
	if strings.Contains(string(req.Bytes()), "superseded") {
		t.Error("a declined revision was applied")
	}
}

func TestOnlyTheRevisionsThatPayAreTaken(t *testing.T) {
	messages := []json.RawMessage{json.RawMessage(`{"role":"user","content":"go"}`),
		call("a"), result("a", "read a.go:"+strings.Repeat("a ", 300)),
		call("b"), result("b", strings.Repeat("a long log ", 30000)),
		call("c"), result("c", "read b.go:"+strings.Repeat("b ", 30000)),
		call("d"), result("d", "read a.go: v2"),
		call("e"), result("e", "read b.go: v2")}
	p := New(prune{})
	p.Pricing = opus
	effects, req := play(t, p, claudeCode("sess", ""), messages)

	e := effectOf(effects, "prune")
	if e.Revised != 1 || e.Declined != 1 {
		t.Fatalf("effect = %+v, want the late large read revised and the early small one declined", e)
	}
	got := req.Messages()
	if strings.Contains(string(got[2].Raw()), "superseded") || !strings.Contains(string(got[6].Raw()), "superseded") {
		t.Errorf("revised the wrong block:\n2: %.80s\n6: %.80s", got[2].Raw(), got[6].Raw())
	}
}

func TestNoRevisionsWithoutPricesOrOnTheFirstRequestSeen(t *testing.T) {
	messages := []json.RawMessage{json.RawMessage(`{"role":"user","content":"go"}`),
		call("a"), result("a", "read main.go:"+strings.Repeat("v1 ", 10000)),
		call("b"), result("b", "read main.go: v2")}

	unpriced := New(prune{})
	if _, req := play(t, unpriced, claudeCode("s", ""), messages); strings.Contains(string(req.Bytes()), "superseded") {
		t.Error("revised with no prices to show it pays")
	}

	midway := New(prune{})
	midway.Pricing = opus
	req := request(t, messages, 5)
	midway.Apply(claudeCode("s", ""), req)
	if strings.Contains(string(req.Bytes()), "superseded") {
		t.Error("revised on the first request seen of a conversation, whose cache janus cannot know")
	}
}

func TestRevisionsThatLoseMoneyStopTheTransformRevising(t *testing.T) {
	p := New(prune{})
	p.Pricing = opus
	var told []string
	p.Notify = func(name, why string) { told = append(told, name+": "+why) }

	// Each lost money: its cut was read once, then the conversation ended.
	for i := range minSettled {
		m := &memo{revisions: []*revision{{cut: map[string]int{"prune": 1000}, cost: 0.01, read: 0.2}}}
		p.mu.Lock()
		p.settle(m)
		p.mu.Unlock()
		if got := p.Disabled()["prune"]; (got != "") != (i == minSettled-1) {
			t.Fatalf("after %d losing revisions, disabled = %q", i+1, got)
		}
	}
	if len(told) != 1 || !strings.HasPrefix(told[0], "prune: its last 5 revisions lost $") {
		t.Errorf("notified %q, want one notice that prune stopped, with why", told)
	}

	messages := []json.RawMessage{json.RawMessage(`{"role":"user","content":"go"}`),
		call("a"), result("a", "read main.go:"+strings.Repeat("v1 ", 10000)),
		call("b"), result("b", "read main.go: v2")}
	if _, req := play(t, p, claudeCode("s", ""), messages); strings.Contains(string(req.Bytes()), "superseded") {
		t.Error("a transform stopped from revising revised all the same")
	}
}

func TestSettledRevisionsCountTheTurnsThatCarriedThem(t *testing.T) {
	now := time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)
	p := New(prune{})
	p.Pricing = opus
	p.now = func() time.Time { return now }
	messages := []json.RawMessage{json.RawMessage(`{"role":"user","content":"go"}`),
		call("a"), result("a", "read main.go:"+strings.Repeat("v1 ", 10000)),
		call("b"), result("b", "read main.go: v2"),
		call("c"), result("c", "ok"), call("d"), result("d", "ok")}
	play(t, p, claudeCode("s", ""), messages)
	revisions := p.conversations["s/"].revisions
	if len(revisions) != 1 || revisions[0].turns != 2 {
		t.Fatalf("revisions = %+v, want one, carried on the two requests after it", revisions)
	}

	now = now.Add(DefaultIdle + time.Minute)
	p.Apply(claudeCode("other", ""), request(t, messages, 1))
	outcomes := p.record["prune"].outcomes
	if len(outcomes) != 1 || outcomes[0] <= 0 {
		t.Errorf("outcomes = %v, want one revision settled, in profit after three reads of its cut", outcomes)
	}
}

func TestExpectedTurns(t *testing.T) {
	for messages, want := range map[int]float64{1: 1, 3: 1, 9: 2.25, 41: 10.25, 401: 100.25} {
		if got := expectedTurns(messages); got != want {
			t.Errorf("expectedTurns(%d) = %g, want %g", messages, got, want)
		}
	}
}

func TestARepeatWithinOneRequestIsStillNewToTheCache(t *testing.T) {
	big := "read log.txt:" + strings.Repeat("x", 3000)
	messages := []json.RawMessage{json.RawMessage(`{"role":"user","content":"go"}`), call("a"), result("a", big), call("b"), result("a", big)}
	p := New(&shorten{over: 300})
	p.Pricing = opus
	header := claudeCode("sess", "")
	p.Apply(header, request(t, messages, 1))
	p.Apply(header, request(t, messages, 3))
	// Message 4 repeats message 2 exactly; both are rewritten the same
	// way, but only message 2 was sent before.
	e := effectOf(p.Apply(header, request(t, messages, 5)), "shorten")
	per := e.Tokens / 2
	if want := usd(per, 0.2) + usd(per, 8); e.Blocks != 2 || !near(e.SavedUSD, want) {
		t.Errorf("effect = %+v, want two blocks: one read from cache, one written ($%g)", e, want)
	}
}
