// Package guard keeps secrets and protected content from leaving the
// machine. It finds secrets in requests going upstream, can replace them
// with placeholders and put them back in the answer, and applies the
// local-only policy, which keeps named files and marked text off the
// upstream altogether.
//
// A detector here has one job: point at text that is a credential. It is
// tuned to be quiet. A guard that cries wolf over every hash and
// identifier gets switched off, and then protects nothing, so each
// detector is a specific, recognisable format, or a value sitting next to
// a name that says what it is.
package guard

import (
	"crypto/sha256"
	"fmt"
	"io"
	"math"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// Finding is one secret found in a piece of text.
type Finding struct {
	// Kind names what it looks like, for example "aws_access_key".
	Kind string
	// Start and End are byte offsets of the secret itself in the text.
	Start, End int
}

// Value is the secret, cut from the text it was found in.
func (f Finding) Value(text string) string { return text[f.Start:f.End] }

// A format is a credential with a recognisable shape. If group is set, the
// secret is that capture group and not the whole match: the rest is
// context, such as the name of the setting it was assigned to.
type format struct {
	kind  string
	re    *regexp.Regexp
	group int
	// plausible, if set, has the last word on a match: it rejects values
	// that fit the shape but are plainly not secrets.
	plausible func(value string) bool
}

// The key in each assignment-style format is matched case-insensitively
// and may be quoted, as it is in JSON and YAML.
const assigned = `["']?\s*[=:]\s*["']?`

var formats = []format{
	{kind: "private_key", re: regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z0-9 ]*PRIVATE KEY-----`)},
	{kind: "anthropic_api_key", re: regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_-]{24,}`)},
	{kind: "openai_api_key", re: regexp.MustCompile(`\bsk-(?:proj-|svcacct-)?[A-Za-z0-9_-]{32,}`), plausible: func(v string) bool { return !strings.HasPrefix(v, "sk-ant-") }},
	{kind: "aws_access_key", re: regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`)},
	{kind: "aws_secret_key", re: regexp.MustCompile(`(?i)aws_?secret_?(?:access_?)?key` + assigned + `([A-Za-z0-9/+=]{40})\b`), group: 1},
	{kind: "github_token", re: regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{36,}|github_pat_[A-Za-z0-9_]{50,})`)},
	{kind: "slack_token", re: regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{12,}`)},
	{kind: "google_api_key", re: regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}\b`)},
	{kind: "stripe_key", re: regexp.MustCompile(`\b[sr]k_(?:live|test)_[0-9a-zA-Z]{20,}`)},
	{kind: "jwt", re: regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{16,}`)},
	// scheme://user:password@host
	{kind: "url_password", re: regexp.MustCompile(`\b[a-zA-Z][a-zA-Z0-9+.-]*://[^\s:/@"']+:([^\s@/"']{6,})@[^\s"']`), group: 1, plausible: notPlaceholder},
	// password = "...", "api_key": "...", TOKEN=...
	{kind: "assigned_secret", re: regexp.MustCompile(`(?i)\b[a-z0-9_.-]*(?:password|passwd|secret|token|api_?key|access_?key|private_?key|credentials?)` + assigned + "([^\\s\"'`,;)}\\]]{8,})"), group: 1,
		plausible: func(v string) bool { return notPlaceholder(v) && looksRandom(v) }},
}

// notPlaceholder rejects the things people write where a secret would go.
func notPlaceholder(v string) bool {
	lower := strings.ToLower(v)
	if strings.ContainsAny(v, "${}<>()%") || strings.HasPrefix(v, "$") { // ${VAR}, <token>, %s, os.getenv(
		return false
	}
	for _, word := range []string{"example", "changeme", "change_me", "placeholder", "redacted", "your", "dummy", "sample", "xxxx", "****", "....", "todo", "none", "null", "true", "false", "secret", "password"} {
		if strings.Contains(lower, word) {
			return false
		}
	}
	// One character repeated, or a simple run, is not a secret.
	return distinct(v) >= 5
}

// looksRandom separates a generated secret from an ordinary word or
// identifier assigned to a suspicious-sounding name: token = parseToken,
// password: getPassword(). A real secret mixes kinds of character and has
// little repetition.
func looksRandom(v string) bool {
	var lower, upper, digit, other bool
	for _, r := range v {
		switch {
		case r >= 'a' && r <= 'z':
			lower = true
		case r >= 'A' && r <= 'Z':
			upper = true
		case r >= '0' && r <= '9':
			digit = true
		default:
			other = true
		}
	}
	classes := 0
	for _, has := range []bool{lower, upper, digit, other} {
		if has {
			classes++
		}
	}
	return classes >= 2 && digit && entropy(v) >= 3.0
}

func distinct(s string) int {
	seen := map[rune]bool{}
	for _, r := range s {
		seen[r] = true
	}
	return len(seen)
}

// entropy is the Shannon entropy of s in bits per character.
func entropy(s string) float64 {
	if s == "" {
		return 0
	}
	counts := map[rune]int{}
	n := 0
	for _, r := range s {
		counts[r]++
		n++
	}
	var h float64
	for _, c := range counts {
		p := float64(c) / float64(n)
		h -= p * math.Log2(p)
	}
	return h
}

// Detector finds secrets.
type Detector struct {
	// Patterns are extra expressions to treat as secrets, from the config.
	// A pattern with a capture group marks the group; otherwise the whole
	// match.
	Patterns []*regexp.Regexp
	// Allow lists values that are never secrets, such as a public test key
	// that appears throughout a codebase.
	Allow []string
	// Entropy turns on the search for long random-looking strings that fit
	// no known format. It finds more and is wrong more often; see
	// docs/GUARD.md for how often.
	Entropy bool

	// seen remembers what Scan found in each text it has been over. A
	// conversation is sent again in full with every turn, so nearly all
	// the text in a request has been scanned before.
	mu   sync.Mutex
	seen map[[sha256.Size]byte][]Finding
}

// Texts shorter than this are scanned each time: that costs less than
// remembering them.
const minRemembered = 256

// maxRemembered bounds the memory of past scans. Each entry is a hash and,
// rarely, a few positions; no text and no secret is kept.
const maxRemembered = 50000

// candidate is a run of characters that could be an encoded secret.
// Padding is only allowed at the end, so that the name in NAME=value is
// not swallowed into the value.
var candidate = regexp.MustCompile(`[A-Za-z0-9+/_-]{32,}={0,2}`)

// hexOnly matches what is almost always a hash or an id, not a secret.
var hexOnly = regexp.MustCompile(`^[0-9a-fA-F]+$`)

const entropyThreshold = 4.5

// Scan returns the secrets in text, in order, without overlaps. The result
// is shared with later calls for the same text and must not be changed.
func (d *Detector) Scan(text string) []Finding {
	if len(text) < minRemembered {
		return d.scan(text)
	}
	// The settings are part of the key, so that a detector whose settings
	// are changed does not answer from before the change.
	h := sha256.New()
	fmt.Fprintf(h, "%t\x00%q\x00", d.Entropy, d.Allow)
	for _, re := range d.Patterns {
		fmt.Fprintf(h, "%q\x00", re.String())
	}
	io.WriteString(h, text)
	var key [sha256.Size]byte
	h.Sum(key[:0])

	d.mu.Lock()
	found, ok := d.seen[key]
	d.mu.Unlock()
	if ok {
		return found
	}
	found = d.scan(text)
	d.mu.Lock()
	if d.seen == nil || len(d.seen) >= maxRemembered {
		d.seen = map[[sha256.Size]byte][]Finding{}
	}
	d.seen[key] = found
	d.mu.Unlock()
	return found
}

// scan does the work of Scan.
func (d *Detector) scan(text string) []Finding {
	allowed := func(start, end int) bool {
		for _, value := range d.Allow {
			if value != "" && text[start:end] == value {
				return true
			}
		}
		return false
	}

	var found []Finding
	for _, f := range formats {
		for _, m := range f.re.FindAllStringSubmatchIndex(text, -1) {
			start, end := m[2*f.group], m[2*f.group+1]
			if start < 0 || end <= start || allowed(start, end) || (f.plausible != nil && !f.plausible(text[start:end])) {
				continue
			}
			found = append(found, Finding{f.kind, start, end})
		}
	}
	for _, re := range d.Patterns {
		for _, m := range re.FindAllStringSubmatchIndex(text, -1) {
			start, end := m[0], m[1]
			if len(m) >= 4 && m[2] >= 0 {
				start, end = m[2], m[3] // a capture group marks the secret
			}
			if end > start && !allowed(start, end) {
				found = append(found, Finding{"custom", start, end})
			}
		}
	}

	// Keep the earliest finding at each place, and the longest if two
	// start together; drop anything inside one already kept. The formats
	// run most specific first and the sort is stable, so a named format
	// wins over a generic one that matched the same text.
	sort.SliceStable(found, func(i, j int) bool {
		if found[i].Start != found[j].Start {
			return found[i].Start < found[j].Start
		}
		return found[i].End > found[j].End
	})
	var kept []Finding
	end := -1
	for _, f := range found {
		if f.Start >= end {
			kept = append(kept, f)
			end = f.End
		}
	}
	if !d.Entropy {
		return kept
	}

	// The entropy search runs last and only fills gaps: it never replaces,
	// extends, or splits something a format already identified.
	named := len(kept)
	for _, m := range candidate.FindAllStringIndex(text, -1) {
		value := text[m[0]:m[1]]
		if hexOnly.MatchString(value) || !strings.ContainsAny(value, "0123456789") || entropy(value) < entropyThreshold || allowed(m[0], m[1]) {
			continue
		}
		overlaps := false
		for _, f := range kept[:named] {
			if m[0] < f.End && f.Start < m[1] {
				overlaps = true
				break
			}
		}
		if !overlaps {
			kept = append(kept, Finding{"high_entropy", m[0], m[1]})
		}
	}
	sort.Slice(kept, func(i, j int) bool { return kept[i].Start < kept[j].Start })
	return kept
}
