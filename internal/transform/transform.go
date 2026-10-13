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
// transforms would make of it now, unless the guard finds that changing it
// pays for the cache it breaks.
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
	// cache_control, which the pipeline puts back. An answer that is not
	// smaller than the block is not used.
	Rewrite(at Position, block anthropic.Block) (out anthropic.Block, ok bool)
}

// Reviser is a transform that can also propose changing a block already
// sent, such as an earlier read of a file that a later read supersedes.
// That breaks the cache from the block on, so the proposal is taken only
// when the guard expects it to pay for that. Revise is asked about every
// block sent before, on every request, so it must be cheap.
type Reviser interface {
	Transform
	Revise(at Position, sent anthropic.Block) (out anthropic.Block, ok bool)
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
	// Pricing gives a model's cache rates, for the guard's estimates.
	// Without a price, new blocks are still rewritten when that makes them
	// smaller, which cannot cost more, but blocks already sent are never
	// revised, since nothing shows that it would pay.
	Pricing Pricing
	// Notify, when set, is told when a transform stops being allowed to
	// revise because its revisions have lost money.
	Notify func(transform, why string)

	transforms []Transform
	idle       time.Duration
	now        func() time.Time

	mu            sync.Mutex
	conversations map[string]*memo
	record        map[string]*track
}

// New returns a pipeline that applies transforms in order, each to the
// output of the one before.
func New(transforms ...Transform) *Pipeline {
	return &Pipeline{transforms: transforms, idle: DefaultIdle, now: time.Now,
		conversations: map[string]*memo{}, record: map[string]*track{}}
}

// memo is one conversation: every block it has sent, by the hash of the
// block as the client sent it, with the form it was sent in.
type memo struct {
	mu        sync.Mutex
	sent      map[[sha256.Size]byte]*form
	previous  int // how many messages the last request had
	revisions []*revision
	used      time.Time
	fresh     bool // no request of it has been seen yet
}

// form is how a block was sent: as the client sent it when raw is nil, or
// as raw, which is what the transforms in steps made of it.
type form struct {
	raw   []byte
	steps []step
}

// step is one transform's part in a block's form: the tokens it cut.
type step struct {
	name string
	cut  int
}

// Effect is what one transform did to one request. Token counts are
// estimates from the size of the JSON; dollars are at list price, and
// zero when the model has no price.
type Effect struct {
	Name string
	// Blocks is how many blocks of the request carry this transform's
	// output. Of those, Rewritten were rewritten for the first time and
	// Revised were blocks already sent that were changed. Declined counts
	// answers that were not used: a rewrite that was not smaller, or a
	// revision the guard turned down.
	Blocks, Rewritten, Revised, Declined int
	// Tokens is about how many fewer tokens the request sends for it.
	Tokens int
	// SavedUSD is what those tokens would have cost on this request: at
	// the cache write rate for blocks new to the cache, and the read rate
	// for the rest. CostUSD is what revising cost: the cache from the
	// first revised block on, written again where it would have been read.
	SavedUSD, CostUSD float64
	// ExpectedUSD is what the rewrites and revisions decided on this
	// request are expected to save over the rest of the conversation, net
	// of their cost. It is the guard's estimate, to compare with what is
	// saved in fact.
	ExpectedUSD float64
	// Unpriced is set when the model had no price, so the dollar figures
	// are zero.
	Unpriced bool
}

// Stages names the transforms whose output a request carries, from its
// effects.
func Stages(effects []Effect) []string {
	var names []string
	for _, e := range effects {
		if e.Blocks > 0 {
			names = append(names, e.Name)
		}
	}
	return names
}

// slot is one rewritable block of the request being transformed.
type slot struct {
	msg, blk int
	block    anthropic.Block // as the client sent it, cache_control and all
	key      [sha256.Size]byte
	f        *form
	seen     bool // its form was decided on an earlier request
	cached   bool // and it sits where the last request sent it
	revised  bool // changed on this one
}

// Apply transforms req in place for the conversation header identifies,
// and reports what each transform did or declined to do, in pipeline
// order. It returns nil when no transform had anything to say.
//
// A block already sent in this conversation is sent again exactly as
// before, unless the guard approves a revision. A conversation janus has
// not seen before, but which has a history, may have had that history
// cached upstream in a form janus cannot know, so all of it is sent as it
// is; only the blocks of a conversation's first request, or new on a later
// one, are given to the transforms.
func (p *Pipeline) Apply(header http.Header, req *anthropic.Request) []Effect {
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
	first := m.fresh
	mayRewrite := !first || len(messages) == 1
	m.fresh = false

	rates, priced := p.rates(req)
	remaining := expectedTurns(len(messages))
	effects := map[string]*Effect{}
	effect := func(name string) *Effect {
		if effects[name] == nil {
			effects[name] = &Effect{Name: name, Unpriced: !priced}
		}
		return effects[name]
	}

	var slots []*slot
	for i, msg := range messages {
		for j, b := range msg.Blocks() {
			if !rewritable(b) {
				continue
			}
			s := &slot{msg: i, blk: j, block: b, key: sha256.Sum256(b.Without("cache_control").Raw())}
			s.f, s.seen = m.sent[s.key]
			if !s.seen {
				s.f = &form{}
				if mayRewrite {
					p.rewrite(Position{Request: req, Message: i, Block: j, Role: msg.Role}, b.Without("cache_control"), s.f, effect, rates, remaining)
				}
				m.sent[s.key] = s.f
			}
			// What the last request sent is cached; anything after it is
			// new to the cache, even a repeat of a block sent before.
			s.cached = s.seen && i < m.previous
			slots = append(slots, s)
		}
	}
	m.carry(slots)
	if !first && priced {
		p.revise(m, req, slots, effect, rates, remaining)
	}

	// Build what is sent, and count what it saves.
	changed := false
	for _, s := range slots {
		if s.f.raw == nil {
			continue
		}
		next, err := anthropic.NewBlock(s.f.raw)
		if err != nil {
			continue // cannot happen: raw was a block when it was stored
		}
		if cc, ok := s.block.Field("cache_control"); ok {
			next = next.With("cache_control", cc)
		}
		blocks := messages[s.msg].Blocks()
		out := append([]anthropic.Block(nil), blocks...)
		out[s.blk] = next
		messages[s.msg] = messages[s.msg].WithBlocks(out)
		changed = true

		rate := rates.Read
		if !s.cached {
			rate = rates.Write
		}
		for _, st := range s.f.steps {
			e := effect(st.name)
			e.Blocks++
			e.Tokens += st.cut
			e.SavedUSD += usd(st.cut, rate)
		}
	}
	if changed {
		req.SetMessages(messages)
	}
	m.previous = len(messages)

	var out []Effect
	for _, t := range p.transforms {
		if e := effects[t.Name()]; e != nil {
			out = append(out, *e)
		}
	}
	return out
}

// rewrite runs the transforms over a block new to the conversation, each
// on what the one before made of it, and keeps each answer that makes the
// block smaller.
func (p *Pipeline) rewrite(at Position, block anthropic.Block, f *form, effect func(string) *Effect, rates billed, remaining float64) {
	for _, t := range p.transforms {
		out, ok := t.Rewrite(at, block)
		if !ok {
			continue
		}
		out = out.Without("cache_control")
		e := effect(t.Name())
		cut := tokens(len(block.Raw())) - tokens(len(out.Raw()))
		if cut <= 0 {
			e.Declined++
			continue
		}
		block = out
		f.raw = block.Raw()
		f.steps = append(f.steps, step{t.Name(), cut})
		e.Rewritten++
		// Written to cache once now, then read on every later turn.
		e.ExpectedUSD += usd(cut, rates.Write) + remaining*usd(cut, rates.Read)
	}
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
// those that have been idle too long, settling their revisions' accounts.
func (p *Pipeline) conversation(id string) *memo {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	for k, m := range p.conversations {
		if now.Sub(m.used) > p.idle {
			delete(p.conversations, k)
			p.settle(m)
		}
	}
	m, ok := p.conversations[id]
	if !ok {
		m = &memo{sent: map[[sha256.Size]byte]*form{}, fresh: true}
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
