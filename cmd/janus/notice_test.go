package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/risadams/janus/internal/config"
	"github.com/risadams/janus/internal/update"
)

// noticeFrom makes this test binary a release build of v, with GitHub
// replaced by a server that answers releasesJSON and counts how often it
// is asked, and returns a config whose log folder is the test's own.
func noticeFrom(t *testing.T, v string) (config.Config, *atomic.Int32) {
	t.Helper()
	var asked atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		w.Write([]byte(releasesJSON))
	}))
	t.Cleanup(srv.Close)
	oldVersion, oldAPI := version, releasesAPI
	version, releasesAPI = v, srv.URL
	t.Cleanup(func() { version, releasesAPI = oldVersion, oldAPI })
	var cfg config.Config
	cfg.Logging.Dir = t.TempDir()
	return cfg, &asked
}

// notices runs the startup check and returns the lines it said.
func notices(cfg config.Config) []string {
	var said []string
	noticeUpdate(context.Background(), cfg, http.DefaultClient, func(line string) { said = append(said, line) })
	return said
}

func TestUpdateNoticeSendsNothingWhenOff(t *testing.T) {
	cfg, asked := noticeFrom(t, "v0.7.0")
	if said := notices(cfg); len(said) != 0 {
		t.Errorf("the notice spoke while off: %q", said)
	}
	if n := asked.Load(); n != 0 {
		t.Errorf("asked GitHub %d times with update.check off, want 0", n)
	}
	if _, err := os.Stat(filepath.Join(cfg.Logging.Dir, update.CacheFile)); err == nil {
		t.Error("a cache was written with update.check off")
	}
}

func TestUpdateNoticeNamesANewerReleaseOnceADay(t *testing.T) {
	cfg, asked := noticeFrom(t, "v0.7.0")
	cfg.Update.Check = true
	for range 3 {
		said := notices(cfg)
		if len(said) != 1 || said[0] != "v0.8.0 is out; janus version -check says how to upgrade." {
			t.Fatalf("notice = %q", said)
		}
	}
	if n := asked.Load(); n != 1 {
		t.Errorf("asked GitHub %d times in three starts, want 1", n)
	}
}

func TestUpdateNoticeCountsPreReleasesWhenAsked(t *testing.T) {
	cfg, _ := noticeFrom(t, "v0.7.0")
	cfg.Update.Check, cfg.Update.Prerelease = true, true
	if said := notices(cfg); len(said) != 1 || said[0] != "v0.9.0-rc.12 is out; janus version -check says how to upgrade." {
		t.Errorf("notice = %q", said)
	}
}

func TestUpdateNoticeIsQuietWhenUpToDate(t *testing.T) {
	cfg, _ := noticeFrom(t, "v0.8.0")
	cfg.Update.Check = true
	if said := notices(cfg); len(said) != 0 {
		t.Errorf("notice on the newest release = %q", said)
	}
}

func TestUpdateNoticeIsQuietOffline(t *testing.T) {
	cfg, _ := noticeFrom(t, "v0.7.0")
	releasesAPI = "http://127.0.0.1:1/releases"
	cfg.Update.Check = true
	if said := notices(cfg); len(said) != 0 {
		t.Errorf("notice offline = %q", said)
	}
}

func TestUpdateNoticeAsksNothingForADevelopmentBuild(t *testing.T) {
	cfg, asked := noticeFrom(t, "dev")
	cfg.Update.Check = true
	notices(cfg)
	if n := asked.Load(); n != 0 {
		t.Errorf("a development build asked GitHub %d times", n)
	}
}
