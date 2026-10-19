package main

import (
	"fmt"
	"io"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/risadams/janus/internal/anthropic"
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
	// said is, per conversation, the notes already printed that need not
	// be repeated, and when the conversation was last seen.
	said map[string]*said
	// quotaStep is, per limit, the tenth of it last reported as used. The
	// limits are the account's, not a conversation's, so this is one for
	// the whole console.
	quotaStep map[string]int
	// handback is, per conversation, the stand-in turns of its main loop
	// since Claude last answered it. It outlives forgetAfter: a person
	// who reached a limit often waits hours for it to reset.
	handback map[string]*standIns
	// contextWarn is the prompt size, in tokens, from which the console
	// says a conversation's turns have grown expensive; 0 never says it.
	contextWarn int
}

type said struct {
	notes map[string]bool
	last  time.Time
	// standIn is when the local model last answered for Claude in this
	// conversation.
	standIn time.Time
	// smallest is the smallest prompt, in tokens, of the conversation's
	// main-loop turns to Claude that the console has seen.
	smallest int
}

// forgetAfter is how long a conversation can be idle before the console
// forgets what it has said about it. One that comes back is told again.
const forgetAfter = time.Hour

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

// Request prints one finished request and the notes about it. A note that
// need only be said once per conversation is left out if it has been.
func (c *Console) Request(ex proxy.Exchange, rec ledger.Record, notes []note) {
	c.print(formatRequest(ex, rec, c.fresh(rec.Session, notes), c.prices, c.paint))
}

// fresh returns notes without those already said in session. Without a
// session there is no conversation to remember, and every note is kept.
func (c *Console) fresh(session string, notes []note) []note {
	if session == "" {
		return notes
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.conversation(session)
	var out []note
	for _, n := range notes {
		if n.once != "" {
			if s.notes[n.once] {
				continue
			}
			s.notes[n.once] = true
		}
		out = append(out, n)
	}
	return out
}

// standingInFor is how long after a stand-in the console takes the local
// model to be carrying a conversation. In a stand-in session, each turn is
// a minute or two; a conversation with no stand-in for longer has either
// gone back to Claude or stopped.
const standingInFor = 10 * time.Minute

// conversation returns what the console remembers of session, forgetting
// conversations idle past forgetAfter. Call it with c.mu held.
func (c *Console) conversation(session string) *said {
	now := c.now()
	if c.said == nil {
		c.said = map[string]*said{}
	}
	for id, s := range c.said {
		if now.Sub(s.last) > forgetAfter {
			delete(c.said, id)
		}
	}
	s := c.said[session]
	if s == nil {
		s = &said{notes: map[string]bool{}}
		c.said[session] = s
	}
	s.last = now
	return s
}

// StoodIn records that the local model answered for Claude in session.
func (c *Console) StoodIn(session string) {
	if session == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.conversation(session).standIn = c.now()
}

// StandingIn reports whether the local model has recently answered for
// Claude in session.
func (c *Console) StandingIn(session string) bool {
	if session == "" {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.conversation(session)
	return !s.standIn.IsZero() && c.now().Sub(s.standIn) <= standingInFor
}

// WarnAtContext sets the prompt size, in tokens, from which the console
// says a conversation's turns have grown expensive; 0 turns it off.
func (c *Console) WarnAtContext(tokens int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.contextWarn = tokens
}

// ContextNotes says, once per conversation, when its main loop has grown
// past the size set by WarnAtContext (#335). Claude Code does not show it,
// and each turn reads the whole conversation from the cache, so a large
// one costs several times what it did when small: turns over 600k tokens
// were 13% of a week's turns and 33% of its cost
// (docs/research/baseline.md). Janus cannot shrink what was already sent
// without paying to write it again; a /compact or a new session can.
func (c *Console) ContextNotes(rec ledger.Record) []note {
	if rec.Class != "main" || rec.Backend != "upstream" || rec.Usage == nil || rec.Session == "" {
		return nil
	}
	u := rec.Usage
	prompt := u.Input + deref(u.CacheRead) + deref(u.CacheWrite)
	c.mu.Lock()
	s := c.conversation(rec.Session)
	if s.smallest == 0 || prompt < s.smallest {
		s.smallest = prompt
	}
	smallest, warnAt := s.smallest, c.contextWarn
	c.mu.Unlock()
	if warnAt <= 0 || prompt < warnAt {
		return nil
	}
	model := ""
	if rec.Response != nil {
		model = rec.Response.Model
	}
	readCost := func(tokens int) (float64, bool) {
		usd, unpriced := c.prices.Cost(model, ledger.Usage{CacheRead: &tokens})
		return usd, unpriced == ""
	}
	now, ok := readCost(prompt)
	if !ok {
		return nil
	}
	text := fmt.Sprintf("this conversation is %s tokens, and each turn reads all of it from the cache: about $%.2f a turn", humanCount(prompt), now)
	if then, ok := readCost(smallest); ok && smallest <= prompt/2 {
		text += fmt.Sprintf(", against $%.2f when it was %s", then, humanCount(smallest))
	}
	text += ". /compact, or a new session, would start it small again"
	return []note{{level: warn, text: text, once: "context"}}
}

// quotaNear is the share of a limit from which the console warns.
const quotaNear = 0.8

// limitNames are the API's names for its windows, as a person says them.
var limitNames = map[string]string{"5h": "five-hour", "7d": "weekly"}

// QuotaNotes says how much of the subscription's limits is used, each
// time a limit's use crosses another tenth, and when one resets (#239).
// Saying it on every request would bury everything else.
func (c *Console) QuotaNotes(q *anthropic.Quota) []note {
	if q == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.quotaStep == nil {
		c.quotaStep = map[string]int{}
	}
	windows := make([]string, 0, len(q.Windows))
	for w := range q.Windows {
		windows = append(windows, w)
	}
	sort.Strings(windows)
	var notes []note
	for _, w := range windows {
		win := q.Windows[w]
		step := int(win.Utilization * 10)
		last, seen := c.quotaStep[w]
		// A small drop is the figure wavering at a boundary; a reset drops
		// it further.
		if seen && (step == last || (step < last && last-step < 2)) {
			continue
		}
		c.quotaStep[w] = step
		name := limitNames[w]
		if name == "" {
			name = w
		}
		text := fmt.Sprintf("%s limit %.0f%% used", name, win.Utilization*100)
		if !win.Resets.IsZero() {
			text += ", resets " + resetTime(win.Resets, c.now())
		}
		lvl := info
		switch {
		case win.Utilization >= 1 || win.Status == "rejected":
			lvl, text = bad, text+"; Claude is refusing requests until then"
		case win.Utilization >= quotaNear:
			lvl = warn
		case seen && step < last:
			text = name + " limit reset: " + strings.TrimPrefix(text, name+" limit ")
		}
		notes = append(notes, note{level: lvl, text: text})
	}
	return notes
}

// resetTime writes when a limit resets: a time of day if it is within a
// day, a weekday and time if later.
func resetTime(at, now time.Time) string {
	at = at.In(now.Location())
	if at.Sub(now) < 20*time.Hour {
		return "at " + at.Format("15:04")
	}
	return at.Format("Mon 15:04")
}

// standIns counts a conversation's main-loop turns the local model gave
// in Claude's place.
type standIns struct {
	turns int
	since time.Time
	last  time.Time
}

// keepStandInsFor is how long the console remembers stand-in turns that
// Claude has not yet taken over from: longer than the five-hour limit.
const keepStandInsFor = 24 * time.Hour

// StoodInTurn counts a main-loop turn the local model gave because Claude
// could not.
func (c *Console) StoodInTurn(session string) {
	if session == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if c.handback == nil {
		c.handback = map[string]*standIns{}
	}
	for id, s := range c.handback {
		if now.Sub(s.last) > keepStandInsFor {
			delete(c.handback, id)
		}
	}
	s := c.handback[session]
	if s == nil {
		s = &standIns{since: now}
		c.handback[session] = s
	}
	s.turns++
	s.last = now
}

// HandBack is called when Claude answers a conversation's main loop. If
// the local model stood in for it since Claude last did, it returns how
// many turns and from when, and forgets them (#241).
func (c *Console) HandBack(session string) (turns int, since time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.handback[session]
	if s == nil || c.now().Sub(s.last) > keepStandInsFor {
		delete(c.handback, session)
		return 0, time.Time{}
	}
	delete(c.handback, session)
	return s.turns, s.since
}

// note is something about a request worth a line of its own.
type note struct {
	level level
	text  string
	// once, when set, is said only the first time in a conversation: the
	// guard replacing a secret it has replaced before, say. A note that
	// differs in once is new.
	once string
}

type level int

const (
	info level = iota
	warn
	bad
)

func (c *Console) paint(s style, text string) string {
	return painter(c.color).paint(s, text)
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
