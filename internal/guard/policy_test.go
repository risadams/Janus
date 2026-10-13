package guard

import (
	"bytes"
	"strings"
	"testing"

	"github.com/risadams/janus/internal/anthropic"
)

func policy(t *testing.T, paths, markers []string) *Policy {
	t.Helper()
	p, err := NewPolicy(paths, markers)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPathGlobs(t *testing.T) {
	p := policy(t, []string{"secrets/**", "*.pem", ".env*", "config/prod.yaml", "private", "keys/*.json"}, nil)
	cases := map[string]string{ // path -> the glob that protects it, or ""
		"secrets/db.yaml":                "secrets/**",
		"secrets/nested/deep/key":        "secrets/**",
		"/Users/ris/app/secrets/db.yaml": "secrets/**",
		"./secrets/x":                    "secrets/**",
		"id_rsa.pem":                     "*.pem",
		"/etc/ssl/private/server.pem":    "*.pem",
		".env":                           ".env*",
		".env.production":                ".env*",
		"app/.env.local":                 ".env*",
		"config/prod.yaml":               "config/prod.yaml",
		"/srv/app/config/prod.yaml":      "config/prod.yaml",
		"private":                        "private",
		"private/notes.txt":              "private", // a directory protects what is in it
		"keys/service.json":              "keys/*.json",
		// Not protected.
		"src/main.go":              "",
		"secrets.go":               "",
		"mysecrets/db.yaml":        "",
		"docs/pem-format.md":       "",
		"environment.yaml":         "",
		"config/dev.yaml":          "",
		"config/prod.yaml.example": "",
		"privateer/ship.txt":       "",
		"keys/nested/service.json": "", // * does not cross a directory
		"README.md":                "",
	}
	for path, want := range cases {
		got, _ := p.matchPath(path)
		if got != want {
			t.Errorf("matchPath(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestPolicyRejectsPatternsThatProtectEverythingOrNothing(t *testing.T) {
	for _, bad := range []string{"", "*", "**", "**/*", "/", "  "} {
		if _, err := NewPolicy([]string{bad}, nil); err == nil {
			t.Errorf("NewPolicy accepted the path %q", bad)
		}
	}
	if _, err := NewPolicy(nil, []string{"ok", " "}); err == nil {
		t.Error("NewPolicy accepted an empty marker")
	}
}

func toolUse(id, name string, input map[string]any) map[string]any {
	return map[string]any{"type": "tool_use", "id": id, "name": name, "input": input}
}
func toolResult(id, content string) map[string]any {
	return map[string]any{"type": "tool_result", "tool_use_id": id, "content": content}
}

// conversation is: read an ordinary file, read a protected one, run a
// command on a protected one, and get a document carrying a marker.
func conversation(t *testing.T) *anthropic.Request {
	return request(t, "You are an agent.",
		user(textBlock("look at the config")),
		assistant(toolUse("t1", "Read", map[string]any{"file_path": "/app/src/main.go"})),
		user(toolResult("t1", "package main")),
		assistant(toolUse("t2", "Read", map[string]any{"file_path": "/app/secrets/db.yaml"})),
		user(toolResult("t2", "value: hunter2-do-not-share")),
		assistant(toolUse("t3", "Bash", map[string]any{"command": "cat ./deploy/id_rsa.pem | head -3"})),
		user(toolResult("t3", "-----BEGIN KEY----- abc")),
		assistant(toolUse("t4", "Read", map[string]any{"file_path": "/app/docs/plan.md"})),
		user(toolResult("t4", "COMPANY CONFIDENTIAL\nthe acquisition target is Acme")),
		user(textBlock("and this note is COMPANY CONFIDENTIAL too: launch is in May")),
	)
}

func TestCheckFindsProtectedContentByPathAndByMarker(t *testing.T) {
	p := policy(t, []string{"secrets/**", "*.pem"}, []string{"COMPANY CONFIDENTIAL"})
	m := p.Check(conversation(t))

	if !m.Protected() {
		t.Fatal("nothing found")
	}
	want := []string{
		`Bash read ./deploy/id_rsa.pem, which "*.pem" makes local-only`,
		`Read read /app/secrets/db.yaml, which "secrets/**" makes local-only`,
		`messages[8] carries the marker "COMPANY CONFIDENTIAL"`,
		`messages[9] carries the marker "COMPANY CONFIDENTIAL"`,
	}
	if strings.Join(m.Reasons, "\n") != strings.Join(want, "\n") {
		t.Errorf("reasons:\n%s\nwant:\n%s", strings.Join(m.Reasons, "\n"), strings.Join(want, "\n"))
	}
	// Reasons go into logs and error messages: paths and markers, never
	// the content.
	for _, secret := range []string{"hunter2", "BEGIN KEY", "Acme", "launch is in May"} {
		if strings.Contains(strings.Join(m.Reasons, " "), secret) {
			t.Errorf("a reason contains protected content: %q", secret)
		}
	}
}

func TestCheckFindsNothingInOrdinaryWork(t *testing.T) {
	p := policy(t, []string{"secrets/**", "*.pem", ".env*"}, []string{"COMPANY CONFIDENTIAL"})
	req := request(t, "sys",
		user(textBlock("the secrets directory is where keys go; see docs/pem-format.md")),
		assistant(toolUse("t1", "Read", map[string]any{"file_path": "/app/src/secrets.go"})),
		user(toolResult("t1", "package secrets // loads .pem files")),
		assistant(toolUse("t2", "Bash", map[string]any{"command": "ls src && go test ./..."})),
		user(toolResult("t2", "ok")),
	)
	if m := p.Check(req); m.Protected() {
		t.Errorf("ordinary work was marked local-only: %v", m.Reasons)
	}
	var none *Policy
	if none.Check(req).Protected() || policy(t, nil, nil).Check(conversation(t)).Protected() {
		t.Error("an absent or empty policy protected something")
	}
}

func TestWithholdReplacesProtectedContentAndNothingElse(t *testing.T) {
	p := policy(t, []string{"secrets/**", "*.pem"}, []string{"COMPANY CONFIDENTIAL"})
	req := conversation(t)
	before := req.Messages()
	m := p.Check(req)

	if n := p.Withhold(req, m); n != 4 {
		t.Fatalf("withheld %d pieces, want 4", n)
	}
	out := req.Bytes()
	for _, gone := range []string{"hunter2", "BEGIN KEY", "Acme", "launch is in May"} {
		if bytes.Contains(out, []byte(gone)) {
			t.Errorf("protected content is still in the request: %q", gone)
		}
	}
	// What was not protected is untouched: the ordinary file, the model's
	// own tool calls, and the first message.
	after, err := anthropic.ParseRequest(out)
	if err != nil {
		t.Fatalf("the request does not parse after withholding: %v", err)
	}
	msgs := after.Messages()
	if len(msgs) != len(before) {
		t.Fatalf("%d messages, want %d", len(msgs), len(before))
	}
	for _, i := range []int{0, 1, 2, 3, 5, 7} {
		if string(msgs[i].Raw()) != string(before[i].Raw()) {
			t.Errorf("messages[%d] changed though it held nothing protected", i)
		}
	}
	// A withheld tool result is still a result for the same call, so the
	// conversation stays well-formed, and it says why it is empty.
	var result anthropic.ToolResultBlock
	msgs[4].Blocks()[0].Decode(&result)
	inner, _ := anthropic.ParseContent(result.Content)
	if result.ToolUseID != "t2" || len(inner) != 1 || !strings.Contains(inner[0].Text(), "Withheld by janus") || !strings.Contains(inner[0].Text(), "/app/secrets/db.yaml") {
		t.Errorf("withheld result = id %q, %q", result.ToolUseID, inner[0].Text())
	}
	if text := msgs[9].Blocks()[0].Text(); !strings.Contains(text, "Withheld by janus") || !strings.Contains(text, "COMPANY CONFIDENTIAL") {
		t.Errorf("withheld text = %q", text)
	}
}

// The same conversation must come out the same every time, and a longer
// one must begin with the shorter, or the prompt cache is lost each turn.
func TestWithholdIsStableAcrossTurns(t *testing.T) {
	p := policy(t, []string{"secrets/**"}, nil)
	turn := func(extra ...map[string]any) []byte {
		msgs := []map[string]any{
			user(textBlock("read it")),
			assistant(toolUse("t1", "Read", map[string]any{"file_path": "secrets/db.yaml"})),
			user(toolResult("t1", "value: hunter2")),
		}
		req := request(t, "sys", append(msgs, extra...)...)
		p.Withhold(req, p.Check(req))
		return req.Bytes()
	}
	a, again := turn(), turn()
	if !bytes.Equal(a, again) {
		t.Error("withholding the same request twice gave different bytes")
	}
	b := turn(assistant(textBlock("I cannot see that file.")), user(textBlock("ok, what else?")))
	// Everything up to the end of the last message of the shorter
	// conversation: the test's requests list messages first.
	prefix := a[:bytes.Index(a, []byte(`],"model"`))]
	if !bytes.HasPrefix(a, []byte(`{"messages":[`)) || !bytes.HasPrefix(b, prefix) {
		t.Error("the withheld conversation's earlier turns changed when it grew")
	}
}

func TestWithholdWithNothingProtectedChangesNothing(t *testing.T) {
	p := policy(t, []string{"secrets/**"}, nil)
	req := request(t, "sys", user(textBlock("hello")))
	raw := string(req.Bytes())
	if n := p.Withhold(req, p.Check(req)); n != 0 || req.Modified() || string(req.Bytes()) != raw {
		t.Errorf("withheld %d, modified %v", n, req.Modified())
	}
}

func TestRemainingIsZeroOnlyAfterWithholding(t *testing.T) {
	p := policy(t, []string{"secrets/**", "*.pem"}, []string{"COMPANY CONFIDENTIAL"})
	req := conversation(t)
	if n := p.Remaining(req); n != 4 {
		t.Errorf("Remaining before = %d, want 4", n)
	}
	if n := p.Withhold(req, p.Check(req)); n != 4 {
		t.Errorf("Withhold = %d, want 4", n)
	}
	if n := p.Remaining(req); n != 0 {
		t.Errorf("Remaining after = %d, want 0", n)
	}
	// Going over it again finds nothing to do and changes nothing.
	before := string(req.Bytes())
	if n := p.Withhold(req, p.Check(req)); n != 0 || string(req.Bytes()) != before {
		t.Errorf("a second pass withheld %d and changed the request", n)
	}
	if n := p.Remaining(conversationWithoutProtected(t)); n != 0 {
		t.Errorf("Remaining on ordinary work = %d", n)
	}
}

func conversationWithoutProtected(t *testing.T) *anthropic.Request {
	return request(t, "sys",
		user(textBlock("hello")),
		assistant(toolUse("t1", "Read", map[string]any{"file_path": "/app/src/main.go"})),
		user(toolResult("t1", "package main")),
	)
}

// A search across the tree quotes protected files without the tool call
// ever naming them.
func TestSearchOutputQuotingAProtectedFileLosesThoseLines(t *testing.T) {
	p := policy(t, []string{"secrets/**", ".env*"}, nil)
	grep := "src/config.go:12:\tpath := \"secrets/db.yaml\"\n" +
		"secrets/db.yaml-1-db_host: db.internal\n" +
		"secrets/db.yaml:2:db_label: tangerine-walrus\n" +
		"./app/.env.production:DB_LABEL=tangerine-walrus\n" +
		"README.md:40:set the passphrase in .env"
	listing := "total 3\n.env\nsecrets/db.yaml\nsrc/main.go\nnote: see secrets"
	req := request(t, "sys",
		user(textBlock("where is the passphrase set? it is in secrets/db.yaml: look there")),
		assistant(toolUse("t1", "Grep", map[string]any{"pattern": "passphrase", "path": "/app"})),
		user(toolResult("t1", grep)),
		assistant(toolUse("t2", "Bash", map[string]any{"command": "ls -R"})),
		user(map[string]any{"type": "tool_result", "tool_use_id": "t2", "cache_control": map[string]any{"type": "ephemeral"},
			"content": []map[string]any{{"type": "text", "text": listing}}}),
	)

	m := p.Check(req)
	if m.Protected() {
		t.Errorf("a search made the whole request local-only: %v", m.Reasons)
	}
	want := []string{
		`a tool result quotes a file that ".env*" makes local-only`,
		`a tool result quotes a file that "secrets/**" makes local-only`,
	}
	if strings.Contains(strings.Join(m.Quoted, " "), "db_host") {
		t.Errorf("a reason repeats part of a protected line: %v", m.Quoted)
	}
	if !m.Any() || strings.Join(m.Quoted, "\n") != strings.Join(want, "\n") {
		t.Fatalf("quoted:\n%s\nwant:\n%s", strings.Join(m.Quoted, "\n"), strings.Join(want, "\n"))
	}
	if n := p.Remaining(req); n != 1 {
		t.Errorf("Remaining before = %d, want 1: the search result", n)
	}
	if n := p.Withhold(req, m); n != 1 {
		t.Errorf("Withhold = %d, want 1", n)
	}

	out := string(req.Bytes())
	for _, gone := range []string{"tangerine-walrus", "db.internal"} {
		if strings.Contains(out, gone) {
			t.Errorf("%q is still in the request", gone)
		}
	}
	// The lines from other files, the listing, and what the user typed
	// are all still there.
	for _, kept := range []string{`src/config.go:12:`, `README.md:40:set the passphrase in .env`, `total 3\n.env\nsecrets/db.yaml\nsrc/main.go`,
		"it is in secrets/db.yaml: look there", `"cache_control":{"type":"ephemeral"}`} {
		if !strings.Contains(out, kept) {
			t.Errorf("%q is missing from the request:\n%s", kept, out)
		}
	}
	if n := strings.Count(out, "janus withheld this line"); n != 3 {
		t.Errorf("%d lines withheld, want 3", n)
	}
	if n := p.Remaining(req); n != 0 {
		t.Errorf("Remaining after = %d", n)
	}
	before := string(req.Bytes())
	if n := p.Withhold(req, p.Check(req)); n != 0 || string(req.Bytes()) != before {
		t.Errorf("a second pass withheld %d and changed the request", n)
	}
}

func TestWithholdKeepsACacheBreakpointOnWhatItReplaces(t *testing.T) {
	p := policy(t, []string{"secrets/**"}, []string{"COMPANY CONFIDENTIAL"})
	cached := map[string]any{"type": "ephemeral", "ttl": "1h"}
	req := request(t, "sys",
		user(map[string]any{"type": "text", "text": "COMPANY CONFIDENTIAL plan", "cache_control": cached}),
		assistant(toolUse("t1", "Read", map[string]any{"file_path": "secrets/a"})),
		user(map[string]any{"type": "tool_result", "tool_use_id": "t1", "content": "hunter2", "is_error": false, "cache_control": cached}),
	)
	if n := p.Withhold(req, p.Check(req)); n != 2 {
		t.Fatalf("Withhold = %d, want 2", n)
	}
	out := string(req.Bytes())
	if strings.Count(out, `"cache_control":{"ttl":"1h","type":"ephemeral"}`)+strings.Count(out, `"cache_control":{"type":"ephemeral","ttl":"1h"}`) != 2 {
		t.Errorf("a cache breakpoint was lost:\n%s", out)
	}
	if strings.Contains(out, "hunter2") || strings.Contains(out, "CONFIDENTIAL plan") {
		t.Errorf("protected content is still there:\n%s", out)
	}
}

func TestMatchCovers(t *testing.T) {
	p, err := NewPolicy([]string{".env"}, []string{"COMPANY CONFIDENTIAL"})
	if err != nil {
		t.Fatal(err)
	}
	req, err := anthropic.ParseRequest([]byte(`{"model":"m","messages":[` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Read","input":{"file_path":"/p/.env"}},{"type":"tool_use","id":"t2","name":"Read","input":{"file_path":"/p/main.go"}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"KEY=1"},{"type":"tool_result","tool_use_id":"t2","content":"package main"},{"type":"text","text":"COMPANY CONFIDENTIAL plan"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	m := p.Check(req)
	for _, c := range []struct {
		message, block int
		id             string
		want           bool
	}{
		{1, 0, "t1", true},  // the result of reading .env
		{1, 1, "t2", false}, // main.go
		{1, 2, "", true},    // a marker
	} {
		if got := m.Covers(c.message, c.block, c.id); got != c.want {
			t.Errorf("Covers(%d, %d, %q) = %v, want %v", c.message, c.block, c.id, got, c.want)
		}
	}
}
