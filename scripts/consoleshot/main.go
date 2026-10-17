// Command consoleshot makes the picture of the Janus console in the docs
// (#223): it runs a scratch Janus against stand-ins for the Anthropic API
// and LM Studio, sends it made-up requests shaped like Claude Code's, keeps
// the console's colored output, and draws it as an SVG terminal window.
//
//	go run ./scripts/consoleshot
//
// Run it from the repository's root, which it builds Janus from.
// It writes docs/assets/janus-console.ansi and janus-console.svg. Nothing
// real is involved: no traffic, no account, no credentials. Run it again
// after a change to the console's format, and commit both files.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"html"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
)

func main() {
	out := flag.String("out", "docs/assets/janus-console", "path for the .ansi and .svg files, without the extension")
	flag.Parse()
	log.SetFlags(0)

	console, err := capture()
	if err != nil {
		log.Fatalf("consoleshot: %v", err)
	}
	// The picture is committed to docs/assets, so it is written as any
	// file in the repository is.
	if err := os.WriteFile(*out+".ansi", console, 0o644); err != nil { // #nosec G306 -- a committed docs asset
		log.Fatalf("consoleshot: %v", err)
	}
	title := "The Janus console: one line per request, with main-loop turns on Claude, subagent turns on a local model, and a note where the local answer failed its checks"
	if err := os.WriteFile(*out+".svg", render(string(console), title), 0o644); err != nil { // #nosec G306 -- a committed docs asset
		log.Fatalf("consoleshot: %v", err)
	}
	log.Printf("wrote %s.ansi and %s.svg", *out, *out)
}

// capture runs the scratch Janus, sends it the requests, and returns what
// it printed for them.
func capture() ([]byte, error) {
	dir, err := os.MkdirTemp("", "consoleshot")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)

	binary := filepath.Join(dir, "janus")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	if out, err := exec.Command("go", "build", "-o", binary, "./cmd/janus").CombinedOutput(); err != nil { // #nosec G204 -- fixed arguments; binary is in this run's temp directory
		return nil, fmt.Errorf("building janus: %v\n%s", err, out)
	}

	upstream, err := serve(http.HandlerFunc(anthropic))
	if err != nil {
		return nil, err
	}
	studio, err := serve(http.HandlerFunc(lmStudio))
	if err != nil {
		return nil, err
	}
	listen, err := freeAddress()
	if err != nil {
		return nil, err
	}
	config := fmt.Sprintf(`{
  "listen": %q,
  "upstream": { "baseUrl": "http://%s" },
  "local": { "baseUrl": "http://%s", "models": { "janus-local": { "model": "qwen/qwen3.8-27b" } } },
  "routing": [ { "name": "subagents", "class": "subagent", "route": "local-then-escalate", "local": "janus-local" } ],
  "features": { "route": true, "escalate": false, "compress": false, "guard": false },
  "logging": { "dir": "logs", "capture": false }
}`, listen, upstream, studio)
	configPath := filepath.Join(dir, "janus.config.json")
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		return nil, err
	}

	// The request lines go to standard error; the start-up banner, with
	// this run's scratch paths, goes to standard output and is not kept.
	var console bytes.Buffer
	janus := exec.Command(binary, "-config", configPath) // #nosec G204 -- the janus just built, with the config just written
	janus.Dir = dir
	janus.Env = append(os.Environ(), "FORCE_COLOR=1", "NO_COLOR=")
	janus.Stderr = &console
	if err := janus.Start(); err != nil {
		return nil, err
	}
	defer janus.Process.Kill()
	if err := waitFor(listen); err != nil {
		return nil, fmt.Errorf("janus did not start: %v\n%s", err, console.String())
	}

	if err := converse("http://" + listen + "/v1/messages"); err != nil {
		return nil, err
	}
	time.Sleep(500 * time.Millisecond) // the last line is printed after the response
	// Killed on purpose, so the exit status says nothing worth checking.
	_ = janus.Process.Kill()
	_ = janus.Wait()
	return console.Bytes(), nil
}

// converse sends the made-up session: a main-loop turn, a subagent whose
// first turn is answered locally while Claude Code asks for its status
// line, a second local turn, one the local model fails, and a last
// main-loop turn. The X-Demo header tells the stand-in Anthropic what to
// answer: input, cache-read, and cache-write tokens, output tokens, the
// stop reason, and seconds to wait.
func converse(endpoint string) error {
	send := func(demo, agent, model, prompt string) error {
		body, _ := json.Marshal(map[string]any{
			"model": model, "stream": true, "max_tokens": 32000,
			"messages": []map[string]string{{"role": "user", "content": prompt}},
		})
		req, _ := http.NewRequest("POST", endpoint, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Anthropic-Version", "2023-06-01")
		req.Header.Set("X-Api-Key", "demo")
		req.Header.Set("X-Claude-Code-Session-Id", "demo-session")
		if agent != "" {
			req.Header.Set("X-Claude-Code-Agent-Id", agent)
		}
		if demo != "" {
			req.Header.Set("X-Demo", demo)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		_, _ = io.Copy(io.Discard, resp.Body) // only the console line matters, not the answer
		return resp.Body.Close()
	}

	if err := send("6,148910,2290,2310,tool_use,3.4", "", "claude-opus-5-5", "Why does the proxy test hang on Linux?"); err != nil {
		return err
	}
	var wg sync.WaitGroup
	var subagentErr error
	wg.Go(func() {
		subagentErr = send("", "a1", "claude-sonnet-5-5", "Find where the proxy reports an exchange.")
	})
	time.Sleep(1200 * time.Millisecond)
	if err := send("4,2210,0,9,end_turn,0.6", "a1", "claude-haiku-4-5", "Describe your most recent action in 3-5 words"); err != nil {
		return err
	}
	wg.Wait()
	if subagentErr != nil {
		return subagentErr
	}
	for _, turn := range []struct{ demo, agent, model, prompt string }{
		{"", "a1", "claude-sonnet-5-5", "Read proxy.go around the deferred report."},
		{"5,61200,1400,880,end_turn,4.1", "a1", "claude-sonnet-5-5", "Summarise what you found."},
		{"8,153400,1810,1120,end_turn,2.6", "", "claude-opus-5-5", "Go on."},
	} {
		if err := send(turn.demo, turn.agent, turn.model, turn.prompt); err != nil {
			return err
		}
	}
	return nil
}

// anthropic stands in for the Anthropic API, answering as X-Demo says.
func anthropic(w http.ResponseWriter, r *http.Request) {
	var body struct{ Model string }
	_ = json.NewDecoder(r.Body).Decode(&body) // a request without a model answers without one
	var in, read, write, out int
	var stop string
	var seconds float64
	_, _ = fmt.Sscanf(strings.ReplaceAll(r.Header.Get("X-Demo"), ",", " "), "%d %d %d %d %s %g", &in, &read, &write, &out, &stop, &seconds)
	time.Sleep(time.Duration(seconds * float64(time.Second)))
	stream(w, body.Model, in, read, write, out, stop, "Done.")
}

// lmStudio stands in for LM Studio: enough of its model API for Janus to
// trust it, and Messages answers whose time and content depend on the
// prompt, since Janus sends the local model none of the client's headers.
func lmStudio(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/api/v0/models":
		_, _ = io.WriteString(w, `{"data":[{"id":"qwen/qwen3.8-27b","state":"loaded","loaded_context_length":65536,"max_context_length":262144}]}`)
		return
	case "/api/v1/models/load":
		_, _ = io.WriteString(w, `{"type":"llm","instance_id":"qwen/qwen3.8-27b","status":"loaded","load_config":{"context_length":65536}}`)
		return
	case "/v1/messages":
	default:
		fmt.Fprintf(w, `{"error":"Unexpected endpoint or method. (%s %s)"}`, r.Method, r.URL.Path) // #nosec G705 -- a stand-in LM Studio on loopback, read only by the Janus under test
		return
	}
	prompt, _ := io.ReadAll(r.Body)
	wait, text, out := 6.0, "Found it in internal/proxy/proxy.go.", 412
	switch {
	case bytes.Contains(prompt, []byte("Find where")):
		wait = 12.3
	case bytes.Contains(prompt, []byte("Read proxy.go")):
		wait = 9.4
	case bytes.Contains(prompt, []byte("Summarise")):
		wait, text, out = 7.8, "", 3 // an empty answer, which fails Janus's checks
	}
	time.Sleep(time.Duration(wait * float64(time.Second)))
	stream(w, "qwen/qwen3.8-27b", 21800, 0, 0, out, "end_turn", text)
}

// stream writes a Messages answer as Anthropic's server-sent events.
func stream(w http.ResponseWriter, model string, in, read, write, out int, stop, text string) {
	w.Header().Set("Content-Type", "text/event-stream")
	events := []struct {
		name string
		data any
	}{
		{"message_start", map[string]any{"type": "message_start", "message": map[string]any{
			"id": "msg_demo", "type": "message", "role": "assistant", "content": []any{}, "model": model,
			"usage": map[string]int{"input_tokens": in, "cache_read_input_tokens": read, "cache_creation_input_tokens": write, "output_tokens": 1}}}},
		{"content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]string{"type": "text", "text": ""}}},
		{"content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]string{"type": "text_delta", "text": text}}},
		{"content_block_stop", map[string]any{"type": "content_block_stop", "index": 0}},
		{"message_delta", map[string]any{"type": "message_delta", "delta": map[string]string{"stop_reason": stop}, "usage": map[string]int{"output_tokens": out}}},
		{"message_stop", map[string]string{"type": "message_stop"}},
	}
	for _, e := range events {
		data, _ := json.Marshal(e.data)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", e.name, data)
	}
}

// serve runs h on a free loopback port for the life of the program and
// returns its address.
func serve(h http.Handler) (string, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(l) }() // runs until consoleshot exits
	return l.Addr().String(), nil
}

// freeAddress finds a loopback port nothing is using, for Janus to listen
// on.
func freeAddress() (string, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	defer l.Close()
	return l.Addr().String(), nil
}

// waitFor waits until something accepts connections at addr.
func waitFor(addr string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for {
		c, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
		if err == nil {
			return c.Close()
		}
		if ctx.Err() != nil {
			return err
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// The window is drawn in the Ris Ink code palette for dark backgrounds.
// Each ANSI color maps to its nearest color in that palette.
const (
	background = "#1a1210"
	foreground = "#f0e6d2"
	dimmed     = "#a49a8e"
	edge       = "#3a2d26"
	font       = `'JetBrains Mono', ui-monospace, Menlo, Consolas, monospace`
	charWidth  = 8.4 // a 14px monospace font's advance
	lineHeight = 21
	padding    = 22
	top        = 52
)

var colors = map[string]string{
	"31": "#e8659a", // red: rose
	"32": "#5ba3b8", // green: teal
	"33": "#c48a1e", // yellow: ochre
	"35": "#b183b5", // magenta: plum
	"36": "#b8875f", // cyan: sepia
}

// run is a stretch of one line in one style, starting at a column.
type run struct {
	col         int
	text        string
	color       string
	bold, faint bool
}

var sgr = regexp.MustCompile(`\x1b\[([0-9;]*)m`)

// render draws console output as an SVG terminal window. Each run is
// placed at its own column, so the columns line up even in a fallback
// font. A note line longer than the request lines wraps, indented under
// its text, as it would in a terminal that wide.
func render(console, title string) []byte {
	var rows [][]run
	for line := range strings.SplitSeq(strings.TrimRight(console, "\n"), "\n") {
		var row []run
		var style run
		col, last := 0, 0
		add := func(text string) {
			if text != "" {
				r := style
				r.col, r.text = col, text
				row = append(row, r)
				col += len([]rune(text))
			}
		}
		for _, m := range sgr.FindAllStringSubmatchIndex(line, -1) {
			add(line[last:m[0]])
			for code := range strings.SplitSeq(line[m[2]:m[3]], ";") {
				switch {
				case code == "" || code == "0":
					style = run{}
				case code == "1":
					style.bold = true
				case code == "2":
					style.faint = true
				case colors[code] != "":
					style.color = colors[code]
				}
			}
			last = m[1]
		}
		add(line[last:])
		rows = append(rows, row)
	}

	// A note is a line whose first visible text is the ↳ arrow; leading
	// blank runs are its indent.
	note := func(row []run) (run, bool) {
		for _, r := range row {
			if strings.TrimSpace(r.text) != "" {
				return r, strings.HasPrefix(strings.TrimSpace(r.text), "↳")
			}
		}
		return run{}, false
	}
	isNote := func(row []run) bool { _, ok := note(row); return ok }
	width := 0
	for _, row := range rows {
		if n := len(row); n > 0 && !isNote(row) {
			width = max(width, row[n-1].col+len([]rune(row[n-1].text)))
		}
	}
	var lines [][]run
	for _, row := range rows {
		n, ok := note(row)
		last := row[len(row)-1]
		if !ok || last.col+len([]rune(last.text)) <= width {
			lines = append(lines, row)
			continue
		}
		lead := n.col + len([]rune(n.text)) - len([]rune(strings.TrimLeft(n.text, " ")))
		start, current := lead, ""
		for word := range strings.FieldsSeq(n.text) {
			if current != "" && start+len([]rune(current))+1+len([]rune(word)) > width {
				lines = append(lines, []run{{col: start, text: current, color: n.color, bold: n.bold, faint: n.faint}})
				start, current = lead+2, ""
			}
			if current != "" {
				current += " "
			}
			current += word
		}
		lines = append(lines, []run{{col: start, text: current, color: n.color, bold: n.bold, faint: n.faint}})
	}

	w := padding*2 + int(float64(width)*charWidth)
	h := top + lineHeight*len(lines) + 18
	var b bytes.Buffer
	t := html.EscapeString(title)
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d" viewBox="0 0 %d %d" role="img" aria-label="%s">`+"\n", w, h, w, h, t)
	fmt.Fprintf(&b, "<title>%s</title>\n", t)
	fmt.Fprintf(&b, `<rect x="0.5" y="0.5" width="%d" height="%d" rx="10" fill="%s" stroke="%s"/>`+"\n", w-1, h-1, background, edge)
	fmt.Fprintf(&b, `<line x1="0.5" y1="34.5" x2="%d" y2="34.5" stroke="%s"/>`+"\n", w-1, edge)
	for i := range 3 {
		fmt.Fprintf(&b, `<circle cx="%d" cy="17.5" r="5" fill="none" stroke="%s"/>`+"\n", 22+i*18, dimmed)
	}
	fmt.Fprintf(&b, `<text x="%.1f" y="22" text-anchor="middle" font-family="%s" font-size="12" fill="%s">janus</text>`+"\n", float64(w)/2, font, dimmed)
	fmt.Fprintf(&b, `<g font-family="%s" font-size="14" xml:space="preserve">`+"\n", font)
	for i, row := range lines {
		y := top + lineHeight*i + 4
		for _, r := range row {
			text := strings.TrimRight(r.text, " ")
			trimmed := strings.TrimLeft(text, " ")
			if trimmed == "" {
				continue
			}
			col := r.col + len([]rune(text)) - len([]rune(trimmed))
			fill := foreground
			switch {
			case r.color != "":
				fill = r.color
			case r.faint:
				fill = dimmed
			}
			weight := ""
			if r.bold {
				weight = ` font-weight="700"`
			}
			fmt.Fprintf(&b, `<text x="%.1f" y="%d" fill="%s"%s>%s</text>`+"\n", padding+float64(col)*charWidth, y, fill, weight, html.EscapeString(trimmed))
		}
	}
	b.WriteString("</g>\n</svg>\n")
	return b.Bytes()
}
