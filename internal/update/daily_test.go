package update

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// counting is a releases API that answers status and body and counts how
// often it is asked.
func counting(t *testing.T, status int, body string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var asked atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &asked
}

const oneRelease = `[{"tag_name":"v1.2.0","html_url":"https://example.invalid/v1.2.0"}]`

func TestDailyAsksAtMostOnceADay(t *testing.T) {
	srv, asked := counting(t, 200, oneRelease)
	path := filepath.Join(t.TempDir(), CacheFile)
	start := time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)

	for _, at := range []time.Time{start, start.Add(time.Hour), start.Add(23 * time.Hour)} {
		r, found, err := Daily(context.Background(), srv.Client(), srv.URL, "janus/test", false, path, at)
		if err != nil || !found || r.Tag != "v1.2.0" || r.URL != "https://example.invalid/v1.2.0" {
			t.Fatalf("at %s: Daily = %+v, %v, %v", at, r, found, err)
		}
	}
	if n := asked.Load(); n != 1 {
		t.Errorf("asked GitHub %d times within a day, want 1", n)
	}
	Daily(context.Background(), srv.Client(), srv.URL, "janus/test", false, path, start.Add(25*time.Hour))
	if n := asked.Load(); n != 2 {
		t.Errorf("asked GitHub %d times after a day had passed, want 2", n)
	}
}

func TestDailyAsksAgainWhenPreReleasesAreTurnedOn(t *testing.T) {
	srv, asked := counting(t, 200, oneRelease)
	path := filepath.Join(t.TempDir(), CacheFile)
	now := time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)
	Daily(context.Background(), srv.Client(), srv.URL, "janus/test", false, path, now)
	Daily(context.Background(), srv.Client(), srv.URL, "janus/test", true, path, now)
	if n := asked.Load(); n != 2 {
		t.Errorf("asked GitHub %d times, want 2: the cached answer left pre-releases out", n)
	}
}

func TestDailyRemembersAFailedCheck(t *testing.T) {
	srv, asked := counting(t, 500, "")
	path := filepath.Join(t.TempDir(), CacheFile)
	now := time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)
	if _, _, err := Daily(context.Background(), srv.Client(), srv.URL, "janus/test", false, path, now); err == nil {
		t.Fatal("a failed check was not reported")
	}
	_, found, err := Daily(context.Background(), srv.Client(), srv.URL, "janus/test", false, path, now.Add(time.Hour))
	if found || err != nil {
		t.Errorf("a remembered failure = %v, %v; want nothing found and no error", found, err)
	}
	if n := asked.Load(); n != 1 {
		t.Errorf("asked GitHub %d times after a failure, want 1", n)
	}
}

func TestDailyDoesNotTrustACheckFromTheFuture(t *testing.T) {
	srv, asked := counting(t, 200, oneRelease)
	path := filepath.Join(t.TempDir(), CacheFile)
	now := time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)
	Daily(context.Background(), srv.Client(), srv.URL, "janus/test", false, path, now.Add(48*time.Hour))
	Daily(context.Background(), srv.Client(), srv.URL, "janus/test", false, path, now)
	if n := asked.Load(); n != 2 {
		t.Errorf("asked GitHub %d times, want 2", n)
	}
}

func TestDailyWorksWithoutAFolderForItsCache(t *testing.T) {
	srv, _ := counting(t, 200, oneRelease)
	path := filepath.Join(t.TempDir(), "missing", CacheFile)
	_, found, err := Daily(context.Background(), srv.Client(), srv.URL, "janus/test", false, path, time.Now())
	if !found || err != nil {
		t.Errorf("Daily with nowhere to keep its cache = %v, %v", found, err)
	}
}
