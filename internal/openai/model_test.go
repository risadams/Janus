package openai

import (
	"fmt"
	"testing"

	"github.com/risadams/janus/internal/anthropic"
	"github.com/risadams/janus/internal/config"
)

func choose(t *testing.T, o config.OpenAI, request string) Choice {
	t.Helper()
	req, err := anthropic.ParseRequest([]byte(request))
	if err != nil {
		t.Fatal(err)
	}
	return Choose(o, req)
}

func TestEachClaudeFamilyHasADefaultModel(t *testing.T) {
	for asked, want := range map[string]string{
		"claude-opus-5-5":   "gpt-6.1-sol",
		"claude-fable-5-1":  "gpt-6.1-sol",
		"claude-sonnet-5-5": "gpt-6.1-sol",
		"claude-haiku-5-5":  "gpt-6-luna",
		"janus-local":       "gpt-6.1-sol",
	} {
		if got := choose(t, config.OpenAI{}, `{"model":"`+asked+`","messages":[]}`).Model; got != want {
			t.Errorf("%s answered by %s, want %s", asked, got, want)
		}
	}
}

// openai.models replaces the defaults one by one: a full name before a
// family, a family before "default".
func TestConfiguredModelsReplaceTheDefaultsOneByOne(t *testing.T) {
	o := config.OpenAI{Models: map[string]string{
		"claude-opus-5-5": "gpt-6-astra",
		"sonnet":          "gpt-6-luna",
		"default":         "gpt-5.6-sol",
	}}
	for asked, want := range map[string]string{
		"claude-opus-5-5":   "gpt-6-astra",
		"claude-opus-5-1":   "gpt-6.1-sol",
		"claude-sonnet-5-5": "gpt-6-luna",
		"claude-haiku-5-5":  "gpt-6-luna",
		"something-else":    "gpt-5.6-sol",
	} {
		if got := choose(t, o, `{"model":"`+asked+`","messages":[]}`).Model; got != want {
			t.Errorf("%s answered by %s, want %s", asked, got, want)
		}
	}
}

func TestEffortAndThinkingBudgetsBecomeReasoningEffort(t *testing.T) {
	for _, c := range []struct {
		name, model, fields, want string
	}{
		{"effort as asked", "claude-sonnet-5-5", `"output_config":{"effort":"high"}`, "high"},
		{"effort over a budget", "claude-sonnet-5-5", `"output_config":{"effort":"medium"},"thinking":{"type":"enabled","budget_tokens":64000}`, "medium"},
		{"small budget", "claude-sonnet-5-5", `"thinking":{"type":"enabled","budget_tokens":2000}`, "low"},
		{"middling budget", "claude-sonnet-5-5", `"thinking":{"type":"enabled","budget_tokens":10000}`, "medium"},
		{"large budget", "claude-sonnet-5-5", `"thinking":{"type":"enabled","budget_tokens":20000}`, "high"},
		{"largest budget", "claude-sonnet-5-5", `"thinking":{"type":"enabled","budget_tokens":32000}`, "xhigh"},
		{"adaptive, no effort", "claude-sonnet-5-5", `"thinking":{"type":"adaptive"}`, ""},
		{"off on sol", "claude-sonnet-5-5", `"thinking":{"type":"disabled"}`, "low"},
		{"off on luna", "claude-haiku-5-5", `"thinking":{"type":"disabled"}`, "none"},
		{"nothing said", "claude-sonnet-5-5", ``, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			fields := c.fields
			if fields != "" {
				fields = "," + fields
			}
			if got := choose(t, config.OpenAI{}, `{"model":"`+c.model+`","messages":[]`+fields+`}`).Effort; got != c.want {
				t.Errorf("effort %q, want %q", got, c.want)
			}
		})
	}
}

func TestEffortIsClampedToWhatTheModelAccepts(t *testing.T) {
	for _, c := range []struct{ model, effort, want string }{
		{"gpt-5.3-codex", "max", "xhigh"},
		{"gpt-5.3-codex", "minimal", "low"},
		{"gpt-6.1-sol", "none", "low"},
		{"gpt-6-luna", "none", "none"},
		{"gpt-6-astra", "max", "max"},
		{"gpt-6.1-sol", "turbo", ""},
		{"some-new-model", "xhigh", "xhigh"},
		{"some-new-model", "off", ""},
	} {
		if got := clampEffort(c.model, c.effort); got != c.want {
			t.Errorf("%s at %q: %q, want %q", c.model, c.effort, got, c.want)
		}
	}
}

// A small output cap goes with the model's lowest effort, however much
// the request asked for, since reasoning counts against the cap (#452).
func TestASmallOutputCapGoesWithTheLowestEffort(t *testing.T) {
	ask := func(maxTokens int, thinking string) Choice {
		body := fmt.Sprintf(`{"model":"claude-sonnet-5-5","max_tokens":%d,%s"messages":[{"role":"user","content":"hi"}]}`, maxTokens, thinking)
		req, err := anthropic.ParseRequest([]byte(body))
		if err != nil {
			t.Fatal(err)
		}
		return Choose(config.OpenAI{}, req)
	}
	for _, c := range []struct {
		name, thinking string
		maxTokens      int
		want           string
	}{
		{"a safety check with no thinking setting", "", 64, "low"},
		{"a status line that asked for high effort", `"output_config":{"effort":"high"},`, 200, "low"},
		{"just under the line", `"output_config":{"effort":"high"},`, SmallCap - 1, "low"},
		{"at the line, as asked", `"output_config":{"effort":"high"},`, SmallCap, "high"},
		{"a main turn, as asked", `"output_config":{"effort":"medium"},`, 32000, "medium"},
		{"a main turn with no setting", "", 32000, ""},
	} {
		if got := ask(c.maxTokens, c.thinking).Effort; got != c.want {
			t.Errorf("%s: effort %q, want %q", c.name, got, c.want)
		}
	}
}
