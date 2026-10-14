package route

import (
	"fmt"

	"github.com/risadams/janus/internal/anthropic"
)

// leftOutNote is added to the first message of a conversation that was cut
// to fit, so the local model knows it is missing the middle.
const leftOutNote = "<system-reminder>\n%d earlier messages of this conversation were left out, to fit the local model's context. Work from what follows; ask if something you need was in them.\n</system-reminder>"

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
	blocks := append([]anthropic.Block(nil), first.Blocks()...)
	return first.WithBlocks(append(blocks, anthropic.NewTextBlock(fmt.Sprintf(leftOutNote, n))))
}
