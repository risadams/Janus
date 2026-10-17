package transform

import (
	"encoding/json"
	"fmt"

	"github.com/risadams/janus/internal/anthropic"
)

// The net-savings guard. Prompt caching makes a token's price depend on
// where it sits: a block new to the cache is written once at the write
// rate, then read on every later turn at the read rate, which for current
// models is a tenth to a fortieth of it. So a rewrite of a new block saves
// money whenever it makes the block smaller, and the guard only checks
// that. Changing a block already sent is different: the cache breaks
// there, and everything after it is written again. That pays only when the
// tokens cut, read on enough later turns, outweigh the rewrite, and the
// guard takes a revision only when it expects that.

// Rates is a model's cache prices, in US dollars per million tokens.
type Rates struct {
	Read, Write5m, Write1h float64
}

// Pricing gives a model's rates, or false when it has none.
type Pricing func(model string) (Rates, bool)

// billed is what one request pays per million tokens: to read the cache,
// and to write it at the lifetime the request asks for.
type billed struct {
	Read, Write float64
}

// rates finds what req is billed at. Claude Code asks for an hour's cache
// on its main loop and five minutes on subagents, and says so on the
// breakpoint that moves along the conversation's tail.
func (p *Pipeline) rates(req *anthropic.Request) (billed, bool) {
	if p.Pricing == nil {
		return billed{}, false
	}
	r, ok := p.Pricing(req.Model())
	if !ok {
		return billed{}, false
	}
	b := billed{Read: r.Read, Write: r.Write5m}
	if ttl(req) == "1h" {
		b.Write = r.Write1h
	}
	return b, true
}

// ttl is the cache lifetime asked for on the request's last breakpoint.
func ttl(req *anthropic.Request) string {
	last := ""
	for _, m := range req.Messages() {
		for _, b := range m.Blocks() {
			raw, ok := b.Field("cache_control")
			if !ok {
				continue
			}
			var cc struct {
				TTL string `json:"ttl"`
			}
			// A cache_control whose ttl is not a string is one the API
			// refuses, so the request fails whatever it is priced at.
			_ = json.Unmarshal(raw, &cc)
			last = cc.TTL
		}
	}
	return last
}

// bytesPerToken matches the router's deliberately rough count:
// docs/research/traffic.md measured 2.75 to 4.1 bytes per token.
const bytesPerToken = 3

func tokens(bytes int) int { return (bytes + bytesPerToken - 1) / bytesPerToken }

func usd(tokens int, perMillion float64) float64 { return float64(tokens) * perMillion / 1e6 }

// expectedTurns is how many more times a request's blocks are expected to
// be sent. A conversation that has gone on for n turns tends to go on
// about as long again: on the 2026-10-12 ledger that guess came to 1.01
// times the turns that actually followed, over four conversations, but
// was within a factor of two of the truth only a third of the time. The
// guard takes half of it, because a revision overestimated costs far more
// than one passed up; janus stats compares the estimate with the outcome.
func expectedTurns(messages int) float64 {
	turns := float64(messages) / 2 // a user message and an answer each
	if turns/2 < 1 {
		return 1
	}
	return turns / 2
}

// revision is a set of blocks changed on one request: what it cost, what
// it cut, and how many later requests carried it, which is what it was
// worth.
type revision struct {
	keys  map[[32]byte]bool
	cut   map[string]int // tokens, by transform
	cost  float64
	read  float64
	turns int
}

// carry counts, for each revision, whether this request still sends any
// of its blocks.
func (m *memo) carry(slots []*slot) {
	for _, r := range m.revisions {
		for _, s := range slots {
			if r.keys[s.key] {
				r.turns++
				break
			}
		}
	}
}

// revise asks the transforms that can revise for changes to blocks already
// sent, and takes the set that the guard expects to pay best, if any does.
// Breaking the cache at one block means every block sent before from there
// on is written again, so the only choice is where to start: from that
// block on, every proposal is taken.
func (p *Pipeline) revise(m *memo, req *anthropic.Request, slots []*slot, effect func(string) *Effect, rates billed, remaining float64) {
	type proposal struct {
		slot  int
		raw   []byte
		steps []step
	}
	var proposals []proposal
	messages := req.Messages()
	for i, s := range slots {
		if !s.cached {
			continue
		}
		current := s.block.Without("cache_control")
		if s.f.raw != nil {
			current, _ = anthropic.NewBlock(s.f.raw)
		}
		var steps []step
		for _, t := range p.transforms {
			r, ok := t.(Reviser)
			if !ok || p.disabled(t.Name()) != "" {
				continue
			}
			out, ok := r.Revise(Position{Request: req, Message: s.msg, Block: s.blk, Role: messages[s.msg].Role}, current)
			if !ok {
				continue
			}
			out = out.Without("cache_control")
			cut := tokens(len(current.Raw())) - tokens(len(out.Raw()))
			if cut <= 0 {
				effect(t.Name()).Declined++
				continue
			}
			current = out
			steps = append(steps, step{t.Name(), cut})
		}
		if steps != nil {
			proposals = append(proposals, proposal{i, current.Raw(), steps})
		}
	}
	if len(proposals) == 0 {
		return
	}

	// What each block sent before would weigh with every proposal taken.
	// Thinking is not a slot but is cached all the same, so the cost is
	// counted over every block, up to the last one sent before.
	revised := map[int][]byte{}
	for _, pr := range proposals {
		revised[pr.slot] = pr.raw
	}
	type cell struct{ msg, blk, size int }
	var cells []cell
	lastSeen := -1
	si := 0
	for i, msg := range messages {
		for j, b := range msg.Blocks() {
			size := len(b.Without("cache_control").Raw())
			if si < len(slots) && slots[si].msg == i && slots[si].blk == j {
				s := slots[si]
				switch raw, ok := revised[si]; {
				case ok:
					size = len(raw)
				case s.f.raw != nil:
					size = len(s.f.raw)
				}
				if s.cached {
					lastSeen = len(cells)
				}
				si++
			}
			cells = append(cells, cell{i, j, size})
		}
	}
	at := func(slot int) int {
		for c, cl := range cells {
			if cl.msg == slots[slot].msg && cl.blk == slots[slot].blk {
				return c
			}
		}
		return len(cells)
	}

	best, bestNet, bestCost := -1, 0.0, 0.0
	for k := range proposals {
		cut := 0
		for _, pr := range proposals[k:] {
			for _, st := range pr.steps {
				cut += st.cut
			}
		}
		rewritten := 0
		for c := at(proposals[k].slot); c <= lastSeen; c++ {
			rewritten += tokens(cells[c].size)
		}
		cost := usd(rewritten, rates.Write-rates.Read)
		// Read smaller on this turn and on every turn expected after it.
		if net := (remaining+1)*usd(cut, rates.Read) - cost; net > bestNet {
			best, bestNet, bestCost = k, net, cost
		}
	}
	taken := proposals
	if best < 0 {
		taken = nil
		best = len(proposals)
	} else {
		taken = proposals[best:]
	}
	for _, pr := range proposals[:best] {
		for _, st := range pr.steps {
			effect(st.name).Declined++
		}
	}
	if len(taken) == 0 {
		return
	}

	r := &revision{keys: map[[32]byte]bool{}, cut: map[string]int{}, cost: bestCost, read: rates.Read}
	total := 0
	for _, pr := range taken {
		s := slots[pr.slot]
		f := &form{raw: pr.raw, steps: append(append([]step(nil), s.f.steps...), pr.steps...)}
		m.sent[s.key] = f
		s.f, s.revised = f, true
		r.keys[s.key] = true
		for _, st := range pr.steps {
			effect(st.name).Revised++
			r.cut[st.name] += st.cut
			total += st.cut
		}
	}
	// The cost and the expectation are shared by what each transform cut.
	for name, cut := range r.cut {
		share := float64(cut) / float64(total)
		e := effect(name)
		e.CostUSD += bestCost * share
		e.ExpectedUSD += bestNet * share
	}
	m.revisions = append(m.revisions, r)
}

// track is the outcome of a transform's settled revisions, most recent
// last, and why it may no longer revise, if it may not.
type track struct {
	outcomes []float64
	disabled string
}

// Revisions are judged on their last window settled; a transform is
// stopped once at least minSettled of them show a loss overall.
const (
	window     = 20
	minSettled = 5
)

// settle closes the accounts of a forgotten conversation's revisions. Each
// was worth its cut, read on the turn it was made and on every later turn
// that carried it, less its cost.
func (p *Pipeline) settle(m *memo) {
	for _, r := range m.revisions {
		total := 0
		for _, cut := range r.cut {
			total += cut
		}
		for name, cut := range r.cut {
			worth := float64(1+r.turns)*usd(cut, r.read) - r.cost*float64(cut)/float64(total)
			t := p.record[name]
			if t == nil {
				t = &track{}
				p.record[name] = t
			}
			t.outcomes = append(t.outcomes, worth)
			if len(t.outcomes) > window {
				t.outcomes = t.outcomes[len(t.outcomes)-window:]
			}
			sum := 0.0
			for _, o := range t.outcomes {
				sum += o
			}
			if t.disabled == "" && len(t.outcomes) >= minSettled && sum < 0 {
				t.disabled = fmt.Sprintf("its last %d revisions lost $%.4f in all: the cache they broke cost more than the tokens they cut saved", len(t.outcomes), -sum)
				if p.Notify != nil {
					p.Notify(name, t.disabled)
				}
			}
		}
	}
	m.revisions = nil
}

// disabled says why a transform may no longer revise, or "" if it may.
func (p *Pipeline) disabled(name string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if t := p.record[name]; t != nil {
		return t.disabled
	}
	return ""
}

// Disabled lists the transforms that may no longer revise, with why.
func (p *Pipeline) Disabled() map[string]string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := map[string]string{}
	for name, t := range p.record {
		if t.disabled != "" {
			out[name] = t.disabled
		}
	}
	return out
}
