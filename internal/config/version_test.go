package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withRename stands in for the first real migration: version 2 of the
// config renames listenAddr to listen.
func withRename(t *testing.T) {
	t.Helper()
	old := migrations
	migrations = []migration{func(raw map[string]any) []string {
		v, ok := raw["listenAddr"]
		if !ok {
			return nil
		}
		delete(raw, "listenAddr")
		raw["listen"] = v
		return []string{"listenAddr is now called listen"}
	}}
	t.Cleanup(func() { migrations = old })
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "janus.config.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestAnOldConfigIsUpgradedInMemoryAndTheFileLeftAlone(t *testing.T) {
	withRename(t)
	const old = `{"listenAddr": "127.0.0.1:9999"}`
	path := writeConfig(t, old)

	cfg, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != "127.0.0.1:9999" || cfg.Version != 2 {
		t.Errorf("listen %q, version %d; want the renamed setting read, at version 2", cfg.Listen, cfg.Version)
	}
	if len(cfg.Upgraded) != 1 || cfg.Upgraded[0] != "listenAddr is now called listen" {
		t.Errorf("upgrade notes = %q", cfg.Upgraded)
	}
	if b, _ := os.ReadFile(path); string(b) != old {
		t.Errorf("reading the config rewrote it:\n%s", b)
	}
}

func TestRewriteUpgradesTheFileAndKeepsACopy(t *testing.T) {
	withRename(t)
	const old = `{"listenAddr": "127.0.0.1:9999", "logging": {"capture": false}}`
	path := writeConfig(t, old)

	notes, err := Rewrite(path)
	if err != nil || len(notes) != 1 {
		t.Fatalf("Rewrite = %q, %v", notes, err)
	}
	if b, _ := os.ReadFile(path + ".bak"); string(b) != old {
		t.Errorf("the copy is %q, want the old file", b)
	}
	var raw map[string]any
	b, _ := os.ReadFile(path)
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	// Only what the migration touched changes: settings left out stay
	// left out, so they keep following the defaults.
	if raw["listen"] != "127.0.0.1:9999" || raw["version"] != 2.0 || raw["upstream"] != nil {
		t.Errorf("rewritten config = %s", b)
	}
	if cfg, err := Read(path); err != nil || len(cfg.Upgraded) != 0 || cfg.Logging.Capture {
		t.Errorf("the rewritten config reads as %+v, %v; want it current, with its settings kept", cfg.Upgraded, err)
	}
}

func TestRewriteOfACurrentConfigChangesNothing(t *testing.T) {
	path := writeConfig(t, fmt.Sprintf(`{"version": %d, "listen": "127.0.0.1:9999"}`, Version()))
	notes, err := Rewrite(path)
	if err != nil || notes != nil {
		t.Errorf("Rewrite = %q, %v", notes, err)
	}
	if _, err := os.Stat(path + ".bak"); err == nil {
		t.Error("a copy was made of a config that did not change")
	}
}

func TestAConfigFromANewerJanusIsExplained(t *testing.T) {
	_, err := Read(writeConfig(t, `{"version": 99, "listen": "127.0.0.1:9999"}`))
	if err == nil || !strings.Contains(err.Error(), "written for a newer janus") || !strings.Contains(err.Error(), "janus update") {
		t.Errorf("err = %v", err)
	}
	// A newer janus adds settings without changing the version, so an
	// unknown setting says what it may be.
	_, err = Read(writeConfig(t, `{"listen": "127.0.0.1:9999", "someNewSetting": true}`))
	if err == nil || !strings.Contains(err.Error(), "a setting from a newer janus") {
		t.Errorf("err = %v", err)
	}
}

func TestTheStarterFileSaysItsVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "janus.config.json")
	if _, _, err := Load(path); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), fmt.Sprintf(`"version": %d`, Version())) {
		t.Errorf("starter file has no version:\n%s", b)
	}
}

func TestIfDownIsReadAsAListStartingWithClaude(t *testing.T) {
	cfg, err := Read(writeConfig(t, `{
		"local": {"baseUrl": "http://studio.local:1234", "models": {"janus-local": {"model": "q"}}},
		"routing": [
			{"name": "main", "class": "main", "route": "upstream", "ifDown": "janus-local"},
			{"route": "shadow", "local": "janus-local", "ifDown": "janus-local"},
			{"route": "upstream"}
		]}`))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"upstream", "janus-local"}
	for i, rule := range cfg.Routing[:2] {
		if strings.Join(rule.Fallback, ",") != strings.Join(want, ",") {
			t.Errorf("routing[%d].fallback = %q, want %q", i, rule.Fallback, want)
		}
	}
	if cfg.Routing[2].Fallback != nil {
		t.Errorf("a rule with no ifDown was given a list: %q", cfg.Routing[2].Fallback)
	}
	if len(cfg.Upgraded) != 2 || !strings.Contains(cfg.Upgraded[0], `"ifDown": "janus-local" is now "fallback": ["upstream", "janus-local"]`) {
		t.Errorf("notes = %q", cfg.Upgraded)
	}
}

// In the current version ifDown is gone, and naming it is the error any
// unknown setting is.
func TestIfDownInACurrentConfigIsRefused(t *testing.T) {
	_, err := Read(writeConfig(t, fmt.Sprintf(`{"version": %d, "routing": [{"route": "upstream", "ifDown": "janus-local"}]}`, Version())))
	if err == nil || !strings.Contains(err.Error(), "ifDown") {
		t.Errorf("err = %v", err)
	}
}
