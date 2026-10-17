// Command checklayout checks that a release built by scripts/release.sh
// is laid out as every installed Janus expects (ADR-004): it serves the
// dist folder as GitHub would and runs janus update's own install code
// against it, for every platform, with the keys the checksums were
// signed with.
//
//	go run ./scripts/checklayout -keys internal/update/release-keys.txt dist v1.2.3
//
// A change to release.sh that renamed an archive, moved the binary inside
// it, or renamed the checksums would break the update of every copy
// already installed; this fails instead.
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"

	"github.com/risadams/janus/internal/update"
)

// targets are the platforms release.sh builds, which janus update must be
// able to update.
var targets = [][2]string{
	{"darwin", "arm64"}, {"darwin", "amd64"},
	{"linux", "amd64"}, {"linux", "arm64"},
	{"windows", "amd64"}, {"windows", "arm64"},
}

func main() {
	keysPath := flag.String("keys", "internal/update/release-keys.txt", "the keys the checksums were signed with")
	flag.Parse()
	if flag.NArg() != 2 {
		fmt.Fprintln(os.Stderr, "usage: checklayout [-keys file] dist-folder version")
		os.Exit(2)
	}
	if err := check(flag.Arg(0), flag.Arg(1), *keysPath); err != nil {
		fmt.Fprintf(os.Stderr, "checklayout: %v\n", err)
		os.Exit(1)
	}
}

func check(dist, version, keysPath string) error {
	text, err := os.ReadFile(keysPath) // #nosec G304 -- the key list named on the command line
	if err != nil {
		return err
	}
	keys, err := update.ParseKeys(string(text))
	if err != nil {
		return fmt.Errorf("%s: %w", keysPath, err)
	}
	srv := httptest.NewServer(http.FileServer(http.Dir(dist)))
	defer srv.Close()

	files, err := os.ReadDir(dist)
	if err != nil {
		return err
	}
	r := update.Release{Tag: version, URL: srv.URL, Assets: map[string]string{}}
	for _, f := range files {
		r.Assets[f.Name()] = srv.URL + "/" + f.Name()
	}
	scratch, err := os.MkdirTemp("", "checklayout-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(scratch)

	for _, t := range targets {
		exe := filepath.Join(scratch, t[0]+"_"+t[1], "janus")
		if err := os.MkdirAll(filepath.Dir(exe), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(exe, []byte("the janus being replaced"), 0o600); err != nil {
			return err
		}
		if err := update.Install(context.Background(), srv.Client(), r, t[0], t[1], exe, "checklayout", keys); err != nil {
			return fmt.Errorf("janus update could not install the %s/%s build: %w", t[0], t[1], err)
		}
		fmt.Printf("ok  %s\n", update.ArchiveName(r, t[0], t[1]))
	}
	return nil
}
