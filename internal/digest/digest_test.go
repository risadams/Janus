package digest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/risadams/janus/internal/anthropic"
	"github.com/risadams/janus/internal/transform"
)

// model stands in for the local model.
type model struct {
	answer string
	err    error
	asked  []string
}

func (m *model) Ask(ctx context.Context, system, user string, maxTokens int) (string, anthropic.Usage, error) {
	m.asked = append(m.asked, user)
	return m.answer, anthropic.Usage{}, m.err
}

const fetchTool = `{"name":"mcp__janus__tool_output","description":"Fetch a digested tool result.","input_schema":{"type":"object"}}`

var bigOutput = strings.Repeat("ok   github.com/x/y/pkg   0.2s\n", 300) + "FAIL github.com/x/y/broken [build failed]\n"

// request is a turn in which Claude ran a tool and got output back.
func request(t *testing.T, tool, output string, extra string) *anthropic.Request {
	t.Helper()
	content, _ := json.Marshal(output)
	body := fmt.Sprintf(`{"model":"claude-opus-5-5","tools":[{"name":"Bash"}%s],"messages":[`+
		`{"role":"user","content":"run the tests"},`+
		`{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":%q,"input":{"command":"go test ./..."}}]},`+
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":%s%s}]}]}`, ifSet(extra != "", ","+extra), tool, content, "")
	req, err := anthropic.ParseRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func ifSet(ok bool, s string) string {
	if ok {
		return s
	}
	return ""
}

func digester(t *testing.T, m *model) (*Digest, *[]string) {
	t.Helper()
	var logged []string
	return &Digest{Model: m, Store: NewStore(filepath.Join(t.TempDir(), "originals"), 7), MinBytes: 4000,
		Logf: func(f string, a ...any) { logged = append(logged, fmt.Sprintf(f, a...)) }}, &logged
}

func at(req *anthropic.Request) transform.Position {
	return transform.Position{Request: req, Message: 2, Block: 0, Role: "user"}
}

func resultBlock(req *anthropic.Request) anthropic.Block {
	return req.Messages()[2].Blocks()[0]
}

func TestDigestsALargeResultAndKeepsTheOriginal(t *testing.T) {
	m := &model{answer: "1 package failed to build: github.com/x/y/broken. 300 others passed."}
	d, _ := digester(t, m)
	req := request(t, "Bash", bigOutput, fetchTool)

	out, ok := d.Rewrite(at(req), resultBlock(req))
	if !ok {
		t.Fatal("a large Bash result was not digested")
	}
	var r anthropic.ToolResultBlock
	out.Decode(&r)
	var text string
	json.Unmarshal(r.Content, &text)
	if !strings.Contains(text, fmt.Sprintf("a local model digested this Bash output (%s bytes, 302 lines)", commas(len(bigOutput)))) ||
		!strings.Contains(text, "call mcp__janus__tool_output with id") || !strings.HasSuffix(text, m.answer) {
		t.Errorf("digest = %q", text)
	}
	if r.ToolUseID != "toolu_1" {
		t.Errorf("the digest lost the tool_use_id: %s", out.Raw())
	}
	if len(m.asked) != 1 || !strings.Contains(m.asked[0], "FAIL github.com/x/y/broken") {
		t.Errorf("the model was not given the output: %q", m.asked)
	}

	id := text[strings.Index(text, `id "`)+4:]
	id = id[:strings.Index(id, `"`)]
	if got, err := d.Store.Get(id); err != nil || got != bigOutput {
		t.Errorf("Get(%q) = %d bytes, %v; want the original", id, len(got), err)
	}
}

func TestLeavesAloneWhatItShould(t *testing.T) {
	image := `[{"type":"text","text":"` + strings.Repeat("a", 5000) + `"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}]`
	cases := map[string]struct {
		req   func(t *testing.T) *anthropic.Request
		setup func(d *Digest, m *model)
		log   string
	}{
		"small": {req: func(t *testing.T) *anthropic.Request { return request(t, "Bash", "ok\n", fetchTool) }},
		"an error": {req: func(t *testing.T) *anthropic.Request {
			req := request(t, "Bash", bigOutput, fetchTool)
			b := resultBlock(req).With("is_error", json.RawMessage("true"))
			msgs := req.Messages()
			msgs[2] = msgs[2].WithBlocks([]anthropic.Block{b})
			req.SetMessages(msgs)
			return req
		}},
		"with an image": {req: func(t *testing.T) *anthropic.Request {
			req := request(t, "Read", "", fetchTool)
			b := resultBlock(req).With("content", json.RawMessage(image))
			msgs := req.Messages()
			msgs[2] = msgs[2].WithBlocks([]anthropic.Block{b})
			req.SetMessages(msgs)
			return req
		}},
		"another tool": {
			req:   func(t *testing.T) *anthropic.Request { return request(t, "Read", bigOutput, fetchTool) },
			setup: func(d *Digest, m *model) { d.Tools = []string{"Bash"} },
		},
		"protected": {
			req: func(t *testing.T) *anthropic.Request { return request(t, "Bash", bigOutput, fetchTool) },
			setup: func(d *Digest, m *model) {
				d.Protected = func(req *anthropic.Request, message, block int, id string) bool { return id == "toolu_1" }
			},
		},
		"no way to fetch the original": {
			req: func(t *testing.T) *anthropic.Request { return request(t, "Bash", bigOutput, "") },
			log: "offers no mcp__<server>__tool_output tool",
		},
		"only an old digest names the fetching tool": {
			req: func(t *testing.T) *anthropic.Request {
				return request(t, "Bash", bigOutput+"\n[janus: ... call mcp__janus__tool_output with id \"0123456789abcdef\".]", "")
			},
			log: "offers no mcp__<server>__tool_output tool",
		},
		"the model fails": {
			req:   func(t *testing.T) *anthropic.Request { return request(t, "Bash", bigOutput, fetchTool) },
			setup: func(d *Digest, m *model) { m.err = errors.New("the local model did not answer within 30s") },
			log:   "sent as it is: the local model did not answer within 30s",
		},
	}
	for name, c := range cases {
		m := &model{answer: "short"}
		d, logged := digester(t, m)
		if c.setup != nil {
			c.setup(d, m)
		}
		req := c.req(t)
		if _, ok := d.Rewrite(at(req), resultBlock(req)); ok {
			t.Errorf("%s: digested", name)
		}
		if c.log != "" && (len(*logged) != 1 || !strings.Contains((*logged)[0], c.log)) {
			t.Errorf("%s: logged %q, want a line saying %q", name, *logged, c.log)
		}
		if c.log == "" && len(*logged) != 0 {
			t.Errorf("%s: logged %q, want nothing", name, *logged)
		}
	}
}

func TestDigestsATextBlockList(t *testing.T) {
	m := &model{answer: "short"}
	d, _ := digester(t, m)
	req := request(t, "Bash", "", fetchTool)
	list, _ := json.Marshal([]map[string]string{{"type": "text", "text": bigOutput[:5000]}, {"type": "text", "text": bigOutput[5000:]}})
	b := resultBlock(req).With("content", list)
	if _, ok := d.Rewrite(at(req), b); !ok || len(m.asked) != 1 || !strings.Contains(m.asked[0], "FAIL github.com/x/y/broken") {
		t.Errorf("a result of text blocks was not digested whole: ok=%v asked=%d", ok, len(m.asked))
	}
}

// Through the pipeline, the guard keeps a digest only if it is smaller, and
// the memo sends the same digest on every later turn without asking again.
func TestInThePipeline(t *testing.T) {
	m := &model{answer: "1 package failed to build: github.com/x/y/broken."}
	d, _ := digester(t, m)
	p := transform.New(d)
	header := map[string][]string{"X-Claude-Code-Session-Id": {"s"}}

	first := request(t, "Bash", "", fetchTool)
	first.SetMessages(first.Messages()[:1])
	p.Apply(header, first)

	req := request(t, "Bash", bigOutput, fetchTool)
	effects := p.Apply(header, req)
	if len(effects) != 1 || effects[0].Rewritten != 1 || effects[0].Tokens < 2500 {
		t.Fatalf("effects = %+v, want one digest cutting about 3,000 tokens", effects)
	}
	sent := string(req.Messages()[2].Raw())

	again := request(t, "Bash", bigOutput, fetchTool)
	p.Apply(header, again)
	if string(again.Messages()[2].Raw()) != sent || len(m.asked) != 1 {
		t.Errorf("the next turn sent another form, or asked the model again (%d times)", len(m.asked))
	}
}

func TestStore(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "originals")
	s := NewStore(dir, 7)
	id, err := s.Put("hello\n")
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := s.Put("hello\n"); again != id {
		t.Errorf("the same text got two ids: %s, %s", id, again)
	}
	if got, err := s.Get(" " + id + "\n"); err != nil || got != "hello\n" {
		t.Errorf("Get = %q, %v", got, err)
	}
	for _, bad := range []string{"../../etc/passwd", "0123456789abcdeg", "", strings.Repeat("a", 64)} {
		if _, err := s.Get(bad); err == nil || errors.Is(err, ErrNotFound) {
			t.Errorf("Get(%q) = %v, want it refused as not an id", bad, err)
		}
	}
	if _, err := s.Get("0123456789abcdef"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get of an unknown id = %v, want ErrNotFound", err)
	}

	if runtime.GOOS != "windows" {
		info, _ := os.Stat(filepath.Join(dir, id+".txt"))
		dirInfo, _ := os.Stat(dir)
		if info.Mode().Perm() != 0o600 || dirInfo.Mode().Perm() != 0o700 {
			t.Errorf("modes: file %v, dir %v; want 0600 and 0700", info.Mode().Perm(), dirInfo.Mode().Perm())
		}
	}

	stray := filepath.Join(dir, "notes.txt")
	os.WriteFile(stray, []byte("not ours"), 0o600)
	old := time.Now().Add(-8 * 24 * time.Hour)
	os.Chtimes(filepath.Join(dir, id+".txt"), old, old)
	os.Chtimes(stray, old, old)
	s.Prune(time.Now())
	if _, err := s.Get(id); !errors.Is(err, ErrNotFound) {
		t.Errorf("an original past retention was kept: %v", err)
	}
	if _, err := os.Stat(stray); err != nil {
		t.Error("Prune deleted a file it did not make")
	}
}

func TestTargetWords(t *testing.T) {
	for bytes, want := range map[int]int{100: 80, 4000: 200, 9000: 400, 1 << 20: 400} {
		if got := targetWords(bytes); got != want {
			t.Errorf("targetWords(%d) = %d, want %d", bytes, got, want)
		}
	}
}

func TestToolOutput(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "originals"), 7)
	text := "one\ntwo\nerror: three\nfour\nerror: five\n"
	id, _ := store.Put(text)
	call := func(args string) (string, bool) {
		return Tool(store).Call(context.Background(), json.RawMessage(args))
	}
	cases := []struct {
		args, want string
		failed     bool
	}{
		{`{"id":"` + id + `"}`, "     1\tone\n     2\ttwo\n     3\terror: three\n     4\tfour\n     5\terror: five\n", false},
		{`{"id":"` + id + `","from_line":2,"to_line":3}`, "     2\ttwo\n     3\terror: three\n", false},
		{`{"id":"` + id + `","pattern":"^error"}`, "     3\terror: three\n     5\terror: five\n", false},
		{`{"id":"` + id + `","pattern":"nothing"}`, `no line between 1 and 5 matches "nothing"; the output has 5 lines`, false},
		{`{"id":"` + id + `","from_line":9}`, "from_line 9 is past the end; the output has 5 lines", true},
		{`{"id":"` + id + `","pattern":"("}`, "the pattern is not a valid regular expression", true},
		{`{"id":"0123456789abcdef"}`, "no original with that id", true},
		{`{"id":"../secrets"}`, `"../secrets" is not an id`, true},
	}
	for _, c := range cases {
		got, failed := call(c.args)
		if failed != c.failed || !strings.HasPrefix(got, c.want) {
			t.Errorf("%s: got %q (failed %v), want %q (failed %v)", c.args, got, failed, c.want, c.failed)
		}
	}

	long := strings.Repeat(strings.Repeat("x", 99)+"\n", 2000)
	id, _ = store.Put(long)
	got, failed := call(`{"id":"` + id + `"}`)
	if failed || len(got) > toolOutputLimit+200 || !strings.Contains(got, "cut short at line") || !strings.Contains(got, "ask for from_line") {
		t.Errorf("a 200 kB original came back as %d bytes, failed %v; want it cut short with a way to read on", len(got), failed)
	}
}

// The command shapes are from the 2026-10-12 captures, where 28 of the 41
// Bash results over 4 KB were files printed for reading.
func TestPrintsFile(t *testing.T) {
	cases := map[string]bool{
		"cat docs/ROADMAP.md":                                                                      true,
		"cd /p && sed -n 150,300p internal/guard/redact.go":                                        true,
		"cd /p && sed -n '/## Decision/,$p' docs/research/models.md":                               true,
		"cd /p && wc -l internal/proxy/*.go && sed -n 1,370p proxy.go":                             true,
		"go test ./internal/guard/ 2>&1 | grep FAIL; sed -n 205,250p internal/proxy/guard_test.go": true,
		"head -n 40 main.go":       true,
		"tail -5 /tmp/run/output":  true,
		"nl -ba cmd/janus/main.go": true,
		"sed -E -n 1,20p a.go":     true,
		// Reading another command's output is not printing a file.
		"cd ~ && janus stats 2>&1 | head -40; echo ---; jq -c '{id}' logs/ledger/2026-10-12.jsonl": false,
		"git branch -D old | tail -1; gh project item-edit --project-id PVT_x":                     false,
		"wc -l internal/route/*.go | sort -n | tail -20; grep -n '^func' internal/x.go":            false,
		"cat * | python3 -c 'import sys'":                                                          false,
		"go test -v ./... 2>&1 | head -n 40":                                                       false,
		"git log --stat -40":                                                                       false,
		"sed -i '' 's/a/b/' x.go && go test ./...":                                                 false,
		"gh issue view 42 --json body":                                                             false,
	}
	for command, want := range cases {
		input, _ := json.Marshal(map[string]string{"command": command})
		if got := printsFile(input); got != want {
			t.Errorf("printsFile(%q) = %v, want %v", command, got, want)
		}
	}
}

func TestLeavesAPrintedFileAlone(t *testing.T) {
	m := &model{answer: "short"}
	d, _ := digester(t, m)
	req := request(t, "Bash", bigOutput, fetchTool)
	b := req.Messages()[1].Blocks()[0].With("input", json.RawMessage(`{"command":"sed -n 1,400p internal/route/router.go"}`))
	msgs := req.Messages()
	msgs[1] = msgs[1].WithBlocks([]anthropic.Block{b})
	req.SetMessages(msgs)
	if _, ok := d.Rewrite(at(req), resultBlock(req)); ok || len(m.asked) != 0 {
		t.Error("the output of sed -n on a source file was digested")
	}
}

func TestReadResultsAreDigestedOnlyWhenAsked(t *testing.T) {
	for _, c := range []struct {
		tools []string
		want  bool
	}{
		{nil, false},
		{[]string{"Bash"}, false},
		{[]string{"Read"}, true},
		{[]string{"Bash", "Read"}, true},
	} {
		m := &model{answer: "short"}
		d, _ := digester(t, m)
		d.Tools = c.tools
		req := request(t, "Read", bigOutput, fetchTool)
		if _, ok := d.Rewrite(at(req), resultBlock(req)); ok != c.want {
			t.Errorf("tools %v: digested %v, want %v", c.tools, ok, c.want)
		}
	}
}
