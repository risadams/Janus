// Command signsums signs a release's checksums.txt, so that a download can
// be checked against a key compiled into Janus rather than against the
// release page it came from. See docs/adr/003-signed-checksums.md.
//
//	go run ./scripts/signsums dist/checksums.txt
//
// signs with the private key in JANUS_RELEASE_KEY (base64, as -new makes
// it), writes dist/checksums.txt.sig, and checks the signature against the
// keys Janus is built with, so that a release whose key Janus does not
// know is never published.
//
//	go run ./scripts/signsums -new | gh secret set JANUS_RELEASE_KEY
//
// makes a new key: the private half goes to stdout and so straight into
// the Actions secret, and the public half is added to the top of the key
// list. Nothing else keeps the private half: a key that is lost is
// replaced, not recovered.
package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/risadams/janus/internal/update"
)

func main() {
	keys := flag.String("keys", "internal/update/release-keys.txt", "the list of public keys")
	newKey := flag.Bool("new", false, "make a new key: the private half to stdout, the public half into -keys")
	flag.Parse()

	var err error
	switch {
	case *newKey && flag.NArg() == 0:
		err = makeKey(*keys)
	case !*newKey && flag.NArg() == 1:
		err = sign(flag.Arg(0), *keys)
	default:
		fmt.Fprintln(os.Stderr, "usage: signsums [-keys file] checksums.txt | signsums -new [-keys file]")
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "signsums: %v\n", err)
		os.Exit(1)
	}
}

// makeKey writes a new private key to stdout and adds its public key to
// the top of the list at path, after the comments that head it. It refuses
// to print the private key on a terminal, where it would stay in the
// scrollback, or into /dev/null, which would add a key nobody holds.
func makeKey(path string) error {
	if info, err := os.Stdout.Stat(); err == nil && info.Mode()&os.ModeCharDevice != 0 {
		return errors.New("the private key would be shown on a terminal or thrown away; pipe it into gh secret set JANUS_RELEASE_KEY")
	}
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		return err
	}
	old, err := os.ReadFile(path) // #nosec G304 -- the key list named on the command line
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	var head, rest []string
	for _, line := range strings.SplitAfter(string(old), "\n") {
		if line == "" {
			continue
		}
		if len(rest) == 0 && strings.HasPrefix(line, "#") {
			head = append(head, line)
		} else {
			rest = append(rest, line)
		}
	}
	list := strings.Join(head, "") + base64.StdEncoding.EncodeToString(pub) + "\n" + strings.Join(rest, "")
	// The list holds public keys and is committed, so anyone may read it.
	if err := os.WriteFile(path, []byte(list), 0o644); err != nil { // #nosec G306 G703 -- public, and the path is the command line's
		return err
	}
	if _, err := fmt.Println(base64.StdEncoding.EncodeToString(priv.Seed())); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "Added the new public key to %s; commit it.\n", path)
	return nil
}

// sign signs the file at sums with the key in JANUS_RELEASE_KEY and checks
// the signature against the keys at keysPath.
func sign(sums, keysPath string) error {
	seed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(os.Getenv("JANUS_RELEASE_KEY")))
	if err != nil || len(seed) != ed25519.SeedSize {
		return errors.New("JANUS_RELEASE_KEY is not set to a key made by signsums -new")
	}
	priv := ed25519.NewKeyFromSeed(seed)
	data, err := os.ReadFile(sums) // #nosec G304 -- the checksums file named on the command line
	if err != nil {
		return err
	}
	sig := ed25519.Sign(priv, data)

	text, err := os.ReadFile(keysPath) // #nosec G304 -- the key list named on the command line
	if err != nil {
		return err
	}
	keys, err := update.ParseKeys(string(text))
	if err != nil {
		return fmt.Errorf("%s: %w", keysPath, err)
	}
	if err := update.VerifyChecksums(data, sig, keys); err != nil {
		return fmt.Errorf("the signing key is not in %s, so Janus could not check this release: %w", keysPath, err)
	}
	// A signature is published beside the release, so anyone may read it.
	if err := os.WriteFile(sums+".sig", sig, 0o644); err != nil { // #nosec G306 G703 -- public, and the path is the command line's
		return err
	}
	fmt.Fprintf(os.Stderr, "signed %s\n", sums)
	return nil
}
