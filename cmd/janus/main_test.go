package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/risadams/janus/internal/config"
)

func TestVersionLine(t *testing.T) {
	if got, want := versionLine(), "janus dev"; got != want {
		t.Errorf("versionLine() = %q, want %q", got, want)
	}
}

func TestRunVersion(t *testing.T) {
	for _, arg := range []string{"version", "-version", "--version"} {
		var out, errOut bytes.Buffer
		if code := run([]string{arg}, &out, &errOut); code != 0 || out.String() != "janus dev\n" {
			t.Errorf("run(%q) = %d, stdout %q, stderr %q", arg, code, out.String(), errOut.String())
		}
	}
}

func TestRunRejectsUnknownCommand(t *testing.T) {
	var out, errOut bytes.Buffer
	cfgPath := filepath.Join(t.TempDir(), "c.json")
	if code := run([]string{"-config", cfgPath, "bogus"}, &out, &errOut); code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	if !strings.Contains(errOut.String(), `unknown command "bogus"`) {
		t.Errorf("stderr = %q", errOut.String())
	}
	if _, err := os.Stat(cfgPath); err == nil {
		t.Error("a starter config was written for a command that was rejected")
	}
}

func TestRunReportsInvalidConfig(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "c.json")
	os.WriteFile(cfgPath, []byte(`{"listen":"0.0.0.0:8787","features":{"route":true}}`), 0o600)

	var out, errOut bytes.Buffer
	if code := run([]string{"-config", cfgPath}, &out, &errOut); code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	for _, want := range []string{cfgPath, "allowRemote", "features.route"} {
		if !strings.Contains(errOut.String(), want) {
			t.Errorf("stderr = %q, missing %q", errOut.String(), want)
		}
	}
}

func TestBannerGivesTheClaudeCodeCommand(t *testing.T) {
	var out bytes.Buffer
	printBanner(&out, config.Default(), "127.0.0.1:8787")
	for _, want := range []string{
		"ANTHROPIC_BASE_URL=http://127.0.0.1:8787 ENABLE_TOOL_SEARCH=true claude",
		`$env:ANTHROPIC_BASE_URL="http://127.0.0.1:8787"`,
		"https://api.anthropic.com",
		"http://studio.tuesday.local:1234",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("banner is missing %q:\n%s", want, out.String())
		}
	}
}
