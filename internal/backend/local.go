package backend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/risadams/janus/internal/anthropic"
)

// Local is LM Studio. It speaks the Messages API natively at /v1/messages
// (docs/research/lmstudio.md), so requests need adapting, not translating.
type Local struct {
	base   *url.URL
	client *http.Client

	mu      sync.Mutex
	models  []Model
	fetched time.Time
}

// Model is one model LM Studio knows about.
type Model struct {
	ID     string
	Loaded bool
	// LoadedContext is the context window the model is loaded with, which
	// is the real limit and often far below MaxContext. Zero if not loaded.
	LoadedContext int
	MaxContext    int
}

// modelsFresh is how long a model list is reused. Short, because models
// are loaded and unloaded by hand while janus runs.
const modelsFresh = 10 * time.Second

// NewLocal returns the LM Studio backend at base. client is used for
// Health and Models; pass nil for one with a short timeout.
func NewLocal(base *url.URL, client *http.Client) *Local {
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	return &Local{base: base, client: client}
}

func (l *Local) Name() string  { return "local" }
func (l *Local) URL() *url.URL { return l.base }

// credentialHeaders must never reach the local server. They are the user's
// Anthropic credentials; LM Studio has no use for them, and local.baseUrl
// may name another machine.
var credentialHeaders = []string{"Authorization", "Proxy-Authorization", "X-Api-Key", "Api-Key", "Cookie"}

// Prepare strips credentials and asks for an uncompressed response, since
// janus rewrites part of it on the way back.
func (l *Local) Prepare(out *http.Request, messages bool) {
	for _, h := range credentialHeaders {
		out.Header.Del(h)
	}
	out.Header.Del(RouteHeader)
	out.Header.Set("Accept-Encoding", "identity")
}

func (l *Local) Health(ctx context.Context) error {
	_, err := l.fetchModels(ctx)
	return err
}

// Models lists what LM Studio has, reusing a recent answer.
func (l *Local) Models(ctx context.Context) ([]Model, error) {
	l.mu.Lock()
	if l.models != nil && time.Since(l.fetched) < modelsFresh {
		models := l.models
		l.mu.Unlock()
		return models, nil
	}
	l.mu.Unlock()

	models, err := l.fetchModels(ctx)
	if err != nil {
		return nil, err
	}
	l.mu.Lock()
	l.models, l.fetched = models, time.Now()
	l.mu.Unlock()
	return models, nil
}

// Model looks up one model by id.
func (l *Local) Model(ctx context.Context, id string) (Model, bool, error) {
	models, err := l.Models(ctx)
	if err != nil {
		return Model{}, false, err
	}
	for _, m := range models {
		if m.ID == id {
			return m, true, nil
		}
	}
	return Model{}, false, nil
}

// ErrNotLMStudio means the server answered, but not the way LM Studio does.
var ErrNotLMStudio = errors.New("the server answered but not like LM Studio")

func (l *Local) fetchModels(ctx context.Context) ([]Model, error) {
	endpoint := strings.TrimRight(l.base.String(), "/") + "/api/v0/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	resp, err := l.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// LM Studio answers 200 even for paths it does not know, with an error
	// in the body, so the body is what has to be checked.
	var list struct {
		Data []struct {
			ID            string `json:"id"`
			State         string `json:"state"`
			LoadedContext int    `json:"loaded_context_length"`
			MaxContext    int    `json:"max_context_length"`
		} `json:"data"`
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if json.Unmarshal(body, &list) != nil || list.Data == nil {
		return nil, fmt.Errorf("%w (HTTP %d)", ErrNotLMStudio, resp.StatusCode)
	}
	models := make([]Model, len(list.Data))
	for i, m := range list.Data {
		models[i] = Model{ID: m.ID, Loaded: m.State == "loaded", LoadedContext: m.LoadedContext, MaxContext: m.MaxContext}
	}
	return models, nil
}

// Stages a request can go through on its way to the local model, as
// recorded in the ledger.
const (
	StageModel        = "set_model"
	StageLiftTools    = "lift_tool_additions"
	StageDropDeferred = "drop_deferred_tools"
	// StageDropThinking is applied on the way upstream, not to the local
	// model; see RepairForUpstream.
	StageDropThinking = "drop_unsigned_thinking"
)

// Normalize rewrites a Claude Code request into one LM Studio accepts, and
// returns the stages it applied.
//
// Three things change (docs/research/traffic.md, "Replay"):
//   - model becomes the local model's id; LM Studio would otherwise answer
//     with whatever happens to be loaded
//   - tool_addition blocks, which carry tool definitions inside messages
//     and which LM Studio rejects, are lifted into the tools array
//   - tools marked defer_loading, placeholders for the above, are dropped
//
// Everything else LM Studio tolerates and is left alone.
func Normalize(req *anthropic.Request, localModel string) (stages []string, err error) {
	req.SetModel(localModel)
	stages = append(stages, StageModel)

	tools := make([]anthropic.Tool, 0, len(req.Tools()))
	known := map[string]bool{}
	dropped := false
	for _, t := range req.Tools() {
		if v, ok := t.Field("defer_loading"); ok && string(v) == "true" {
			dropped = true
			continue
		}
		tools = append(tools, t)
		known[t.Name] = true
	}

	messages := req.Messages()
	lifted := false
	for i, m := range messages {
		var kept []anthropic.Block
		changed := false
		for _, b := range m.Blocks() {
			if b.Type != "tool_addition" {
				kept = append(kept, b)
				continue
			}
			changed = true
			var added struct {
				Tool struct {
					Definition json.RawMessage `json:"definition"`
				} `json:"tool"`
			}
			if err := b.Decode(&added); err != nil || len(added.Tool.Definition) == 0 {
				return nil, fmt.Errorf("messages[%d]: a tool_addition block has no tool definition", i)
			}
			tool, err := anthropic.NewTool(added.Tool.Definition)
			if err != nil {
				return nil, fmt.Errorf("messages[%d]: tool_addition: %w", i, err)
			}
			if !known[tool.Name] {
				tools = append(tools, tool)
				known[tool.Name] = true
			}
		}
		if changed {
			lifted = true
			if len(kept) == 0 {
				// A message cannot be empty; say what used to be there.
				kept = []anthropic.Block{anthropic.NewTextBlock("(tool definitions were added here)")}
			}
			messages[i] = m.WithBlocks(kept)
		}
	}

	if lifted {
		req.SetMessages(messages)
		stages = append(stages, StageLiftTools)
	}
	if dropped {
		stages = append(stages, StageDropDeferred)
	}
	if lifted || dropped {
		req.SetTools(tools)
	}
	return stages, nil
}

// RepairForUpstream removes from a request's history the thinking blocks a
// local model produced, and reports whether it removed any.
//
// The Anthropic API signs the thinking blocks it returns and rejects a
// request that sends one back with a missing or invalid signature
// ("Invalid `signature` in `thinking` block"). A local model's thinking
// blocks have no signature, so a conversation that was answered locally
// for a turn and then goes upstream would fail on them. Thinking is the
// model's scratch work, not part of what it said or did, and dropping an
// earlier turn's is something the API allows.
//
// Blocks the API signed itself are never touched: their signature is not
// empty, including when the thinking text is.
func RepairForUpstream(req *anthropic.Request) (repaired bool) {
	messages := req.Messages()
	for i, m := range messages {
		if m.Role != "assistant" {
			continue
		}
		var kept []anthropic.Block
		dropped := false
		for _, b := range m.Blocks() {
			if b.Type == "thinking" {
				var t anthropic.ThinkingBlock
				if b.Decode(&t) == nil && t.Signature == "" {
					dropped = true
					continue
				}
			}
			kept = append(kept, b)
		}
		if !dropped {
			continue
		}
		if len(kept) == 0 {
			// A message cannot be empty.
			kept = []anthropic.Block{anthropic.NewTextBlock("(no reply)")}
		}
		messages[i] = m.WithBlocks(kept)
		repaired = true
	}
	if repaired {
		req.SetMessages(messages)
	}
	return repaired
}
