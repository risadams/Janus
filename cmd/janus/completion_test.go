package main

import (
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func completionScript(t *testing.T, shell string) string {
	t.Helper()
	stdout, stderr, code := runCLI(t, "completion", shell)
	if code != 0 {
		t.Fatalf("janus completion %s exited %d: %s", shell, code, stderr)
	}
	return stdout
}

// Every command, verb, and flag is in every shell's script, and every
// command and verb has its flags gathered: a command that made its flag
// set some other way, or only after doing work, would be missing here.
func TestCompletionsOfferEveryCommandVerbAndFlag(t *testing.T) {
	sets := gatherFlagSets()
	want := map[string]bool{}
	addFlags := func(name string) {
		fs := sets[name]
		if fs == nil {
			t.Errorf("%q made no flag set when run with -h", name)
			return
		}
		fs.VisitAll(func(f *flag.Flag) { want["-"+f.Name] = true })
	}
	addFlags("janus")
	for _, c := range commands() {
		want[c.name] = true
		for _, v := range c.verbs {
			want[v.name] = true
			if c.name != "completion" {
				addFlags("janus " + c.name + " " + v.name)
			}
		}
		if len(c.verbs) == 0 {
			addFlags("janus " + c.name)
		}
	}
	for _, shell := range completionShells {
		script := completionScript(t, shell.name)
		for w := range want {
			if shell.name == "fish" && strings.HasPrefix(w, "-") {
				w = "-o " + w[1:] // fish's way of naming a flag with one dash
			}
			if !strings.Contains(script, w) {
				t.Errorf("the %s completion leaves out %s", shell.name, w)
			}
		}
	}
}

// newFlagSet is how janus completion sees a command's flags, so a flag set
// made with flag.NewFlagSet directly would go uncompleted.
func TestEveryFlagSetIsMadeWithNewFlagSet(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		n := strings.Count(string(data), "flag.NewFlagSet(")
		if f == "completion.go" {
			n-- // newFlagSet itself
		}
		if n > 0 {
			t.Errorf("%s makes a flag set with flag.NewFlagSet; use newFlagSet, so janus completion sees it", f)
		}
	}
}

// The scripts skip a flag's value by the flag's name alone, whatever the
// command, which is right only while no flag takes a value in one command
// and is a switch in another.
func TestNoFlagTakesAValueInOneCommandAndNotAnother(t *testing.T) {
	takes := map[string]bool{}
	for name, fs := range gatherFlagSets() {
		fs.VisitAll(func(f *flag.Flag) {
			v := !isBoolFlag(f)
			if seen, ok := takes[f.Name]; ok && seen != v {
				t.Errorf("-%s takes a value in some commands and not in %q", f.Name, name)
			}
			takes[f.Name] = v
		})
	}
}

func TestCompletionNamesTheShellsItKnows(t *testing.T) {
	_, stderr, code := runCLI(t, "completion", "tcsh")
	if code != 2 || !strings.Contains(stderr, "bash|fish|powershell|zsh") {
		t.Errorf("janus completion tcsh: exit %d, %q; want 2 and the shells listed", code, stderr)
	}
}

// completionCases are command lines, the cursor at their end, and what
// each shell should offer there. nil means nothing, which leaves the shell
// to complete a file name.
var completionCases = []struct {
	line string
	want []string
}{
	{"janus st", []string{"stats"}},
	{"janus ", []string{"version", "stats", "doctor", "route", "replay", "audit", "mcp", "claude", "local", "service", "update", "config", "completion"}},
	{"janus -", []string{"-config", "-listen", "-local", "-upstream"}},
	{"janus stats -j", []string{"-json"}},
	{"janus -config x.json st", []string{"stats"}},
	{"janus stats -config ", nil},
	{"janus stats -config=x.json -w", []string{"-watch"}},
	{"janus service ", []string{"install", "uninstall", "status"}},
	{"janus service install -", []string{"-config"}},
	{"janus config u", []string{"upgrade"}},
	{"janus completion ", []string{"bash", "fish", "powershell", "zsh"}},
	{"janus replay ", nil},
	{"janus claude -- -", nil},
}

func writeScript(t *testing.T, shell string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "janus."+shell)
	if err := os.WriteFile(path, []byte(completionScript(t, shell)), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func checkOffered(t *testing.T, shell, line, out string, want []string) {
	t.Helper()
	got := strings.Fields(out)
	if len(got) == 0 {
		got = nil
	}
	if !slices.Equal(got, want) {
		t.Errorf("%s at %q offers %q, want %q", shell, line, got, want)
	}
}

func lookShell(t *testing.T, name string) string {
	t.Helper()
	if runtime.GOOS == "windows" && name != "pwsh" {
		t.Skipf("%s on Windows is not where its completion runs", name)
	}
	path, err := exec.LookPath(name)
	if err != nil {
		t.Skipf("%s is not installed", name)
	}
	return path
}

func TestBashCompletes(t *testing.T) {
	bash := lookShell(t, "bash")
	script := writeScript(t, "bash")
	for _, c := range completionCases {
		words := strings.Split(c.line, " ")
		var quoted []string
		for _, w := range words {
			quoted = append(quoted, shQuote(w, false))
		}
		prog := "source " + shQuote(script, false) + "\n" +
			"COMP_WORDS=(" + strings.Join(quoted, " ") + ")\n" +
			"COMP_CWORD=" + strconv.Itoa(len(words)-1) + "\n" +
			"_janus\nprintf '%s\\n' \"${COMPREPLY[@]}\"\n"
		out, err := exec.Command(bash, "--norc", "--noprofile", "-c", prog).CombinedOutput()
		if err != nil {
			t.Fatalf("bash: %v\n%s", err, out)
		}
		checkOffered(t, "bash", c.line, string(out), c.want)
	}
}

// zsh's completion system needs a terminal, so this stands in for its
// _describe and _files, which is all the script calls, and checks what the
// script hands them.
func TestZshCompletes(t *testing.T) {
	zsh := lookShell(t, "zsh")
	script := writeScript(t, "zsh")
	for _, c := range completionCases {
		words := strings.Split(c.line, " ")
		var quoted []string
		for _, w := range words {
			quoted = append(quoted, shQuote(w, false))
		}
		prog := "compdef() { :; }\n" +
			"_files() { :; }\n" +
			"_describe() { local x; for x in ${(P)2}; do [[ ${x%%:*} == ${words[CURRENT]}* ]] && print -r -- ${x%%:*}; done; :; }\n" +
			"source " + shQuote(script, false) + "\n" +
			"words=(" + strings.Join(quoted, " ") + ")\n" +
			"CURRENT=" + strconv.Itoa(len(words)) + "\n" +
			"_janus\n"
		out, err := exec.Command(zsh, "-f", "-c", prog).CombinedOutput()
		if err != nil {
			t.Fatalf("zsh: %v\n%s", err, out)
		}
		checkOffered(t, "zsh", c.line, string(out), c.want)
	}
}

func TestFishCompletes(t *testing.T) {
	fish := lookShell(t, "fish")
	script := writeScript(t, "fish")
	for _, c := range completionCases {
		// fish offers files where the script offers nothing, so those
		// cases are checked only for offering no word of janus's.
		prog := "source " + shQuote(script, true) + "\n" +
			"complete -C " + shQuote(c.line, true) + " | string replace -r '\\t.*' ''\n"
		out, err := exec.Command(fish, "--no-config", "-c", prog).CombinedOutput()
		if err != nil {
			t.Fatalf("fish: %v\n%s", err, out)
		}
		if c.want == nil {
			for _, w := range strings.Fields(string(out)) {
				if strings.HasPrefix(w, "-") {
					t.Errorf("fish at %q offers %q, want only file names", c.line, w)
				}
			}
			continue
		}
		checkOffered(t, "fish", c.line, string(out), c.want)
	}
}

// pwsh takes seconds to start, so every case runs in one of it, each
// case's answers ending in a line of their own.
func TestPowerShellCompletes(t *testing.T) {
	pwsh := lookShell(t, "pwsh")
	// Dot-sourcing a file that does not end in .ps1 opens it in whatever
	// application claims it, rather than running it.
	script := filepath.Join(t.TempDir(), "janus.ps1")
	if err := os.WriteFile(script, []byte(completionScript(t, "powershell")), 0o600); err != nil {
		t.Fatal(err)
	}
	var lines []string
	prog := ". " + psQuote(script) + "\n"
	for _, c := range completionCases {
		if strings.Contains(c.line, " -- ") {
			continue // PowerShell keeps -- to itself
		}
		lines = append(lines, c.line)
		prog += "$line = " + psQuote(c.line) + "\n" +
			"(TabExpansion2 -inputScript $line -cursorColumn $line.Length).CompletionMatches |" +
			" Where-Object ResultType -eq 'ParameterValue' | ForEach-Object CompletionText\n" +
			"'--end--'\n"
	}
	out, err := exec.Command(pwsh, "-NoProfile", "-NonInteractive", "-Command", prog).CombinedOutput()
	if err != nil {
		t.Fatalf("pwsh: %v\n%s", err, out)
	}
	answers := strings.Split(strings.ReplaceAll(string(out), "\r", ""), "--end--\n")
	if len(answers) != len(lines)+1 {
		t.Fatalf("pwsh answered %d cases of %d:\n%s", len(answers)-1, len(lines), out)
	}
	for i, line := range lines {
		for _, c := range completionCases {
			if c.line == line {
				checkOffered(t, "PowerShell", line, answers[i], c.want)
			}
		}
	}
}
