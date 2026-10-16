package update

import (
	"crypto/ed25519"
	"encoding/base64"
	"strings"
	"testing"
)

func newKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

const sums = "0123abcd  janus_1.0.0_darwin_arm64.tar.gz\n"

func TestVerifyChecksumsAcceptsAnyListedKey(t *testing.T) {
	oldPub, oldPriv := newKey(t)
	newPub, _ := newKey(t)
	sig := ed25519.Sign(oldPriv, []byte(sums))
	// A release signed before the key was rotated still checks.
	if err := VerifyChecksums([]byte(sums), sig, []ed25519.PublicKey{newPub, oldPub}); err != nil {
		t.Errorf("a signature by a rotated key was refused: %v", err)
	}
}

func TestVerifyChecksumsRefusesATamperedFile(t *testing.T) {
	pub, priv := newKey(t)
	sig := ed25519.Sign(priv, []byte(sums))
	tampered := strings.Replace(sums, "0123", "9999", 1)
	if err := VerifyChecksums([]byte(tampered), sig, []ed25519.PublicKey{pub}); err == nil {
		t.Error("a changed checksums.txt was accepted")
	}
}

func TestVerifyChecksumsRefusesAnotherKey(t *testing.T) {
	pub, _ := newKey(t)
	_, other := newKey(t)
	if err := VerifyChecksums([]byte(sums), ed25519.Sign(other, []byte(sums)), []ed25519.PublicKey{pub}); err == nil {
		t.Error("a signature by an unknown key was accepted")
	}
}

func TestVerifyChecksumsFailsClosedWithoutKeys(t *testing.T) {
	_, priv := newKey(t)
	err := VerifyChecksums([]byte(sums), ed25519.Sign(priv, []byte(sums)), nil)
	if err == nil || !strings.Contains(err.Error(), "knows no release key") {
		t.Errorf("with no keys: %v", err)
	}
}

func TestVerifyChecksumsRefusesWhatIsNotASignature(t *testing.T) {
	pub, _ := newKey(t)
	if err := VerifyChecksums([]byte(sums), []byte("not a signature"), []ed25519.PublicKey{pub}); err == nil {
		t.Error("a short signature was accepted")
	}
}

func TestParseKeysSkipsCommentsAndRefusesJunk(t *testing.T) {
	pub, _ := newKey(t)
	text := "# a comment\n\n" + base64.StdEncoding.EncodeToString(pub) + "\n"
	keys, err := ParseKeys(text)
	if err != nil || len(keys) != 1 || !keys[0].Equal(pub) {
		t.Errorf("ParseKeys = %v, %v", keys, err)
	}
	if _, err := ParseKeys("not-a-key\n"); err == nil {
		t.Error("a line that is not a key was accepted")
	}
}

// A list with no key would leave janus update unable to check anything,
// and the release workflow unable to publish.
func TestTheShippedKeyListHasAKey(t *testing.T) {
	keys, err := ReleaseKeys()
	if err != nil {
		t.Fatalf("release-keys.txt: %v", err)
	}
	if len(keys) == 0 {
		t.Error("release-keys.txt lists no key")
	}
}
