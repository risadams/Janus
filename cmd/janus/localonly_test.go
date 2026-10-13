package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/risadams/janus/internal/config"
	"github.com/risadams/janus/internal/ledger"
	"github.com/risadams/janus/internal/proxy"
)

func TestNewPolicyUsesTheConfig(t *testing.T) {
	cfg := config.Default()
	if newPolicy(cfg) != nil {
		t.Error("a policy was built with nothing to protect")
	}
	cfg.Guard.LocalOnly = config.LocalOnly{Paths: []string{"secrets/**"}}
	if newPolicy(cfg) == nil {
		t.Error("no policy was built")
	}
}

func TestRecordNotesLocalOnlyContent(t *testing.T) {
	why := []string{`Read read /app/secrets/db.yaml, which "secrets/**" makes local-only`}
	cases := []struct {
		name    string
		backend string
		guard   proxy.Guard
		console string
	}{
		{"withheld", "upstream", proxy.Guard{LocalOnly: why, Withheld: 1}, "↳ local-only: 1 withheld from upstream (" + why[0] + ")"},
		{"kept local", "local", proxy.Guard{LocalOnly: why}, "↳ local-only: kept on this machine (" + why[0] + ")"},
		// With the redactor on as well, and nothing for it to find.
		{"withheld and scanned", "upstream", proxy.Guard{LocalOnly: why, Withheld: 1, Scanned: true}, "↳ local-only: 1 withheld from upstream"},
	}
	for _, c := range cases {
		var console bytes.Buffer
		ex := exchangeCarrying("nothing secret")
		ex.Backend, ex.Guard = c.backend, &c.guard
		rec := record(ex, ledger.Open(filepath.Join(t.TempDir(), "ledger")), nil, nil, testConsole(&console))
		g := rec.Guard
		if g == nil || len(g.LocalOnly) != 1 || g.Withheld != c.guard.Withheld || g.Findings != 0 {
			t.Errorf("%s: guard = %+v", c.name, g)
			continue
		}
		if (g.Redacted != nil) != c.guard.Scanned {
			t.Errorf("%s: redacted = %v, but scanned = %v", c.name, g.Redacted, c.guard.Scanned)
		}
		if !strings.Contains(console.String(), c.console) {
			t.Errorf("%s: console = %q", c.name, console.String())
		}
	}
}

// localOnlyFixture is a ledger of four requests under a local-only policy,
// and a config file that sets the policy.
func localOnlyFixture(t *testing.T) (dir, configPath string) {
	t.Helper()
	dir = t.TempDir()
	l := ledger.Open(filepath.Join(dir, "ledger"))
	day := filepath.Join(dir, "captures", "2026-10-11")
	os.MkdirAll(day, 0o700)
	when := time.Date(2026, 10, 11, 18, 0, 0, 0, time.UTC)

	conversation := func(result string) string {
		b, _ := json.Marshal(map[string]any{"model": "claude-opus-5-5", "messages": []map[string]any{
			{"role": "user", "content": "read it"},
			{"role": "assistant", "content": []map[string]any{{"type": "tool_use", "id": "t1", "name": "Read", "input": map[string]any{"file_path": "/app/secrets/db.yaml"}}}},
			{"role": "user", "content": []map[string]any{{"type": "tool_result", "tool_use_id": "t1", "content": result}}},
		}})
		return string(b)
	}
	why := []string{`Read read /app/secrets/db.yaml, which "secrets/**" makes local-only`}
	notice := "[Withheld by janus: this content is local-only and was not sent. " + why[0] + ". " +
		"Do not ask for it again or guess at it; tell the user that this content is kept on their machine.]"
	add := func(id string, rec ledger.Record, request, sent string) {
		rec.V, rec.ID, rec.Time = 1, id, when
		rec.Method, rec.Path, rec.Outcome = "POST", "/v1/messages", ledger.OutcomeOK
		when = when.Add(time.Minute)
		rec.Capture = filepath.Join("2026-10-11", id+"_v1-messages")
		os.WriteFile(filepath.Join(day, id+"_v1-messages.request.txt"), []byte(request), 0o600)
		if sent != "" {
			os.WriteFile(filepath.Join(day, id+"_v1-messages.sent.txt"), []byte(sent), 0o600)
		}
		if err := l.Append(rec); err != nil {
			t.Fatal(err)
		}
	}
	original := conversation("value: not-for-anthropic")
	add("p1-withheld", ledger.Record{Class: "main", Backend: "upstream", Reason: "passthrough",
		Guard: &ledger.Guard{LocalOnly: why, Withheld: 1}}, original, conversation(notice))
	add("p2-local", ledger.Record{Class: "main", Backend: "local", Route: "local", Reason: "local-only policy: " + why[0],
		Guard: &ledger.Guard{LocalOnly: why}}, original, original)
	add("p3-refused", ledger.Record{Class: "main", Backend: "none", Route: "local", Status: 502, Reason: "local-only policy: refused",
		Guard: &ledger.Guard{LocalOnly: why}}, original, "")
	// Sent before the policy was set: the content went.
	add("p4-before", ledger.Record{Class: "main", Backend: "upstream", Reason: "passthrough"}, original, "")

	cfg := config.Default()
	cfg.Features.Guard = true
	cfg.Guard.LocalOnly = config.LocalOnly{Paths: []string{"secrets/**"}}
	data, _ := json.Marshal(cfg)
	configPath = filepath.Join(dir, "janus.config.json")
	os.WriteFile(configPath, data, 0o600)
	return dir, configPath
}

func TestAuditReportsLocalOnlyContent(t *testing.T) {
	dir, configPath := localOnlyFixture(t)
	var out, errOut bytes.Buffer
	if code := run([]string{"audit", "-config", configPath, "-dir", dir}, &out, &errOut); code != 0 || errOut.Len() > 0 {
		t.Fatalf("exit %d, stderr %q", code, errOut.String())
	}
	wantLines(t, out.String(),
		"Where 4 requests carrying your content went",
		"1 to Anthropic, exactly as your client sent them",
		"1 to Anthropic, changed by janus on the way (1 with local-only content withheld)",
		"1 to the local model only, never to Anthropic",
		"1 refused or stopped by janus, and sent nowhere",
		"Local-only content",
		"1 requests went to Anthropic with it replaced by a notice",
		"2 requests were kept from Anthropic because of it",
	)
	if strings.Contains(out.String(), "not-for-anthropic") {
		t.Error("the audit printed local-only content")
	}
}

// -verify checks what was sent against the policy as well as for secrets.
func TestAuditVerifyFindsLocalOnlyContentThatWasSent(t *testing.T) {
	dir, configPath := localOnlyFixture(t)
	var out, errOut bytes.Buffer
	code := run([]string{"audit", "-config", configPath, "-dir", dir, "-verify"}, &out, &errOut)
	if code != 1 {
		t.Errorf("exit %d, want 1: local-only content went upstream in p4", code)
	}
	wantLines(t, out.String(),
		"Verification: re-read 2 requests as they were sent to Anthropic",
		"1 contain nothing that looks like a secret",
		"1 STILL CONTAIN SOMETHING THAT LOOKS LIKE A SECRET",
		`p4-before local-only content in Read read /app/secrets/db.yaml, which "secrets/**" makes local-only`,
	)
	if strings.Contains(out.String(), "p1-withheld") {
		t.Error("a withheld request was reported as a leak")
	}
}

func TestAuditOneRequestShowsWhatWasWithheld(t *testing.T) {
	dir, configPath := localOnlyFixture(t)
	var out, errOut bytes.Buffer
	if code := run([]string{"audit", "-config", configPath, "-dir", dir, "p1-withheld"}, &out, &errOut); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errOut.String())
	}
	wantLines(t, out.String(), `Local-only 1 pieces withheld before sending: Read read /app/secrets/db.yaml, which "secrets/**" makes local-only`)
	if strings.Contains(out.String(), "NOT replaced") {
		t.Errorf("the audit says secrets were sent:\n%s", out.String())
	}
}
