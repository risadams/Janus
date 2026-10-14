package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/risadams/janus/internal/ledger"
)

// auditFixture lays out a log directory holding a ledger and captures for
// six requests, one of each fate, and returns the directory.
func auditFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	l := ledger.Open(filepath.Join(dir, "ledger"))
	day := filepath.Join(dir, "captures", "2026-10-11")
	os.MkdirAll(day, 0o700)
	when := time.Date(2026, 10, 11, 18, 0, 0, 0, time.UTC)
	one := 1

	body := func(text string) string {
		b, _ := json.Marshal(map[string]any{"model": "claude-opus-5-5", "messages": []map[string]any{{"role": "user", "content": text}}})
		return string(b)
	}
	add := func(id string, rec ledger.Record, request, sent string) {
		rec.V, rec.ID, rec.Time = 1, id, when
		rec.Method, rec.Outcome = "POST", ledger.OutcomeOK
		if rec.Path == "" {
			rec.Path = "/v1/messages"
		}
		when = when.Add(time.Minute)
		if request != "" {
			rec.Capture = filepath.Join("2026-10-11", id+"_v1-messages")
			os.WriteFile(filepath.Join(day, id+"_v1-messages.request.txt"), []byte(request), 0o600)
			if sent != "" {
				os.WriteFile(filepath.Join(day, id+"_v1-messages.sent.txt"), []byte(sent), 0o600)
			}
		}
		if err := l.Append(rec); err != nil {
			t.Fatal(err)
		}
	}
	secret := "aws_access_key_id = " + fakeAWSKey
	guardFound := func() *ledger.Guard {
		return &ledger.Guard{Findings: 1, Kinds: map[string]int{"aws_access_key": 1}, Where: []string{"messages[0] user (text)"}}
	}

	// 1. Sent as received, nothing in it.
	add("r1-clean", ledger.Record{Class: "main", Backend: "upstream", Reason: "passthrough"}, body("count the lines"), "")
	// 2. Redacted before sending.
	redacted := guardFound()
	redacted.Redacted, redacted.Restored = &one, 2
	add("r2-redacted", ledger.Record{Class: "main", Backend: "upstream", Reason: "passthrough", Guard: redacted}, body(secret), body("aws_access_key_id = JANUS_REDACTED_0123456789ab"))
	// 3. Sent with the secret in it: the guard was only detecting.
	add("r3-exposed", ledger.Record{Class: "main", Backend: "upstream", Reason: "passthrough", Guard: guardFound()}, body(secret), "")
	// 4. Answered locally; never went to Anthropic, secret and all.
	add("r4-local", ledger.Record{Class: "subagent", Backend: "local", Route: "local", Reason: "rule subagents", Stages: []string{"set_model"}}, body(secret), body(secret))
	// 5. Stopped by the guard.
	add("r5-stopped", ledger.Record{Class: "main", Backend: "none", Reason: "guard: unreadable; not sent", Status: 500,
		Guard: &ledger.Guard{Blocked: "the request could not be read to check it for secrets"}}, "{not json", "")
	// 6. Sent upstream after repair, with no capture saved.
	add("r6-repaired", ledger.Record{Class: "subagent", Backend: "upstream", Reason: "passthrough", Stages: []string{"drop_unsigned_thinking"}}, "", "")
	// Not requests carrying content: left out of the audit.
	add("h1", ledger.Record{Class: "connectivity", Backend: "upstream", Method: "HEAD", Path: "/api/hello"}, "", "")
	add("s1", ledger.Record{Class: "main", Backend: "local", Route: "shadow", ShadowOf: "r1-clean"}, "", "")
	return dir
}

func runAuditCmd(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	var out, errOut bytes.Buffer
	args = append([]string{"audit", "-config", filepath.Join(t.TempDir(), "none.json")}, args...)
	code = run(args, &out, &errOut)
	return out.String(), errOut.String(), code
}

func TestAuditSaysWhereEverythingWent(t *testing.T) {
	out, errOut, code := runAuditCmd(t, "-dir", auditFixture(t))
	if code != 0 || errOut != "" {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	wantLines(t, out,
		"Where 6 requests carrying your content went",
		"2 to Anthropic, exactly as your client sent them",
		"2 to Anthropic, changed by janus on the way (1 with secrets replaced, 1 with a local model's thinking removed)",
		"1 to the local model only, never to Anthropic",
		"1 refused or stopped by janus, and sent nowhere",
		"2 requests bound for Anthropic carried something that looks like a secret (aws_access_key 2).",
		"1 had them replaced with placeholders before sending",
		"1 were SENT WITH THE SECRETS IN THEM",
		"r3-exposed aws_access_key in messages[0] user (text)",
		"Treat those secrets as disclosed and rotate them.",
		"Stopped by the guard r5-stopped the request could not be read to check it for secrets",
	)
	if strings.Contains(out, fakeAWSKey) {
		t.Error("the audit printed a secret")
	}
	if strings.Contains(out, "Verification") {
		t.Error("verification ran without -verify")
	}
}

// -verify does not take the ledger's word for it: it reads what was sent.
func TestAuditVerifyReadsWhatWasSent(t *testing.T) {
	out, _, code := runAuditCmd(t, "-dir", auditFixture(t), "-verify")
	if code != 1 {
		t.Errorf("exit %d, want 1 because a request was sent with a secret in it", code)
	}
	wantLines(t, out,
		"Verification: re-read 4 requests as they were sent to Anthropic",
		// r1 was clean; r2 is checked as it was sent, redacted, and is clean.
		"2 contain nothing that looks like a secret",
		"1 STILL CONTAIN SOMETHING THAT LOOKS LIKE A SECRET",
		"r3-exposed aws_access_key in messages[0] user (text)",
		"1 could not be checked: no capture was saved, or it has been pruned",
	)
	if strings.Contains(out, "r4-local") {
		t.Error("a request that never went to Anthropic was reported as a leak")
	}
	if strings.Contains(out, fakeAWSKey) {
		t.Error("verification printed a secret")
	}
}

func TestAuditVerifyPassesWhenEverythingSentIsClean(t *testing.T) {
	dir := auditFixture(t)
	// Take away the one exposed request.
	path := filepath.Join(dir, "ledger", "2026-10-11.jsonl")
	data, _ := os.ReadFile(path)
	var kept []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if !strings.Contains(line, "r3-exposed") {
			kept = append(kept, line)
		}
	}
	os.WriteFile(path, []byte(strings.Join(kept, "\n")+"\n"), 0o600)

	out, _, code := runAuditCmd(t, "-dir", dir, "-verify")
	if code != 0 {
		t.Errorf("exit %d, want 0\n%s", code, out)
	}
	wantLines(t, out, "0 STILL CONTAIN SOMETHING THAT LOOKS LIKE A SECRET", "0 were SENT WITH THE SECRETS IN THEM")
}

func TestAuditOneRequest(t *testing.T) {
	dir := auditFixture(t)

	out, _, code := runAuditCmd(t, "-dir", dir, "r2-redacted")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	wantLines(t, out,
		"Request r2-redacted",
		"Went to Anthropic",
		"Guard 1 found (aws_access_key) in messages[0] user (text)",
		"1 distinct secrets replaced before sending; 2 placeholders put back in the answer",
		"r2-redacted_v1-messages.sent.txt",
		"(as janus sent it; your client's original is beside it as .request.txt)",
	)

	out, _, _ = runAuditCmd(t, "-dir", dir, "r3-exposed")
	wantLines(t, out, "NOT replaced: the request was sent with them in it", "r3-exposed_v1-messages.request.txt", "(as your client sent it; janus did not change it)")

	out, _, _ = runAuditCmd(t, "-dir", dir, "r4-local")
	wantLines(t, out, "Went to the local model only; it was not sent to Anthropic", "Changed set_model")

	out, _, _ = runAuditCmd(t, "-dir", dir, "r5-stopped")
	wantLines(t, out, "Went to nowhere: janus refused or stopped it", "Guard stopped: the request could not be read")

	out, _, _ = runAuditCmd(t, "-dir", dir, "r6-repaired")
	wantLines(t, out, "Changed drop_unsigned_thinking", "Sent not saved: captures were off, or it has been pruned")
}

func TestAuditBodyPrintsExactlyWhatWasSent(t *testing.T) {
	dir := auditFixture(t)

	out, _, code := runAuditCmd(t, "-dir", dir, "-body", "r2-redacted")
	want, _ := os.ReadFile(filepath.Join(dir, "captures", "2026-10-11", "r2-redacted_v1-messages.sent.txt"))
	if code != 0 || out != string(want) || strings.Contains(out, fakeAWSKey) || !strings.Contains(out, "JANUS_REDACTED_") {
		t.Errorf("exit %d; -body should print the redacted request and nothing else:\n%s", code, out)
	}

	// A request janus did not change: what was sent is what the client sent.
	out, _, _ = runAuditCmd(t, "-dir", dir, "-body", "r1-clean")
	if !strings.Contains(out, "count the lines") {
		t.Errorf("-body of an unchanged request: %q", out)
	}

	for id, wantIn := range map[string]string{
		"r5-stopped":  "was not sent anywhere",
		"r6-repaired": "no capture of request r6-repaired was saved",
		"no-such-id":  `no request "no-such-id"`,
	} {
		out, errOut, code := runAuditCmd(t, "-dir", dir, "-body", id)
		if code != 1 || out != "" || !strings.Contains(errOut, wantIn) {
			t.Errorf("-body %s: exit %d, stdout %q, stderr %q", id, code, out, errOut)
		}
	}
}

func TestAuditOfNothingAndBadArguments(t *testing.T) {
	out, _, code := runAuditCmd(t, "-dir", t.TempDir())
	if code != 0 || !strings.Contains(out, "No requests carrying content were recorded") {
		t.Errorf("exit %d: %s", code, out)
	}
	dir := auditFixture(t)
	for _, args := range [][]string{{"-dir", dir, "-body"}, {"-dir", dir, "a", "b"}, {"-dir", dir, "-since", "yesterday"}} {
		if _, errOut, code := runAuditCmd(t, args...); code != 2 || errOut == "" {
			t.Errorf("audit %v: exit %d, stderr %q", args[2:], code, errOut)
		}
	}
	// No secrets found is reported as that, not as proof of absence.
	clean := t.TempDir()
	ledger.Open(filepath.Join(clean, "ledger")).Append(ledger.Record{V: 1, ID: "x", Time: time.Now(), Method: "POST", Path: "/v1/messages", Class: "main", Backend: "upstream", Outcome: ledger.OutcomeOK})
	out, _, _ = runAuditCmd(t, "-dir", clean)
	wantLines(t, out, "None were found in anything bound for Anthropic.", "it is not proof there were none")
}

// A request where the local-only policy withheld something and the guard,
// replacing secrets, found none: the audit says so in words, not as
// "0 found () in" (#188).
func TestAuditOfARequestWithNoSecretsSaysSo(t *testing.T) {
	dir := t.TempDir()
	zero := 0
	rec := ledger.Record{V: 1, ID: "w1-withheld", Time: time.Date(2026, 10, 14, 12, 30, 40, 0, time.UTC),
		Method: "POST", Path: "/v1/messages", Class: "main", Backend: "upstream", Reason: "rule main", Outcome: ledger.OutcomeOK,
		Guard: &ledger.Guard{LocalOnly: []string{`Bash read vault/db.yaml, which "vault/**" makes local-only`}, Withheld: 1, Redacted: &zero}}
	if err := ledger.Open(filepath.Join(dir, "ledger")).Append(rec); err != nil {
		t.Fatal(err)
	}
	out, _, code := runAuditCmd(t, "-dir", dir, "w1-withheld")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	wantLines(t, out, "Local-only 1 piece withheld before sending", "Guard no secrets found")
	if strings.Contains(out, "0 found") || strings.Contains(out, "()") {
		t.Errorf("the audit printed an empty finding:\n%s", out)
	}
}
