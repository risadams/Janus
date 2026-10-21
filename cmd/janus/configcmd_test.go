package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/risadams/janus/internal/config"
)

func TestConfigUpgradeOfACurrentConfigChangesNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "janus.config.json")
	os.WriteFile(path, []byte(`{"listen": "127.0.0.1:9999"}`), 0o600)
	out, errOut, code := runCLI(t, "config", "upgrade", "-config", path)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	wantLines(t, out, fmt.Sprintf("is already in this version's form (config version %d); nothing to change.", config.Version()))
	if b, _ := os.ReadFile(path); string(b) != `{"listen": "127.0.0.1:9999"}` {
		t.Errorf("the file changed: %s", b)
	}
}

func TestConfigUpgradeRefusesAConfigFromANewerJanus(t *testing.T) {
	path := filepath.Join(t.TempDir(), "janus.config.json")
	os.WriteFile(path, []byte(`{"version": 99}`), 0o600)
	_, errOut, code := runCLI(t, "config", "upgrade", "-config", path)
	if code != 1 || !strings.Contains(errOut, "written for a newer janus") {
		t.Errorf("exit %d, stderr %q", code, errOut)
	}
	if _, _, code := runCLI(t, "config"); code != 2 {
		t.Errorf("janus config with no verb: exit %d, want 2", code)
	}
}

func TestUpgradedNoteSaysWhatChangedAndHowToKeepIt(t *testing.T) {
	note := upgradedNote("j.json", []string{"listenAddr is now called listen", "x moved to y"})
	for _, want := range []string{"j.json was written for an older janus", "  - listenAddr is now called listen\n  - x moved to y", "janus config upgrade -config j.json"} {
		if !strings.Contains(note, want) {
			t.Errorf("note is missing %q:\n%s", want, note)
		}
	}
}

func TestStatsSaysWhenItSkippedRecordsFromANewerJanus(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "2026-10-16.jsonl"), []byte(
		`{"v":1,"id":"now","time":"2026-10-16T09:00:00Z","method":"POST","path":"/v1/messages","class":"main","backend":"upstream","status":200,"outcome":"ok","usage":{"input":10,"output":100}}`+"\n"+
			`{"v":2,"id":"later","time":"2026-10-16T09:01:00Z","usage":{"input":"ten"}}`+"\n"), 0o600)
	out, _, code := runCLI(t, "stats", "-config", noConfig(t), "-dir", dir, "-since", "2026-10-01")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	wantLines(t, out, "1 records were written by a newer janus", "janus update")
	out, _, _ = runCLI(t, "stats", "-config", noConfig(t), "-dir", dir, "-since", "2026-10-01", "-json")
	if !strings.Contains(out, `"newer_records": 1`) {
		t.Errorf("-json has no newer_records:\n%s", out)
	}
}
