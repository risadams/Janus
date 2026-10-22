package openai

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/risadams/janus/internal/anthropic"
	"github.com/risadams/janus/internal/config"
	"github.com/risadams/janus/internal/proxy"
)

// Backend is the OpenAI API as a backend the proxy can send a Messages
// request to. The proxy relays it like any other; what is OpenAI's own is
// here: the request is translated on the way out and the answer on the way
// back (Translation), and the outbound request carries the key and none of
// Claude Code's headers (Prepare).
type Backend struct {
	base   *url.URL
	key    Key
	config config.OpenAI
	client *http.Client
	// out is set once OpenAI refuses the key or the account runs out; see
	// Out.
	out atomic.Pointer[string]
}

// NewBackend returns the OpenAI backend for o, sending with key. client is
// used only for Health; pass nil for the default. A backend with no key
// can plan, as janus route does, but OpenAI refuses everything it sends;
// janus refuses to start serving a rule that names openai without one.
func NewBackend(o config.OpenAI, key Key, client *http.Client) (*Backend, error) {
	base, err := url.Parse(o.URL())
	if err != nil || (base.Scheme != "https" && base.Scheme != "http") || base.Host == "" {
		return nil, fmt.Errorf("openai: base URL %q is not an http(s) address", o.URL())
	}
	if client == nil {
		client = http.DefaultClient
	}
	return &Backend{base: base, key: key, config: o, client: client}, nil
}

// Name is "openai", as the ledger records it.
func (b *Backend) Name() string  { return "openai" }
func (b *Backend) URL() *url.URL { return b.base }

// responsesPath is where Messages requests go, translated.
const responsesPath = "/v1/responses"

// Prepare replaces every header the client sent with the key and a content
// type, as NewRequest builds a request from nothing: what Claude Code sent
// carries its Anthropic credential, which must not reach OpenAI. A
// Messages call goes to the Responses API in its place.
func (b *Backend) Prepare(out *http.Request, messages bool) {
	out.Header = http.Header{}
	b.key.authorize(out.Header)
	if messages {
		out.URL.Path = strings.TrimSuffix(b.base.Path, "/") + responsesPath
		out.URL.RawPath, out.URL.RawQuery = "", ""
	}
}

// Health asks only whether the API answers at all. It sends no key, so any
// HTTP response counts.
func (b *Backend) Health(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.base.String(), nil)
	if err != nil {
		return err
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close() // any answer will do; its body is not read
	return nil
}

// Translation is one request on its way to OpenAI.
type Translation struct {
	// Choice is the model and effort chosen for it.
	Choice Choice

	b       *Backend
	opts    RequestOptions
	failure *Failure
	dropped []string
	result  *Result
}

// Translation chooses the model and effort for req and checks that req can
// be translated at all, so that a planner knows before it commits to
// OpenAI.
func (b *Backend) Translation(header http.Header, req *anthropic.Request) (*Translation, error) {
	choice := Choose(b.config, req)
	opts := RequestOptions{Model: choice.Model, Effort: choice.Effort, SessionID: header.Get("X-Claude-Code-Session-Id")}
	if _, err := TranslateRequest(req, opts); err != nil {
		return nil, err
	}
	return &Translation{Choice: choice, b: b, opts: opts}, nil
}

// Translate is for the plan (proxy.Plan.Translate): it translates the body
// as the guard leaves it, which may differ from the request Translation
// was given by the secrets it replaced, and returns how to read the answer
// back.
func (t *Translation) Translate(body []byte) ([]byte, func(*http.Response) error, error) {
	req, err := anthropic.ParseRequest(body)
	if err != nil {
		return nil, nil, err
	}
	tr, err := TranslateRequest(req, t.opts)
	if err != nil {
		return nil, nil, err
	}
	t.dropped = tr.Dropped
	return tr.Body, t.b.back(t, ResponseOptions{Tools: tr.Tools, Summary: tr.Summary, scrub: t.b.key.scrub}), nil
}

// Failure is OpenAI's error answer to the request, once it has been read:
// what a fallback list decides on (#428). Nil before then, and when OpenAI
// answered. It is read on the goroutine that reads the response.
func (t *Translation) Failure() *Failure { return t.failure }

// Report is what the ledger keeps of the translation (#381): what Claude
// Code asked for that OpenAI could not be sent, and the part of the answer
// OpenAI spent reasoning, which Anthropic's usage has no field for. Call
// it once the answer has been read.
func (t *Translation) Report() proxy.Translation {
	r := proxy.Translation{Dropped: t.dropped}
	if t.result != nil {
		reasoning := t.result.Usage.Reasoning
		r.Reasoning = &reasoning
	}
	return r
}

// Window is the context window of the model a translation chose, or zero
// when it is not known.
func (t *Translation) Window() int { return Window(t.b.config, t.Choice.Model) }

// Out says why OpenAI is out until janus is restarted, or "" if it is not:
// it refused the key, or the account has run out (Kind). Both need the
// owner, so asking again would only be refused again.
func (b *Backend) Out() string {
	if why := b.out.Load(); why != nil {
		return *why
	}
	return ""
}

// maxAnswer bounds a whole (not streamed) answer or an error read to be
// translated. A Responses answer is at most MaxOutputTokens of output.
const maxAnswer = 32 << 20

// back returns what turns OpenAI's response into the Messages response it
// stands for, in place: an error into Anthropic's error shape and status,
// a stream event by event as it arrives, a whole answer all at once.
// OpenAI's own headers are kept, for the ledger, except those describing a
// body that has been replaced.
func (b *Backend) back(t *Translation, opts ResponseOptions) func(*http.Response) error {
	return func(resp *http.Response) error {
		resp.Header.Del("Content-Encoding")
		resp.Header.Del("Content-Length")
		resp.ContentLength = -1
		if resp.StatusCode >= 400 {
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxAnswer)) // a short read is explained by its status
			_ = resp.Body.Close()
			f := TranslateError(resp.StatusCode, resp.Header, b.key.scrub(raw), b.config.Key)
			t.failure = &f
			if f.Kind == KeyRefused || f.Kind == QuotaExhausted {
				why := f.Message
				b.out.CompareAndSwap(nil, &why)
			}
			replace(resp, f.Status, f.Body())
			return nil
		}
		if media, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); media == "text/event-stream" {
			src := resp.Body
			pr, pw := io.Pipe()
			go func() {
				res, err := TranslateStream(pw, src, opts)
				_ = src.Close() // the stream has been read as far as it will be
				if err == nil {
					// Before the pipe closes, so that whoever reads it to
					// the end sees this.
					t.result = &res
				}
				// ErrBrokenStream reaches the proxy as a broken body,
				// and Claude Code as a broken stream, as from Anthropic.
				pw.CloseWithError(err)
			}()
			resp.Body = pr
			return nil
		}
		raw, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswer))
		_ = resp.Body.Close()
		if err != nil {
			return fmt.Errorf("the OpenAI answer broke off: %w", err)
		}
		out, res, err := TranslateResponse(raw, opts)
		switch {
		case err != nil:
			replace(resp, http.StatusBadGateway, errorBody("api_error", "janus: OpenAI's answer could not be read ("+err.Error()+"), which is most likely a bug in janus; please report it with the ledger record."))
		case res.Err != nil:
			t.failure = res.Err
			replace(resp, res.Err.Status, res.Err.Body())
		default:
			t.result = &res
			replace(resp, http.StatusOK, out)
		}
		return nil
	}
}

// replace makes resp a JSON answer of body with status.
func replace(resp *http.Response, status int, body []byte) {
	resp.StatusCode, resp.Status = status, strings.TrimSpace(strconv.Itoa(status)+" "+http.StatusText(status))
	resp.Header.Set("Content-Type", "application/json")
	resp.Body = io.NopCloser(bytes.NewReader(body))
}

func errorBody(kind, message string) []byte {
	return Failure{Type: kind, Message: message}.Body()
}
