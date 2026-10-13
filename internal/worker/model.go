// Package worker holds the tools janus offers Claude for handing work to
// the local model: reading that would otherwise fill Claude's context, done
// locally, with only a short answer going back.
//
// Model, the way to put one question to the local model, is also what the
// digest transform uses to write its digests.
package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/risadams/janus/internal/anthropic"
	"github.com/risadams/janus/internal/backend"
	"github.com/risadams/janus/internal/proxy"
)

// Model is the local model a worker asks.
type Model struct {
	Local *backend.Local
	// ID is the model's id in LM Studio.
	ID string
	// Timeout bounds one question. Zero means no limit.
	Timeout time.Duration
	Client  *http.Client
}

// maxAnswer bounds how much of a local answer is read.
const maxAnswer = 4 << 20

// Ask puts one question to the local model and returns its answer as text.
func (m *Model) Ask(ctx context.Context, system, user string, maxTokens int) (string, anthropic.Usage, error) {
	body, _ := json.Marshal(map[string]any{
		"model":      m.ID,
		"max_tokens": maxTokens,
		"system":     system,
		"messages":   []map[string]string{{"role": "user", "content": user}},
	})
	if m.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, m.Timeout)
		defer cancel()
	}
	client := m.Client
	if client == nil {
		client = &http.Client{}
	}

	got := proxy.Fetch(ctx, client, m.Local, http.Header{}, body, maxAnswer)
	switch {
	case errors.Is(got.Err, context.DeadlineExceeded):
		return "", anthropic.Usage{}, fmt.Errorf("the local model did not answer within %s", m.Timeout)
	case got.Err != nil:
		return "", anthropic.Usage{}, fmt.Errorf("could not reach the local model: %w", got.Err)
	case got.Summary.Error != nil:
		return "", got.Summary.Usage, fmt.Errorf("the local model returned an error: %s: %s", got.Summary.Error.Type, got.Summary.Error.Message)
	case got.Status >= 400:
		return "", got.Summary.Usage, fmt.Errorf("the local model answered HTTP %d", got.Status)
	}

	answer, ok := anthropic.Assemble(got.Body, got.Header.Get("Content-Type"))
	if !ok {
		return "", got.Summary.Usage, errors.New("the local model's answer could not be read")
	}
	var text strings.Builder
	for _, b := range answer.Blocks {
		if b.Type == "text" {
			text.WriteString(b.Text)
		}
	}
	out := strings.TrimSpace(text.String())
	if out == "" {
		return "", got.Summary.Usage, errors.New("the local model returned an empty answer")
	}
	return out, got.Summary.Usage, nil
}

// contextTokens is the context the model is loaded with, or a cautious
// guess if LM Studio does not say.
func (m *Model) contextTokens(ctx context.Context) int {
	const unknown = 16000
	info, found, err := m.Local.Model(ctx, m.ID)
	if err != nil || !found || info.LoadedContext == 0 {
		return unknown
	}
	return info.LoadedContext
}
