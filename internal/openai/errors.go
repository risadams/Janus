package openai

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// Kind is what an OpenAI error means for the chain of providers (#377).
type Kind string

// The kinds of OpenAI error (docs/research/openai.md, Errors).
const (
	// TooLong: the conversation does not fit the model. The next provider
	// may take it.
	TooLong Kind = "too_long"
	// BadRequest: most likely a translation bug. The next provider gets the
	// original request.
	BadRequest Kind = "bad_request"
	// KeyRefused: OpenAI refused the key, or does not serve the region.
	// OpenAI is out until a restart or a config change.
	KeyRefused Kind = "key_refused"
	// RateLimited: a per-minute limit, which passes by itself.
	RateLimited Kind = "rate_limited"
	// QuotaExhausted: credit, spend, or a monthly limit, which only the
	// owner can lift, and which has no reset time.
	QuotaExhausted Kind = "quota_exhausted"
	// ServerError: OpenAI's fault.
	ServerError Kind = "server_error"
	// Overloaded: OpenAI is too busy.
	Overloaded Kind = "overloaded"
)

// quotaCodes are the 429 codes that retrying does not help.
var quotaCodes = map[string]bool{
	"insufficient_quota": true, "credit_balance_exhausted": true,
	"organization_spend_limit_exceeded": true, "project_spend_limit_exceeded": true,
	"organization_usage_limit_exceeded": true,
}

// Failure is an OpenAI error as Claude Code is to be told it: in
// Anthropic's shape, with a message that begins "janus:" and says what to
// do.
type Failure struct {
	Kind Kind
	// Status is the HTTP status to answer Claude Code with. It is never 401
	// or 403, which Claude Code would take as its own login failing.
	Status int
	// Type is the Anthropic error type.
	Type    string
	Message string
	// Code is OpenAI's code, for the ledger.
	Code string
	// RetryAfter is OpenAI's Retry-After, passed on for a per-minute limit.
	RetryAfter string
}

// body is the Anthropic error body, which is also the data of a stream's
// error event.
func (f Failure) body() any {
	return map[string]any{"type": "error", "error": map[string]string{"type": f.Type, "message": f.Message}}
}

// Body is the Failure as an Anthropic error response body.
func (f Failure) Body() []byte {
	b, _ := json.Marshal(f.body()) // strings only, so it marshals
	return b
}

// TranslateError reads an HTTP error answer from OpenAI. keySource is
// openai.key, such as "env:OPENAI_API_KEY", so that a refused key is
// explained by where it came from.
func TranslateError(status int, header http.Header, body []byte, keySource string) Failure {
	var e struct {
		Error *wireError `json:"error"`
	}
	_ = json.Unmarshal(body, &e) // a body that is not OpenAI's shape is explained by its status
	we := e.Error
	if we == nil {
		we = &wireError{}
	}
	f := classify(status, we)
	if f.Kind == KeyRefused {
		f.Message = keyRefused(status, keySource)
	}
	if f.Kind == RateLimited {
		f.RetryAfter = header.Get("Retry-After")
	}
	return f
}

// classify maps an OpenAI error onto what Claude Code is told. A
// mid-stream error comes with no status of its own, so it is classified by
// its code alone.
func classify(status int, we *wireError) Failure {
	said := strings.TrimSpace(we.Message)
	if said == "" {
		said = "no message"
	}
	said = fmt.Sprintf("OpenAI said: %q", said)
	f := Failure{Code: we.Code}
	switch {
	case we.Code == "context_length_exceeded":
		// "prompt is too long" is Anthropic's wording, which may be what
		// Claude Code looks for before offering to compact (#399).
		f.Kind, f.Status, f.Type = TooLong, http.StatusBadRequest, "invalid_request_error"
		f.Message = "janus: prompt is too long for the OpenAI model. Run /compact, or move to a provider with a larger window. " + said
	case status == http.StatusUnauthorized || status == http.StatusForbidden || we.Code == "invalid_api_key":
		f.Kind, f.Status, f.Type = KeyRefused, http.StatusInternalServerError, "api_error"
		f.Message = keyRefused(status, "")
	case quotaCodes[we.Code] || we.Type == "insufficient_quota":
		f.Kind, f.Status, f.Type = QuotaExhausted, http.StatusTooManyRequests, "rate_limit_error"
		f.Message = fmt.Sprintf("janus: OpenAI's account has run out (%s). Retrying will not help: the account's owner has to add credit or raise the limit. %s", codeOr(we.Code, "quota"), said)
	case status == http.StatusTooManyRequests || we.Code == "rate_limit_exceeded" || we.Code == "slow_down":
		f.Kind, f.Status, f.Type = RateLimited, http.StatusTooManyRequests, "rate_limit_error"
		f.Message = "janus: OpenAI's per-minute rate limit was reached. It passes in seconds to minutes; try again shortly. " + said
	case status == http.StatusServiceUnavailable || we.Code == "server_is_overloaded":
		f.Kind, f.Status, f.Type = Overloaded, 529, "overloaded_error"
		f.Message = "janus: OpenAI is overloaded. Try again shortly. " + said
	case status == http.StatusBadRequest || we.Type == "invalid_request_error":
		f.Kind, f.Status, f.Type = BadRequest, http.StatusBadRequest, "invalid_request_error"
		f.Message = "janus: OpenAI refused the request janus translated, which is most likely a bug in janus; please report it with the ledger record. " + said
	default:
		f.Kind, f.Status, f.Type = ServerError, http.StatusInternalServerError, "api_error"
		f.Message = "janus: OpenAI failed to answer. Try again shortly. " + said
	}
	return f
}

// keyRefused explains a 401 or 403 without OpenAI's own message, which
// can quote part of the key.
func keyRefused(status int, keySource string) string {
	from := "the source openai.key names"
	if kind, name, ok := strings.Cut(keySource, ":"); ok {
		switch kind {
		case "env":
			from = "the environment variable " + name
		case "keychain":
			from = "the credential store entry " + name
		}
	}
	if status == http.StatusForbidden {
		return fmt.Sprintf("janus: OpenAI refused the request (HTTP 403): the API key janus reads from %s may not have access to this model, or OpenAI may not serve your region. Check the key's project and permissions.", from)
	}
	return fmt.Sprintf("janus: OpenAI refused the API key janus reads from %s. Check that it holds a current key, then restart janus.", from)
}

func codeOr(code, fallback string) string {
	if code == "" {
		return fallback
	}
	return code
}
