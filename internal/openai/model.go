package openai

import (
	"encoding/json"
	"strings"

	"github.com/risadams/janus/internal/anthropic"
	"github.com/risadams/janus/internal/config"
)

// Choice is the OpenAI model and reasoning effort for one request.
type Choice struct {
	Model string
	// Effort is reasoning.effort, or empty to leave it to the model.
	Effort string
}

// Choose picks the model and effort for Claude Code's request
// (docs/research/openai.md, Models). The model is openai.models' entry for
// the requested model's full name, else for its family ("opus", "sonnet",
// "haiku", "fable"), else for "default", with DefaultOpenAIModels filling
// in what openai.models leaves out.
func Choose(o config.OpenAI, req *anthropic.Request) Choice {
	model := chooseModel(o, req.Model())
	return Choice{Model: model, Effort: clampEffort(model, requestedEffort(req))}
}

func chooseModel(o config.OpenAI, asked string) string {
	lookup := func(key string) (string, bool) {
		if m, ok := o.Models[key]; ok && m != "" {
			return m, true
		}
		m, ok := config.DefaultOpenAIModels[key]
		return m, ok
	}
	if m, ok := o.Models[asked]; ok && m != "" {
		return m
	}
	if family := Family(asked); family != "" {
		if m, ok := lookup(family); ok {
			return m
		}
	}
	m, _ := lookup("default")
	return m
}

// Family is the Claude family a model name belongs to, or empty.
func Family(model string) string {
	lower := strings.ToLower(model)
	for _, f := range []string{"opus", "sonnet", "haiku", "fable"} {
		if strings.Contains(lower, f) {
			return f
		}
	}
	return ""
}

// requestedEffort is the effort Claude Code asked for: output_config.effort
// as it is, else one picked from a thinking budget, else "off" for thinking
// switched off, else empty. The budget thresholds are a starting point, not
// a measurement (docs/research/openai.md, Models).
func requestedEffort(req *anthropic.Request) string {
	if v, ok := req.Field("output_config"); ok {
		var oc struct {
			Effort string `json:"effort"`
		}
		if json.Unmarshal(v, &oc) == nil && oc.Effort != "" {
			return oc.Effort
		}
	}
	v, ok := req.Field("thinking")
	if !ok {
		return ""
	}
	var th struct {
		Type         string `json:"type"`
		BudgetTokens int    `json:"budget_tokens"`
	}
	if json.Unmarshal(v, &th) != nil {
		return ""
	}
	switch {
	case th.Type == "disabled":
		return "off"
	case th.Type != "enabled":
		return ""
	case th.BudgetTokens < 4000:
		return "low"
	case th.BudgetTokens < 16000:
		return "medium"
	case th.BudgetTokens < 32000:
		return "high"
	default:
		return "xhigh"
	}
}

// contextWindows is each model's context window, in tokens, from its model
// page (docs/research/openai.md, Models).
var contextWindows = map[string]int{
	"gpt-6-astra":   1_050_000,
	"gpt-6.1-sol":   1_050_000,
	"gpt-6-luna":    1_050_000,
	"gpt-5.6-sol":   1_050_000,
	"gpt-5.3-codex": 400_000,
}

// Window is model's context window: openai.contextTokens' entry for it,
// else the one janus knows, else zero for not known.
func Window(o config.OpenAI, model string) int {
	if n := o.ContextTokens[model]; n > 0 {
		return n
	}
	return contextWindows[model]
}

// efforts is reasoning.effort's scale, lowest first.
var efforts = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}

// effortRange is the lowest and highest effort each model accepts, from
// its model page (docs/research/openai.md, Models).
var effortRange = map[string][2]string{
	"gpt-6-astra":   {"low", "max"},
	"gpt-6.1-sol":   {"low", "max"},
	"gpt-6-luna":    {"none", "max"},
	"gpt-5.6-sol":   {"none", "max"},
	"gpt-5.3-codex": {"low", "xhigh"},
}

func rank(effort string) int {
	for i, e := range efforts {
		if e == effort {
			return i
		}
	}
	return -1
}

// clampEffort fits an effort to what model accepts. "off" is the model's
// lowest. For a model janus has no range for, chosen by configuration,
// the effort goes as asked and "off" is left to the model.
func clampEffort(model, effort string) string {
	if effort == "" {
		return ""
	}
	r, known := effortRange[model]
	if !known {
		if effort == "off" || rank(effort) < 0 {
			return ""
		}
		return effort
	}
	lo, hi := rank(r[0]), rank(r[1])
	n := rank(effort)
	switch {
	case effort == "off", n >= 0 && n < lo:
		return r[0]
	case n > hi:
		return r[1]
	case n < 0:
		return "" // an effort janus does not know; the model's own default
	default:
		return effort
	}
}
