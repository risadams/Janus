// completion.go is janus completion, which prints a tab-completion script
// for bash, fish, PowerShell, or zsh (#268). The scripts are generated
// from the commands table and from the flag sets the commands themselves
// make, so a new command or flag is completed without anyone remembering
// to add it.

package main

import (
	"flag"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
	"sync"
)

// completionShells are the shells janus completion writes for.
var completionShells = []verb{
	{"bash", "print the bash completion script"},
	{"fish", "print the fish completion script"},
	{"powershell", "print the PowerShell completion script"},
	{"zsh", "print the zsh completion script"},
}

// gathering, while janus completion collects the flag sets, holds each
// one a command makes, by the flag set's name. It is nil the rest of the
// time.
var gathering struct {
	sync.Mutex
	sets map[string]*flag.FlagSet
}

// gatherMu keeps two collections from sharing gathering.sets.
var gatherMu sync.Mutex

// newFlagSet makes a command's flag set. Every command makes its flags
// this way, never with flag.NewFlagSet, so that janus completion can see
// them: it runs each command with -h, which defines the flags and stops.
func newFlagSet(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	gathering.Lock()
	if gathering.sets != nil {
		gathering.sets[name] = fs
	}
	gathering.Unlock()
	return fs
}

// gatherFlagSets runs janus, each command, and each command's verbs with
// -h, and returns the flag sets they made. -h stops a command as soon as
// its flags are parsed, before it reads a config or does anything else.
func gatherFlagSets() map[string]*flag.FlagSet {
	gatherMu.Lock()
	defer gatherMu.Unlock()
	gathering.Lock()
	gathering.sets = map[string]*flag.FlagSet{}
	gathering.Unlock()
	run([]string{"-h"}, io.Discard, io.Discard)
	for _, c := range commands() {
		if c.name == "completion" {
			continue
		}
		if len(c.verbs) == 0 {
			c.run([]string{"-h"}, io.Discard, io.Discard)
		}
		for _, v := range c.verbs {
			c.run([]string{v.name, "-h"}, io.Discard, io.Discard)
		}
	}
	gathering.Lock()
	defer gathering.Unlock()
	sets := gathering.sets
	gathering.sets = nil
	return sets
}

// completionContext is what may come next at one place on the command
// line: after janus itself, after a command, or after a command's verb.
// Its key is "command/verb", with either left empty: "/" is janus alone,
// "stats/" is janus stats, "service/install" is janus service install.
type completionContext struct {
	key string
	// words are the commands or verbs that may come next.
	words []completionItem
	flags []completionItem
}

type completionItem struct{ name, about string }

// completionSpec is everything a completion script needs.
type completionSpec struct {
	contexts []completionContext
	// verbCommands are the commands whose first word is a verb.
	verbCommands []string
	// valueFlags are the flags, in any command, that take a value, which
	// is never a command or verb. No flag takes a value in one command and
	// not in another, which a test holds.
	valueFlags []string
}

func buildCompletionSpec() completionSpec {
	sets := gatherFlagSets()
	values := map[string]bool{}
	flagsOf := func(name string) []completionItem {
		fs := sets[name]
		if fs == nil {
			return nil
		}
		var items []completionItem
		fs.VisitAll(func(f *flag.Flag) {
			_, usage := flag.UnquoteUsage(f)
			about := firstLine(usage)
			if about == "" {
				about = "-" + f.Name
			}
			items = append(items, completionItem{"-" + f.Name, about})
			if !isBoolFlag(f) {
				values["-"+f.Name] = true
			}
		})
		return items
	}

	var spec completionSpec
	root := completionContext{key: "/", flags: flagsOf("janus")}
	for _, c := range commands() {
		root.words = append(root.words, completionItem{c.name, c.about})
	}
	spec.contexts = append(spec.contexts, root)
	for _, c := range commands() {
		if len(c.verbs) == 0 {
			spec.contexts = append(spec.contexts, completionContext{key: c.name + "/", flags: flagsOf("janus " + c.name)})
			continue
		}
		spec.verbCommands = append(spec.verbCommands, c.name)
		ctx := completionContext{key: c.name + "/"}
		for _, v := range c.verbs {
			ctx.words = append(ctx.words, completionItem(v))
		}
		spec.contexts = append(spec.contexts, ctx)
		for _, v := range c.verbs {
			spec.contexts = append(spec.contexts, completionContext{
				key: c.name + "/" + v.name, flags: flagsOf("janus " + c.name + " " + v.name)})
		}
	}
	for f := range values {
		spec.valueFlags = append(spec.valueFlags, f)
	}
	sort.Strings(spec.valueFlags)
	return spec
}

func isBoolFlag(f *flag.Flag) bool {
	b, ok := f.Value.(interface{ IsBoolFlag() bool })
	return ok && b.IsBoolFlag()
}

// firstLine is a flag's usage up to its first line break, since a shell
// shows a description on one line.
func firstLine(s string) string {
	s, _, _ = strings.Cut(s, "\n")
	return strings.TrimSpace(s)
}

func runCompletion(args []string, stdout, stderr io.Writer) int {
	usage := func() {
		fmt.Fprintf(stderr, "Usage: janus completion bash|fish|powershell|zsh\n\n"+
			"Prints a tab-completion script for the shell. docs/guide/install.md says\n"+
			"where each shell looks for one.\n")
	}
	if len(args) != 1 {
		usage()
		return 2
	}
	write := map[string]func(io.Writer, completionSpec){
		"bash": writeBash, "fish": writeFish, "powershell": writePowerShell, "zsh": writeZsh,
	}[args[0]]
	if write == nil {
		if slices.Contains([]string{"-h", "-help", "--help", "help"}, args[0]) {
			usage()
			return 0
		}
		fmt.Fprintf(stderr, "janus completion: no completion for %q\n", args[0])
		usage()
		return 2
	}
	write(stdout, buildCompletionSpec())
	return 0
}

// names is the items' names, separated by spaces.
func names(items []completionItem) string {
	var ns []string
	for _, it := range items {
		ns = append(ns, it.name)
	}
	return strings.Join(ns, " ")
}

// shQuote quotes s for bash, zsh, or fish's single quotes. fish reads \'
// and \\ inside single quotes, where the POSIX shells end the quote.
func shQuote(s string, fish bool) string {
	if fish {
		return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(s) + "'"
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

const generatedNote = "Generated by janus completion %s from janus's own commands and flags;\n# regenerate it after updating janus, rather than editing it."

// writeBash writes a bash script. bash shows no descriptions, so only the
// names go in.
func writeBash(w io.Writer, spec completionSpec) {
	fmt.Fprintf(w, "# bash completion for janus. "+generatedNote+"\n", "bash")
	fmt.Fprintf(w, `_janus() {
    local cur=${COMP_WORDS[COMP_CWORD]} prev=${COMP_WORDS[COMP_CWORD-1]}
    local cmd= verb= skip= w i
    for ((i = 1; i < COMP_CWORD; i++)); do
        w=${COMP_WORDS[i]}
        if [[ -n $skip ]]; then skip=; continue; fi
        case $w in
            --) return ;;
            %s) skip=1; continue ;;
            -*) continue ;;
        esac
        if [[ -z $cmd ]]; then
            cmd=$w
        elif [[ -z $verb ]]; then
            case $cmd in %s) verb=$w ;; esac
        fi
    done
    case $prev in %s) return ;; esac
    local words=
    case "$cmd/$verb" in
`, strings.Join(spec.valueFlags, "|"), strings.Join(spec.verbCommands, "|"), strings.Join(spec.valueFlags, "|"))
	for _, c := range spec.contexts {
		fmt.Fprintf(w, "        %s)\n", shQuote(c.key, false))
		fmt.Fprintf(w, "            if [[ $cur == -* ]]; then words=%s; else words=%s; fi ;;\n",
			shQuote(names(c.flags), false), shQuote(names(c.words), false))
	}
	fmt.Fprintf(w, `    esac
    COMPREPLY=($(compgen -W "$words" -- "$cur"))
}
complete -o default -F _janus janus
`)
}

// writeZsh writes a zsh script, which works both from a directory on
// $fpath, as _janus, and sourced.
func writeZsh(w io.Writer, spec completionSpec) {
	fmt.Fprintf(w, "#compdef janus\n# zsh completion for janus. "+generatedNote+"\n", "zsh")
	fmt.Fprintf(w, `_janus() {
    local cmd= verb= skip= w i
    for ((i = 2; i < CURRENT; i++)); do
        w=${words[i]}
        if [[ -n $skip ]]; then skip=; continue; fi
        case $w in
            --) _files; return ;;
            %s) skip=1; continue ;;
            -*) continue ;;
        esac
        if [[ -z $cmd ]]; then
            cmd=$w
        elif [[ -z $verb ]]; then
            case $cmd in %s) verb=$w ;; esac
        fi
    done
    case ${words[CURRENT-1]} in %s) _files; return ;; esac
    local -a items
    case "$cmd/$verb" in
`, strings.Join(spec.valueFlags, "|"), strings.Join(spec.verbCommands, "|"), strings.Join(spec.valueFlags, "|"))
	described := func(items []completionItem) string {
		var parts []string
		for _, it := range items {
			parts = append(parts, shQuote(it.name+":"+it.about, false))
		}
		return "(" + strings.Join(parts, " ") + ")"
	}
	for _, c := range spec.contexts {
		fmt.Fprintf(w, "        %s)\n", shQuote(c.key, false))
		fmt.Fprintf(w, "            if [[ ${words[CURRENT]} == -* ]]; then items=%s; else items=%s; fi ;;\n",
			described(c.flags), described(c.words))
	}
	fmt.Fprintf(w, `    esac
    if (( ${#items} )); then
        _describe janus items
    else
        _files
    fi
}
if [[ $funcstack[1] == _janus ]]; then
    _janus "$@"
else
    compdef _janus janus
fi
`)
}

// writeFish writes a fish script. fish's -o is a flag with one dash and a
// long name, which is how Go's flags are spelled.
func writeFish(w io.Writer, spec completionSpec) {
	fmt.Fprintf(w, "# fish completion for janus. "+generatedNote+"\n", "fish")
	fmt.Fprintf(w, `function __janus_context
    set -l cmd
    set -l verb
    set -l skip
    for w in (commandline -opc)[2..-1]
        if test -n "$skip"
            set skip
            continue
        end
        switch $w
            case --
                echo --
                return
            case %s
                set skip 1
                continue
            case '-*'
                continue
        end
        if test -z "$cmd"
            set cmd $w
        else if test -z "$verb"; and contains -- $cmd %s
            set verb $w
        end
    end
    echo "$cmd/$verb"
end

function __janus_at
    test (__janus_context) = $argv[1]
end

`, strings.Join(spec.valueFlags, " "), strings.Join(spec.verbCommands, " "))
	for _, c := range spec.contexts {
		cond := shQuote("__janus_at "+c.key, true)
		if len(c.words) > 0 {
			fmt.Fprintf(w, "complete -c janus -n %s -f\n", cond)
		}
		for _, it := range c.words {
			fmt.Fprintf(w, "complete -c janus -n %s -a %s -d %s\n", cond, it.name, shQuote(it.about, true))
		}
		for _, it := range c.flags {
			value := ""
			if slices.Contains(spec.valueFlags, it.name) {
				value = " -r"
			}
			fmt.Fprintf(w, "complete -c janus -n %s -o %s%s -d %s\n", cond, strings.TrimPrefix(it.name, "-"), value, shQuote(it.about, true))
		}
	}
}

// psQuote quotes s for PowerShell's single quotes, in which only a quote
// is special, written twice.
func psQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// writePowerShell writes a PowerShell script. An empty answer leaves
// PowerShell to complete a file name, as for a flag's value.
func writePowerShell(w io.Writer, spec completionSpec) {
	fmt.Fprintf(w, "# PowerShell completion for janus. "+generatedNote+"\n", "powershell")
	quoted := func(ss []string) string {
		var qs []string
		for _, s := range ss {
			qs = append(qs, psQuote(s))
		}
		return "@(" + strings.Join(qs, ", ") + ")"
	}
	// Each item is a hashtable, since an array of two-item arrays is
	// flattened into one array when it has a single item.
	items := func(items []completionItem) string {
		var qs []string
		for _, it := range items {
			qs = append(qs, "@{ n = "+psQuote(it.name)+"; d = "+psQuote(it.about)+" }")
		}
		return "@(" + strings.Join(qs, ", ") + ")"
	}
	fmt.Fprintf(w, `Register-ArgumentCompleter -Native -CommandName janus -ScriptBlock {
    param($wordToComplete, $commandAst, $cursorPosition)
    $valueFlags = %s
    $verbCommands = %s
    $words = @{
`, quoted(spec.valueFlags), quoted(spec.verbCommands))
	for _, c := range spec.contexts {
		fmt.Fprintf(w, "        %s = %s\n", psQuote(c.key), items(c.words))
	}
	fmt.Fprintf(w, "    }\n    $flags = @{\n")
	for _, c := range spec.contexts {
		fmt.Fprintf(w, "        %s = %s\n", psQuote(c.key), items(c.flags))
	}
	fmt.Fprintf(w, `    }
    $cmd = ''
    $verb = ''
    $skip = $false
    $elements = $commandAst.CommandElements
    for ($i = 1; $i -lt $elements.Count; $i++) {
        $e = $elements[$i]
        if ($e.Extent.EndOffset -ge $cursorPosition) { break }
        $w = $e.ToString()
        if ($skip) { $skip = $false; continue }
        if ($w -eq '--') { return }
        if ($valueFlags -contains $w) { $skip = $true; continue }
        if ($w.StartsWith('-')) { continue }
        if ($cmd -eq '') { $cmd = $w }
        elseif ($verb -eq '' -and $verbCommands -contains $cmd) { $verb = $w }
    }
    if ($skip) { return }
    $key = "$cmd/$verb"
    $candidates = if ($wordToComplete.StartsWith('-')) { $flags[$key] } else { $words[$key] }
    foreach ($c in $candidates) {
        if ($c.n.StartsWith($wordToComplete)) {
            [System.Management.Automation.CompletionResult]::new($c.n, $c.n, 'ParameterValue', $c.d)
        }
    }
}
`)
}
