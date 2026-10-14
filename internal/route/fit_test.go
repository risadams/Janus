package route

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/risadams/janus/internal/anthropic"
)

// longSession builds a Claude Code-shaped conversation: a first message of
// instructions and a request, then turns of a tool use and its result,
// each result about resultBytes long, ending on a tool result.
func longSession(turns, resultBytes int) string {
	var b strings.Builder
	b.WriteString(`{"model":"claude-opus-5-5","max_tokens":128000,"system":[{"type":"text","text":"You are Claude Code."}],"tools":[{"name":"Bash","input_schema":{"type":"object"}}],"messages":[`)
	b.WriteString(`{"role":"user","content":[{"type":"text","text":"<system-reminder>CLAUDE.md says: run the tests.</system-reminder>"},{"type":"text","text":"tag a release"}]}`)
	for i := range turns {
		fmt.Fprintf(&b, `,{"role":"assistant","content":[{"type":"text","text":"step %d"},{"type":"tool_use","id":"t%d","name":"Bash","input":{"command":"step %d"}}]}`, i, i, i)
		fmt.Fprintf(&b, `,{"role":"user","content":[{"type":"tool_result","tool_use_id":"t%d","content":"%s"}]}`, i, strings.Repeat("x", resultBytes))
	}
	b.WriteString(`]}`)
	return b.String()
}

// A conversation cut to fit keeps its first message, says how much was
// left out, and keeps the latest turns byte for byte, starting at an
// assistant message so that every tool result still follows its tool use.
func TestFitConversationKeepsTheStartAndTheLatestTurns(t *testing.T) {
	req := parse(t, longSession(200, 3000))
	before := req.Messages()
	budget := 20000

	leftOut, ok := fitConversation(req, budget)
	if !ok || leftOut == 0 {
		t.Fatalf("not fitted: left out %d, ok %v", leftOut, ok)
	}
	if got := EstimateTokens(req.Bytes()); got > budget {
		t.Errorf("about %d tokens after fitting, over the budget of %d", got, budget)
	}
	out, err := anthropic.ParseRequest(req.Bytes())
	if err != nil {
		t.Fatalf("the fitted request does not parse: %v", err)
	}
	after := out.Messages()
	if len(after) != len(before)-leftOut {
		t.Errorf("%d messages, want %d less %d", len(after), len(before), leftOut)
	}

	// The first message, with the note at its end.
	first := after[0].Blocks()
	if len(first) != 3 || first[1].Text() != "tag a release" || !strings.Contains(first[2].Text(), fmt.Sprintf("%d earlier messages of this conversation were left out", leftOut)) {
		t.Errorf("first message = %s", after[0].Raw())
	}
	// Then the latest turns, as they were.
	if after[1].Role != "assistant" {
		t.Errorf("the kept turns start with a %s message", after[1].Role)
	}
	for i := 1; i < len(after); i++ {
		if string(after[i].Raw()) != string(before[leftOut+i].Raw()) {
			t.Fatalf("kept message %d differs from the original", i)
		}
	}
	// Roles alternate, and each tool result answers the tool use before it.
	for i := 1; i < len(after); i++ {
		if after[i].Role == after[i-1].Role {
			t.Fatalf("messages %d and %d are both %s", i-1, i, after[i].Role)
		}
		for _, b := range after[i].Blocks() {
			if b.Type != "tool_result" {
				continue
			}
			var result anthropic.ToolResultBlock
			b.Decode(&result)
			if !strings.Contains(string(after[i-1].Raw()), `"id":"`+result.ToolUseID+`"`) {
				t.Errorf("message %d answers %s, which the message before it did not use", i, result.ToolUseID)
			}
		}
	}

	// Under the budget already, nothing changes.
	small := parse(t, longSession(3, 10))
	raw := string(small.Bytes())
	if n, ok := fitConversation(small, 100000); !ok || n != 0 || string(small.Bytes()) != raw {
		t.Errorf("a request under the budget was changed: left out %d, ok %v", n, ok)
	}
}

// When even the first message and the latest turn are over the budget,
// there is nothing to leave out that would help, and the request is left
// as it was.
func TestFitConversationGivesUpWhenTheLatestTurnAloneIsTooBig(t *testing.T) {
	req := parse(t, longSession(5, 200000))
	raw := string(req.Bytes())
	if _, ok := fitConversation(req, 20000); ok {
		t.Error("fitted, though the latest tool result alone is over the budget")
	}
	if string(req.Bytes()) != raw {
		t.Error("the request was changed though it could not be fitted")
	}
}

// At the 2026-10-13 session limit the main conversation was about 417k
// tokens, and there was no stand-in. Now the local model stands in on its
// latest turns, and says so; Claude's copy is untouched. A routed request
// over the context still goes to Claude whole, since it can.
func TestAStandInOnALongConversationLeavesOutOlderTurns(t *testing.T) {
	local, _ := lmStudio(t, nil) // qwen loaded with a 61,696-token context
	r := router(local, nil)
	standIn := qwen["janus-local"]
	r.Rules = []Rule{{Route: RouteUpstream, IfDown: &standIn}}
	body := longSession(400, 3000) // about 417k tokens
	req := parse(t, body)

	plan, _, _ := r.Explain(context.Background(), mainHeader(), req)
	got, ok := plan.Fallback(context.Background())
	if !ok {
		t.Fatalf("no stand-in: %s", got.Reason)
	}
	defer got.Release()
	if !slices.Contains(got.Stages, StageLeaveOut) || !strings.Contains(got.Reason, "messages left out to fit the local model's context") {
		t.Errorf("stages %v, reason %q", got.Stages, got.Reason)
	}
	if n := EstimateTokens(got.Body); n > 61696-answerReserve(61696) {
		t.Errorf("about %d tokens sent to a 61,696-token context", n)
	}
	if string(req.Bytes()) != body {
		t.Error("the request Claude was sent was changed")
	}

	// The ledger's reason says so, through the proxy's switch to the
	// stand-in.
	_, header, _, ex, _ := claudeDown(t, upstreamSays(429, `{"type":"error","error":{"type":"rate_limit_error","message":"limit"}}`), nil, body)
	if ex.Backend != "local" || header.Get("X-Janus-Fallback") == "" ||
		!regexp.MustCompile(`^rule main: upstream answered HTTP 429; sent local, \d+ of 801 messages left out to fit the local model's context$`).MatchString(ex.Reason) {
		t.Errorf("through the proxy: backend %q, reason %q", ex.Backend, ex.Reason)
	}

	r.Rules = []Rule{{Class: Main, Route: RouteLocal, Local: qwen["janus-local"]}}
	routed, _, _ := r.Explain(context.Background(), mainHeader(), parse(t, body))
	if routed.Backend.Name() != "upstream" {
		t.Errorf("a routed request over the context went %s", routed.Backend.Name())
	}
}
