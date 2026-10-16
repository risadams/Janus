package update

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeRelease is a release of v1.2.0 served by a test server, built the
// way scripts/release.sh and the release workflow build one.
type fakeRelease struct {
	files map[string][]byte
	srv   *httptest.Server
	key   ed25519.PublicKey
	priv  ed25519.PrivateKey
}

func newFakeRelease(t *testing.T, goos, goarch string, binary []byte) *fakeRelease {
	t.Helper()
	f := &fakeRelease{files: map[string][]byte{}}
	f.key, f.priv = newKey(t)
	r := Release{Tag: "v1.2.0"}
	name := ArchiveName(r, goos, goarch)
	dir := strings.TrimSuffix(strings.TrimSuffix(name, ".tar.gz"), ".zip")
	exe := "janus"
	if goos == "windows" {
		exe = "janus.exe"
	}
	f.files[name] = archiveOf(t, goos == "windows", map[string][]byte{
		dir + "/" + exe:    binary,
		dir + "/README.md": []byte("readme"),
	})
	sum := sha256.Sum256(f.files[name])
	f.setSums(hex.EncodeToString(sum[:]) + "  " + name + "\n")
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, ok := f.files[strings.TrimPrefix(r.URL.Path, "/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(b)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// setSums replaces checksums.txt and signs it again.
func (f *fakeRelease) setSums(sums string) {
	f.files["checksums.txt"] = []byte(sums)
	f.files["checksums.txt.sig"] = ed25519.Sign(f.priv, []byte(sums))
}

func (f *fakeRelease) release() Release {
	r := Release{Tag: "v1.2.0", URL: "https://example.invalid/v1.2.0", Assets: map[string]string{}}
	for name := range f.files {
		r.Assets[name] = f.srv.URL + "/" + name
	}
	return r
}

func archiveOf(t *testing.T, isZip bool, files map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	if isZip {
		zw := zip.NewWriter(&buf)
		for name, b := range files {
			w, _ := zw.Create(name)
			w.Write(b)
		}
		zw.Close()
		return buf.Bytes()
	}
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, b := range files {
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(b)), Typeflag: tar.TypeReg})
		tw.Write(b)
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

// oldBinary is the janus being replaced.
func oldBinary(t *testing.T) string {
	t.Helper()
	exe := filepath.Join(t.TempDir(), "janus")
	if err := os.WriteFile(exe, []byte("old janus"), 0o755); err != nil {
		t.Fatal(err)
	}
	return exe
}

func install(f *fakeRelease, goos, exe string, keys []ed25519.PublicKey) error {
	return Install(context.Background(), f.srv.Client(), f.release(), goos, "arm64", exe, "janus/test", keys)
}

func wantContent(t *testing.T, path, want string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil || string(b) != want {
		t.Errorf("%s holds %q (%v), want %q", filepath.Base(path), b, err, want)
	}
}

func TestInstallReplacesTheBinary(t *testing.T) {
	f := newFakeRelease(t, "darwin", "arm64", []byte("new janus"))
	exe := oldBinary(t)
	if err := install(f, "darwin", exe, []ed25519.PublicKey{f.key}); err != nil {
		t.Fatal(err)
	}
	wantContent(t, exe, "new janus")
	// Windows has no execute bit to keep.
	if info, _ := os.Stat(exe); runtime.GOOS != "windows" && info.Mode().Perm()&0o100 == 0 {
		t.Errorf("the new binary is not executable: %v", info.Mode())
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(exe), ".janus-update-*")); len(left) > 0 {
		t.Errorf("left behind: %v", left)
	}
}

func TestInstallOnWindowsMovesTheRunningBinaryAside(t *testing.T) {
	f := newFakeRelease(t, "windows", "arm64", []byte("new janus"))
	exe := oldBinary(t)
	// What the last update left: removed now that it is not running.
	os.WriteFile(exe+".old", []byte("older janus"), 0o755)
	if err := install(f, "windows", exe, []ed25519.PublicKey{f.key}); err != nil {
		t.Fatal(err)
	}
	wantContent(t, exe, "new janus")
	wantContent(t, exe+".old", "old janus")
}

func TestInstallRefusesABadChecksum(t *testing.T) {
	f := newFakeRelease(t, "darwin", "arm64", []byte("new janus"))
	name := ArchiveName(Release{Tag: "v1.2.0"}, "darwin", "arm64")
	f.setSums(strings.Repeat("0", 64) + "  " + name + "\n")
	exe := oldBinary(t)
	err := install(f, "darwin", exe, []ed25519.PublicKey{f.key})
	if err == nil || !strings.Contains(err.Error(), "does not match its checksum") {
		t.Errorf("a tampered archive: %v", err)
	}
	wantContent(t, exe, "old janus")
}

func TestInstallRefusesABadSignature(t *testing.T) {
	f := newFakeRelease(t, "darwin", "arm64", []byte("new janus"))
	other, _ := newKey(t)
	exe := oldBinary(t)
	err := install(f, "darwin", exe, []ed25519.PublicKey{other})
	if err == nil || !strings.Contains(err.Error(), "not signed by a Janus release key") {
		t.Errorf("checksums signed by an unknown key: %v", err)
	}
	wantContent(t, exe, "old janus")
}

func TestInstallRefusesAReleaseWithoutASignature(t *testing.T) {
	f := newFakeRelease(t, "darwin", "arm64", []byte("new janus"))
	delete(f.files, "checksums.txt.sig")
	exe := oldBinary(t)
	err := install(f, "darwin", exe, []ed25519.PublicKey{f.key})
	if err == nil || !strings.Contains(err.Error(), "no signed checksums") {
		t.Errorf("an unsigned release: %v", err)
	}
	wantContent(t, exe, "old janus")
}

func TestInstallRefusesAPlatformWithoutABuild(t *testing.T) {
	f := newFakeRelease(t, "darwin", "arm64", []byte("new janus"))
	exe := oldBinary(t)
	err := install(f, "plan9", exe, []ed25519.PublicKey{f.key})
	if err == nil || !strings.Contains(err.Error(), "no build for plan9/arm64") {
		t.Errorf("a platform with no build: %v", err)
	}
	wantContent(t, exe, "old janus")
}

func TestInstallRefusesAnArchiveWithoutTheBinary(t *testing.T) {
	f := newFakeRelease(t, "darwin", "arm64", []byte("new janus"))
	name := ArchiveName(Release{Tag: "v1.2.0"}, "darwin", "arm64")
	f.files[name] = archiveOf(t, false, map[string][]byte{"elsewhere/janus": []byte("new janus")})
	sum := sha256.Sum256(f.files[name])
	f.setSums(hex.EncodeToString(sum[:]) + "  " + name + "\n")
	exe := oldBinary(t)
	if err := install(f, "darwin", exe, []ed25519.PublicKey{f.key}); err == nil {
		t.Error("an archive with the binary in the wrong place was installed")
	}
	wantContent(t, exe, "old janus")
}

func TestSumForReadsBothFormsOfSha256sum(t *testing.T) {
	d := strings.Repeat("ab", 32)
	for _, line := range []string{d + "  janus.tar.gz", d + " *janus.tar.gz"} {
		if got, err := sumFor([]byte("other\n"+line+"\n"), "janus.tar.gz"); err != nil || got != d {
			t.Errorf("%q: %q, %v", line, got, err)
		}
	}
}
