package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/risadams/janus/internal/ledger"
	"github.com/risadams/janus/internal/proxy"
)

// Console is where the proxy reports, one line per request, as it works.
// The ledger is the record; this is for a person watching. So each line
// leads with what a person asks first (what kind of request, who answered,
// did it work) and puts numbers in a form read at a glance, and anything
// unusual goes on a line of its own beneath.
//
// It is also the writer behind the proxy's log.Logger, so every message
// carries the same time stamp, and the date is printed once when it
// changes rather than on every line.
type Console struct {
	mu     sync.Mutex
	w      io.Writer
	color  bool
	prices ledger.Prices
	now    func() time.Time
	day    string
}

// NewConsole writes to w, in color if color is set, pricing requests with
// prices as janus stats does.
func NewConsole(w io.Writer, color bool, prices ledger.Prices) *Console {
	return &Console{w: w, color: color, prices: prices, now: time.Now}
}

// Logger is a log.Logger that writes through the console.
func (c *Console) Logger() *log.Logger {
	return log.New(c, "", 0)
}

// Write prints a message from the logger, stamped with the time.
func (c *Console) Write(p []byte) (int, error) {
	c.print(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

// Printf prints a message, stamped with the time.
func (c *Console) Printf(format string, args ...any) {
	c.print(fmt.Sprintf(format, args...))
}

func (c *Console) print(text string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if day := now.Format("2006-01-02"); day != c.day {
		c.day = day
		fmt.Fprintln(c.w, c.paint(dim, "── "+now.Format("Monday 2 January 2006")+" ──"))
	}
	fmt.Fprintf(c.w, "%s  %s\n", c.paint(dim, now.Format("15:04:05")), text)
}

// Request prints one finished request and the notes about it.
func (c *Console) Request(ex proxy.Exchange, rec ledger.Record, notes []note) {
	c.print(formatRequest(ex, rec, notes, c.prices, c.paint))
}

// note is something about a request worth a line of its own.
type note struct {
	level level
	text  string
}

type level int

const (
	info level = iota
	warn
	bad
)

// style is a color, or a weight, for a piece of a line.
type style string

const (
	dim     style = "2"
	bold    style = "1"
	red     style = "31"
	green   style = "32"
	yellow  style = "33"
	magenta style = "35"
	cyan    style = "36"
)

func (c *Console) paint(s style, text string) string {
	if !c.color || text == "" {
		return text
	}
	return "\x1b[" + string(s) + "m" + text + "\x1b[0m"
}

// formatRequest lays out one request. Columns are padded before they are
// painted, so they line up with color on or off.
func formatRequest(ex proxy.Exchange, rec ledger.Record, notes []note, prices ledger.Prices, paint func(style, string) string) string {
	var b strings.Builder
	class, classStyle := className(rec.Class)
	b.WriteString(paint(classStyle, pad(class, 9)))
	b.WriteString("  ")

	messages := rec.Request != nil || rec.Response != nil || ex.Summary != nil
	if messages {
		who, local := answeredBy(ex, rec)
		if local {
			b.WriteString(paint(magenta, pad(who, 22)))
		} else {
			b.WriteString(pad(who, 22))
		}
	} else {
		b.WriteString(pad(clip(ex.Method+" "+ex.Path, 22), 22))
	}
	b.WriteString("  ")

	ok := ex.Status >= 200 && ex.Status < 300 && (ex.Summary == nil || ex.Summary.Error == nil)
	took := humanDuration(ex.Duration)
	switch {
	case ex.Backend == "none":
		b.WriteString(paint(red, "✗ refused") + " " + took)
	case ok:
		b.WriteString(paint(green, "✓") + " " + pad(took, 6))
	default:
		b.WriteString(paint(red, fmt.Sprintf("✗ %d", ex.Status)) + " " + took)
		if ex.Summary != nil && ex.Summary.Error != nil {
			b.WriteString("  " + paint(red, humanError(ex.Summary.Error.Type)))
		}
	}

	if s := ex.Summary; ok && s != nil {
		stop, stopStyle := humanStop(s.StopReason, s.Complete)
		b.WriteString("  " + paint(stopStyle, pad(stop, 10)))
		if u := rec.Usage; u != nil {
			b.WriteString("  " + tokens(*u, paint))
			b.WriteString(cost(rec, prices, paint))
		}
	}

	line := strings.TrimRight(b.String(), " ")
	b.Reset()
	b.WriteString(line)
	for _, n := range notes {
		s := cyan
		switch n.level {
		case warn:
			s = yellow
		case bad:
			s = red
		}
		b.WriteString("\n          " + paint(s, "↳ "+n.text))
	}
	return b.String()
}

// className is the short name a request's class is shown by, and its
// style: the main loop stands out, subagents are colored, and Claude Code's
// background requests (status lines, quota checks, token counts) are dim.
func className(class string) (string, style) {
	switch class {
	case "main":
		return "main", bold
	case "subagent":
		return "subagent", cyan
	case "compaction":
		return "compact", dim
	case "token_count":
		return "count", dim
	case "connectivity":
		return "ping", dim
	case "status", "quota", "safety":
		return class, dim
	}
	return "other", dim
}

// answeredBy names the model that answered, for a person: "Opus 5.5" for
// Claude, the model's own name with "(local)" for LM Studio.
func answeredBy(ex proxy.Exchange, rec ledger.Record) (who string, local bool) {
	model := ""
	if rec.Response != nil {
		model = rec.Response.Model
	}
	if model == "" && rec.Request != nil {
		model = rec.Request.Model
	}
	if ex.Backend == "local" {
		if ex.BackendModel != "" {
			model = ex.BackendModel
		}
		if i := strings.LastIndex(model, "/"); i >= 0 {
			model = model[i+1:]
		}
		return model + " (local)", true
	}
	return claudeName(model), false
}

// claudeName turns "claude-opus-5-5" into "Opus 5.5" and
// "claude-haiku-4-5-20251001" into "Haiku 4.5". Anything else is left as
// it is.
func claudeName(model string) string {
	rest, ok := strings.CutPrefix(model, "claude-")
	if !ok {
		return model
	}
	parts := strings.Split(rest, "-")
	if len(parts) < 2 {
		return model
	}
	var version []string
	for _, p := range parts[1:] {
		if len(p) >= 8 || p == "" || strings.Trim(p, "0123456789") != "" {
			break // a date stamp, or not a version number
		}
		version = append(version, p)
	}
	if len(version) == 0 {
		return model
	}
	return strings.ToUpper(parts[0][:1]) + parts[0][1:] + " " + strings.Join(version, ".")
}

// tokens says how big the prompt was, how much of it came from the cache,
// and how much was written back. The share from the cache is colored when
// the prompt is large enough for it to matter: a low share on a large
// prompt is the cache being rebuilt, at many times the price.
func tokens(u ledger.Usage, paint func(style, string) string) string {
	read, write := deref(u.CacheRead), deref(u.CacheWrite)
	prompt := u.Input + read + write
	in := humanCount(prompt) + " in"
	width := len(in)
	if u.CacheRead != nil && prompt > 0 {
		share := 100 * read / prompt
		s := dim
		if prompt >= 10_000 {
			s = green
			if share < 80 {
				s = yellow
			}
		}
		cached := fmt.Sprintf("%d%% cached", share)
		in += " · " + paint(s, cached)
		width += len(" · ") - len("·") + 1 + len(cached) // "·" is one column
	}
	// Padded by what shows, not by the color codes around it.
	return in + strings.Repeat(" ", max(0, 22-width)) + "  " + pad(humanCount(u.Output)+" out", 9)
}

// cost is the request's estimated list price, as janus stats reckons it.
// A turn that costs more than half a dollar is marked: it is usually the
// cache being written afresh.
func cost(rec ledger.Record, prices ledger.Prices, paint func(style, string) string) string {
	if rec.Backend == "local" {
		return "  " + paint(dim, "free")
	}
	model := ""
	if rec.Response != nil {
		model = rec.Response.Model
	}
	usd, unpriced := prices.Cost(model, *rec.Usage)
	switch {
	case unpriced != "":
		return ""
	case usd >= 0.5:
		return "  " + paint(yellow, fmt.Sprintf("$%.2f", usd))
	case usd < 0.001:
		return "  " + paint(dim, "<$0.001")
	}
	return "  " + paint(dim, fmt.Sprintf("$%.3f", usd))
}

// humanStop says why the model stopped, and colors the reasons that mean
// the answer may be unfinished.
func humanStop(reason string, complete bool) (string, style) {
	if !complete {
		return "cut off", yellow
	}
	switch reason {
	case "end_turn":
		return "answered", dim
	case "tool_use":
		return "tool call", dim
	case "stop_sequence":
		return "stopped", dim
	case "pause_turn":
		return "paused", dim
	case "max_tokens":
		return "max tokens", yellow
	case "refusal":
		return "refused", red
	case "":
		return "", dim
	}
	return reason, dim
}

// humanError says what an API error type means.
func humanError(kind string) string {
	switch kind {
	case "rate_limit_error":
		return "rate limited"
	case "overloaded_error":
		return "overloaded"
	case "invalid_request_error":
		return "bad request"
	case "authentication_error":
		return "not signed in"
	case "permission_error":
		return "not allowed"
	case "not_found_error":
		return "not found"
	case "request_too_large":
		return "request too large"
	case "api_error":
		return "server error"
	}
	return kind
}

// humanCount is 950, 1.9k, 151k, or 1.2M.
func humanCount(n int) string {
	switch {
	case n < 1000:
		return fmt.Sprintf("%d", n)
	case n < 10_000:
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	case n < 1_000_000:
		return fmt.Sprintf("%dk", (n+500)/1000)
	}
	return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
}

// humanDuration is 967ms, 20.6s, or 2m05s.
func humanDuration(d time.Duration) string {
	switch {
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	case d < time.Minute:
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	d = d.Round(time.Second)
	return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
}

// pad fills text out to width runes, so columns line up whatever the
// characters.
func pad(text string, width int) string {
	if n := len([]rune(text)); n < width {
		return text + strings.Repeat(" ", width-n)
	}
	return text
}

// clip shortens text to width runes, ending it with "…" if it was cut.
func clip(text string, width int) string {
	r := []rune(text)
	if len(r) <= width {
		return text
	}
	return string(r[:width-1]) + "…"
}

func deref(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

// wantColor reports whether output to f should be in color: only on a
// terminal, never with NO_COLOR set (https://no-color.org), always with
// FORCE_COLOR set. On Windows, only in a terminal known to understand
// color codes, as the old console shows them as text.
func wantColor(f *os.File, getenv func(string) string) bool {
	switch {
	case getenv("NO_COLOR") != "":
		return false
	case getenv("FORCE_COLOR") != "":
		return true
	case getenv("TERM") == "dumb":
		return false
	case runtime.GOOS == "windows" && getenv("WT_SESSION") == "" && getenv("TERM") == "":
		return false
	}
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}
