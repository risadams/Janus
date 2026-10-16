package update

import (
	"bufio"
	"crypto/ed25519"
	_ "embed"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// releaseKeysFile is the list of keys that sign releases, kept as text so
// that the same list is what people check a download against by hand.
//
//go:embed release-keys.txt
var releaseKeysFile string

// ReleaseKeys returns the public keys that sign releases, the newest
// first.
func ReleaseKeys() ([]ed25519.PublicKey, error) {
	return ParseKeys(releaseKeysFile)
}

// ParseKeys reads a list of keys in the shape of release-keys.txt: one
// base64 ed25519 public key per line, with blank lines and lines starting
// with # left out.
func ParseKeys(text string) ([]ed25519.PublicKey, error) {
	var keys []ed25519.PublicKey
	sc := bufio.NewScanner(strings.NewReader(text))
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		b, err := base64.StdEncoding.DecodeString(line)
		if err != nil || len(b) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("line %d is not a base64 ed25519 public key", n)
		}
		keys = append(keys, ed25519.PublicKey(b))
	}
	return keys, sc.Err()
}

// VerifyChecksums reports whether sig, the contents of a release's
// checksums.txt.sig, is a signature of sums, its checksums.txt, by one of
// keys. Only once it is can the sums be trusted to say which archives are
// genuine.
func VerifyChecksums(sums, sig []byte, keys []ed25519.PublicKey) error {
	if len(keys) == 0 {
		return errors.New("this build of janus knows no release key, so it cannot check a download")
	}
	if len(sig) != ed25519.SignatureSize {
		return errors.New("checksums.txt.sig is not an ed25519 signature")
	}
	for _, k := range keys {
		if ed25519.Verify(k, sums, sig) {
			return nil
		}
	}
	return errors.New("checksums.txt is not signed by a Janus release key: the download may have been tampered with")
}
