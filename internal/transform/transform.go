// Package transform rewrites the content of requests bound upstream, to
// send fewer tokens, without breaking prompt caching.
//
// The API caches a request's prefix, and Claude Code's turns each read
// everything sent before from cache and write only the new tail
// (docs/research/traffic.md). A block that has been sent once is part of
// that cached prefix, so it must be sent in the same bytes on every later
// turn: any change to it turns the rest of the conversation from cheap
// cache reads into a fresh cache write. The pipeline guarantees this with
// a memo per conversation. The first time a block is seen, the transforms
// decide its form; from then on it is sent in that form, whatever the
// transforms would make of it now.
package transform

import (
	"crypto/sha256"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/risadams/janus/internal/anthropic"
)

// Transform rewrites content blocks.
type Transform interface {
	// Name is recorded as a stage in the ledger of every request that
	// carries this transform's output.
	Name() string
	// Rewrite returns what to send in place of block, or ok false to leave
	// it as it is. It is asked once per distinct block per conversation,
	// and its answer is kept, so it need not be deterministic: a local
	// model's summary is fine. The block is given without its
	// cache_control, which the pipeline puts back.
	Rewrite(at Position, block anthropic.Block) (out anthropic.Block, ok bool)
}

// Position is where a block sits. Request is the whole request, for a
// transform that needs context; it must not be changed.
type Position struct {
	Request *anthropic.Request
	Message int
	Block   int
	Role    string
}

// DefaultIdle is how long a conversation's memo is kept after its last
// request. Claude Code's main loop caches for an hour and its subagents
// for five minutes, so after an hour nothing a conversation sent is still
// cached, and a block sent in another form costs nothing extra.
const DefaultIdle = time.Hour

// Pipeline applies transforms to requests, keeping each conversation's
// blocks in the form they were first sent.
type Pipeline struct {
	transforms []Transform
	idle       time.Duration
	now        func() time.Time

	mu            sync.Mutex
	conversations map[string]*memo
}

// New returns a pipeline that applies transforms in order, each to the
// output of the one before.
func New(transforms ...Transform) *Pipeline {
	return &Pipeline{transforms: transforms, idle: DefaultIdle, now: time.Now, conversations: map[string]*memo{}}
}

// memo is one conversation: every block it has sent, by the hash of the
// block as the client sent it, with the form it was sent in.
type memo struct {
	mu    sync.Mutex
	sent  map[[sha256.Size]byte]form
	used  time.Time
	fresh bool // no request of it has been seen yet
}

// form is how a block was sent: as the client sent it when raw is nil, or
// as raw, which is what the transforms named in by made of it.
type form struct {
	raw []byte
	by  []string
}

// Apply transforms req in place for the conversation header identifies,
// and returns the names of the transforms whose output it now carries, in
// pipeline order. It changes nothing, and returns nil, when there are no
// transforms or no blocks to change.
//
// A block already sent in this conversation is sent again exactly as
// before. A conversation janus has not seen before, but which has a
// history, may have had that history cached upstream in a form janus
// cannot know, so all of it is sent as it is; only the blocks of a
// conversation's first request, or new on a later one, are given to the
// transforms.
func (p *Pipeline) Apply(header http.Header, req *anthropic.Request) []string {
	if p == nil || len(p.transforms) == 0 {
		return nil
	}
	messages := req.Messages()
	if len(messages) == 0 {
		return nil
	}
	m := p.conversation(Conversation(header, req))
	m.mu.Lock()
	defer m.mu.Unlock()
	// The first request seen of a conversation already under way repeats
	// a history that may be cached upstream in a form janus cannot know.
	mayRewrite := !m.fresh || len(messages) == 1
	m.fresh = false

	used := map[string]bool{}
	changed := false
	for i, msg := range messages {
		blocks := msg.Blocks()
		var out []anthropic.Block
		for j, b := range blocks {
			if !rewritable(b) {
				continue
			}
			bare := b.Without("cache_control")
			key := sha256.Sum256(bare.Raw())
			f, seen := m.sent[key]
			if !seen {
				if mayRewrite {
					f = p.rewrite(Position{Request: req, Message: i, Block: j, Role: msg.Role}, bare)
				}
				m.sent[key] = f
			}
			if f.raw == nil {
				continue
			}
			next, err := anthropic.NewBlock(f.raw)
			if err != nil {
				continue // cannot happen: f.raw was a block when it was stored
			}
			if cc, ok := b.Field("cache_control"); ok {
				next = next.With("cache_control", cc)
			}
			if out == nil {
				out = append([]anthropic.Block(nil), blocks...)
			}
			out[j] = next
			for _, name := range f.by {
				used[name] = true
			}
		}
		if out != nil {
			messages[i] = msg.WithBlocks(out)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	req.SetMessages(messages)
	var names []string
	for _, t := range p.transforms {
		if used[t.Name()] {
			names = append(names, t.Name())
		}
	}
	return names
}

// rewrite runs the transforms over one block, each on what the one before
// made of it.
func (p *Pipeline) rewrite(at Position, block anthropic.Block) form {
	var f form
	for _, t := range p.transforms {
		out, ok := t.Rewrite(at, block)
		if !ok {
			continue
		}
		block = out.Without("cache_control")
		f.raw = block.Raw()
		f.by = append(f.by, t.Name())
	}
	return f
}

// rewritable reports whether transforms may touch a block at all. Thinking
// carries a signature over its exact content, which the API checks
// (docs/research/integrity.md).
func rewritable(b anthropic.Block) bool {
	switch b.Type {
	case "thinking", "redacted_thinking":
		return false
	}
	return true
}

// conversation finds or starts the memo for a conversation, and forgets
// those that have been idle too long.
func (p *Pipeline) conversation(id string) *memo {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	for k, m := range p.conversations {
		if now.Sub(m.used) > p.idle {
			delete(p.conversations, k)
		}
	}
	m, ok := p.conversations[id]
	if !ok {
		m = &memo{sent: map[[sha256.Size]byte]form{}, fresh: true}
		p.conversations[id] = m
	}
	m.used = now
	return m
}

// Conversation names the conversation a request belongs to. Claude Code
// sends its session id on every request, and an agent id on a subagent's,
// whose conversation is its own. Any other client's conversation is known
// by its first message, which every later request repeats.
func Conversation(header http.Header, req *anthropic.Request) string {
	if session := header.Get("X-Claude-Code-Session-Id"); session != "" {
		return session + "/" + header.Get("X-Claude-Code-Agent-Id")
	}
	var first []byte
	if messages := req.Messages(); len(messages) > 0 {
		first = messages[0].Raw()
	}
	return fmt.Sprintf("first-message/%x", sha256.Sum256(first))
}
