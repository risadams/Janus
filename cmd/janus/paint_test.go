package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/risadams/janus/internal/anthropic"
	"github.com/risadams/janus/internal/ledger"
)

var escapes = regexp.MustCompile("\x1b\\[[0-9;]*m")

func uncolored(s string) string { return escapes.ReplaceAllString(s, "") }

// runPiped runs a command with its stdout on a pipe rather than a buffer,
// so that it decides on color as it would for a real file.
func runPiped(t *testing.T, args ...string) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		got <- string(b)
	}()
	var stderr bytes.Buffer
	run(args, w, &stderr)
	w.Close()
	return <-got
}

func TestPainterOffLeavesTextAlone(t *testing.T) {
	if got := painter(false).paint(red, "fail"); got != "fail" {
		t.Errorf("paint with color off = %q", got)
	}
	if got := painter(true).paint(red, "fail"); got != "\x1b[31mfail\x1b[0m" {
		t.Errorf("paint with color on = %q", got)
	}
}

func TestColorIsOnlyForFilesThatWantIt(t *testing.T) {
	if newPainter(&bytes.Buffer{}) {
		t.Error("a buffer was given color")
	}
	r, w, _ := os.Pipe()
	defer r.Close()
	defer w.Close()
	if newPainter(w) {
		t.Error("a pipe was given color")
	}
	t.Setenv("FORCE_COLOR", "1")
	if !newPainter(w) {
		t.Error("FORCE_COLOR did not turn color on")
	}
	t.Setenv("NO_COLOR", "1")
	if newPainter(w) {
		t.Error("NO_COLOR did not win over FORCE_COLOR")
	}
}

func TestDoctorColorsEachResult(t *testing.T) {
	t.Setenv("FORCE_COLOR", "1")
	up := serverSaying(200, "Anthropic API")
	defer up.Close()
	dir := t.TempDir()
	// Marshalled, not spliced, so that a Windows path's backslashes are
	// escaped in the JSON.
	data, _ := json.Marshal(map[string]any{
		"listen":   freeAddr(t),
		"upstream": map[string]any{"baseUrl": up.URL},
		"local":    map[string]any{"baseUrl": "http://127.0.0.1:1"},
		"logging":  map[string]any{"dir": filepath.Join(dir, "logs")},
	})
	path := filepath.Join(dir, "janus.config.json")
	os.WriteFile(path, data, 0o600)
	t.Setenv("CLAUDE_CONFIG_DIR", dir)

	out := runPiped(t, "doctor", "-config", path)
	// The level is padded inside the color, so the names after it start in
	// the same column whether or not color is on.
	for _, want := range []string{"\x1b[32mok  \x1b[0m  config", "\x1b[33mwarn\x1b[0m  local"} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor output is missing %q:\n%q", want, out)
		}
	}
	for _, line := range strings.Split(uncolored(out), "\n") {
		if strings.HasPrefix(line, "  ok    ") || strings.HasPrefix(line, "  warn  ") || strings.HasPrefix(line, "  fail  ") {
			continue
		}
		if strings.HasPrefix(line, "  ok") || strings.HasPrefix(line, "  warn") {
			t.Errorf("a result is out of line once its color is taken away: %q", line)
		}
	}
}

func TestStatsJSONIsNeverColored(t *testing.T) {
	t.Setenv("FORCE_COLOR", "1")
	out := runPiped(t, "stats", "-json", "-dir", seedLedger(t))
	if strings.Contains(out, "\x1b[") {
		t.Errorf("-json output has color codes in it:\n%q", out)
	}
}

// Taking the color away from colored output must leave exactly the plain
// output: nothing is padded differently, and nothing shifts a column.
func TestStatsLinesUpWithColorOnOrOff(t *testing.T) {
	now := time.Date(2026, 10, 15, 14, 30, 0, 0, time.Local)
	quota := &anthropic.Quota{Windows: map[string]anthropic.QuotaWindow{
		"5h":      {Utilization: 0.93, Resets: now.Add(2 * time.Hour)},
		"7d":      {Utilization: 0.80, Resets: now.Add(72 * time.Hour)},
		"7d_opus": {Utilization: 0.10},
	}}
	rep := ledger.Report{
		Total:    ledger.Row{Requests: 12},
		Outcomes: map[string]int{ledger.OutcomeOK: 10, ledger.OutcomeAPIError: 2},
		Quota:    &ledger.QuotaSummary{At: now, Latest: quota},
	}
	rep.LocalSaving.Requests = 3

	for name, draw := range map[string]func(io.Writer, painter){
		"today":  func(w io.Writer, p painter) { printToday(w, p, rep, now, false, time.Second) },
		"limits": func(w io.Writer, p painter) { printQuota(w, p, rep.Quota, now) },
	} {
		var plain, colored bytes.Buffer
		draw(&plain, false)
		draw(&colored, true)
		if !strings.Contains(colored.String(), "\x1b[31m") || !strings.Contains(colored.String(), "\x1b[33m") {
			t.Errorf("%s: a limit at 93%% is not red, or one at 80%% not yellow:\n%q", name, colored.String())
		}
		if got := uncolored(colored.String()); got != plain.String() {
			t.Errorf("%s: without its color the output differs from the plain one.\ncolored: %q\nplain:   %q", name, got, plain.String())
		}
	}
}
