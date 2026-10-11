package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/risadams/janus/internal/backend"
)

// asked is one question the fake local model received.
type asked struct {
	Model, System, User string
	MaxTokens           int
	Header              http.Header
}

type fakeModel struct {
	mu     sync.Mutex
	asked  []asked
	answer func(n int, a asked) (status int, body string) // n counts from 1
}

func (f *fakeModel) questions() []asked {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]asked(nil), f.asked...)
}

func message(text string) string {
	b, _ := json.Marshal(map[string]any{
		"type": "message", "model": "qwen/qwen3.8-27b", "stop_reason": "end_turn",
		"content": []map[string]string{{"type": "text", "text": text}},
		"usage":   map[string]int{"input_tokens": 1000, "output_tokens": 50},
	})
	return string(b)
}

// setup makes a project directory and a Worker whose model is a fake with
// the given loaded context and behaviour.
func setup(t *testing.T, contextTokens int, answer func(n int, a asked) (int, string)) (*Worker, *fakeModel, string, *[]Call) {
	t.Helper()
	fake := &fakeModel{answer: answer}
	if fake.answer == nil {
		fake.answer = func(int, asked) (int, string) { return 200, message("It says hello (notes.txt:1).") }
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v0/models" {
			fmt.Fprintf(w, `{"data":[{"id":"qwen/qwen3.8-27b","state":"loaded","loaded_context_length":%d}]}`, contextTokens)
			return
		}
		var body struct {
			Model     string                     `json:"model"`
			System    string                     `json:"system"`
			MaxTokens int                        `json:"max_tokens"`
			Messages  []struct{ Content string } `json:"messages"`
		}
		raw, _ := io.ReadAll(r.Body)
		json.Unmarshal(raw, &body)
		a := asked{Model: body.Model, System: body.System, MaxTokens: body.MaxTokens, Header: r.Header.Clone()}
		if len(body.Messages) > 0 {
			a.User = body.Messages[0].Content
		}
		fake.mu.Lock()
		fake.asked = append(fake.asked, a)
		n := len(fake.asked)
		fake.mu.Unlock()
		status, out := fake.answer(n, a)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		io.WriteString(w, out)
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)

	root := t.TempDir()
	var calls []Call
	w := &Worker{
		Model:  &Model{Local: backend.NewLocal(u, nil), ID: "qwen/qwen3.8-27b", Timeout: 2 * time.Second},
		Root:   root,
		Record: func(c Call) { calls = append(calls, c) },
	}
	return w, fake, root, &calls
}

func put(t *testing.T, root, name, content string) {
	t.Helper()
	path := filepath.Join(root, name)
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func ask(w *Worker, args map[string]any) (string, bool) {
	raw, _ := json.Marshal(args)
	return w.Tools()[0].Call(context.Background(), raw)
}

func TestAskFilesGivesTheModelNumberedFilesAndReturnsItsAnswer(t *testing.T) {
	w, fake, root, calls := setup(t, 100000, nil)
	put(t, root, "notes.txt", "hello\nworld\n")
	put(t, root, "src/main.go", "package main\n\nfunc main() {}\n")

	out, failed := ask(w, map[string]any{"paths": []string{"notes.txt", "src/*.go"}, "question": "What does notes.txt say?"})
	if failed {
		t.Fatalf("failed: %s", out)
	}
	if !strings.HasPrefix(out, "It says hello (notes.txt:1).") {
		t.Errorf("answer = %q", out)
	}
	// The footer tells Claude who read what, and how far to trust it.
	if !strings.Contains(out, "[janus: qwen/qwen3.8-27b read 2 files, 41 bytes; this is its reading, not a quotation]") {
		t.Errorf("footer missing or wrong:\n%s", out)
	}

	qs := fake.questions()
	if len(qs) != 1 {
		t.Fatalf("%d questions put to the model, want 1 for files this small", len(qs))
	}
	q := qs[0]
	if q.Model != "qwen/qwen3.8-27b" || q.MaxTokens != 300*2+200 {
		t.Errorf("model %q, max_tokens %d", q.Model, q.MaxTokens)
	}
	for _, want := range []string{
		"QUESTION: What does notes.txt say?",
		"=== notes.txt ===\n1\thello\n2\tworld\n",
		"=== src/main.go ===\n1\tpackage main\n2\t\n3\tfunc main() {}\n",
	} {
		if !strings.Contains(q.User, want) {
			t.Errorf("the model was not shown %q in:\n%s", want, q.User)
		}
	}
	if !strings.Contains(q.System, "path:line") || !strings.Contains(q.System, "At most 300 words") || !strings.Contains(q.System, "Do not guess") {
		t.Errorf("system prompt = %q", q.System)
	}
	if q.Header.Get("Authorization") != "" {
		t.Error("a credential header was sent to the local model")
	}

	c := (*calls)[0]
	if len(*calls) != 1 || c.Tool != "ask_files" || c.Files != 2 || c.BytesRead != 41 || c.ModelCalls != 1 || c.Failed ||
		c.Usage.Input != 1000 || c.Usage.Output != 50 || c.AnswerBytes != len(out) || c.Duration <= 0 {
		t.Errorf("recorded call = %+v", c)
	}
}

func TestAskFilesReadsLargeInputInPartsThenCombines(t *testing.T) {
	// A 4,000-token context gives parts of the 12 kB minimum, so 40 kB of
	// log is read in several passes.
	w, fake, root, calls := setup(t, 4000, func(n int, a asked) (int, string) {
		if strings.Contains(a.User, "NOTES TAKEN WHILE READING") {
			return 200, message("The error is on line 700 (big.log:700).")
		}
		if strings.Contains(a.User, "700\tERROR disk full") {
			return 200, message("big.log:700 ERROR disk full")
		}
		return 200, message("nothing relevant")
	})
	var log strings.Builder
	for i := 1; i <= 1000; i++ {
		if i == 700 {
			log.WriteString("ERROR disk full\n")
		} else {
			fmt.Fprintf(&log, "INFO request %d handled without incident\n", i)
		}
	}
	put(t, root, "big.log", log.String())

	out, failed := ask(w, map[string]any{"paths": []string{"big.log"}, "question": "Where is the error?", "max_words": 50})
	if failed || !strings.HasPrefix(out, "The error is on line 700 (big.log:700).") {
		t.Fatalf("failed=%v out=%q", failed, out)
	}

	qs := fake.questions()
	parts, final := qs[:len(qs)-1], qs[len(qs)-1]
	if len(parts) < 3 {
		t.Fatalf("%d reading passes, want several", len(parts))
	}
	for i, p := range parts {
		if !strings.Contains(p.User, fmt.Sprintf("PART %d OF %d", i+1, len(parts))) || !strings.Contains(p.System, "Do not answer the question") {
			t.Errorf("pass %d was not a note-taking pass: %.80q", i+1, p.User)
		}
		if len(p.User) > 14<<10 {
			t.Errorf("pass %d was %d bytes, over the size the model can take", i+1, len(p.User))
		}
		// A part that starts mid-file says which file it continues, and
		// line numbers carry on rather than restarting.
		if i > 0 && !strings.Contains(p.User, "=== big.log (continued) ===") {
			t.Errorf("pass %d does not say which file it continues", i+1)
		}
	}
	// Every line reached the model exactly once.
	all := ""
	for _, p := range parts {
		all += p.User
	}
	for _, line := range []string{"1\tINFO request 1 handled", "700\tERROR disk full", "1000\tINFO request 1000 handled"} {
		if strings.Count(all, line) != 1 {
			t.Errorf("%q appears %d times across the passes, want once", line, strings.Count(all, line))
		}
	}
	// The final pass sees the notes, not the file.
	if !strings.Contains(final.User, "big.log:700 ERROR disk full") || strings.Contains(final.User, "INFO request 5 handled") || final.MaxTokens != 50*2+200 {
		t.Errorf("final pass: max_tokens %d, user %.200q", final.MaxTokens, final.User)
	}
	if c := (*calls)[0]; c.ModelCalls != len(qs) || c.Usage.Input != 1000*len(qs) {
		t.Errorf("recorded %d model calls and %d input tokens for %d questions", c.ModelCalls, c.Usage.Input, len(qs))
	}
}

func TestAskFilesStaysInsideTheRoot(t *testing.T) {
	w, fake, root, _ := setup(t, 100000, nil)
	outside := t.TempDir()
	put(t, outside, "secret.txt", "the password is swordfish\n")
	put(t, root, "ok.txt", "fine\n")
	os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(root, "link.txt"))
	os.Symlink(outside, filepath.Join(root, "linkdir"))

	for name, path := range map[string]string{
		"an absolute path":            filepath.Join(outside, "secret.txt"),
		"dot-dot":                     "../" + filepath.Base(outside) + "/secret.txt",
		"a link that leads out":       "link.txt",
		"a file through a linked dir": "linkdir/secret.txt",
		"a glob that reaches a link":  "*.txt",
	} {
		out, failed := ask(w, map[string]any{"paths": []string{path}, "question": "What is the password?"})
		if !failed || !strings.Contains(out, "outside the project root") {
			t.Errorf("%s: failed=%v out=%q", name, failed, out)
		}
	}
	for _, q := range fake.questions() {
		if strings.Contains(q.User, "swordfish") {
			t.Fatal("a file outside the root was shown to the model")
		}
	}

	// A path that is inside is unaffected, absolute or not.
	real, _ := filepath.EvalSymlinks(root)
	if out, failed := ask(w, map[string]any{"paths": []string{filepath.Join(real, "ok.txt")}, "question": "?"}); failed {
		t.Errorf("an absolute path inside the root was refused: %s", out)
	}
}

func TestAskFilesInputProblems(t *testing.T) {
	w, fake, root, calls := setup(t, 100000, nil)
	put(t, root, "a.txt", "text\n")
	put(t, root, "image.bin", "PNG\x00\x00binary\x00data")
	os.MkdirAll(filepath.Join(root, "dir"), 0o755)
	for i := 0; i < 70; i++ {
		put(t, root, fmt.Sprintf("many/f%02d.txt", i), "x\n")
	}

	cases := map[string]struct {
		args   map[string]any
		wantIn string
	}{
		"no question":             {map[string]any{"paths": []string{"a.txt"}}, "Give both paths"},
		"no paths":                {map[string]any{"question": "?"}, "Give both paths"},
		"nothing matches":         {map[string]any{"paths": []string{"nope.txt"}, "question": "?"}, "nope.txt matched nothing."},
		"a directory":             {map[string]any{"paths": []string{"dir"}, "question": "?"}, "dir is a directory; name files or use a glob such as dir/*."},
		"too many files":          {map[string]any{"paths": []string{"many/*.txt"}, "question": "?"}, "match 70 files and the limit is 60"},
		"a bad glob":              {map[string]any{"paths": []string{"[unclosed"}, "question": "?"}, "not a valid path or glob"},
		"arguments not an object": {nil, "Give both paths"},
	}
	for name, c := range cases {
		out, failed := ask(w, c.args)
		if !failed || !strings.Contains(out, c.wantIn) {
			t.Errorf("%s: failed=%v out=%q, want it to fail mentioning %q", name, failed, out, c.wantIn)
		}
	}
	if n := len(fake.questions()); n != 0 {
		t.Errorf("the model was asked %d times for requests that should have been turned away first", n)
	}
	if len(*calls) != len(cases) || !(*calls)[0].Failed || (*calls)[0].Why == "" {
		t.Errorf("%d calls recorded, want every one, failed, with a reason", len(*calls))
	}

	// Binary files are skipped; text beside them is still read.
	out, failed := ask(w, map[string]any{"paths": []string{"a.txt", "image.bin"}, "question": "?"})
	if failed || !strings.Contains(out, "read 2 files") || strings.Contains(fake.questions()[0].User, "PNG") {
		t.Errorf("failed=%v out=%q", failed, out)
	}
}

func TestAskFilesBoundsWhatItReadsAndWhatItReturns(t *testing.T) {
	// Too much for the model to read in the allowed number of passes.
	w, fake, root, _ := setup(t, 4000, nil)
	put(t, root, "huge.log", strings.Repeat("a line of log output that goes on for a while\n", 12000)) // about 550 kB
	out, failed := ask(w, map[string]any{"paths": []string{"huge.log"}, "question": "?"})
	if !failed || !strings.Contains(out, "too much to read in one call") || len(fake.questions()) != 0 {
		t.Errorf("failed=%v out=%q, model asked %d times", failed, out, len(fake.questions()))
	}

	// A model that ignores the length limit is cut off.
	w, _, root, _ = setup(t, 100000, func(int, asked) (int, string) { return 200, message(strings.Repeat("word ", 5000)) })
	put(t, root, "a.txt", "x\n")
	out, failed = ask(w, map[string]any{"paths": []string{"a.txt"}, "question": "?", "max_words": 20})
	if failed || !strings.Contains(out, "[cut off at the length limit]") || len(out) > 20*9+400 {
		t.Errorf("failed=%v, %d bytes returned for a 20-word limit", failed, len(out))
	}

	// max_words is clamped, not trusted.
	w, fake, root, _ = setup(t, 100000, nil)
	put(t, root, "a.txt", "x\n")
	ask(w, map[string]any{"paths": []string{"a.txt"}, "question": "?", "max_words": 999999})
	if got := fake.questions()[0].MaxTokens; got != 1500*2+200 {
		t.Errorf("max_tokens = %d, want the cap", got)
	}
}

func TestAskFilesWhenTheModelFails(t *testing.T) {
	cases := map[string]struct {
		answer func(int, asked) (int, string)
		wantIn string
	}{
		"an error": {func(int, asked) (int, string) {
			return 400, `{"type":"error","error":{"type":"invalid_request_error","message":"too long"}}`
		}, "invalid_request_error: too long"},
		"a server error": {func(int, asked) (int, string) { return 500, "boom" }, "HTTP 500"},
		"an empty answer": {func(int, asked) (int, string) {
			return 200, `{"type":"message","content":[],"stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":9}}`
		}, "empty answer"},
		"not a message": {func(int, asked) (int, string) { return 200, `{"error":"Unexpected endpoint"}` }, "could not be read"},
	}
	for name, c := range cases {
		w, _, root, calls := setup(t, 100000, c.answer)
		put(t, root, "a.txt", "x\n")
		out, failed := ask(w, map[string]any{"paths": []string{"a.txt"}, "question": "?"})
		// The failure tells Claude what to do instead.
		if !failed || !strings.Contains(out, c.wantIn) || !strings.Contains(out, "Read the files yourself instead.") {
			t.Errorf("%s: failed=%v out=%q", name, failed, out)
		}
		if c := (*calls)[0]; !c.Failed || c.ModelCalls != 1 {
			t.Errorf("%s: recorded call = %+v", name, c)
		}
	}

	// Too slow.
	w, _, root, _ := setup(t, 100000, func(int, asked) (int, string) { time.Sleep(400 * time.Millisecond); return 200, message("late") })
	w.Model.Timeout = 100 * time.Millisecond
	put(t, root, "a.txt", "x\n")
	if out, failed := ask(w, map[string]any{"paths": []string{"a.txt"}, "question": "?"}); !failed || !strings.Contains(out, "did not answer within 100ms") {
		t.Errorf("timeout: failed=%v out=%q", failed, out)
	}
}

func TestToolDefinitionIsUsable(t *testing.T) {
	w, _, _, _ := setup(t, 100000, nil)
	tools := w.Tools()
	if len(tools) != 1 || tools[0].Name != "ask_files" {
		t.Fatalf("tools = %+v", tools)
	}
	var schema struct {
		Type       string
		Properties map[string]any
		Required   []string
	}
	if err := json.Unmarshal(tools[0].InputSchema, &schema); err != nil {
		t.Fatalf("the input schema is not valid JSON: %v", err)
	}
	if schema.Type != "object" || schema.Properties["paths"] == nil || schema.Properties["question"] == nil || strings.Join(schema.Required, ",") != "paths,question" {
		t.Errorf("schema = %+v", schema)
	}
	// The description is what makes Claude choose the tool, and what stops
	// it trusting the answer too far.
	for _, want := range []string{"never enter your context", "Do NOT use it to find a string", "This tool is slow", "Read a file yourself when you need its exact text", "not a quotation"} {
		if !strings.Contains(tools[0].Description, want) {
			t.Errorf("description is missing %q", want)
		}
	}
}
