package guard

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/risadams/janus/internal/anthropic"
)

// request builds a Messages request around the given pieces, encoding them
// properly so that newlines and quotes in a secret survive.
func request(t *testing.T, system string, messages ...map[string]any) *anthropic.Request {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"model":    "claude-opus-5-5",
		"system":   []map[string]any{{"type": "text", "text": system}},
		"messages": messages,
	})
	if err != nil {
		t.Fatal(err)
	}
	req, err := anthropic.ParseRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func user(blocks ...map[string]any) map[string]any {
	return map[string]any{"role": "user", "content": blocks}
}
func assistant(blocks ...map[string]any) map[string]any {
	return map[string]any{"role": "assistant", "content": blocks}
}
func textBlock(s string) map[string]any { return map[string]any{"type": "text", "text": s} }

func TestScanRequestLooksEverywhereAUsersSecretCanBe(t *testing.T) {
	ps := positives()
	awsID, pem, dbURL, github := ps[3], ps[13], ps[14], ps[7]

	req := request(t, "You are an agent. "+awsID.text,
		user(textBlock("please deploy this")),
		assistant(
			map[string]any{"type": "thinking", "thinking": "", "signature": rnd[:88]},
			map[string]any{"type": "tool_use", "id": "t1", "name": "Bash", "input": map[string]any{"command": "git clone https://" + github.want + "@github.com/acme/app.git"}},
		),
		user(map[string]any{"type": "tool_result", "tool_use_id": "t1", "content": []map[string]any{textBlock("cat .env\n" + dbURL.text + "\n" + pem.text)}}),
		user(map[string]any{"type": "tool_result", "tool_use_id": "t2", "content": "plain string result, nothing here"}),
	)

	// With the entropy search on, to show the signature is still skipped.
	report := (&Detector{Entropy: true}).ScanRequest(req)

	if report.Findings != 4 {
		t.Errorf("findings = %d, want 4: %+v", report.Findings, report)
	}
	for _, kind := range []string{"aws_access_key", "github_token", "url_password", "private_key"} {
		if report.Kinds[kind] != 1 {
			t.Errorf("kinds = %v, missing %s", report.Kinds, kind)
		}
	}
	if report.Kinds["high_entropy"] != 0 {
		t.Errorf("a thinking signature was reported as a secret: %v", report.Kinds)
	}
	want := []string{"system[0]", "messages[1] assistant (tool_use)", "messages[2] user (tool_result)"}
	if strings.Join(report.Where, " | ") != strings.Join(want, " | ") {
		t.Errorf("where = %q, want %q", report.Where, want)
	}
}

// The report is going into logs. It must say what was found without
// repeating it.
func TestReportHoldsNoSecrets(t *testing.T) {
	ps := positives()
	var blocks []map[string]any
	for _, p := range ps {
		blocks = append(blocks, textBlock(p.text))
	}
	report := (&Detector{}).ScanRequest(request(t, "sys", user(blocks...)))
	if report.Findings != len(ps) {
		t.Fatalf("findings = %d, want %d", report.Findings, len(ps))
	}
	logged, _ := json.Marshal(report)
	all := string(logged) + report.Summary()
	for _, p := range ps {
		if strings.Contains(all, p.want) || (len(p.want) > 12 && strings.Contains(all, p.want[:12])) {
			t.Errorf("the report contains part of a %s", p.kind)
		}
	}
}

func TestSecretsInToolArgumentsAreFoundByTheirKey(t *testing.T) {
	generic := positives()[16].want
	req := request(t, "sys", assistant(map[string]any{"type": "tool_use", "id": "t1", "name": "Deploy",
		"input": map[string]any{"env": map[string]any{"password": generic, "region": "us-east-1"}, "args": []string{"--verbose"}}}))

	report := (&Detector{}).ScanRequest(req)
	if report.Findings != 1 || report.Kinds["assigned_secret"] != 1 {
		t.Errorf("report = %+v; a value under a key called password should be found", report)
	}
}

func TestCleanRequest(t *testing.T) {
	raw := `{"model":"m","system":"You are an agent.","messages":[{"role":"user","content":"count the lines in a.txt"},{"role":"assistant","content":[{"type":"tool_use","id":"t","name":"Bash","input":{"command":"wc -l a.txt"}}]}]}`
	req, _ := anthropic.ParseRequest([]byte(raw))
	report := (&Detector{}).ScanRequest(req)
	if report.Findings != 0 || len(report.Where) != 0 || report.Summary() != "" {
		t.Errorf("report = %+v", report)
	}
	if req.Modified() {
		t.Error("scanning changed the request")
	}
}

func TestSummary(t *testing.T) {
	r := Report{Kinds: map[string]int{"assigned_secret": 1, "aws_access_key": 3, "jwt": 1}}
	if got := r.Summary(); got != "aws_access_key x3, assigned_secret, jwt" {
		t.Errorf("Summary() = %q", got)
	}
}
