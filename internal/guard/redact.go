package guard

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/risadams/janus/internal/anthropic"
)

// A placeholder stands in for a secret in what goes upstream. It is one
// plain token with no characters that need escaping anywhere, so that a
// model can repeat it exactly and it survives inside JSON, a shell
// command, or a URL.
const placeholderPrefix = "JANUS_REDACTED_"

const placeholderIDLength = 12

// placeholderLike matches anything shaped like a placeholder, whether or
// not it is one janus issued.
var placeholderLike = regexp.MustCompile(placeholderPrefix + `[0-9a-f]{` + fmt.Sprint(placeholderIDLength) + `}`)

// Redactor replaces secrets with placeholders in requests going upstream,
// and puts them back in what comes down.
//
// A secret always gets the same placeholder for as long as its key is the
// same. That keeps the request's bytes the same from turn to turn, which
// the prompt cache needs, and lets the model refer back to "that key" by a
// name that does not change. The proxy keeps its key on disk (LoadKey), so
// that holds across restarts too: a new key would change every placeholder
// already sent, and the whole conversation after the first one would be
// written to the cache again.
type Redactor struct {
	Detector *Detector

	key []byte // makes placeholders unguessable from the secret

	mu sync.Mutex
	// known is every secret seen so far. Once a value has been recognised
	// as a secret it is replaced wherever it turns up, including places
	// where nothing marks it as one: an assistant message that repeated a
	// password has no "password =" in front of it.
	known map[string]string // value -> kind
}

// NewRedactor returns a Redactor using d to find secrets, with a key of
// its own that lasts as long as it does.
func NewRedactor(d *Detector) *Redactor {
	key := make([]byte, keySize)
	rand.Read(key)
	return NewRedactorWithKey(d, key)
}

// NewRedactorWithKey returns a Redactor using d to find secrets and key to
// name them. Two redactors with the same key give a secret the same
// placeholder.
func NewRedactorWithKey(d *Detector, key []byte) *Redactor {
	return &Redactor{Detector: d, key: key, known: map[string]string{}}
}

// KeyFile is where the proxy keeps its placeholder key, in its log folder.
const KeyFile = "redact.key"

// keySize is the length of a placeholder key, in bytes.
const keySize = 32

// LoadKey returns the placeholder key kept at path, creating it if there
// is none. The file is readable by its owner only. A file that is there
// but is not a key is an error, not replaced: replacing it would change
// every placeholder already sent.
func LoadKey(path string) ([]byte, error) {
	key, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		key = make([]byte, keySize)
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, err
		}
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if os.IsExist(err) {
			return LoadKey(path) // made since we looked
		}
		if err != nil {
			return nil, err
		}
		if _, err := f.Write(key); err != nil {
			f.Close()
			os.Remove(path)
			return nil, err
		}
		if err := f.Close(); err != nil {
			os.Remove(path)
			return nil, err
		}
		return key, nil
	}
	if err != nil {
		return nil, err
	}
	if len(key) != keySize {
		return nil, fmt.Errorf("%s is %d bytes, not a %d-byte placeholder key. Delete it and a new key is made, at the cost of every conversation holding a replaced secret being written to the prompt cache again", path, len(key), keySize)
	}
	return key, nil
}

func (r *Redactor) placeholder(value string) string {
	mac := hmac.New(sha256.New, r.key)
	mac.Write([]byte(value))
	return placeholderPrefix + hex.EncodeToString(mac.Sum(nil))[:placeholderIDLength]
}

// Redaction is one request with its secrets replaced, and what is needed
// to put them back in the response.
type Redaction struct {
	// Body is the request to send. It is the original, byte for byte, if
	// nothing was found.
	Body []byte
	// Report says what was found, without the values.
	Report Report
	// Replaced is how many distinct secrets were replaced.
	Replaced int

	restore map[string]string // placeholder -> secret
	// literal holds placeholder-shaped text that was in the client's own
	// request and is not a placeholder: a document about janus, say. The
	// model repeating one of these is not an error.
	literal map[string]bool
}

// Active reports whether anything was replaced, and so whether the
// response needs restoring.
func (rd *Redaction) Active() bool { return rd != nil && len(rd.restore) > 0 }

// ErrStillPresent means a secret was found but could not be removed. The
// request must not be sent.
var ErrStillPresent = errors.New("a secret was found in the request and could not be removed from it")

// Redact replaces the secrets in a Messages request body.
//
// It fails closed. If the body cannot be read, or a secret is still there
// after replacing, it returns an error and the caller must not send the
// request upstream.
func (r *Redactor) Redact(body []byte) (*Redaction, error) {
	req, err := anthropic.ParseRequest(body)
	if err != nil {
		return nil, fmt.Errorf("the request could not be read to check it for secrets: %w", err)
	}
	found := r.Detector.secretsIn(req)

	r.mu.Lock()
	for value, kind := range found.values {
		r.known[value] = kind
	}
	// Longest first, so that a secret containing another is replaced whole.
	secrets := make([]string, 0, len(r.known))
	for value := range r.known {
		secrets = append(secrets, value)
	}
	r.mu.Unlock()
	sort.Slice(secrets, func(i, j int) bool {
		if len(secrets[i]) != len(secrets[j]) {
			return len(secrets[i]) > len(secrets[j])
		}
		return secrets[i] < secrets[j]
	})

	rd := &Redaction{Body: body, Report: found.report, restore: map[string]string{}, literal: map[string]bool{}}
	// damagedLike takes in whole placeholders and partial ones alike: the
	// bare prefix in a conversation about janus is the client's own text.
	for _, m := range damagedLike.FindAll(body, -1) {
		rd.literal[string(m)] = true
	}

	out := body
	for _, value := range secrets {
		token := r.placeholder(value)
		replaced := false
		// A secret sits inside a JSON string, so what is in the body is its
		// escaped form. Clients differ in what they escape; try each form.
		for _, form := range escapedForms(value) {
			if bytes.Contains(out, form) {
				out = bytes.ReplaceAll(out, form, []byte(token))
				replaced = true
			}
		}
		if replaced {
			rd.restore[token] = value
			delete(rd.literal, token)
		}
	}
	rd.Replaced = len(rd.restore)
	if rd.Replaced == 0 {
		return rd, nil
	}

	// Check the result the way an attacker would: read it back and look.
	check, err := anthropic.ParseRequest(out)
	if err != nil {
		return nil, fmt.Errorf("replacing secrets left the request unreadable: %w", err)
	}
	var all strings.Builder
	textOf(check, func(_ string, text string) { all.WriteString(text); all.WriteString("\n") })
	remaining := all.String()
	for _, value := range secrets {
		if strings.Contains(remaining, value) {
			return nil, ErrStillPresent
		}
	}
	rd.Body = out
	return rd, nil
}

// escapedForms is the ways value can appear inside a JSON string: as Go
// writes it, and as JavaScript's JSON.stringify does, which escapes less.
func escapedForms(value string) [][]byte {
	strict, _ := json.Marshal(value) // escapes <, >, & and U+2028/9 as well
	var loose bytes.Buffer
	enc := json.NewEncoder(&loose)
	enc.SetEscapeHTML(false)
	enc.Encode(value)

	forms := [][]byte{trimQuotes(bytes.TrimRight(loose.Bytes(), "\n"))}
	if s := trimQuotes(strict); !bytes.Equal(s, forms[0]) {
		forms = append(forms, s)
	}
	// JSON.stringify leaves U+2028 and U+2029 as they are; Go escapes them.
	if raw := strings.NewReplacer("\\u2028", "\u2028", "\\u2029", "\u2029").Replace(string(forms[0])); raw != string(forms[0]) {
		forms = append(forms, []byte(raw))
	}
	return forms
}

func trimQuotes(b []byte) []byte {
	if len(b) >= 2 && b[0] == '"' && b[len(b)-1] == '"' {
		return b[1 : len(b)-1]
	}
	return b
}

// jsonEscape is value as it appears inside a JSON string.
func jsonEscape(value string) string {
	return string(escapedForms(value)[0])
}

// ErrUnresolvable means the model referred to a redacted secret in a form
// janus cannot put back: a placeholder it never issued, or a damaged one.
// Passing that on would hand the client a command or an edit with a
// meaningless token where a secret should be, so the turn is stopped.
var ErrUnresolvable = errors.New("the model referred to a redacted secret in a form janus cannot restore")

// restoreIn puts secrets back into text. escape adapts each secret to
// where the text sits: plain for message text, escaped once for raw JSON.
func (rd *Redaction) restoreIn(text string, escape func(string) string) (out string, restored int, err error) {
	out = placeholderLike.ReplaceAllStringFunc(text, func(token string) string {
		if value, ok := rd.restore[token]; ok {
			restored++
			return escape(value)
		}
		if !rd.literal[token] {
			err = ErrUnresolvable
		}
		return token
	})
	return out, restored, err
}

// RestoreJSON puts secrets back into a whole JSON response body. Every
// placeholder in it is inside a JSON string, at one level of escaping.
func (rd *Redaction) RestoreJSON(body []byte) (out []byte, restored int, err error) {
	text, n, err := rd.restoreIn(string(body), jsonEscape)
	if err != nil {
		return nil, 0, err
	}
	if damaged(text, rd) {
		return nil, 0, ErrUnresolvable
	}
	return []byte(text), n, nil
}

// damagedLike matches the start of a placeholder that is not followed by a
// whole id: the prefix on its own, or with the wrong characters after it.
var damagedLike = regexp.MustCompile(`(?i)` + placeholderPrefix + `[0-9a-z]*`)

// damaged reports whether text still holds something that set out to be a
// placeholder and is not a whole one that janus knows.
func damaged(text string, rd *Redaction) bool {
	for _, m := range damagedLike.FindAllString(text, -1) {
		if rd.literal[m] {
			continue
		}
		if placeholderLike.MatchString(m) && len(m) == len(placeholderPrefix)+placeholderIDLength {
			if _, ok := rd.restore[m]; ok {
				continue // cannot happen after restoring, but is not damage
			}
		}
		return true
	}
	return false
}

// found is the detector's result with the values, for the redactor's use
// only. Nothing outside this package sees values.
type found struct {
	report Report
	values map[string]string // value -> kind
}

// secretsIn is ScanRequest that also returns what it found.
func (d *Detector) secretsIn(req *anthropic.Request) found {
	f := found{report: Report{Kinds: map[string]int{}}, values: map[string]string{}}
	textOf(req, func(where, text string) {
		hits := d.Scan(text)
		if len(hits) == 0 {
			return
		}
		f.report.Findings += len(hits)
		for _, h := range hits {
			f.report.Kinds[h.Kind]++
			f.values[h.Value(text)] = h.Kind
		}
		f.report.Where = append(f.report.Where, where)
	})
	return f
}
