package capture

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/risadams/janus/internal/anthropic"
	"github.com/risadams/janus/internal/proxy"
)

func n(i int) *int { return &i }

const request = `{"model":"claude-opus-5-5","stream":true,` +
	`"system":[{"type":"text","text":"You are an agent."}],` +
	`"tools":[{"name":"Read","description":"Read a file"},{"name":"Bash","description":"Run a command"}],` +
	`"messages":[{"role":"user","content":"Read notes.txt"},` +
	`{"role":"assistant","content":[{"type":"text","text":"On it."},{"type":"tool_use","id":"toolu_1","name":"Read","input":{"file_path":"notes.txt"}}]},` +
	`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"a line with ` + "```" + ` backticks"}]}]}`

const stream = "event: message_start\n" +
	`data: {"type":"message_start","message":{"id":"msg_1","model":"claude-opus-5-5","usage":{"input_tokens":2}}}` + "\n\n" +
	`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}` + "\n\n" +
	`data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"c2ln"}}` + "\n\n" +
	`data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}` + "\n\n" +
	`data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"It says "}}` + "\n\n" +
	`data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"hello."}}` + "\n\n" +
	`data: {"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"toolu_2","name":"Bash","input":{}}}` + "\n\n" +
	`data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"command\":"}}` + "\n\n" +
	`data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"\"ls\"}"}}` + "\n\n" +
	`data: {"type":"message_stop"}` + "\n\n"

func exchange() proxy.Exchange {
	header := http.Header{}
	header.Set("Authorization", "Bearer secret-token")
	header.Set("X-Api-Key", "sk-secret")
	header.Set("Cookie", "session=secret-cookie")
	header.Set("Anthropic-Version", "2023-06-01")
	respHeader := http.Header{}
	respHeader.Set("Content-Type", "text/event-stream; charset=utf-8")
	return proxy.Exchange{
		ID:             "20261011T143927.123-ab12cd",
		Start:          time.Date(2026, 10, 11, 14, 39, 27, 123e6, time.UTC),
		Method:         "POST",
		Path:           "/v1/messages",
		Header:         header,
		Status:         200,
		TTFB:           812 * time.Millisecond,
		Duration:       1931 * time.Millisecond,
		RequestBody:    []byte(request),
		ResponseHeader: respHeader,
		ResponseBody:   []byte(stream),
		Summary: &anthropic.Summary{Model: "claude-opus-5-5", StopReason: "tool_use", Complete: true,
			Usage: anthropic.Usage{Input: 2, Output: 118, CacheRead: n(18327), CacheWrite: n(0)}},
	}
}

func wantAll(t *testing.T, md string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(md, want) {
			t.Errorf("render is missing %q", want)
		}
	}
	if t.Failed() {
		t.Logf("render was:\n%s", md)
	}
}

func TestRenderMessagesExchange(t *testing.T) {
	md := Render(exchange())
	wantAll(t, md,
		"# POST /v1/messages",
		"- **ID:** 20261011T143927.123-ab12cd",
		"- **Status:** 200",
		"- **Duration:** 1.931s (first byte after 812ms)",
		"- **Answered by:** claude-opus-5-5, stop reason tool_use",
		"- **Tokens:** input 2, cache write 0, cache read 18327, output 118",
		"Anthropic-Version: 2023-06-01",
		"<model>\n\nclaude-opus-5-5\n\n</model>",
		"<system-prompt>\n\nYou are an agent.\n\n</system-prompt>",
		"2 tools: Read, Bash",
		"### user\n\nRead notes.txt\n\n",
		"### assistant\n\nOn it.\n\n",
		`"name": "Read"`,
		// The response is shown as the message its events add up to.
		"**thinking**\n\n_(signed; the text was not sent)_",
		"**text**\n\nIt says hello.",
		"**tool_use** Bash (toolu_2)",
		`"command": "ls"`,
	)
	if strings.Contains(md, "content_block_delta") {
		t.Error("the render shows raw stream events instead of the assembled message")
	}
}

func TestRenderRedactsCredentials(t *testing.T) {
	md := Render(exchange())
	for _, secret := range []string{"secret-token", "sk-secret", "secret-cookie"} {
		if strings.Contains(md, secret) {
			t.Errorf("the render contains the credential %q", secret)
		}
	}
	wantAll(t, md, "Authorization: [redacted]", "X-Api-Key: [redacted]", "Cookie: [redacted]")
}

// A tool result containing a code fence must not be able to close the
// fence the render puts around it.
func TestRenderFencesContentThatContainsFences(t *testing.T) {
	md := Render(exchange())
	wantAll(t, md, "````json\n{\n  \"type\": \"tool_result\"", "backticks\"\n}\n````")
}

func TestRenderOtherStates(t *testing.T) {
	cases := map[string]struct {
		edit  func(*proxy.Exchange)
		wants []string
	}{
		"api error": {func(ex *proxy.Exchange) {
			ex.Status = 429
			ex.ResponseHeader.Set("Content-Type", "application/json")
			ex.ResponseBody = []byte(`{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`)
			ex.Summary = &anthropic.Summary{Complete: true, Error: &anthropic.APIError{Type: "rate_limit_error", Message: "slow down"}}
		}, []string{"- **Error:** rate_limit_error: slow down", `"type": "rate_limit_error"`}},
		"cancelled and incomplete": {func(ex *proxy.Exchange) {
			ex.Cancelled = true
			ex.Summary.Complete = false
		}, []string{"- **Cancelled:**", "- **Incomplete:**"}},
		"unreadable": {func(ex *proxy.Exchange) {
			ex.Summary = nil
			ex.Unread = "content-encoding br"
			ex.ResponseBody = []byte{1, 2, 3}
		}, []string{"- **Response not read:** content-encoding br", "_3 bytes that janus could not decode (content-encoding br)._"}},
		"no response": {func(ex *proxy.Exchange) {
			ex.Status, ex.Summary, ex.ResponseBody, ex.ResponseHeader = 0, nil, nil, nil
		}, []string{"- **Status:** 0", "_(empty body)_"}},
		"truncated": {func(ex *proxy.Exchange) {
			ex.RequestTruncated, ex.ResponseTruncated = true, true
		}, []string{"The request was too large", "The response was too large"}},
		"not a messages call": {func(ex *proxy.Exchange) {
			ex.Path = "/v1/messages/count_tokens"
			ex.Summary = nil
			ex.ResponseHeader.Set("Content-Type", "application/json")
			ex.ResponseBody = []byte(`{"input_tokens":12}`)
		}, []string{"# POST /v1/messages/count_tokens", "<request>", `"input_tokens": 12`}},
		"request that is not json": {func(ex *proxy.Exchange) {
			ex.RequestBody = []byte("plain text body")
		}, []string{"<request>", "plain text body"}},
	}
	for name, c := range cases {
		ex := exchange()
		c.edit(&ex)
		md := Render(ex)
		for _, want := range c.wants {
			if !strings.Contains(md, want) {
				t.Errorf("%s: render is missing %q\n%s", name, want, md)
			}
		}
	}
}

func TestWriteSavesThreePrivateFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "captures")
	w := New(dir, 7, 1024)

	name, err := w.Write(exchange())
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join("2026-10-11", "20261011T143927.123-ab12cd_v1-messages"); name != want {
		t.Errorf("name = %q, want %q", name, want)
	}

	base := filepath.Join(dir, name)
	for ext, want := range map[string]string{".request.txt": request, ".response.txt": stream} {
		got, err := os.ReadFile(base + ext)
		if err != nil || string(got) != want {
			t.Errorf("%s: err=%v, content matches=%v; the raw files must be byte for byte", ext, err, string(got) == want)
		}
	}
	md, _ := os.ReadFile(base + ".md")
	if !strings.HasPrefix(string(md), "# POST /v1/messages") {
		t.Errorf(".md does not look like a render: %.60s", md)
	}

	for _, path := range []string{base + ".md", base + ".request.txt", base + ".response.txt"} {
		if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
			t.Errorf("%s mode = %o, want 600", filepath.Base(path), info.Mode().Perm())
		}
	}
	for _, path := range []string{dir, filepath.Dir(base)} {
		if info, _ := os.Stat(path); info.Mode().Perm() != 0o700 {
			t.Errorf("%s mode = %o, want 700", path, info.Mode().Perm())
		}
	}
}

func TestSanitize(t *testing.T) {
	for in, want := range map[string]string{
		"/v1/messages":                 "v1-messages",
		"/":                            "root",
		"/api/hello":                   "api-hello",
		"/../../etc/passwd":            "..-..-etc-passwd",
		"/v1/files/a b?c":              "v1-files-a-b-c",
		"/" + strings.Repeat("x", 200): strings.Repeat("x", 80),
	} {
		if got := sanitize(in); got != want {
			t.Errorf("sanitize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestWriteReportsFailure(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "file")
	os.WriteFile(blocker, nil, 0o600)
	if _, err := New(filepath.Join(blocker, "captures"), 7, 1024).Write(exchange()); err == nil {
		t.Error("Write into a path that cannot be a directory succeeded")
	}
}

// seed writes one capture's three files, each of the given size.
func seed(t *testing.T, dir, day, id string, size int) {
	t.Helper()
	os.MkdirAll(filepath.Join(dir, day), 0o700)
	for _, ext := range []string{".md", ".request.txt", ".response.txt"} {
		if err := os.WriteFile(filepath.Join(dir, day, id+"_v1-messages"+ext), make([]byte, size), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func remaining(t *testing.T, dir string) string {
	t.Helper()
	var names []string
	filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err == nil && path != dir {
			rel, _ := filepath.Rel(dir, path)
			names = append(names, rel)
		}
		return nil
	})
	return strings.Join(names, "\n")
}

func TestPruneByAge(t *testing.T) {
	dir := t.TempDir()
	seed(t, dir, "2026-10-01", "old", 10)
	seed(t, dir, "2026-10-04", "edge", 10)
	seed(t, dir, "2026-10-11", "new", 10)
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)

	// Seven days back from noon on the 11th is noon on the 4th. The 4th's
	// directory still holds captures from that afternoon, so it stays.
	(&Writer{dir: dir, retain: 7 * 24 * time.Hour}).Prune(now)

	got := remaining(t, dir)
	if strings.Contains(got, "2026-10-01") {
		t.Errorf("a directory past retention survived:\n%s", got)
	}
	if !strings.Contains(got, "2026-10-04") || !strings.Contains(got, "2026-10-11") {
		t.Errorf("a directory within retention was removed:\n%s", got)
	}
}

func TestPruneBySizeRemovesOldestWholeCaptures(t *testing.T) {
	dir := t.TempDir()
	seed(t, dir, "2026-10-10", "20261010T100000.000-aaaaaa", 100) // 300 bytes
	seed(t, dir, "2026-10-11", "20261011T090000.000-bbbbbb", 100) // 300 bytes
	seed(t, dir, "2026-10-11", "20261011T100000.000-cccccc", 100) // 300 bytes

	// 900 bytes on disk and room for 550: one capture is not enough to
	// remove, and the second must go whole even though dropping two of its
	// files would already fit.
	(&Writer{dir: dir, maxBytes: 550}).Prune(time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC))

	got := remaining(t, dir)
	want := strings.Join([]string{
		"2026-10-11",
		"2026-10-11/20261011T100000.000-cccccc_v1-messages.md",
		"2026-10-11/20261011T100000.000-cccccc_v1-messages.request.txt",
		"2026-10-11/20261011T100000.000-cccccc_v1-messages.response.txt",
	}, "\n")
	if got != want {
		t.Errorf("after pruning:\n%s\nwant:\n%s", got, want)
	}
}

func TestPruneLeavesAnythingJanusDidNotMake(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "notes.txt"), make([]byte, 5000), 0o600)
	os.MkdirAll(filepath.Join(dir, "my-saved-captures"), 0o700)
	os.WriteFile(filepath.Join(dir, "my-saved-captures", "keep.md"), make([]byte, 5000), 0o600)
	os.WriteFile(filepath.Join(dir, "2020-01-01"), []byte("a file, not a day directory"), 0o600)
	seed(t, dir, "2020-01-02", "ancient", 10)

	(&Writer{dir: dir, retain: 24 * time.Hour, maxBytes: 1}).Prune(time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC))

	got := remaining(t, dir)
	for _, keep := range []string{"notes.txt", "my-saved-captures/keep.md", "2020-01-01"} {
		if !strings.Contains(got, keep) {
			t.Errorf("%s was removed; prune must only touch day directories:\n%s", keep, got)
		}
	}
	if strings.Contains(got, "2020-01-02") {
		t.Errorf("an expired day directory survived:\n%s", got)
	}
}

func TestPruneWithNoLimitsOrNoDirectoryDoesNothing(t *testing.T) {
	dir := t.TempDir()
	seed(t, dir, "2020-01-02", "ancient", 1000)
	(&Writer{dir: dir}).Prune(time.Now())
	if !strings.Contains(remaining(t, dir), "ancient") {
		t.Error("with no limits set, a capture was removed")
	}
	if err := (&Writer{dir: filepath.Join(dir, "missing"), retain: time.Hour}).Prune(time.Now()); err != nil {
		t.Errorf("Prune of a directory that does not exist = %v", err)
	}
}

func TestWriteShadowSitsBesideTheRealCaptureAndIsPrunedWithIt(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "captures")
	w := New(dir, 0, 0)
	name, err := w.Write(exchange())
	if err != nil {
		t.Fatal(err)
	}
	shadow := exchange()
	shadow.ID += "-shadow"
	shadow.RequestBody = []byte(`{"model":"qwen/qwen3.8-27b","messages":[]}`)
	if err := w.WriteShadow(name, shadow); err != nil {
		t.Fatal(err)
	}

	base := filepath.Join(dir, name)
	for ext, want := range map[string]string{".shadow.request.txt": `{"model":"qwen/qwen3.8-27b","messages":[]}`, ".shadow.response.txt": stream} {
		got, err := os.ReadFile(base + ext)
		if err != nil || string(got) != want {
			t.Errorf("%s: err %v, matches %v", ext, err, string(got) == want)
		}
		if info, _ := os.Stat(base + ext); info != nil && info.Mode().Perm() != 0o600 {
			t.Errorf("%s mode = %o, want 600", ext, info.Mode().Perm())
		}
	}
	if md, _ := os.ReadFile(base + ".shadow.md"); !strings.Contains(string(md), "-shadow") || strings.Contains(string(md), "secret-token") {
		t.Errorf(".shadow.md is wrong or leaks a credential")
	}

	// All six files belong to one capture, so they go together.
	if stem(base+".shadow.response.txt") != base || stem(base+".md") != base {
		t.Errorf("stem does not group shadow files with their capture")
	}
	(&Writer{dir: dir, maxBytes: 1}).Prune(time.Now())
	if left := remaining(t, dir); left != "" {
		t.Errorf("after pruning to nothing, these remain:\n%s", left)
	}
}

func TestWriteAttemptSavesTheRejectedAnswerBesideTheCapture(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "captures")
	w := New(dir, 0, 0)
	name, _ := w.Write(exchange())
	if err := w.WriteAttempt(name, []byte(`{"type":"message","content":[]}`)); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name) + ".attempt.response.txt"
	data, err := os.ReadFile(path)
	if err != nil || string(data) != `{"type":"message","content":[]}` {
		t.Errorf("attempt file: %q, %v", data, err)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %o, want 600", info.Mode().Perm())
	}
	if stem(path) != filepath.Join(dir, name) {
		t.Error("the attempt file is not grouped with its capture for pruning")
	}
}

func TestWriteSavesWhatWasSentWhenItDiffers(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "captures")
	w := New(dir, 0, 0)

	plain, _ := w.Write(exchange())
	if _, err := os.Stat(filepath.Join(dir, plain) + ".sent.txt"); err == nil {
		t.Error("a request sent as received got a .sent.txt")
	}

	changed := exchange()
	changed.ID = "20261011T143928.000-ffffff"
	changed.SentBody = []byte(`{"model":"claude-opus-5-5","messages":[{"role":"user","content":"key JANUS_REDACTED_0123456789ab"}]}`)
	name, err := w.Write(changed)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name) + ".sent.txt"
	data, err := os.ReadFile(path)
	if err != nil || string(data) != string(changed.SentBody) {
		t.Errorf(".sent.txt: %q, %v", data, err)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %o, want 600", info.Mode().Perm())
	}
	if stem(path) != filepath.Join(dir, name) {
		t.Error(".sent.txt is not grouped with its capture for pruning")
	}
}
