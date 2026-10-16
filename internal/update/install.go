package update

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Bounds on what is downloaded. A release archive is around ten
// megabytes and its binary a little more; these leave room to grow and
// stop a wrong answer from filling the disk.
const (
	maxArchive = 200 << 20
	maxBinary  = 400 << 20
	maxSums    = 64 << 10
)

// ArchiveName is the name of r's archive for one platform, as
// scripts/release.sh names it: janus_1.2.3_darwin_arm64.tar.gz, or .zip
// for Windows.
func ArchiveName(r Release, goos, goarch string) string {
	ext := ".tar.gz"
	if goos == "windows" {
		ext = ".zip"
	}
	return "janus_" + strings.TrimPrefix(r.Tag, "v") + "_" + goos + "_" + goarch + ext
}

// Install downloads r's build for goos and goarch, checks it, and puts its
// binary in place of the file at exe. The signature on checksums.txt is
// checked against keys before the sums are trusted, and the archive
// against the sums before anything is unpacked; any failure leaves exe as
// it was. The new binary is written beside exe and renamed over it, so
// that exe is always one whole binary or the other. On Windows, where a
// running program cannot be replaced, the old one is first moved aside
// to exe+".old", which the next Install removes.
func Install(ctx context.Context, client *http.Client, r Release, goos, goarch, exe, userAgent string, keys []ed25519.PublicKey) error {
	name := ArchiveName(r, goos, goarch)
	archiveURL, ok := r.Assets[name]
	if !ok {
		return fmt.Errorf("%s has no build for %s/%s (%s)", r.Tag, goos, goarch, name)
	}
	sumsURL, sigURL := r.Assets["checksums.txt"], r.Assets["checksums.txt.sig"]
	if sumsURL == "" || sigURL == "" {
		return fmt.Errorf("%s has no signed checksums, so it cannot be checked; download it by hand from %s", r.Tag, r.URL)
	}

	sums, err := fetch(ctx, client, sumsURL, userAgent, maxSums)
	if err != nil {
		return err
	}
	sig, err := fetch(ctx, client, sigURL, userAgent, ed25519.SignatureSize+1)
	if err != nil {
		return err
	}
	if err := VerifyChecksums(sums, sig, keys); err != nil {
		return err
	}
	want, err := sumFor(sums, name)
	if err != nil {
		return err
	}
	archive, err := fetch(ctx, client, archiveURL, userAgent, maxArchive)
	if err != nil {
		return err
	}
	if got := sha256.Sum256(archive); hex.EncodeToString(got[:]) != want {
		return fmt.Errorf("%s does not match its checksum: the download may have been tampered with", name)
	}

	binary := "janus"
	if goos == "windows" {
		binary = "janus.exe"
	}
	newBinary, err := unpack(archive, strings.TrimSuffix(strings.TrimSuffix(name, ".tar.gz"), ".zip")+"/"+binary, goos == "windows")
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return replace(exe, newBinary, goos == "windows")
}

// fetch downloads url, refusing an answer longer than limit.
func fetch(ctx context.Context, client *http.Client, url, userAgent string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("downloading %s: GitHub answered %s", path.Base(url), resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("downloading %s: %w", path.Base(url), err)
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("downloading %s: it is larger than any release file should be", path.Base(url))
	}
	return b, nil
}

// sumFor finds the SHA-256 of name in sums, which is in the form
// sha256sum writes: the hex digest, two spaces (or a space and a star),
// and the file name.
func sumFor(sums []byte, name string) (string, error) {
	sc := bufio.NewScanner(bytes.NewReader(sums))
	for sc.Scan() {
		digest, file, ok := strings.Cut(sc.Text(), " ")
		file = strings.TrimPrefix(strings.TrimPrefix(file, " "), "*")
		if ok && file == name && len(digest) == sha256.Size*2 {
			return strings.ToLower(digest), nil
		}
	}
	return "", fmt.Errorf("checksums.txt has no sum for %s", name)
}

// unpack returns the file at member in a .tar.gz archive, or in a .zip if
// isZip is set.
func unpack(archive []byte, member string, isZip bool) ([]byte, error) {
	if isZip {
		zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
		if err != nil {
			return nil, err
		}
		for _, f := range zr.File {
			if f.Name != member {
				continue
			}
			rc, err := f.Open()
			if err != nil {
				return nil, err
			}
			defer rc.Close()
			return readBinary(rc)
		}
		return nil, fmt.Errorf("no %s in it", member)
	}
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("no %s in it", member)
		}
		if err != nil {
			return nil, err
		}
		if h.Name == member && h.Typeflag == tar.TypeReg {
			return readBinary(tr)
		}
	}
}

func readBinary(r io.Reader) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, maxBinary+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxBinary {
		return nil, errors.New("its binary is larger than any release binary should be")
	}
	if len(b) == 0 {
		return nil, errors.New("its binary is empty")
	}
	return b, nil
}

// replace puts binary in place of exe, keeping exe's permissions.
func replace(exe string, binary []byte, windows bool) error {
	info, err := os.Stat(exe)
	if err != nil {
		return err
	}
	dir := filepath.Dir(exe)
	aside := exe + ".old"
	if windows {
		// Left by the last update, once the program it was is no longer
		// running.
		os.Remove(aside)
	}
	tmp, err := os.CreateTemp(dir, ".janus-update-*")
	if err != nil {
		return fmt.Errorf("cannot write beside %s: %w", exe, err)
	}
	defer os.Remove(tmp.Name()) // fails harmlessly once renamed
	_, werr := tmp.Write(binary)
	cerr := tmp.Close()
	if werr != nil || cerr != nil {
		return fmt.Errorf("writing the new binary: %w", errors.Join(werr, cerr))
	}
	if err := os.Chmod(tmp.Name(), info.Mode().Perm()|0o100); err != nil {
		return err
	}
	if windows {
		if err := os.Rename(exe, aside); err != nil {
			return fmt.Errorf("moving the running janus aside: %w", err)
		}
		if err := os.Rename(tmp.Name(), exe); err != nil {
			os.Rename(aside, exe)
			return fmt.Errorf("putting the new binary in place: %w", err)
		}
		return nil
	}
	if err := os.Rename(tmp.Name(), exe); err != nil {
		return fmt.Errorf("putting the new binary in place: %w", err)
	}
	return nil
}
