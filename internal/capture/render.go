package capture

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/risadams/janus/internal/anthropic"
	"github.com/risadams/janus/internal/proxy"
)

// secretHeaders are shown as [redacted] in the Markdown render. The
// Markdown is the file people open, paste, and share, so it must never
// carry a credential.
var secretHeaders = map[string]bool{
	"authorization":       true,
	"proxy-authorization": true,
	"x-api-key":           true,
	"api-key":             true,
	"cookie":              true,
	"set-cookie":          true,
}

// Render turns one exchange into a readable Markdown document: what was
// asked, with what tools, and what came back.
func Render(ex proxy.Exchange) string {
	var b strings.Builder

	fmt.Fprintf(&b, "# %s %s\n\n", ex.Method, ex.Path)
	fmt.Fprintf(&b, "- **ID:** %s\n", ex.ID)
	fmt.Fprintf(&b, "- **Time:** %s\n", ex.Start.Format(time.RFC3339))
	fmt.Fprintf(&b, "- **Status:** %d\n", ex.Status)
	fmt.Fprintf(&b, "- **Duration:** %s", ex.Duration.Round(time.Millisecond))
	if ex.TTFB > 0 {
		fmt.Fprintf(&b, " (first byte after %s)", ex.TTFB.Round(time.Millisecond))
	}
	b.WriteString("\n")
	if ex.Cancelled {
		b.WriteString("- **Cancelled:** the client disconnected before the response finished\n")
	}
	if s := ex.Summary; s != nil {
		if s.Error != nil {
			fmt.Fprintf(&b, "- **Error:** %s: %s\n", s.Error.Type, s.Error.Message)
		} else {
			fmt.Fprintf(&b, "- **Answered by:** %s, stop reason %s\n", orDash(s.Model), orDash(s.StopReason))
			fmt.Fprintf(&b, "- **Tokens:** %s\n", usageLine(s.Usage))
			if !s.Complete {
				b.WriteString("- **Incomplete:** the response ended before it should have\n")
			}
		}
	}
	if ex.Unread != "" {
		fmt.Fprintf(&b, "- **Response not read:** %s\n", ex.Unread)
	}
	if ex.Fault != "" {
		fmt.Fprintf(&b, "- **Fault:** %s\n", ex.Fault)
	}
	b.WriteString("\n")

	writeHeaders(&b, ex)
	writeRequest(&b, ex)
	writeResponse(&b, ex)
	return b.String()
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func usageLine(u anthropic.Usage) string {
	parts := []string{fmt.Sprintf("input %d", u.Input)}
	if u.CacheWrite != nil {
		parts = append(parts, fmt.Sprintf("cache write %d", *u.CacheWrite))
	}
	if u.CacheRead != nil {
		parts = append(parts, fmt.Sprintf("cache read %d", *u.CacheRead))
	}
	parts = append(parts, fmt.Sprintf("output %d", u.Output))
	if u.Thinking != nil {
		parts = append(parts, fmt.Sprintf("of which thinking %d", *u.Thinking))
	}
	return strings.Join(parts, ", ")
}

func writeHeaders(b *strings.Builder, ex proxy.Exchange) {
	b.WriteString("<headers>\n\n")
	keys := make([]string, 0, len(ex.Header))
	for k := range ex.Header {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := strings.Join(ex.Header[k], ", ")
		if secretHeaders[strings.ToLower(k)] {
			v = "[redacted]"
		}
		fmt.Fprintf(b, "%s: %s\n", k, v)
	}
	b.WriteString("\n</headers>\n\n")
}

// writeRequest tags out the parts of a Messages request. Anything that is
// not one, or does not parse, is shown as pretty-printed JSON, or as it is.
func writeRequest(b *strings.Builder, ex proxy.Exchange) {
	req, err := anthropic.ParseRequest(ex.RequestBody)
	if err != nil || !ex.IsMessages() {
		b.WriteString("<request>\n\n")
		fence(b, "json", pretty(ex.RequestBody))
		b.WriteString("</request>\n\n")
		return
	}
	if ex.RequestTruncated {
		b.WriteString("_The request was too large to keep whole; what follows is cut short._\n\n")
	}

	fmt.Fprintf(b, "<model>\n\n%s\n\n</model>\n\n", req.Model())

	if system := req.System(); len(system) > 0 {
		b.WriteString("<system-prompt>\n\n")
		writeBlocks(b, system)
		b.WriteString("</system-prompt>\n\n")
	}

	if tools := req.Tools(); len(tools) > 0 {
		names := make([]string, len(tools))
		for i, t := range tools {
			names[i] = t.Name
		}
		fmt.Fprintf(b, "<tools>\n\n%d tools: %s\n\n", len(tools), strings.Join(names, ", "))
		raws := make([]json.RawMessage, len(tools))
		for i, t := range tools {
			raws[i] = t.Raw()
		}
		all, _ := json.Marshal(raws)
		fence(b, "json", pretty(all))
		b.WriteString("</tools>\n\n")
	}

	b.WriteString("<messages>\n\n")
	for _, m := range req.Messages() {
		fmt.Fprintf(b, "### %s\n\n", m.Role)
		writeBlocks(b, m.Blocks())
	}
	b.WriteString("</messages>\n\n")
}

// writeBlocks shows text as text, since that is what a reader is looking
// for, and every other block as JSON.
func writeBlocks(b *strings.Builder, blocks []anthropic.Block) {
	if len(blocks) == 0 {
		b.WriteString("_(empty)_\n\n")
	}
	for _, block := range blocks {
		if block.Type == "text" {
			b.WriteString(block.Text())
			b.WriteString("\n\n")
			continue
		}
		fence(b, "json", pretty(block.Raw()))
	}
}

// writeResponse shows a streamed response as the message it adds up to,
// which is far shorter than its events. The events themselves are in the
// .response.txt file.
func writeResponse(b *strings.Builder, ex proxy.Exchange) {
	b.WriteString("<response>\n\n")
	defer b.WriteString("</response>\n")

	body := ex.ResponseBody
	switch {
	case len(bytes.TrimSpace(body)) == 0:
		b.WriteString("_(empty body)_\n\n")
	case ex.Unread != "" && ex.Summary == nil:
		fmt.Fprintf(b, "_%d bytes that janus could not decode (%s)._\n\n", len(body), ex.Unread)
	case isEventStream(ex):
		if ex.ResponseTruncated {
			b.WriteString("_The response was too large to keep whole; what follows is cut short._\n\n")
		}
		writeAssembled(b, body)
	default:
		fence(b, "json", pretty(body))
	}
}

func isEventStream(ex proxy.Exchange) bool {
	return strings.HasPrefix(strings.ToLower(ex.ResponseHeader.Get("Content-Type")), "text/event-stream")
}

// writeAssembled replays a Messages event stream into its content blocks:
// text and thinking joined up, tool calls with their arguments whole.
func writeAssembled(b *strings.Builder, stream []byte) {
	type block struct {
		kind, name, id string
		text           strings.Builder
		signed         bool
	}
	var blocks []*block
	at := func(i int) *block {
		for len(blocks) <= i {
			blocks = append(blocks, &block{})
		}
		return blocks[i]
	}

	for _, line := range strings.Split(string(stream), "\n") {
		payload, ok := strings.CutPrefix(strings.TrimRight(line, "\r"), "data:")
		if !ok {
			continue
		}
		var ev struct {
			Type  string `json:"type"`
			Index int    `json:"index"`
			Block struct {
				Type string `json:"type"`
				Name string `json:"name"`
				ID   string `json:"id"`
			} `json:"content_block"`
			Delta struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				Thinking    string `json:"thinking"`
				PartialJSON string `json:"partial_json"`
			} `json:"delta"`
			Error *anthropic.APIError `json:"error"`
		}
		if json.Unmarshal([]byte(payload), &ev) != nil || ev.Index < 0 || ev.Index > 10000 {
			continue
		}
		switch ev.Type {
		case "content_block_start":
			blk := at(ev.Index)
			blk.kind, blk.name, blk.id = ev.Block.Type, ev.Block.Name, ev.Block.ID
		case "content_block_delta":
			blk := at(ev.Index)
			blk.text.WriteString(ev.Delta.Text)
			blk.text.WriteString(ev.Delta.Thinking)
			blk.text.WriteString(ev.Delta.PartialJSON)
			if ev.Delta.Type == "signature_delta" {
				blk.signed = true
			}
		case "error":
			if ev.Error != nil {
				fmt.Fprintf(b, "**error** %s: %s\n\n", ev.Error.Type, ev.Error.Message)
			}
		}
	}

	if len(blocks) == 0 {
		b.WriteString("_(no content blocks)_\n\n")
	}
	for _, blk := range blocks {
		switch blk.kind {
		case "text":
			fmt.Fprintf(b, "**text**\n\n%s\n\n", blk.text.String())
		case "thinking":
			text := blk.text.String()
			if text == "" && blk.signed {
				text = "_(signed; the text was not sent)_"
			}
			fmt.Fprintf(b, "**thinking**\n\n%s\n\n", text)
		case "tool_use":
			fmt.Fprintf(b, "**tool_use** %s (%s)\n\n", blk.name, blk.id)
			fence(b, "json", pretty([]byte(blk.text.String())))
		default:
			fmt.Fprintf(b, "**%s**\n\n", orDash(blk.kind))
			if blk.text.Len() > 0 {
				fence(b, "", blk.text.String())
			}
		}
	}
}

// fence writes a fenced code block, with a fence longer than any run of
// backticks inside it so the content cannot close it early.
func fence(b *strings.Builder, lang, content string) {
	ticks := "```"
	for strings.Contains(content, ticks) {
		ticks += "`"
	}
	fmt.Fprintf(b, "%s%s\n%s\n%s\n\n", ticks, lang, content, ticks)
}

// pretty indents JSON, and returns anything else as it is.
func pretty(data []byte) string {
	if len(bytes.TrimSpace(data)) == 0 {
		return "(empty)"
	}
	var out bytes.Buffer
	if err := json.Indent(&out, data, "", "  "); err != nil {
		return string(data)
	}
	return out.String()
}
