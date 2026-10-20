package guard

import (
	"slices"
	"strings"
)

// This file tells, in a Bash command, the arguments that a program takes as
// an expression from those it may take as a path: the filter of jq, the
// pattern of grep, the text of an issue. A protected pattern named in one
// of them is not a file the command reads (#353). On a week of real use the
// policy withheld the output of commands such as
//
//	gh api repos/o/r/environments --jq '.environments[]|.name'
//	git grep -n '"\*\.pem"' -- internal | head -3
//
// because ".env*" matched the jq path and "*.pem" the search pattern
// (docs/research/results-1.0.md).
//
// It fails closed. A command is read only as far as the shell is followed
// here; anything else (command or process substitution, a subshell, a
// heredoc that is still in the command, a comment, a program that runs
// what it is given) leaves every word to be searched. And an expression is
// let off only where what the command prints cannot become a path some
// later program reads: its output goes to the tool result, through pipes
// into filters that treat their input as text, and nowhere else.

// shellWord is one word of a command, as the program receives it once
// quotes are taken off.
type shellWord struct {
	text string
	// open says the shell may still change the word before the program
	// sees it: it holds an unquoted glob character or brace, a tilde, or
	// a variable to expand.
	open bool
	// quoted says some of the word was quoted or escaped.
	quoted bool
}

// redirection is one of a command's redirections, such as 2>&1 or >out.
type redirection struct {
	op     string // >, >>, >|, <, <>, >& or <&
	target string
}

// simpleCommand is one program with its arguments and redirections.
type simpleCommand struct {
	words     []shellWord
	redirects []redirection
	pipe      bool // its output goes to the next command
}

// parseShell splits a command into simple commands, the way the shell
// would. ok is false for anything it does not follow.
func parseShell(s string) (cmds []simpleCommand, ok bool) {
	var cur simpleCommand
	var word strings.Builder
	inWord, open, quoted := false, false, false
	var pending *redirection // a redirection waiting for its target

	endWord := func() {
		if !inWord {
			return
		}
		if pending != nil {
			pending.target = word.String()
			cur.redirects = append(cur.redirects, *pending)
			pending = nil
		} else {
			cur.words = append(cur.words, shellWord{word.String(), open, quoted})
		}
		word.Reset()
		inWord, open, quoted = false, false, false
	}
	endCommand := func(pipe bool) bool {
		endWord()
		if pending != nil {
			return false
		}
		if len(cur.words) == 0 && len(cur.redirects) == 0 {
			return !pipe
		}
		cur.pipe = pipe
		cmds = append(cmds, cur)
		cur = simpleCommand{}
		return true
	}

	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\':
			if i+1 == len(s) {
				return nil, false
			}
			i++
			if s[i] != '\n' { // a backslash and newline join lines
				word.WriteByte(s[i])
				inWord, quoted = true, true
			}
		case c == '\'':
			end := strings.IndexByte(s[i+1:], '\'')
			if end < 0 {
				return nil, false
			}
			word.WriteString(s[i+1 : i+1+end])
			inWord, quoted = true, true
			i += end + 1
		case c == '"':
			inWord, quoted = true, true
			for i++; i < len(s) && s[i] != '"'; i++ {
				switch s[i] {
				case '\\':
					if i+1 < len(s) && strings.IndexByte("$`\"\\\n", s[i+1]) >= 0 {
						i++
						if s[i] != '\n' {
							word.WriteByte(s[i])
						}
						continue
					}
					word.WriteByte('\\')
				case '`':
					return nil, false
				case '$':
					if i+1 < len(s) && s[i+1] == '(' {
						return nil, false
					}
					open = true
					word.WriteByte('$')
				default:
					word.WriteByte(s[i])
				}
			}
			if i == len(s) {
				return nil, false
			}
		case c == '`', c == '(', c == ')':
			return nil, false
		case c == '$':
			if i+1 < len(s) && s[i+1] == '(' {
				return nil, false
			}
			word.WriteByte(c)
			inWord, open = true, true
		case c == '#' && !inWord:
			return nil, false
		case c == '~' && !inWord, c == '*', c == '?', c == '[', c == '{':
			word.WriteByte(c)
			inWord, open = true, true
		case c == ' ', c == '\t':
			endWord()
		case c == '\n', c == ';':
			if !endCommand(false) {
				return nil, false
			}
		case c == '|':
			pipe := true
			if i+1 < len(s) && s[i+1] == '|' {
				pipe = false
				i++
			} else if i+1 < len(s) && s[i+1] == '&' {
				i++
			}
			if !endCommand(pipe) {
				return nil, false
			}
		case c == '&' && i+1 < len(s) && s[i+1] == '>':
			// &> and &>> send both outputs to a file.
			endWord()
			if pending != nil {
				return nil, false
			}
			i++
			op := ">"
			if i+1 < len(s) && s[i+1] == '>' {
				op = ">>"
				i++
			}
			pending = &redirection{op: op}
		case c == '&':
			if i+1 < len(s) && s[i+1] == '&' {
				i++
			}
			if !endCommand(false) {
				return nil, false
			}
		case c == '<', c == '>':
			if inWord && !quoted && !open && strings.Trim(word.String(), "0123456789") == "" {
				word.Reset() // the descriptor, as in 2>
				inWord = false
			} else {
				endWord()
			}
			if pending != nil || i+1 < len(s) && (s[i+1] == '(' || c == '<' && s[i+1] == '<') {
				return nil, false // process substitution, heredoc, here-string
			}
			op := string(c)
			if i+1 < len(s) && strings.IndexByte(">|&", s[i+1]) >= 0 && (c == '>' || s[i+1] != '|') {
				op += string(s[i+1])
				i++
			}
			if strings.HasSuffix(op, "&") {
				// A copy of a descriptor: >&2, 2>&1, <&-.
				end := i + 1
				for end < len(s) && (s[end] >= '0' && s[end] <= '9' || s[end] == '-') {
					end++
				}
				if end == i+1 {
					return nil, false
				}
				cur.redirects = append(cur.redirects, redirection{op, s[i+1 : end]})
				i = end - 1
				continue
			}
			pending = &redirection{op: op}
		default:
			word.WriteByte(c)
			inWord = true
		}
	}
	if !endCommand(false) || len(cmds) > 0 && cmds[len(cmds)-1].pipe {
		return nil, false
	}
	return cmds, true
}

// program is the name of the program a simple command runs, after any
// assignments before it, or "" if that is not known before it runs.
func (c simpleCommand) program() (name string, at int) {
	for at < len(c.words) && isAssignment(c.words[at]) {
		at++
	}
	if at == len(c.words) || c.words[at].open {
		return "", at
	}
	w := c.words[at].text
	return w[strings.LastIndex(w, "/")+1:], at
}

func isAssignment(w shellWord) bool {
	name, _, ok := strings.Cut(w.text, "=")
	if !ok || name == "" {
		return false
	}
	for i, r := range name {
		if !(r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || i > 0 && r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

// textFilters read what is piped to them as text and print text. None of
// them opens a file named in its input or runs it, so an expression's
// output may pass through them on its way to the tool result. sed and awk
// are not among them: each can run its input as a command.
var textFilters = map[string]bool{
	"head": true, "tail": true, "wc": true, "sort": true, "uniq": true, "cut": true,
	"tr": true, "cat": true, "column": true, "nl": true, "fold": true, "rev": true,
	"tac": true, "grep": true, "egrep": true, "fgrep": true, "jq": true, "less": true,
}

// outputStaysPut reports whether what cmds[i] prints can only reach the
// tool result: through pipes into text filters, with no stage writing it
// to a file that a later command could read as a list of paths.
func outputStaysPut(cmds []simpleCommand, i int) bool {
	for k := i; ; k++ {
		for _, r := range cmds[k].redirects {
			if strings.ContainsAny(r.op, ">") && !strings.HasSuffix(r.op, "&") && r.target != "/dev/null" {
				return false
			}
		}
		if !cmds[k].pipe {
			return true
		}
		if k+1 == len(cmds) {
			return false
		}
		if name, _ := cmds[k+1].program(); !textFilters[name] {
			return false
		}
	}
}

// dataArguments removes from a Bash command the arguments its programs take
// as an expression or as message text, never as a path to read: jq's
// filter, the --jq of gh, the pattern of grep, git grep, and rg, and the
// title and body gh gives an issue or pull request. Everything else is
// kept, one word to a line, without its quotes. A command it cannot follow
// comes back as it was.
func dataArguments(command string) string {
	cmds, ok := parseShell(command)
	if !ok || strings.Contains(command, "POSIXLY_CORRECT") {
		return command
	}
	for _, c := range cmds {
		if name, _ := c.program(); runners[name] {
			return command
		}
	}
	var kept []string
	for i, c := range cmds {
		var drop map[int]bool
		if outputStaysPut(cmds, i) {
			drop = expressionWords(c)
		}
		for j, w := range c.words {
			if drop[j] {
				continue
			}
			kept = append(kept, w.text)
		}
		for _, r := range c.redirects {
			kept = append(kept, r.target)
		}
	}
	// Even with nothing dropped, the words as the program sees them are
	// what to search: a path written \.env or "secrets"/db.yaml is found
	// once its quotes are off.
	return strings.Join(kept, "\n")
}

// expressionWords returns the indexes of the words of c that its program
// takes as an expression or as text.
func expressionWords(c simpleCommand) map[int]bool {
	name, at := c.program()
	if at > 0 {
		return nil // VAR=value before a program can change how it reads its arguments
	}
	switch name {
	case "jq":
		return optionOperands(c.words, at+1, jqArgs)
	case "grep", "egrep", "fgrep":
		return optionOperands(c.words, at+1, grepArgs)
	case "rg":
		return optionOperands(c.words, at+1, rgArgs)
	case "git":
		for j := at + 1; j < len(c.words); j++ {
			switch w := c.words[j].text; {
			case w == "-C" || w == "-c":
				j++
			case w == "grep":
				return optionOperands(c.words, j+1, gitGrepArgs)
			case strings.HasPrefix(w, "-"):
				continue
			default:
				return nil
			}
		}
	case "gh":
		return ghExpressions(c.words, at+1)
	}
	return nil
}

// argSpec describes a program's options closely enough to tell its
// operands from the values of its options. An option it does not list
// stops the reading, since its value might be taken for an operand.
type argSpec struct {
	flags  string         // short options without a value
	values string         // short options with one value
	long   map[string]int // long options, and how many values each takes
	// pattern lists the options whose value is an expression. Any of
	// them, or one of fromFile, means the first operand is not one.
	pattern  []string
	fromFile []string
	// jq says the expression is a jq program, whose import and include
	// read files.
	jq bool
}

var jqArgs = argSpec{
	flags:  "acCehjMnrRsSV",
	values: "fL",
	long: map[string]int{
		"--seq": 0, "--stream": 0, "--stream-errors": 0, "--slurp": 0, "--raw-input": 0,
		"--null-input": 0, "--compact-output": 0, "--tab": 0, "--color-output": 0,
		"--monochrome-output": 0, "--ascii-output": 0, "--unbuffered": 0, "--sort-keys": 0,
		"--raw-output": 0, "--raw-output0": 0, "--join-output": 0, "--exit-status": 0,
		"--args": 0, "--jsonargs": 0, "--indent": 1, "--from-file": 1, "--library-path": 1,
		"--arg": 2, "--argjson": 2, "--slurpfile": 2, "--rawfile": 2,
	},
	fromFile: []string{"-f", "--from-file"},
	jq:       true,
}

var grepArgs = argSpec{
	flags:  "abcEFGhHiIlLnoqrRsTUvwxyzZ",
	values: "ABCDdefm",
	long: map[string]int{
		"--recursive": 0, "--dereference-recursive": 0, "--ignore-case": 0, "--line-number": 0,
		"--count": 0, "--files-with-matches": 0, "--files-without-match": 0, "--invert-match": 0,
		"--word-regexp": 0, "--line-regexp": 0, "--only-matching": 0, "--quiet": 0, "--silent": 0,
		"--no-filename": 0, "--with-filename": 0, "--extended-regexp": 0, "--fixed-strings": 0,
		"--basic-regexp": 0, "--perl-regexp": 0, "--null": 0, "--text": 0, "--no-messages": 0,
		"--color": 0, "--colour": 0,
		"--regexp": 1, "--file": 1, "--include": 1, "--exclude": 1, "--exclude-dir": 1,
		"--max-count": 1, "--context": 1, "--after-context": 1, "--before-context": 1,
	},
	pattern:  []string{"-e", "--regexp"},
	fromFile: []string{"-f", "--file"},
}

var rgArgs = argSpec{
	flags:  "abcFHiIlLnNopPqsSuUvwxz0",
	values: "ABCdefgjmMrtTE",
	long: map[string]int{
		"--ignore-case": 0, "--smart-case": 0, "--case-sensitive": 0, "--line-number": 0,
		"--no-line-number": 0, "--count": 0, "--files-with-matches": 0, "--files-without-match": 0,
		"--invert-match": 0, "--word-regexp": 0, "--line-regexp": 0, "--only-matching": 0,
		"--quiet": 0, "--fixed-strings": 0, "--hidden": 0, "--no-ignore": 0, "--with-filename": 0,
		"--no-filename": 0, "--no-heading": 0, "--heading": 0, "--multiline": 0, "--json": 0,
		"--regexp": 1, "--file": 1, "--glob": 1, "--iglob": 1, "--type": 1, "--type-not": 1,
		"--max-count": 1, "--context": 1, "--after-context": 1, "--before-context": 1,
		"--max-depth": 1, "--replace": 1,
	},
	pattern:  []string{"-e", "--regexp"},
	fromFile: []string{"-f", "--file"},
}

var gitGrepArgs = argSpec{
	flags:  "acEFGhHiIlLnopPqrvwWz",
	values: "ABCefm",
	long: map[string]int{
		"--cached": 0, "--untracked": 0, "--no-index": 0, "--ignore-case": 0, "--line-number": 0,
		"--count": 0, "--files-with-matches": 0, "--files-without-match": 0, "--invert-match": 0,
		"--word-regexp": 0, "--only-matching": 0, "--quiet": 0, "--extended-regexp": 0,
		"--fixed-strings": 0, "--basic-regexp": 0, "--perl-regexp": 0, "--full-name": 0,
		"--and": 0, "--or": 0, "--not": 0, "--all-match": 0, "--break": 0, "--heading": 0,
		"--regexp": 1, "--max-count": 1, "--context": 1, "--after-context": 1,
		"--before-context": 1, "--max-depth": 1, "--threads": 1,
	},
	pattern:  []string{"-e", "--regexp"},
	fromFile: []string{"-f"},
}

// optionOperands reads the words from start on with spec, and returns the
// indexes of the expressions among them: the values of spec.pattern
// options, or else the first operand. Options may come after operands, as
// GNU getopt allows, so the whole command is read before the first operand
// is judged.
func optionOperands(words []shellWord, start int, spec argSpec) map[int]bool {
	var exprs, operands []int
	operandsOnly, patternGiven := false, false
	for j := start; j < len(words); j++ {
		w := words[j]
		if operandsOnly || w.text == "-" || !strings.HasPrefix(w.text, "-") {
			operands = append(operands, j)
			continue
		}
		if w.open {
			return nil
		}
		if w.text == "--" {
			operandsOnly = true
			continue
		}
		if strings.HasPrefix(w.text, "--") {
			name, _, attached := strings.Cut(w.text, "=")
			n, known := spec.long[name]
			if !known || attached && n == 0 {
				return nil
			}
			if slices.Contains(spec.fromFile, name) {
				patternGiven = true
			}
			if slices.Contains(spec.pattern, name) {
				patternGiven = true
				if attached {
					exprs = append(exprs, j)
				} else {
					exprs = append(exprs, j+1)
				}
			}
			if attached {
				n--
			}
			j += n
			continue
		}
		for k := 1; k < len(w.text); k++ {
			ch := w.text[k]
			if strings.IndexByte(spec.flags, ch) >= 0 {
				continue
			}
			if strings.IndexByte(spec.values, ch) < 0 {
				return nil
			}
			opt := "-" + string(ch)
			if slices.Contains(spec.fromFile, opt) {
				patternGiven = true
			}
			if k+1 < len(w.text) { // the value is the rest of the word
				if slices.Contains(spec.pattern, opt) {
					patternGiven = true
					exprs = append(exprs, j)
				}
			} else {
				j++
				if slices.Contains(spec.pattern, opt) {
					patternGiven = true
					exprs = append(exprs, j)
				}
			}
			break
		}
	}
	if !patternGiven && len(operands) > 0 {
		exprs = append(exprs, operands[0])
	}
	out := map[int]bool{}
	for _, j := range exprs {
		if j < len(words) && expressionSafe(words[j], spec.jq) {
			out[j] = true
		}
	}
	return out
}

// expressionSafe reports whether a word given as an expression is the
// expression the program sees, and cannot itself read a file.
func expressionSafe(w shellWord, jq bool) bool {
	if w.open {
		return false
	}
	if jq {
		for _, reads := range []string{"import", "include", "modulemeta"} {
			if strings.Contains(w.text, reads) {
				return false
			}
		}
	}
	return true
}

// ghCommands are gh's own command groups. Any other first word may be an
// extension, a separate program that could read what it is given.
var ghCommands = map[string]bool{
	"api": true, "issue": true, "pr": true, "release": true, "repo": true, "run": true,
	"search": true, "project": true, "label": true, "workflow": true, "cache": true,
}

// ghExpressions finds the --jq expressions of a gh command, and the title
// and body of an issue or pull request it creates, edits, or comments on.
func ghExpressions(words []shellWord, start int) map[int]bool {
	if start >= len(words) || !ghCommands[words[start].text] {
		return nil
	}
	text := false
	if g := words[start].text; (g == "issue" || g == "pr") && start+1 < len(words) {
		switch words[start+1].text {
		case "create", "edit", "comment":
			text = true
		}
	}
	out := map[int]bool{}
	for j := start + 1; j < len(words); j++ {
		name, _, attached := strings.Cut(words[j].text, "=")
		var jq bool
		switch {
		case name == "--jq" || name == "-q":
			jq = true
		case text && (name == "--title" || name == "-t" || name == "--body" || name == "-b"):
		default:
			continue
		}
		at := j
		if !attached {
			at = j + 1
			j++
		}
		if at < len(words) && expressionSafe(words[at], jq) {
			out[at] = true
		}
	}
	return out
}
