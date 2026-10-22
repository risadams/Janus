package route

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/risadams/janus/internal/config"
	"github.com/risadams/janus/internal/openai"
	"github.com/risadams/janus/internal/openai/openaitest"
)

// longConversation is a conversation of turns pairs of messages, each
// about size bytes, as Claude Code would send it.
func longConversation(turns, size int) string {
	messages := []map[string]any{{"role": "user", "content": "Start the work."}}
	for i := 0; i < turns; i++ {
		messages = append(messages,
			map[string]any{"role": "assistant", "content": strings.Repeat("a", size)},
			map[string]any{"role": "user", "content": strings.Repeat("u", size)})
	}
	b, _ := json.Marshal(map[string]any{"model": "claude-opus-5-5", "max_tokens": 1000, "messages": messages})
	return string(b)
}

// windowed is a fake OpenAI whose model has a window of tokens.
func windowed(t *testing.T, tokens int, replies ...openaitest.Reply) (*openai.Backend, *openaitest.Server) {
	t.Helper()
	srv := openaitest.New(t, replies...)
	cfg := config.OpenAI{BaseURL: srv.URL, Key: "env:OPENAI_API_KEY", ContextTokens: map[string]int{"gpt-6.1-sol": tokens}}
	key, _ := openai.LoadKey(cfg, func(string) (string, bool) { return "janus-test-key", true })
	b, err := openai.NewBackend(cfg, key, nil)
	if err != nil {
		t.Fatal(err)
	}
	return b, srv
}

// A conversation larger than the OpenAI model's window has older turns
// left out, with a note, and the cut stays where it was on the next turn,
// so OpenAI's prompt cache holds (#407).
func TestAConversationIsCutToFitTheOpenAIWindowTheSameWayEachTurn(t *testing.T) {
	ok := openaitest.Answer(openaitest.Text{Text: "ok"})
	oa, srv := windowed(t, 4000, ok, ok)
	up := httptest.NewServer(never(t))
	defer up.Close()
	j := listRouter(t, up, oa, "openai")
	send := func(body string) result {
		req, _ := http.NewRequest(http.MethodPost, j.url+"/v1/messages", strings.NewReader(body))
		req.Header.Set("X-Claude-Code-Session-Id", "s1")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return result{resp.StatusCode, "", <-j.reported}
	}

	first := send(longConversation(10, 1000))
	if first.Exchange.Backend != "openai" || !strings.Contains(strings.Join(first.Exchange.Stages, ","), StageLeaveOut) ||
		!strings.Contains(first.Exchange.Reason, "earlier messages left out to fit gpt-6.1-sol's window") {
		t.Fatalf("first turn: %s, stages %v, reason %q", first.Exchange.Backend, first.Exchange.Stages, first.Exchange.Reason)
	}
	second := send(longConversation(11, 1000))
	a, b := string(srv.Requests()[0].Body), string(srv.Requests()[1].Body)
	if !strings.Contains(a, "were left out, to fit this model's context window") {
		t.Error("the model was not told turns were left out")
	}
	// The second turn repeats the first's input, item for item, and adds
	// to it.
	inputs := func(body string) []json.RawMessage {
		var v struct {
			Input []json.RawMessage `json:"input"`
		}
		_ = json.Unmarshal([]byte(body), &v)
		return v.Input
	}
	ia, ib := inputs(a), inputs(b)
	if len(ib) <= len(ia) {
		t.Fatalf("the second turn sent %d input items, the first %d", len(ib), len(ia))
	}
	for i := range ia {
		if string(ia[i]) != string(ib[i]) {
			t.Fatalf("input[%d] changed between turns, so the cache would break:\n%s\n%s", i, ia[i], ib[i])
		}
	}
	if second.Exchange.Backend != "openai" {
		t.Errorf("second turn went to %s", second.Exchange.Backend)
	}
}

// Too large to fit even with older turns left out, the chain moves on and
// the reason says why.
func TestAConversationTooLargeForTheWindowMovesOn(t *testing.T) {
	oa, srv := windowed(t, 1500)
	var sent []string
	up := httptest.NewServer(claudeAnswers(&sent))
	defer up.Close()
	j := listRouter(t, up, oa, "openai", "upstream")
	res := j.send(t, longConversation(3, 6000))
	if res.Exchange.Backend != "upstream" || len(srv.Requests()) != 0 || !strings.Contains(res.Exchange.Reason, "even its first and latest turns are more than the") {
		t.Errorf("went to %s, OpenAI asked %d times, reason %q", res.Exchange.Backend, len(srv.Requests()), res.Exchange.Reason)
	}
}
