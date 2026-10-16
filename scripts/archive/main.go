// Command archive packs a folder into a release archive that is the same
// bytes wherever and whenever it is made, so that anyone can rebuild a
// release from its tag and compare (#264, ADR-004). tar, gzip, and zip
// write file times, owners, and the order a folder happens to list in,
// and macOS's tar writes differently from GNU tar; this writes none of
// that.
//
//	go run ./scripts/archive -o dist/janus_1.2.3_linux_amd64.tar.gz dist/janus_1.2.3_linux_amd64
//
// The archive holds the folder itself, as its first path element, then
// its files in sorted order. Every entry is dated SOURCE_DATE_EPOCH (the
// tagged commit's time, set by release.sh), owned by no one, and given
// mode 0755 if it is executable and 0644 if not. A name ending in .zip
// makes a zip; anything else, a gzipped tar.
package main

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

func main() {
	out := flag.String("o", "", "the archive to write")
	flag.Parse()
	if *out == "" || flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: archive -o file.tar.gz|file.zip folder")
		os.Exit(2)
	}
	epoch, err := strconv.ParseInt(os.Getenv("SOURCE_DATE_EPOCH"), 10, 64)
	if err != nil {
		fmt.Fprintf(os.Stderr, "archive: SOURCE_DATE_EPOCH must be set to a time in seconds, such as git log -1 --format=%%ct gives\n")
		os.Exit(2)
	}
	if err := pack(*out, flag.Arg(0), time.Unix(epoch, 0).UTC()); err != nil {
		fmt.Fprintf(os.Stderr, "archive: %v\n", err)
		os.Exit(1)
	}
}

// entry is one file to pack: its name in the archive, where it is on
// disk, and whether it is executable.
type entry struct {
	name, path string
	exec       bool
}

func pack(out, dir string, at time.Time) error {
	base := filepath.Base(dir)
	var entries []entry
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("%s is not a regular file", path)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		// Windows has no execute bit, so a .exe counts as executable by
		// its name, and a file built on Windows packs as it would on Unix.
		exec := info.Mode().Perm()&0o111 != 0 || strings.HasSuffix(path, ".exe")
		entries = append(entries, entry{name: base + "/" + filepath.ToSlash(rel), path: path, exec: exec})
		return nil
	})
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].name < entries[j].name })

	f, err := os.Create(out)
	if err != nil {
		return err
	}
	if strings.HasSuffix(out, ".zip") {
		err = writeZip(f, base, entries, at)
	} else {
		err = writeTarGz(f, base, entries, at)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(out)
	}
	return err
}

func mode(e entry) int64 {
	if e.exec {
		return 0o755
	}
	return 0o644
}

func writeTarGz(w io.Writer, base string, entries []entry, at time.Time) error {
	// No name and no time in the gzip header, which gzip would otherwise
	// fill from the file.
	gz, err := gzip.NewWriterLevel(w, gzip.BestCompression)
	if err != nil {
		return err
	}
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: base + "/", Mode: 0o755, ModTime: at, Format: tar.FormatUSTAR}); err != nil {
		return err
	}
	for _, e := range entries {
		b, err := os.ReadFile(e.path)
		if err != nil {
			return err
		}
		h := &tar.Header{Typeflag: tar.TypeReg, Name: e.name, Mode: mode(e), Size: int64(len(b)), ModTime: at, Format: tar.FormatUSTAR}
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		if _, err := tw.Write(b); err != nil {
			return err
		}
	}
	return errors.Join(tw.Close(), gz.Close())
}

func writeZip(w io.Writer, base string, entries []entry, at time.Time) error {
	zw := zip.NewWriter(w)
	dir := &zip.FileHeader{Name: base + "/", Modified: at}
	dir.SetMode(fs.ModeDir | 0o755)
	if _, err := zw.CreateHeader(dir); err != nil {
		return err
	}
	for _, e := range entries {
		b, err := os.ReadFile(e.path)
		if err != nil {
			return err
		}
		h := &zip.FileHeader{Name: e.name, Method: zip.Deflate, Modified: at}
		h.SetMode(fs.FileMode(mode(e)))
		fw, err := zw.CreateHeader(h)
		if err != nil {
			return err
		}
		if _, err := fw.Write(b); err != nil {
			return err
		}
	}
	return zw.Close()
}
