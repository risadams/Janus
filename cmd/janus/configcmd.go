// configcmd.go is `janus config upgrade`, which rewrites a config written
// for an older janus in this one's form (#269). Janus reads an old config
// as it is, upgrading it in memory each time; the file changes only when
// someone asks.

package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/risadams/janus/internal/config"
)

// configVerbs are what janus config does.
var configVerbs = []verb{
	{"upgrade", "rewrite the config in this version's form, keeping a .bak"},
}

func runConfig(args []string, stdout, stderr io.Writer) int {
	usage := func() {
		fmt.Fprintf(stderr, "Usage: janus config upgrade [-config file]\n\n"+
			"upgrade rewrites a config written for an older janus in this version's form,\n"+
			"keeping the old file beside it as .bak.\n")
	}
	if len(args) == 0 || args[0] != "upgrade" {
		usage()
		if len(args) > 0 && (args[0] == "-h" || args[0] == "-help" || args[0] == "--help" || args[0] == "help") {
			return 0
		}
		return 2
	}
	fs := newFlagSet("janus config upgrade", stderr)
	configPath := fs.String("config", config.DefaultPath, "path to the config file")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "janus config upgrade: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	notes, err := config.Rewrite(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "janus config upgrade: %v\n", err)
		return 1
	}
	if len(notes) == 0 {
		fmt.Fprintf(stdout, "%s is already in this version's form (config version %d); nothing to change.\n", *configPath, config.Version())
		return 0
	}
	fmt.Fprintf(stdout, "Rewrote %s for config version %d, keeping the old file as %s.bak:\n", *configPath, config.Version(), *configPath)
	for _, n := range notes {
		fmt.Fprintf(stdout, "  - %s\n", n)
	}
	return 0
}

// upgradedNote says that a config was written for an older janus, what
// reading it in this version's form changed, and how to make that stick.
func upgradedNote(path string, notes []string) string {
	return fmt.Sprintf("%s was written for an older janus. It works as it is, read as:\n  - %s\n"+
		"janus config upgrade -config %s rewrites it this way, keeping a copy.",
		path, strings.Join(notes, "\n  - "), path)
}
