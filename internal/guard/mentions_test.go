package guard

import (
	"strings"
	"testing"
)

// A tool call that only mentions a protected pattern, in what it writes or
// in a heredoc it copies to a file, did not read a protected file, and its
// result is not withheld (#187). On 2026-10-14 the policy withheld a Write
// of a docs page and two Bash commands writing issue text, all of which
// quoted the pattern itself.
func TestMentioningAPatternIsNotReadingIt(t *testing.T) {
	p := policy(t, []string{"vault/**", "*.p12"}, nil)
	calls := map[string]map[string]any{
		"Write content": toolUse("t1", "Write", map[string]any{
			"file_path": "/app/docs/guide/configure.md",
			"content":   "localOnly paths: [\"vault/**\", \"*.p12\"] keep vault/db.yaml off Anthropic",
		}),
		"Edit strings": toolUse("t1", "Edit", map[string]any{
			"file_path":  "/app/docs/GUARD.md",
			"old_string": "protect vault/**",
			"new_string": "protect vault/** and *.p12",
		}),
		"Bash description": toolUse("t1", "Bash", map[string]any{
			"command":     "go test ./internal/guard",
			"description": "Test that vault/db.yaml stays local",
		}),
		"heredoc to a file": toolUse("t1", "Bash", map[string]any{
			"command": "cat > /tmp/notes.md <<'EOF'\nset localOnly to \"vault/**\" and \"*.p12\"\nEOF\nwc -l /tmp/notes.md",
		}),
		"heredoc as an issue body": toolUse("t1", "Bash", map[string]any{
			"command": "gh issue create --title x --body-file - <<'EOF'\nA Bash read of vault/db.yaml was withheld.\nEOF",
		}),
		"heredoc in a commit message": toolUse("t1", "Bash", map[string]any{
			"command": "git commit -q -F - <<'EOF'\nStop withholding mentions of vault/**\nEOF",
		}),
		"heredoc with <<- and tabs": toolUse("t1", "Bash", map[string]any{
			"command": "tee docs/x.md <<-EOF >/dev/null\n\tkeys live in vault/keys.p12\n\tEOF",
		}),
	}
	for name, call := range calls {
		req := request(t, "sys", assistant(call), user(toolResult("t1", "done")))
		if m := p.Check(req); m.Protected() {
			t.Errorf("%s: withheld, for %v", name, m.Reasons)
		}
	}
}

// What the narrowing must not let through: a protected file read through a
// heredoc that is code, or by a path argument, or by any tool Janus does
// not know.
func TestReadingAProtectedFileIsStillCaught(t *testing.T) {
	p := policy(t, []string{"vault/**", "*.p12"}, nil)
	calls := map[string]map[string]any{
		"heredoc run by a shell": toolUse("t1", "Bash", map[string]any{
			"command": "bash <<'EOF'\ncat vault/db.yaml\nEOF",
		}),
		"heredoc run by python": toolUse("t1", "Bash", map[string]any{
			"command": "python3 - <<'EOF'\nprint(open('vault/db.yaml').read())\nEOF",
		}),
		"heredoc piped to a shell": toolUse("t1", "Bash", map[string]any{
			"command": "cat <<'EOF' | sh\ncat vault/db.yaml\nEOF",
		}),
		"script written then run by sh": toolUse("t1", "Bash", map[string]any{
			"command": "cat > run.sh <<'EOF'\ncat vault/db.yaml\nEOF\nsh run.sh",
		}),
		"script written then run directly": toolUse("t1", "Bash", map[string]any{
			"command": "cat > run.sh <<'EOF'\ncat vault/db.yaml\nEOF\nchmod +x run.sh && ./run.sh",
		}),
		"heredoc never closed": toolUse("t1", "Bash", map[string]any{
			"command": "cat > notes.md <<EOF\ncat vault/db.yaml",
		}),
		"read outside a heredoc": toolUse("t1", "Bash", map[string]any{
			"command": "cat > notes.md <<'EOF'\nhello\nEOF\ncat vault/db.yaml",
		}),
		"Write to a protected path": toolUse("t1", "Write", map[string]any{
			"file_path": "/app/vault/new.yaml", "content": "x",
		}),
		"Edit of a protected path": toolUse("t1", "Edit", map[string]any{
			"file_path": "/app/certs/client.p12", "old_string": "a", "new_string": "b",
		}),
		"an MCP tool": toolUse("t1", "mcp__files__read", map[string]any{
			"content": "vault/db.yaml",
		}),
	}
	for name, call := range calls {
		req := request(t, "sys", assistant(call), user(toolResult("t1", "secret: hunter2")))
		if m := p.Check(req); !m.Protected() {
			t.Errorf("%s: not withheld", name)
		}
	}
}

func TestDataHeredocs(t *testing.T) {
	for command, want := range map[string]string{
		"cat > a.md <<'EOF'\nbody\nEOF":                  "cat > a.md <<'EOF'\nEOF",
		"cat > a.md <<\"EOF\"\nbody\nEOF\nls":            "cat > a.md <<\"EOF\"\nEOF\nls",
		"X=1 cat > a.md <<EOF\nbody\nEOF":                "X=1 cat > a.md <<EOF\nEOF",
		"cat > a.md <<'EOF'\nbody\nEOF\ncat a.md":        "cat > a.md <<'EOF'\nEOF\ncat a.md",     // read back, not run
		"cat > a.sh <<'EOF'\nbody\nEOF\n./a.sh":          "cat > a.sh <<'EOF'\nbody\nEOF\n./a.sh", // run
		"cat <<< 'here string'":                          "cat <<< 'here string'",
		"cat > a.md <<'EOF\"\nbody\nEOF":                 "cat > a.md <<'EOF\"\nbody\nEOF", // mismatched quotes
		"sort <<EOF\nb\na\nEOF":                          "sort <<EOF\nb\na\nEOF",          // not a sink
		"echo hi":                                        "echo hi",
		"cat > a <<A\n1\nA\ncat > b <<B\n2\nB":           "cat > a <<A\nA\ncat > b <<B\nB",
		"cat > a <<A\n1\nA\nenv FOO=1 true":              "cat > a <<A\n1\nA\nenv FOO=1 true", // a runner anywhere
		"git commit -F - <<'EOF'\nmsg\nEOF\ngit push -q": "git commit -F - <<'EOF'\nEOF\ngit push -q",
	} {
		if got := dataHeredocs(command); got != want {
			t.Errorf("dataHeredocs(%q)\n = %q\nwant %q", command, got, want)
		}
	}
}

// FuzzDataHeredocs feeds the heredoc parser commands a model might write.
// It must not panic, and whatever it returns is the command itself or the
// command with whole lines taken out: it never invents text.
func FuzzDataHeredocs(f *testing.F) {
	for _, seed := range []string{
		"cat > a.md <<'EOF'\nbody\nEOF",
		"tee x <<-E\n\tline\n\tE",
		"gh issue create --body-file - <<'EOF'\ntext\nEOF\necho done",
		"cat <<EOF | sh\nls\nEOF",
		"<<", "<<-", "cat <<'", "a\n<<EOF\n",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, command string) {
		got := dataHeredocs(command)
		if got == command {
			return
		}
		lines := strings.Split(command, "\n")
		at := 0
		for _, l := range strings.Split(got, "\n") {
			for at < len(lines) && lines[at] != l {
				at++
			}
			if at == len(lines) {
				t.Fatalf("dataHeredocs(%q) = %q, which is not the command with lines removed", command, got)
			}
			at++
		}
	})
}
