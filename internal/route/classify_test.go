package route

import (
	"net/http"
	"testing"
)

func TestClassify(t *testing.T) {
	h := func(pairs ...string) http.Header {
		header := http.Header{}
		for i := 0; i < len(pairs); i += 2 {
			header.Set(pairs[i], pairs[i+1])
		}
		return header
	}
	session := []string{"X-Claude-Code-Session-Id", "s1"}
	agent := append([]string{"X-Claude-Code-Agent-Id", "a1"}, session...)

	cases := []struct {
		method, path string
		header       http.Header
		want         Class
	}{
		{"POST", "/v1/messages", h(session...), Main},
		{"POST", "/v1/messages", h(agent...), Subagent},
		{"POST", "/v1/messages", h(), Unknown}, // not Claude Code
		{"POST", "/v1/messages/count_tokens", h(session...), Unknown},
		{"GET", "/v1/messages", h(session...), Unknown},
		{"HEAD", "/api/hello", h(), Connectivity},
		{"GET", "/", h(), Connectivity},
		{"POST", "/", h(), Unknown},
		{"GET", "/v1/models", h(session...), Unknown},
	}
	for _, c := range cases {
		if got := Classify(c.method, c.path, c.header); got != c.want {
			t.Errorf("Classify(%s %s, %v) = %s, want %s", c.method, c.path, c.header, got, c.want)
		}
	}
}
