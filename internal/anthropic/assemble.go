package anthropic

import (
	"bytes"
	"encoding/json"
	"strings"
)

// Answer is a Messages response put back together: the content blocks a
// stream delivered in pieces, whole.
type Answer struct {
	StopReason string
	Blocks     []AnswerBlock
}

// AnswerBlock is one content block of an Answer.
type AnswerBlock struct {
	Type string
	// Text is the text of a text block, or the thinking of a thinking
	// block.
	Text string
	// Name, ID, and Input describe a tool_use block. Input is the JSON the
	// model produced, exactly as it produced it: it may not be valid.
	Name  string
	ID    string
	Input string
}

// Assemble rebuilds the answer from a response body, which is either an
// event stream or a JSON message. ok is false if the body is neither.
func Assemble(body []byte, contentType string) (answer Answer, ok bool) {
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(contentType)), "text/event-stream") {
		return assembleStream(body)
	}
	var msg struct {
		Type       string `json:"type"`
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type     string          `json:"type"`
			Text     string          `json:"text"`
			Thinking string          `json:"thinking"`
			Name     string          `json:"name"`
			ID       string          `json:"id"`
			Input    json.RawMessage `json:"input"`
		} `json:"content"`
	}
	if json.Unmarshal(body, &msg) != nil || msg.Type != "message" {
		return Answer{}, false
	}
	answer.StopReason = msg.StopReason
	for _, c := range msg.Content {
		answer.Blocks = append(answer.Blocks, AnswerBlock{
			Type: c.Type, Text: c.Text + c.Thinking, Name: c.Name, ID: c.ID, Input: string(c.Input),
		})
	}
	return answer, true
}

// maxBlocks bounds the block index a stream may name, so that a hostile or
// broken stream cannot make Assemble allocate without limit.
const maxBlocks = 4096

func assembleStream(body []byte) (Answer, bool) {
	var answer Answer
	// Pointers, because growing a slice copies its elements, and a
	// strings.Builder that has been written to panics once copied. A
	// stream that writes to an earlier block after a later one has
	// appeared would otherwise crash this (found by fuzzing).
	var text []*strings.Builder // a Builder's writes never fail
	seen := false

	for _, line := range bytes.Split(body, []byte("\n")) {
		payload, ok := bytes.CutPrefix(bytes.TrimRight(line, "\r"), []byte("data:"))
		if !ok {
			continue
		}
		var ev struct {
			Type  string `json:"type"`
			Index int    `json:"index"`
			Block struct {
				Type  string          `json:"type"`
				Name  string          `json:"name"`
				ID    string          `json:"id"`
				Text  string          `json:"text"`
				Input json.RawMessage `json:"input"`
			} `json:"content_block"`
			Delta struct {
				Text        string `json:"text"`
				Thinking    string `json:"thinking"`
				PartialJSON string `json:"partial_json"`
				StopReason  string `json:"stop_reason"`
			} `json:"delta"`
		}
		if json.Unmarshal(payload, &ev) != nil || ev.Index < 0 || ev.Index >= maxBlocks {
			continue
		}
		grow := func() {
			for len(answer.Blocks) <= ev.Index {
				answer.Blocks = append(answer.Blocks, AnswerBlock{})
				text = append(text, &strings.Builder{})
			}
		}
		switch ev.Type {
		case "message_start":
			seen = true
		case "content_block_start":
			grow()
			b := &answer.Blocks[ev.Index]
			b.Type, b.Name, b.ID = ev.Block.Type, ev.Block.Name, ev.Block.ID
			_, _ = text[ev.Index].WriteString(ev.Block.Text)
		case "content_block_delta":
			grow()
			_, _ = text[ev.Index].WriteString(ev.Delta.Text)
			_, _ = text[ev.Index].WriteString(ev.Delta.Thinking)
			_, _ = text[ev.Index].WriteString(ev.Delta.PartialJSON)
		case "message_delta":
			if ev.Delta.StopReason != "" {
				answer.StopReason = ev.Delta.StopReason
			}
		}
	}
	for i := range answer.Blocks {
		if answer.Blocks[i].Type == "tool_use" {
			answer.Blocks[i].Input = text[i].String()
		} else {
			answer.Blocks[i].Text = text[i].String()
		}
	}
	return answer, seen
}
