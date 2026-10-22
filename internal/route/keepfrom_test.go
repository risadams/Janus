package route

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/risadams/janus/internal/guard"
	"github.com/risadams/janus/internal/openai/openaitest"
)

// With content kept from one provider and not the other (#406), a list
// passes over the provider it may not reach and goes on to the next; with
// none left, it is refused, naming the policy; content the policy does not
// name goes as the list says.
func TestContentKeptFromOneProviderGoesToTheNext(t *testing.T) {
	kf := guard.NewKeepFrom()
	if err := kf.Add("openai", []string{"vendor-x/**"}, []string{"PROJECT ORION"}, false); err != nil {
		t.Fatal(err)
	}
	body := func(text string) string {
		b, _ := json.Marshal(map[string]any{"model": "claude-opus-5-5", "max_tokens": 100,
			"messages": []map[string]any{{"role": "user", "content": text}}})
		return string(b)
	}
	const kept = "PROJECT ORION ships in May; plan the launch."

	oa, srv := fakeOpenAI(t, openaitest.Answer(openaitest.Text{Text: "From OpenAI."}))
	var sent []string
	up := httptest.NewServer(claudeAnswers(&sent))
	defer up.Close()
	j := listRouter(t, up, oa, "openai", "upstream")
	j.Barred = kf.Barred

	res := j.send(t, body(kept))
	if res.Exchange.Backend != "upstream" || len(srv.Requests()) != 0 || !strings.Contains(res.Exchange.Reason, `guard.keepFrom.openai keeps this from it (messages[0] carries the marker "PROJECT ORION")`) {
		t.Errorf("kept content: went to %s, OpenAI asked %d times, reason %q", res.Exchange.Backend, len(srv.Requests()), res.Exchange.Reason)
	}
	if res := j.send(t, body("Plan the launch.")); res.Exchange.Backend != "openai" {
		t.Errorf("other content: went to %s", res.Exchange.Backend)
	}

	only := listRouter(t, up, oa, "openai")
	only.Barred = kf.Barred
	res = only.send(t, body(kept))
	if res.Status != http.StatusForbidden || !strings.Contains(res.Body, "janus: no provider on the list can take this request: guard.keepFrom.openai") {
		t.Errorf("nowhere allowed: status %d, body %s", res.Status, res.Body)
	}
	if len(srv.Requests()) != 1 || len(sent) != 1 {
		t.Errorf("OpenAI asked %d times, Claude %d", len(srv.Requests()), len(sent))
	}
}
