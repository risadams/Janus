package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// decode reads a -json report as a script would, by field name, so these
// tests hold the names docs/GUARD.md and docs/TROUBLESHOOTING.md give.
func decode(t *testing.T, out string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	return m
}

// field follows a dotted path through decoded JSON: "secrets.exposed.0.id".
func field(t *testing.T, m any, path string) any {
	t.Helper()
	for _, key := range strings.Split(path, ".") {
		switch v := m.(type) {
		case map[string]any:
			m = v[key]
		case []any:
			i := int(key[0] - '0')
			if i >= len(v) {
				t.Fatalf("%s: index %d past the end of %v", path, i, v)
			}
			m = v[i]
		default:
			t.Fatalf("%s: %q is not in %v", path, key, m)
		}
	}
	return m
}

func wantFields(t *testing.T, m map[string]any, want map[string]any) {
	t.Helper()
	for path, w := range want {
		if got := field(t, m, path); got != w {
			t.Errorf("%s = %v, want %v", path, got, w)
		}
	}
}

func TestAuditJSONHoldsTheSameTallyAsTheReport(t *testing.T) {
	t.Setenv("FORCE_COLOR", "1")
	out, errOut, code := runAuditCmd(t, "-dir", auditFixture(t), "-json", "-verify")
	if code != 1 {
		t.Errorf("exit %d, want 1: a request was sent with a secret in it (%s)", code, errOut)
	}
	if strings.Contains(out, "\x1b[") {
		t.Error("-json output has color codes in it")
	}
	if strings.Contains(out, fakeAWSKey) {
		t.Error("-json output has a secret in it")
	}
	wantFields(t, decode(t, out), map[string]any{
		"requests":                     6.0,
		"anthropic.untouched":          2.0,
		"anthropic.changed":            2.0,
		"anthropic.redacted":           1.0,
		"anthropic.withheld":           0.0,
		"anthropic.repaired":           1.0,
		"local_only":                   1.0,
		"stopped":                      1.0,
		"kept_local":                   0.0,
		"secrets.requests":             2.0,
		"secrets.replaced":             1.0,
		"secrets.kinds.aws_access_key": 2.0,
		"secrets.exposed.0.id":         "r3-exposed",
		"secrets.exposed.0.kinds.0":    "aws_access_key",
		"secrets.exposed.0.where.0":    "messages[0] user (text)",
		"blocked.0.id":                 "r5-stopped",
		"blocked.0.why":                "the request could not be read to check it for secrets",
		"verify.checked":               4.0,
		"verify.clean":                 2.0,
		"verify.unchecked":             1.0,
		"verify.leaks.0.id":            "r3-exposed",
		"from":                         "2026-10-11T18:00:00Z",
	})
}

func TestAuditJSONLeavesOutWhatWasNotAskedFor(t *testing.T) {
	out, _, code := runAuditCmd(t, "-dir", auditFixture(t), "-json")
	if code != 0 {
		t.Errorf("exit %d, want 0 without -verify", code)
	}
	m := decode(t, out)
	if _, ok := m["verify"]; ok {
		t.Error("verify is in the report without -verify")
	}

	// An empty range still has every list, empty, and no dates.
	out, _, _ = runAuditCmd(t, "-dir", t.TempDir(), "-json")
	m = decode(t, out)
	if _, ok := m["from"]; ok {
		t.Error("from is in a report of no requests")
	}
	for _, list := range []string{"blocked", "secrets.exposed"} {
		if v, ok := field(t, m, list).([]any); !ok || len(v) != 0 {
			t.Errorf("%s = %v, want an empty list", list, field(t, m, list))
		}
	}
}

func TestAuditJSONOfOneRequestIsItsLedgerRecord(t *testing.T) {
	out, _, code := runAuditCmd(t, "-dir", auditFixture(t), "-json", "r2-redacted")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	wantFields(t, decode(t, out), map[string]any{"id": "r2-redacted", "backend": "upstream", "guard.redacted": 1.0})

	if _, _, code := runAuditCmd(t, "-dir", auditFixture(t), "-json", "-body", "r2-redacted"); code != 2 {
		t.Errorf("-json with -body: exit %d, want 2", code)
	}
}

func TestDoctorJSONListsEveryCheck(t *testing.T) {
	t.Setenv("FORCE_COLOR", "1")
	up := serverSaying(200, "Anthropic API")
	defer up.Close()
	lms := serverSaying(200, lmStudioModels)
	defer lms.Close()
	dir := t.TempDir()
	data, _ := json.Marshal(map[string]any{
		"listen":   freeAddr(t),
		"upstream": map[string]any{"baseUrl": up.URL},
		"local":    map[string]any{"baseUrl": lms.URL},
		"logging":  map[string]any{"dir": filepath.Join(dir, "logs")},
	})
	path := filepath.Join(dir, "janus.config.json")
	os.WriteFile(path, data, 0o600)
	t.Setenv("CLAUDE_CONFIG_DIR", dir)

	var out, errOut bytes.Buffer
	code := run([]string{"doctor", "-json", "-config", path}, &out, &errOut)
	if code != 0 {
		t.Errorf("exit %d: %s", code, errOut.String())
	}
	if strings.Contains(out.String(), "\x1b[") {
		t.Error("-json output has color codes in it")
	}
	m := decode(t, out.String())
	wantFields(t, m, map[string]any{"ok": true, "version": "dev", "checks.0.name": "config", "checks.0.level": "ok"})
	names := map[string]bool{}
	for _, c := range field(t, m, "checks").([]any) {
		names[c.(map[string]any)["name"].(string)] = true
		if c.(map[string]any)["detail"] == nil {
			t.Errorf("check %v has no detail", c)
		}
	}
	for _, want := range []string{"config", "listen", "upstream", "logs", "update"} {
		if !names[want] {
			t.Errorf("no %s check in %v", want, names)
		}
	}
}

func TestDoctorJSONSaysWhenSomethingFailed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "janus.config.json")
	os.WriteFile(path, []byte("{not json"), 0o600)
	var out, errOut bytes.Buffer
	if code := run([]string{"doctor", "-json", "-config", path}, &out, &errOut); code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
	wantFields(t, decode(t, out.String()), map[string]any{"ok": false, "checks.0.name": "config", "checks.0.level": "fail"})
}
