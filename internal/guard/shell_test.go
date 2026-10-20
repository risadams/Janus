package guard

import (
	"strings"
	"testing"
)

// realPolicy is the policy in daily use when #353 was found.
func realPolicy(t *testing.T) *Policy {
	return policy(t, []string{".env*", "secrets/**", "*.pem"}, nil)
}

// A pattern named in an expression or in a message's text is not a file
// the command reads, and its output is not withheld (#353). The first two
// are the shape of commands the policy withheld in a week of real use
// (docs/research/results-1.0.md).
func TestNamingAPatternInAnExpressionIsNotReadingIt(t *testing.T) {
	p := realPolicy(t)
	calls := map[string]map[string]any{
		"a jq path in gh --jq": toolUse("t1", "Bash", map[string]any{
			"command": "gh api repos/o/r/environments --jq '.environments[]|.name' 2>&1; echo ---; gh issue view 262 --json title --jq '.title'",
		}),
		"a pattern git grep searches for": toolUse("t1", "Bash", map[string]any{
			"command": `git grep -n '"\*\.pem"' -- internal | head -3`,
		}),
		"a jq filter": toolUse("t1", "Bash", map[string]any{
			"command": "jq -r '.environments[] | .name' deploy.json | sort | uniq -c",
		}),
		"a grep pattern": toolUse("t1", "Bash", map[string]any{
			"command": "grep -rn 'secrets/' internal docs",
		}),
		"grep patterns given with -e": toolUse("t1", "Bash", map[string]any{
			"command": "grep -n -e '.env' -e '*.pem' src/*.go",
		}),
		"an rg pattern": toolUse("t1", "Bash", map[string]any{
			"command": `rg -n --no-heading '\.env' src`,
		}),
		"an issue's title and body": toolUse("t1", "Bash", map[string]any{
			"command": `gh issue create --title "Stop reading .env" --body 'The guard withholds secrets/db.yaml'`,
		}),
		"the Grep tool's pattern": toolUse("t1", "Grep", map[string]any{
			"pattern": `\.env|secrets/`, "path": "internal",
		}),
	}
	for name, call := range calls {
		req := request(t, "sys", assistant(call), user(toolResult("t1", "done")))
		if m := p.Check(req); m.Protected() {
			t.Errorf("%s: withheld, for %v", name, m.Reasons)
		}
	}
}

// The reads closest to what TestNamingAPatternInAnExpressionIsNotReadingIt
// lets through: a protected file given to the same programs, an expression
// that reads one or whose output becomes a path, and commands the reading
// cannot follow.
func TestReadsNextToAnExpressionAreStillCaught(t *testing.T) {
	p := realPolicy(t)
	for name, command := range map[string]string{
		"jq given the file":                   "jq . .env",
		"jq given the file after the filter":  "jq '.password' secrets/db.yaml",
		"jq reading its program from a file":  "jq -f prog.jq .env.json",
		"jq's program file":                   "jq --from-file .env x.json",
		"jq importing a module":               `jq -n 'import "secrets/db" as $d; $d'`,
		"jq given an unquoted glob":           "jq .env*",
		"jq's output piped to xargs":          `jq -rn '"secrets/db.yaml"' | xargs cat`,
		"jq's output read in a loop":          `jq -rn '"secrets/db.yaml"' | while read f; do cat "$f"; done`,
		"jq's output as tar's file list":      `jq -rn '"secrets/db.yaml"' | tar -cf - -T -`,
		"jq's output written to a file":       `jq -rn '"secrets/db.yaml"' > list; tar -cf - -T list`,
		"jq's output substituted":             `cat $(jq -rn '"secrets/db.yaml"')`,
		"jq's output run by a shell":          `jq -rn '"cat .env"' | sh`,
		"jq's output run by sed":              `jq -rn '"cat .env"' | sed e`,
		"grep with an option after the file":  "grep .env -e password",
		"grep given the file":                 "grep -e password .env",
		"grep's patterns from the file":       "grep -f .env x",
		"grep told which files to read":       "grep --include '*.pem' -rh BEGIN .",
		"grep with an option it may not know": "grep --label x '*.pem' -rh BEGIN .",
		"grep with POSIXLY_CORRECT":           "POSIXLY_CORRECT=1 grep x -e .env",
		"POSIXLY_CORRECT exported earlier":    "export POSIXLY_CORRECT=1; grep x -e .env",
		"git grep given the path":             "git grep -h BEGIN -- '*.pem'",
		"rg with an option it does not know":  "rg --pre ./show.sh x '*.pem'",
		"a gh extension":                      "gh secretly --jq .env",
		"gh reading the body from a file":     "gh issue create --title x --body-file .env",
		"gh api sending the file":             "gh api gists --jq '.url' --input .env",
		"an escaped path":                     `cat \.env`,
		"a quoted path":                       `cat "secrets"/db.yaml`,
	} {
		req := request(t, "sys", assistant(toolUse("t1", "Bash", map[string]any{"command": command})), user(toolResult("t1", "hunter2")))
		if m := p.Check(req); !m.Protected() {
			t.Errorf("%s: %s was not withheld", name, command)
		}
	}
	for name, input := range map[string]map[string]any{
		"Grep given the file": {"pattern": "x", "path": ".env"},
		"Grep given a glob":   {"pattern": "BEGIN", "glob": "*.pem"},
	} {
		req := request(t, "sys", assistant(toolUse("t1", "Grep", input)), user(toolResult("t1", "hunter2")))
		if m := p.Check(req); !m.Protected() {
			t.Errorf("%s: not withheld", name)
		}
	}
}

func TestParseShell(t *testing.T) {
	for command, want := range map[string]string{
		`a 'b c' "d\"e" f\ g`:      "a|b c|d\"e|f g",
		"a 2>&1 >/dev/null; b | c": "a >&1 >/dev/null ; b pipe ; c",
		"a && b || c & d":          "a ; b ; c ; d",
		"x=1 a <in >>out &>both":   "x=1|a <in >>out >both",
		"a \\\n b":                 "a|b",
		`a "$HOME/x" ~/y '$z'`:     "a|$HOME/x*|~/y*|$z",
		"a *.go":                   "a|*.go*",
		"cat <<EOF":                "!",
		"a $(b)":                   "!",
		"a `b`":                    "!",
		`a "$(b)"`:                 "!",
		"a <(b)":                   "!",
		"(a)":                      "!",
		"a # note":                 "!",
		"a 'b":                     "!",
		"a |":                      "!",
		"a >":                      "!",
	} {
		cmds, ok := parseShell(command)
		got := "!"
		if ok {
			var parts []string
			for _, c := range cmds {
				var words []string
				for _, w := range c.words {
					if w.open {
						w.text += "*"
					}
					words = append(words, w.text)
				}
				part := strings.Join(words, "|")
				for _, r := range c.redirects {
					part += " " + r.op + r.target
				}
				if c.pipe {
					part += " pipe"
				}
				parts = append(parts, part)
			}
			got = strings.Join(parts, " ; ")
		}
		if got != want {
			t.Errorf("parseShell(%q) = %q, want %q", command, got, want)
		}
	}
}

// FuzzDataArguments feeds the shell reader commands a model might write. It
// must not panic, and a command it cannot read comes back unchanged, so
// that every word of it is searched.
func FuzzDataArguments(f *testing.F) {
	for _, seed := range []string{
		"gh api x --jq '.a[]|.b' 2>&1; echo ---",
		`git grep -n '"\*\.pem"' -- internal | head -3`,
		"jq -rn '.x' a.json | xargs cat",
		`grep -e a -f b --include=c -- d`,
		`a "b\"c" 'd' e\ f >g 2>&1 <h`,
		"x=$(y)", "a <<EOF\nb\nEOF", "&>", "2>&", `"`, "|", "\\",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, command string) {
		got := dataArguments(command)
		if _, ok := parseShell(command); !ok && got != command {
			t.Fatalf("dataArguments(%q) = %q, though the command could not be read", command, got)
		}
	})
}
