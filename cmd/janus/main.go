// main.go — entry point for the janus binary.
//
// Nothing is wired up yet: the proxy, config, and backends arrive in M1. For
// now the binary only reports its version, which is enough for CI and the
// release pipeline to have something real to build.
package main

import (
	"fmt"
	"os"
)

// version is overridden at build time with
// -ldflags "-X main.version=v1.2.3"; "dev" marks a plain `go build`.
var version = "dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] != "version" && os.Args[1] != "-version" && os.Args[1] != "--version" {
		fmt.Fprintf(os.Stderr, "janus: unknown command %q\n", os.Args[1])
		os.Exit(2)
	}
	fmt.Println(versionLine())
}

func versionLine() string {
	return "janus " + version
}
