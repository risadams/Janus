package backend

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Backend is a model server janus can send a Messages request to. The
// proxy does the sending and relaying for all of them; a Backend supplies
// what differs: where it is, what the outbound request must look like, and
// whether it is up.
type Backend interface {
	// Name identifies the backend in the ledger: "upstream" or "local".
	Name() string
	// URL is the base the request path is appended to.
	URL() *url.URL
	// Prepare adjusts the outbound request's headers for this backend,
	// after its URL has been set. messages is true for a Messages API call.
	Prepare(out *http.Request, messages bool)
	// Health reports whether the backend can be reached.
	Health(ctx context.Context) error
}

// RouteHeader is a request header a client can set to choose the route
// itself, overriding the rules. It is an instruction to janus and is never
// passed on to a backend.
const RouteHeader = "X-Janus-Route"

// Upstream is the Anthropic API, or a gateway in front of it. Requests go
// to it as the client sent them.
type Upstream struct {
	base   *url.URL
	client *http.Client
}

// NewUpstream returns the upstream backend at base. client is used only
// for Health; pass nil for the default.
func NewUpstream(base *url.URL, client *http.Client) *Upstream {
	if client == nil {
		client = http.DefaultClient
	}
	return &Upstream{base: base, client: client}
}

func (u *Upstream) Name() string  { return "upstream" }
func (u *Upstream) URL() *url.URL { return u.base }

// Prepare leaves the request as the client sent it, with two exceptions:
// on a Messages call, Accept-Encoding is narrowed so that janus can read
// the response; and the route override header, if the client set one, is
// removed.
func (u *Upstream) Prepare(out *http.Request, messages bool) {
	out.Header.Del(RouteHeader)
	if messages {
		narrowEncoding(out.Header)
	}
}

// Health asks only whether the upstream answers at all. It sends no
// credentials, so any HTTP response counts.
func (u *Upstream) Health(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.base.String(), nil)
	if err != nil {
		return err
	}
	resp, err := u.client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// narrowEncoding limits what a Messages request asks the upstream to
// compress its response with to what janus can read: gzip, or nothing.
//
// This is the one request header janus changes on the way upstream. The
// ledger depends on reading every response, the standard library cannot
// decode brotli or zstd, and Claude Code offers both. The client still
// only ever receives an encoding it asked for: gzip if it offered gzip,
// otherwise none.
func narrowEncoding(h http.Header) {
	offered := h.Values("Accept-Encoding")
	if len(offered) == 0 {
		return // no compression was asked for, and none will be used
	}
	if acceptsGzip(offered) {
		h.Set("Accept-Encoding", "gzip")
	} else {
		h.Set("Accept-Encoding", "identity")
	}
}

// acceptsGzip reports whether an Accept-Encoding header allows gzip, either
// by name or through the wildcard, and not with a quality of zero.
func acceptsGzip(values []string) bool {
	for _, value := range values {
		for _, part := range strings.Split(value, ",") {
			coding, params, _ := strings.Cut(strings.TrimSpace(part), ";")
			coding = strings.ToLower(strings.TrimSpace(coding))
			if coding != "gzip" && coding != "x-gzip" && coding != "*" {
				continue
			}
			if weight, ok := strings.CutPrefix(strings.TrimSpace(params), "q="); ok {
				if f, err := strconv.ParseFloat(strings.TrimSpace(weight), 64); err == nil && f == 0 {
					continue
				}
			}
			return true
		}
	}
	return false
}
