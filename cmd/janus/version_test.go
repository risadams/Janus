package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/risadams/janus/internal/update"
)

// A list of releases in GitHub's shape: a stable v0.8.0, and pre-releases
// of v0.9.0 up to rc.12.
const releasesJSON = `[
 {"tag_name":"v0.9.0-rc.12","html_url":"https://github.com/risadams/Janus/releases/tag/v0.9.0-rc.12","prerelease":true},
 {"tag_name":"v0.9.0-rc.9","html_url":"https://github.com/risadams/Janus/releases/tag/v0.9.0-rc.9","prerelease":true},
 {"tag_name":"v0.8.0","html_url":"https://github.com/risadams/Janus/releases/tag/v0.8.0","prerelease":false}
]`

// releaseBuild makes this test binary a release build of v, with GitHub
// replaced by a server that answers body.
func releaseBuild(t *testing.T, v, body string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	oldVersion, oldAPI := version, releasesAPI
	version, releasesAPI = v, srv.URL
	t.Cleanup(func() { version, releasesAPI = oldVersion, oldAPI })
}

func runCLI(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = run(args, &out, &errOut)
	return out.String(), errOut.String(), code
}

// A config of its own, so a check never reads one from the folder the
// tests run in.
func noConfig(t *testing.T) string {
	return filepath.Join(t.TempDir(), "janus.config.json")
}

func TestVersionCheckOffersOnlyStableReleasesByDefault(t *testing.T) {
	releaseBuild(t, "v0.7.0", releasesJSON)
	out, _, code := runCLI(t, "version", "-check", "-config", noConfig(t))
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	wantLines(t, out, "janus v0.7.0", "v0.8.0 is out.", "To upgrade: janus update", "download it from https://github.com/risadams/Janus/releases/tag/v0.8.0")
	if strings.Contains(out, "rc.12") {
		t.Errorf("a pre-release was offered without -pre:\n%s", out)
	}
}

func TestVersionCheckPreOffersTheNewestPreRelease(t *testing.T) {
	releaseBuild(t, "v0.9.0-rc.9", releasesJSON)
	out, _, _ := runCLI(t, "version", "-check", "-pre", "-config", noConfig(t))
	wantLines(t, out, "v0.9.0-rc.12 is out.")
}

func TestVersionCheckTakesPreReleasesFromTheConfig(t *testing.T) {
	releaseBuild(t, "v0.9.0-rc.9", releasesJSON)
	path := noConfig(t)
	os.WriteFile(path, []byte(`{"update":{"prerelease":true}}`), 0o600)
	out, _, _ := runCLI(t, "version", "-check", "-config", path)
	wantLines(t, out, "v0.9.0-rc.12 is out.")
}

// rc.9 is newer than any stable release, so without -pre there is nothing
// to upgrade to.
func TestVersionCheckDoesNotOfferAnOlderStableRelease(t *testing.T) {
	releaseBuild(t, "v0.9.0-rc.9", releasesJSON)
	out, _, _ := runCLI(t, "version", "-check", "-config", noConfig(t))
	wantLines(t, out, "This is the newest release.")
}

func TestVersionCheckSaysWhenThereIsNoStableReleaseYet(t *testing.T) {
	releaseBuild(t, "v0.9.0-rc.9", `[{"tag_name":"v0.9.0-rc.12","prerelease":true}]`)
	out, _, _ := runCLI(t, "version", "-check", "-config", noConfig(t))
	wantLines(t, out, "No stable release has been published yet.", "-pre counts pre-releases too.")
}

func TestVersionCheckOfflineIsExplained(t *testing.T) {
	_, errOut, code := runCLI(t, "version", "-check", "-config", noConfig(t))
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.HasPrefix(errOut, "janus: could not check for a newer release:") {
		t.Errorf("stderr = %q", errOut)
	}
}

func TestVersionWithoutCheckSendsNothing(t *testing.T) {
	asked := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { asked = true }))
	defer srv.Close()
	old := releasesAPI
	releasesAPI = srv.URL
	defer func() { releasesAPI = old }()
	out, _, code := runCLI(t, "version")
	if code != 0 || out != "janus dev\n" {
		t.Errorf("janus version = %q, exit %d", out, code)
	}
	if asked {
		t.Error("janus version asked for releases without -check")
	}
}

func TestUpgradeAdviceFitsHowJanusWasInstalled(t *testing.T) {
	releaseBuild(t, "v0.7.0", releasesJSON)
	for how, want := range map[install]string{
		archive:   "To upgrade: janus update",
		goInstall: "go install github.com/risadams/janus/cmd/janus@v0.8.0",
		homebrew:  "brew upgrade janus",
	} {
		r := release(t, "v0.8.0", "https://github.com/risadams/Janus/releases/tag/v0.8.0")
		if got := upgradeHow(how, r); !strings.Contains(got, want) {
			t.Errorf("install %d: %q, want it to contain %q", how, got, want)
		}
	}
}

func TestHomebrewIsRecognisedByItsCellar(t *testing.T) {
	if !isHomebrew("/opt/homebrew/Cellar/janus/1.0.0/bin/janus") {
		t.Error("a binary in the cellar was not taken for Homebrew's")
	}
	if isHomebrew("/usr/local/bin/janus") {
		t.Error("a binary outside the cellar was taken for Homebrew's")
	}
}

func TestDoctorSaysWhenANewerReleaseIsOut(t *testing.T) {
	releaseBuild(t, "v0.7.0", releasesJSON)
	up := serverSaying(200, "Anthropic API")
	defer up.Close()
	lms := serverSaying(200, lmStudioModels)
	defer lms.Close()
	out, code, _ := doctor(t, up.URL, lms.URL, nil)
	if code != 0 {
		t.Errorf("exit code = %d, want 0: a newer release is worth knowing, not a fault", code)
	}
	wantLines(t, out, "warn update v0.8.0 is out.")

	out, _, _ = doctor(t, up.URL, lms.URL, map[string]any{"update": map[string]any{"prerelease": true}})
	wantLines(t, out, "warn update v0.9.0-rc.12 is out.")
}

func TestDoctorCannotCheckForUpdatesIsOnlyAWarning(t *testing.T) {
	old := version
	version = "v0.7.0"
	defer func() { version = old }()
	up := serverSaying(200, "Anthropic API")
	defer up.Close()
	lms := serverSaying(200, lmStudioModels)
	defer lms.Close()
	out, code, _ := doctor(t, up.URL, lms.URL, nil)
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	wantLines(t, out, "warn update could not check for a newer release:")
}

func release(t *testing.T, tag, url string) update.Release {
	t.Helper()
	v, ok := update.Parse(tag)
	if !ok {
		t.Fatalf("%q is not a version", tag)
	}
	return update.Release{Version: v, Tag: tag, URL: url}
}
