// Package validate decides whether a local model's answer is fit to give
// to the client, or should be thrown away in favour of asking Claude.
//
// The checks are cheap and deterministic. They catch answers that are
// broken, not answers that are wrong: a reply that is empty, cut off,
// refuses, or calls a tool that does not exist or with arguments its
// schema forbids. Whether a well-formed answer is a good one is beyond
// them.
package validate

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/risadams/janus/internal/anthropic"
)

// The checks, by the names used in configuration.
const (
	Empty     = "empty"     // the answer has no text and no tool call
	Truncated = "truncated" // the answer ran out of tokens
	Tools     = "tools"     // a tool call names a tool that was not offered
	Schema    = "schema"    // a tool call's arguments do not fit the tool's schema
	Refusal   = "refusal"   // the answer is a refusal or an apology, not an attempt
)

// All lists every check, in the order they run.
var All = []string{Empty, Truncated, Tools, Schema, Refusal}

// Known reports whether name is a check.
func Known(name string) bool {
	for _, c := range All {
		if c == name {
			return true
		}
	}
	return false
}

// Check judges a local answer to req. skip names checks to leave out. It
// returns the name of the first check that failed and why, or two empty
// strings if the answer passes.
//
// Three things are always required, whatever is skipped: the response
// finished, it was not an error, and it could be read.
func Check(req *anthropic.Request, summary anthropic.Summary, answer anthropic.Answer, readable bool, skip []string) (failed, why string) {
	switch {
	case summary.Error != nil:
		return "error", fmt.Sprintf("the local model returned an error: %s", summary.Error.Type)
	case !summary.Complete:
		return "incomplete", "the local answer ended before it should have"
	case !readable || summary.Malformed > 0:
		return "malformed", "the local answer could not be read"
	}
	skipped := map[string]bool{}
	for _, s := range skip {
		skipped[s] = true
	}

	var text strings.Builder
	calls := 0
	for _, b := range answer.Blocks {
		switch b.Type {
		case "text":
			text.WriteString(b.Text)
		case "tool_use":
			calls++
		}
	}

	if !skipped[Empty] && calls == 0 && strings.TrimSpace(text.String()) == "" {
		// This is what a model produces when its tool call is lost between
		// the model and the server's parser: a normal-looking response
		// with nothing in it.
		return Empty, "the local answer has no text and no tool call"
	}
	if !skipped[Truncated] && answer.StopReason == "max_tokens" {
		return Truncated, "the local answer ran out of tokens"
	}

	if !skipped[Tools] || !skipped[Schema] {
		schemas := map[string]json.RawMessage{}
		for _, t := range req.Tools() {
			schema, _ := t.Field("input_schema")
			schemas[t.Name] = schema
		}
		for _, b := range answer.Blocks {
			if b.Type != "tool_use" {
				continue
			}
			schema, offered := schemas[b.Name]
			if !offered {
				if !skipped[Tools] {
					return Tools, fmt.Sprintf("the local answer calls %q, which is not one of the tools offered", b.Name)
				}
				continue
			}
			if skipped[Schema] {
				continue
			}
			var input any
			if err := json.Unmarshal([]byte(b.Input), &input); err != nil {
				return Schema, fmt.Sprintf("the arguments to %s are not valid JSON", b.Name)
			}
			if len(schema) > 0 {
				if problem := conforms(input, schema, "", 0); problem != "" {
					return Schema, fmt.Sprintf("the arguments to %s do not fit its schema: %s", b.Name, problem)
				}
			} else if _, isObject := input.(map[string]any); !isObject {
				return Schema, fmt.Sprintf("the arguments to %s are not a JSON object", b.Name)
			}
		}
	}

	if !skipped[Refusal] && calls == 0 && refuses(text.String()) {
		return Refusal, "the local answer is a refusal, not an attempt"
	}
	return "", ""
}

// refusalOpenings are how a refusal or a confused non-answer begins. Only
// the start of a short answer is examined: a long answer that happens to
// contain "I cannot" somewhere is probably explaining something.
var refusalOpenings = []string{
	"i cannot", "i can't", "i can not", "i'm unable", "i am unable", "i'm not able", "i am not able",
	"i'm sorry", "i am sorry", "sorry,", "i apologize", "unfortunately, i",
	"as an ai", "as a language model", "i don't have access", "i do not have access",
	"i don't have the ability", "i do not have the ability",
}

const refusalMaxLength = 400

func refuses(text string) bool {
	t := strings.ToLower(strings.TrimSpace(text))
	if t == "" || len(t) > refusalMaxLength {
		return false
	}
	t = strings.ReplaceAll(t, "’", "'") // a typographic apostrophe
	for _, opening := range refusalOpenings {
		if strings.HasPrefix(t, opening) {
			return true
		}
	}
	return false
}

// maxDepth bounds how deep conforms follows a schema into a value.
const maxDepth = 16

// conforms checks a value against a JSON Schema and returns what is wrong,
// or "" if nothing is.
//
// It understands the part of JSON Schema that tool definitions use: type,
// properties, required, additionalProperties: false, items, and enum.
// Keywords it does not know (patterns, numeric ranges, combinators, and so
// on) are ignored, which makes it lenient: it can pass arguments a full
// validator would reject, and it never rejects ones a full validator
// would pass.
func conforms(value any, schema json.RawMessage, path string, depth int) string {
	if depth > maxDepth || len(schema) == 0 {
		return ""
	}
	var s struct {
		Type                 json.RawMessage            `json:"type"`
		Properties           map[string]json.RawMessage `json:"properties"`
		Required             []string                   `json:"required"`
		AdditionalProperties json.RawMessage            `json:"additionalProperties"`
		Items                json.RawMessage            `json:"items"`
		Enum                 []any                      `json:"enum"`
	}
	if json.Unmarshal(schema, &s) != nil {
		return "" // true, false, or something unreadable: no opinion
	}
	at := path
	if at == "" {
		at = "the arguments"
	}

	if types := typeNames(s.Type); len(types) > 0 && !isOneOf(value, types) {
		return fmt.Sprintf("%s should be %s, not %s", at, strings.Join(types, " or "), typeOf(value))
	}
	if len(s.Enum) > 0 {
		found := false
		for _, allowed := range s.Enum {
			if same(allowed, value) {
				found = true
				break
			}
		}
		if !found {
			return fmt.Sprintf("%s is not one of the allowed values", at)
		}
	}

	switch v := value.(type) {
	case map[string]any:
		for _, name := range s.Required {
			if _, ok := v[name]; !ok {
				return fmt.Sprintf("%s is missing", join(path, name))
			}
		}
		closed := string(s.AdditionalProperties) == "false"
		for name, item := range v {
			sub, declared := s.Properties[name]
			if !declared {
				if closed {
					return fmt.Sprintf("%s is not a parameter", join(path, name))
				}
				continue
			}
			if problem := conforms(item, sub, join(path, name), depth+1); problem != "" {
				return problem
			}
		}
	case []any:
		for i, item := range v {
			if problem := conforms(item, s.Items, fmt.Sprintf("%s[%d]", at, i), depth+1); problem != "" {
				return problem
			}
		}
	}
	return ""
}

func join(path, name string) string {
	if path == "" {
		return name
	}
	return path + "." + name
}

// typeNames reads a schema's type, which is a name or a list of names.
func typeNames(raw json.RawMessage) []string {
	var one string
	if json.Unmarshal(raw, &one) == nil {
		return []string{one}
	}
	// A type that is neither a name nor a list of names gives none, so no
	// type is checked, as when a schema has no type at all. Such a schema
	// is not valid JSON Schema: the API refuses a tool that has one, so a
	// session whose tools carry it fails on its first request upstream,
	// and a local answer is only ever checked against tools the API has
	// taken.
	var many []string
	_ = json.Unmarshal(raw, &many)
	return many
}

func typeOf(value any) string {
	switch v := value.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case string:
		return "string"
	case float64:
		if v == float64(int64(v)) {
			return "integer"
		}
		return "number"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	}
	return "unknown"
}

func isOneOf(value any, types []string) bool {
	actual := typeOf(value)
	for _, t := range types {
		if t == actual || (t == "number" && actual == "integer") {
			return true
		}
	}
	return false
}

// same compares two decoded JSON values that are scalars.
func same(a, b any) bool {
	switch a.(type) {
	case nil, bool, string, float64:
		return a == b
	}
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}
