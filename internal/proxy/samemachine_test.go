package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSameMachineTurnsAwayWebPages(t *testing.T) {
	reached := 0
	var reasons []string
	h := SameMachine(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached++ }),
		func(r *http.Request, why string) { reasons = append(reasons, why) })

	cases := []struct {
		name, host, origin string
		allowed            bool
	}{
		{"Claude Code, by address", "127.0.0.1:8787", "", true},
		{"Claude Code, by name", "localhost:8787", "", true},
		{"by name, any case", "LocalHost:8787", "", true},
		{"IPv6 loopback", "[::1]:8787", "", true},
		{"no port", "127.0.0.1", "", true},
		{"another loopback address", "127.0.0.2:8787", "", true},
		{"a page served from this machine", "localhost:8787", "http://localhost:3000", true},
		// A page that pointed its own name at 127.0.0.1.
		{"DNS rebinding", "evil.example:8787", "", false},
		{"DNS rebinding with a lookalike", "localhost.evil.example:8787", "", false},
		{"a LAN address", "192.168.1.20:8787", "", false},
		{"no host at all", "", "", false},
		// A page sending cross-site to the loopback address.
		{"cross-site request", "127.0.0.1:8787", "https://evil.example", false},
		{"sandboxed page", "127.0.0.1:8787", "null", false},
		{"a lookalike origin", "127.0.0.1:8787", "http://127.0.0.1.evil.example", false},
	}
	for _, c := range cases {
		before := reached
		r := httptest.NewRequest("POST", "/v1/messages", strings.NewReader("{}"))
		r.Host = c.host
		if c.origin != "" {
			r.Header.Set("Origin", c.origin)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if got := reached > before; got != c.allowed {
			t.Errorf("%s: allowed = %v, want %v", c.name, got, c.allowed)
		}
		if !c.allowed {
			body, _ := io.ReadAll(w.Result().Body)
			if w.Code != http.StatusForbidden || !strings.Contains(string(body), `"type":"permission_error"`) || !strings.Contains(string(body), "allowRemote") {
				t.Errorf("%s: status %d, body %s", c.name, w.Code, body)
			}
		}
	}
	if len(reasons) != 7 {
		t.Errorf("%d refusals reported, want 7: %q", len(reasons), reasons)
	}
}
