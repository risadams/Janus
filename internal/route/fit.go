package route

import (
	"fmt"
	"sync"

	"github.com/risadams/janus/internal/anthropic"
)

// leftOutNote is added to the first message of a conversation that was cut
// to fit, so the local model knows it is missing the middle.
const leftOutNote = "<system-reminder>\n%d earlier messages of this conversation were left out, to fit the local model's context. Work from what follows; ask if something you need was in them.\n</system-reminder>"

// openaiLeftOutNote is the same for an OpenAI model with a smaller window
// than the conversation (#407).
const openaiLeftOutNote = "<system-reminder>\n%d earlier messages of this conversation were left out, to fit this model's context window. Work from what follows; ask if something you need was in them.\n</system-reminder>"

// answerReserve is the part of a local model's context kept free for its
// answer when a conversation is cut to fit: a quarter, at most 16,384
// tokens. A prompt that fills the context leaves the model no room to
// answer at all.
func answerReserve(context int) int {
	return min(context/4, 16384)
}

// fitConversation cuts req's messages so that the request comes to no more
// than budget tokens, for a local model standing in on a conversation too
// long for it (#171). It keeps the system prompt, the tools, the first
// message (which carries Claude Code's instructions and the user's first
// request), and as many of the latest messages as fit, and says in the
// first message how many were left out. It returns how many were left
// out, or ok false if even the first and last turn do not fit.
//
// The kept part starts at an assistant message, so that the conversation
// still alternates user and assistant, and every tool result still follows
// the tool use it answers: a tool result whose tool use was left out would
// make the request invalid.
//
// Only the local model's copy is cut; what goes to Claude, and so its
// prompt cache, is untouched.
func fitConversation(req *anthropic.Request, budget int) (leftOut int, ok bool) {
	messages := req.Messages()
	if len(messages) < 3 || messages[0].Role != "user" {
		return 0, false
	}
	// Bytes each message adds to the body: its JSON and the comma before
	// it. EstimateTokens works from bytes, so this predicts its answer.
	size := make([]int, len(messages))
	for i, m := range messages {
		size[i] = len(m.Raw()) + 1
	}
	over := len(req.Bytes()) - budget*bytesPerToken
	if over <= 0 {
		return 0, true
	}
	note := len(fmt.Sprintf(leftOutNote, len(messages))) + 64 // the note and its block's JSON

	// The earliest assistant message from which the rest fits.
	removed := 0
	for start := 1; start < len(messages); start++ {
		if messages[start].Role == "assistant" && start > 1 && removed-note >= over {
			cut := append([]anthropic.Message{withNote(messages[0], start-1)}, messages[start:]...)
			req.SetMessages(cut)
			if EstimateTokens(req.Bytes()) <= budget {
				return start - 1, true
			}
			req.SetMessages(messages)
		}
		removed += size[start]
	}
	return 0, false
}

// withNote adds the note saying n messages were left out to the end of the
// first message.
func withNote(first anthropic.Message, n int) anthropic.Message {
	return withNoteText(first, leftOutNote, n)
}

func withNoteText(first anthropic.Message, note string, n int) anthropic.Message {
	blocks := append([]anthropic.Block(nil), first.Blocks()...)
	return first.WithBlocks(append(blocks, anthropic.NewTextBlock(fmt.Sprintf(note, n))))
}

// fitAt cuts req's messages to the first one and those from start on,
// with the note saying how many were left out, if start is a place the
// conversation can be cut (an assistant message, as fitConversation
// says) and the result comes to no more than budget tokens. Otherwise it
// leaves req as it was and reports false.
func fitAt(req *anthropic.Request, start, budget int, note string) bool {
	messages := req.Messages()
	if start < 2 || start >= len(messages) || messages[start].Role != "assistant" || messages[0].Role != "user" {
		return false
	}
	req.SetMessages(append([]anthropic.Message{withNoteText(messages[0], note, start-1)}, messages[start:]...))
	if EstimateTokens(req.Bytes()) <= budget {
		return true
	}
	req.SetMessages(messages)
	return false
}

// fitStably cuts req to fit budget for a provider whose prompt cache the
// cut must not break on every turn (#407). fitConversation cuts as little
// as it can, so the place moves as the conversation grows and every turn
// would be a fresh cache write. Here the cut stays where it was last time,
// at the message prev, for as long as the conversation still fits from
// there; when it no longer does, it moves on far enough to fit half of
// budget, so that it can stay put again for many turns. It returns where
// it cut (0 for not at all) and false if even the first and latest turns
// do not fit.
func fitStably(req *anthropic.Request, budget, prev int, note string) (start int, ok bool) {
	if EstimateTokens(req.Bytes()) <= budget && prev == 0 {
		return 0, true
	}
	if prev > 0 && fitAt(req, prev, budget, note) {
		return prev, true
	}
	messages := req.Messages()
	for _, target := range []int{budget / 2, budget} {
		for s := max(prev+1, 2); s < len(messages); s++ {
			if fitAt(req, s, target, note) {
				// Within the half it was cut for, which is within budget.
				return s, true
			}
		}
	}
	return 0, false
}

// cuts remembers, by session, the message a conversation was last cut at.
type cuts struct {
	mu sync.Mutex
	at map[string]int
}

func (c *cuts) get(session string) int {
	if session == "" {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at[session]
}

func (c *cuts) set(session string, start int) {
	if session == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.at == nil {
		c.at = map[string]int{}
	}
	c.at[session] = start
}
