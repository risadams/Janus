// Package openai is janus's side of the OpenAI API, which can answer
// Claude Code's requests in place of Anthropic (M10). It holds the one
// credential janus keeps of its own, the OpenAI API key (ADR-005), builds
// the requests that carry it, and translates between the Messages API
// Claude Code speaks and OpenAI's Responses API, both ways, as
// docs/research/openai.md decided.
package openai

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/risadams/janus/internal/config"
)

// redacted is what a Key prints as, in every form.
const redacted = "[redacted]"

// Key is the OpenAI API key, read once at startup by LoadKey and kept in
// memory only. It prints as [redacted] through fmt, slog, JSON, and text
// marshalling, so a log line, a capture, the ledger, or doctor that is
// handed one by mistake still never shows it. The only way out is the
// Authorization header NewRequest sets on a request to openai.baseUrl.
type Key struct {
	secret string
}

// String implements fmt.Stringer.
func (Key) String() string { return redacted }

// GoString implements fmt.GoStringer, for %#v.
func (Key) GoString() string { return redacted }

// Format implements fmt.Formatter, so that no verb, %x and %q included,
// reaches the secret.
func (Key) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, redacted) }

// LogValue implements slog.LogValuer.
func (Key) LogValue() slog.Value { return slog.StringValue(redacted) }

// MarshalJSON implements json.Marshaler.
func (Key) MarshalJSON() ([]byte, error) { return []byte(`"` + redacted + `"`), nil }

// MarshalText implements encoding.TextMarshaler.
func (Key) MarshalText() ([]byte, error) { return []byte(redacted), nil }

// Set reports whether the key was found.
func (k Key) Set() bool { return k.secret != "" }

// LoadKey reads the key from where openai.key says it is kept. lookupEnv
// is os.LookupEnv, or a fake in tests. The errors name the source and say
// what to do, and never include the value found.
func LoadKey(o config.OpenAI, lookupEnv func(string) (string, bool)) (Key, error) {
	kind, name := o.KeySource()
	switch kind {
	case "env":
		value, ok := lookupEnv(name)
		if !ok {
			return Key{}, fmt.Errorf("openai.key is %q, and %s is not set in the environment janus started in; export it before starting janus", o.Key, name)
		}
		return parseKey(o.Key, name, value)
	case "keychain":
		value, err := readKeychain(name)
		if err != nil {
			return Key{}, fmt.Errorf("openai.key is %q, but the key could not be read: %w", o.Key, err)
		}
		return parseKey(o.Key, name, value)
	default:
		// Validate refuses anything else, so this is a config that was not
		// validated.
		return Key{}, errors.New(`openai.key must say where the key is kept, as in "env:OPENAI_API_KEY"`)
	}
}

// parseKey checks a key as found. Surrounding whitespace, as from
// $(cat file), is dropped. Anything that could not be sent as one header
// value is refused, so that a stray newline cannot add a header.
func parseKey(source, name, value string) (Key, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return Key{}, fmt.Errorf("openai.key is %q, and %s is empty; put the API key in it", source, name)
	}
	for _, c := range value {
		if c <= ' ' || c > '~' {
			return Key{}, fmt.Errorf("openai.key is %q, and %s holds a space or a character an API key does not have; check it holds only the key", source, name)
		}
	}
	return Key{secret: value}, nil
}

// NewRequest builds a POST of body to path under base (openai.baseUrl),
// carrying the key. It takes no headers from the client: the request
// Claude Code sent carries its Anthropic credential, and building OpenAI's
// request from nothing is what keeps that credential from crossing. For
// the same reason the key is set here and nowhere else, so it cannot reach
// Anthropic or LM Studio.
func NewRequest(ctx context.Context, key Key, base, path string, body []byte) (*http.Request, error) {
	if !key.Set() {
		return nil, errors.New("openai: no API key")
	}
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, fmt.Errorf("openai: base URL %q is not an http(s) address", base)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(u.String(), "/")+path, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("openai: %w", err)
	}
	key.authorize(req.Header)
	return req, nil
}

// scrub replaces the key wherever it appears in b. OpenAI's errors can
// quote a key back, and what OpenAI says is passed on to Claude Code and
// kept in the ledger and captures.
func (k Key) scrub(b []byte) []byte {
	if k.secret == "" {
		return b
	}
	return bytes.ReplaceAll(b, []byte(k.secret), []byte(redacted))
}

// authorize sets h to carry the key and a JSON body, and nothing else.
func (k Key) authorize(h http.Header) {
	h.Set("Authorization", "Bearer "+k.secret)
	h.Set("Content-Type", "application/json")
}
