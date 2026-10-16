package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/risadams/janus/internal/update"
)

// A release of v0.8.0 for linux/amd64, served the way GitHub serves one.
type releaseServer struct {
	files map[string][]byte
	priv  ed25519.PrivateKey
	key   ed25519.PublicKey
}

const updateArchive = "janus_0.8.0_linux_amd64.tar.gz"

// updateFrom makes this test binary a release build of v0.7.0, at a file
// of its own, with GitHub replaced by a server publishing v0.8.0, and
// returns the server's files, so a test can spoil one, and the binary.
func updateFrom(t *testing.T) (*releaseServer, string) {
	t.Helper()
	rs := &releaseServer{files: map[string][]byte{}}
	rs.key, rs.priv, _ = ed25519.GenerateKey(nil)
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	body := []byte("janus v0.8.0")
	tw.WriteHeader(&tar.Header{Name: "janus_0.8.0_linux_amd64/janus", Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg})
	tw.Write(body)
	tw.Close()
	gz.Close()
	rs.files[updateArchive] = buf.Bytes()
	sum := sha256.Sum256(buf.Bytes())
	sums := hex.EncodeToString(sum[:]) + "  " + updateArchive + "\n"
	rs.files["checksums.txt"] = []byte(sums)
	rs.files["checksums.txt.sig"] = ed25519.Sign(rs.priv, []byte(sums))

	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/releases" {
			var assets []map[string]string
			for name := range rs.files {
				assets = append(assets, map[string]string{"name": name, "browser_download_url": srv.URL + "/" + name})
			}
			json.NewEncoder(w).Encode([]map[string]any{{"tag_name": "v0.8.0", "html_url": "https://example.invalid/v0.8.0", "assets": assets}})
			return
		}
		b, ok := rs.files[strings.TrimPrefix(r.URL.Path, "/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(b)
	}))
	t.Cleanup(srv.Close)

	// Resolved, as janus service install and janus update both resolve
	// the binary's path: on macOS the temporary folder is behind a link.
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	exe := filepath.Join(dir, "janus")
	os.WriteFile(exe, []byte("janus v0.7.0"), 0o755)
	home := t.TempDir()
	old := []any{version, releasesAPI, releaseKeys, executable, updateService, updatePlatform}
	version, releasesAPI = "v0.7.0", srv.URL+"/releases"
	releaseKeys = func() ([]ed25519.PublicKey, error) { return []ed25519.PublicKey{rs.key}, nil }
	executable = func() (string, error) { return exe, nil }
	updateService = func() (serviceHost, error) {
		return serviceHost{goos: "linux", home: home, exe: exe, run: func(string, ...string) (string, error) { return "", nil }}, nil
	}
	updatePlatform = func() (string, string) { return "linux", "amd64" }
	t.Cleanup(func() {
		version, releasesAPI = old[0].(string), old[1].(string)
		releaseKeys = old[2].(func() ([]ed25519.PublicKey, error))
		executable = old[3].(func() (string, error))
		updateService = old[4].(func() (serviceHost, error))
		updatePlatform = old[5].(func() (string, string))
	})
	return rs, exe
}

func binaryIs(t *testing.T, exe, want string) {
	t.Helper()
	if b, _ := os.ReadFile(exe); string(b) != want {
		t.Errorf("the binary is %q, want %q", b, want)
	}
}

func TestUpdateReplacesTheBinaryWithACheckedRelease(t *testing.T) {
	_, exe := updateFrom(t)
	out, errOut, code := runCLI(t, "update", "-config", noConfig(t))
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	binaryIs(t, exe, "janus v0.8.0")
	wantLines(t, out, "Checked its signature and checksum", "janus v0.7.0 is now v0.8.0", "Restart janus to run the new version.")
}

func TestUpdateLeavesTheBinaryOnABadChecksum(t *testing.T) {
	rs, exe := updateFrom(t)
	rs.files[updateArchive] = append(rs.files[updateArchive], 0) // not what was summed
	_, errOut, code := runCLI(t, "update", "-config", noConfig(t))
	if code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
	binaryIs(t, exe, "janus v0.7.0")
	wantLines(t, errOut, "does not match its checksum", "Nothing was changed")
}

func TestUpdateLeavesTheBinaryOnABadSignature(t *testing.T) {
	rs, exe := updateFrom(t)
	_, other, _ := ed25519.GenerateKey(nil)
	rs.files["checksums.txt.sig"] = ed25519.Sign(other, rs.files["checksums.txt"])
	_, errOut, code := runCLI(t, "update", "-config", noConfig(t))
	if code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
	binaryIs(t, exe, "janus v0.7.0")
	wantLines(t, errOut, "not signed by a Janus release key", "Nothing was changed")
}

func TestUpdateDoesNothingOnTheNewestRelease(t *testing.T) {
	_, exe := updateFrom(t)
	version = "v0.8.0"
	out, _, code := runCLI(t, "update", "-config", noConfig(t))
	if code != 0 {
		t.Errorf("exit %d", code)
	}
	binaryIs(t, exe, "janus v0.7.0")
	wantLines(t, out, "janus v0.8.0 is the newest release.")
}

func TestUpdateRefusesADevelopmentBuild(t *testing.T) {
	_, exe := updateFrom(t)
	version = "dev"
	_, errOut, code := runCLI(t, "update", "-config", noConfig(t))
	if code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
	binaryIs(t, exe, "janus v0.7.0")
	wantLines(t, errOut, "development build")
}

func TestUpdateRefusesHomebrewsCopy(t *testing.T) {
	updateFrom(t)
	cellar := filepath.Join(t.TempDir(), "Cellar", "janus", "0.7.0", "bin")
	os.MkdirAll(cellar, 0o755)
	exe := filepath.Join(cellar, "janus")
	os.WriteFile(exe, []byte("janus v0.7.0"), 0o755)
	executable = func() (string, error) { return exe, nil }
	_, errOut, code := runCLI(t, "update", "-config", noConfig(t))
	if code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
	binaryIs(t, exe, "janus v0.7.0")
	wantLines(t, errOut, "installed with Homebrew", "brew upgrade janus")
}

// The exact release, not @latest: the Go module proxy can be slow to list
// a new tag, and @latest leaves pre-releases out once there is a stable
// release, which would drop -pre.
func TestUpdateNamesTheExactReleaseForAGoInstall(t *testing.T) {
	r := release(t, "v0.9.0-rc.16", "https://example.invalid/v0.9.0-rc.16")
	if why := updateRefusal(goInstall, r); !strings.Contains(why, "go install github.com/risadams/janus/cmd/janus@v0.9.0-rc.16") {
		t.Errorf("refusal = %q", why)
	}
	if why := updateRefusal(archive, r); why != "" {
		t.Errorf("a release archive was refused: %q", why)
	}
}

func TestUpdateRestartsTheServiceThatRunsIt(t *testing.T) {
	for _, goos := range []string{"linux", "darwin"} {
		t.Run(goos, func(t *testing.T) {
			_, exe := updateFrom(t)
			var ran []string
			host := serviceHost{goos: goos, home: t.TempDir(), uid: 501, exe: exe,
				run: func(name string, args ...string) (string, error) {
					ran = append(ran, name+" "+strings.Join(args, " "))
					return "", nil
				}}
			unit := systemdUnit(exe, "/etc/janus.json")
			if goos == "darwin" {
				unit = launchdPlist(exe, "/etc/janus.json", "/tmp/janus.log")
			}
			os.MkdirAll(filepath.Dir(unitPath(host)), 0o755)
			os.WriteFile(unitPath(host), []byte(unit), 0o644)
			updateService = func() (serviceHost, error) { return host, nil }

			out, errOut, code := runCLI(t, "update", "-config", noConfig(t))
			if code != 0 {
				t.Fatalf("exit %d: %s", code, errOut)
			}
			want := "systemctl --user restart janus.service"
			if goos == "darwin" {
				want = "launchctl kill SIGTERM gui/501/" + serviceLabel
			}
			if len(ran) != 1 || ran[0] != want {
				t.Errorf("ran %q, want %q", ran, want)
			}
			wantLines(t, out, "Restarting the janus service")
		})
	}
}

func TestUpdateLeavesAServiceForAnotherBinaryAlone(t *testing.T) {
	_, exe := updateFrom(t)
	ran := false
	host := serviceHost{goos: "linux", home: t.TempDir(), exe: exe,
		run: func(string, ...string) (string, error) { ran = true; return "", nil }}
	os.MkdirAll(filepath.Dir(unitPath(host)), 0o755)
	os.WriteFile(unitPath(host), []byte(systemdUnit("/usr/local/bin/janus", "/etc/janus.json")), 0o644)
	updateService = func() (serviceHost, error) { return host, nil }
	out, _, _ := runCLI(t, "update", "-config", noConfig(t))
	if ran {
		t.Error("restarted a service that runs another janus")
	}
	wantLines(t, out, "Restart janus to run the new version.")
}

func TestUpdateTellsAGoInstallTheExactReleaseToFetch(t *testing.T) {
	_, exe := updateFrom(t)
	version = "dev"
	old := readBuildInfo
	readBuildInfo = func() (*debug.BuildInfo, bool) {
		return &debug.BuildInfo{Main: debug.Module{Path: "github.com/risadams/janus", Version: "v0.7.0"}}, true
	}
	t.Cleanup(func() { readBuildInfo = old })

	_, errOut, code := runCLI(t, "update", "-config", noConfig(t))
	if code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
	binaryIs(t, exe, "janus v0.7.0")
	wantLines(t, errOut, "installed with go install", "go install github.com/risadams/janus/cmd/janus@v0.8.0")

	// Already the newest: nothing to refuse.
	readBuildInfo = func() (*debug.BuildInfo, bool) {
		return &debug.BuildInfo{Main: debug.Module{Path: "github.com/risadams/janus", Version: "v0.8.0"}}, true
	}
	out, _, code := runCLI(t, "update", "-config", noConfig(t))
	if code != 0 {
		t.Errorf("exit %d on the newest release, want 0", code)
	}
	wantLines(t, out, "janus v0.8.0 is the newest release.")
}

// Install is given the release Latest found, assets and all.
func TestUpdateFindsTheReleasesFiles(t *testing.T) {
	updateFrom(t)
	r, found, err := update.Latest(t.Context(), http.DefaultClient, releasesAPI, "janus/test", false)
	if err != nil || !found || r.Assets[updateArchive] == "" || r.Assets["checksums.txt.sig"] == "" {
		t.Errorf("Latest = %+v, %v, %v", r, found, err)
	}
}
