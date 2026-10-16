package update

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func mustParse(t *testing.T, s string) Version {
	t.Helper()
	v, ok := Parse(s)
	if !ok {
		t.Fatalf("Parse(%q) failed", s)
	}
	return v
}

// The order is semver.org §11's own example, with Janus's tags around it.
func TestVersionsCompareBySemver(t *testing.T) {
	ordered := []string{
		"v0.9.0-rc.2", "v0.9.0-rc.9", "v0.9.0-rc.12", "v0.9.0",
		"1.0.0-alpha", "1.0.0-alpha.1", "1.0.0-alpha.beta", "1.0.0-beta",
		"1.0.0-beta.2", "1.0.0-beta.11", "1.0.0-rc.1", "1.0.0", "v1.0.1", "v1.10.0", "v2.0.0",
	}
	for i := range ordered {
		for j := range ordered {
			got := mustParse(t, ordered[i]).Compare(mustParse(t, ordered[j]))
			want := sign(i - j)
			if got != want {
				t.Errorf("Compare(%s, %s) = %d, want %d", ordered[i], ordered[j], got, want)
			}
		}
	}
}

func TestBuildMetadataIsIgnored(t *testing.T) {
	if mustParse(t, "v1.2.3+abc").Compare(mustParse(t, "1.2.3")) != 0 {
		t.Error("build metadata changed the order")
	}
}

func TestParseRefusesWhatIsNotAVersion(t *testing.T) {
	for _, s := range []string{"", "v1", "v1.2", "v1.2.3.4", "va.b.c", "v1.2.+3", "v1.2.3-", "v1.2.3-rc..1", "v1.2.3-rc_1", "nightly", "v-1.2.3"} {
		if _, ok := Parse(s); ok {
			t.Errorf("Parse(%q) accepted it", s)
		}
	}
}

func releases(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("User-Agent"); got != "janus/test" {
			t.Errorf("User-Agent = %q, want janus/test", got)
		}
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Error("the check sent credentials")
		}
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// GitHub lists by creation date, not version, so a fix to an older line
// can come first; the newest version still wins.
const listed = `[
 {"tag_name":"v0.9.1","html_url":"https://example.test/v0.9.1","prerelease":false},
 {"tag_name":"v1.0.0-rc.12","html_url":"https://example.test/rc12","prerelease":true},
 {"tag_name":"v1.0.0-rc.9","html_url":"https://example.test/rc9","prerelease":true},
 {"tag_name":"v2.0.0","html_url":"https://example.test/draft","draft":true},
 {"tag_name":"nightly","html_url":"https://example.test/nightly"},
 {"tag_name":"v1.0.0-rc.13","html_url":"https://example.test/rc13","prerelease":false},
 {"tag_name":"v0.9.0","html_url":"https://example.test/v0.9.0","prerelease":false}
]`

func TestLatestTakesStableReleasesUnlessAskedForPreReleases(t *testing.T) {
	srv := releases(t, 200, listed)
	for _, tc := range []struct {
		pre  bool
		want string
	}{
		// rc.13 is not flagged as a pre-release, but its tag says it is one.
		{false, "v0.9.1"},
		{true, "v1.0.0-rc.13"},
	} {
		r, found, err := Latest(context.Background(), srv.Client(), srv.URL, "janus/test", tc.pre)
		if err != nil || !found {
			t.Fatalf("pre=%v: found=%v err=%v", tc.pre, found, err)
		}
		if r.Tag != tc.want {
			t.Errorf("pre=%v: newest = %s, want %s", tc.pre, r.Tag, tc.want)
		}
	}
}

func TestLatestFindsNoStableReleaseAmongPreReleases(t *testing.T) {
	srv := releases(t, 200, `[{"tag_name":"v0.9.0-rc.12","prerelease":true}]`)
	_, found, err := Latest(context.Background(), srv.Client(), srv.URL, "janus/test", false)
	if err != nil || found {
		t.Errorf("found=%v err=%v, want nothing found and no error", found, err)
	}
}

func TestLatestExplainsARateLimit(t *testing.T) {
	srv := releases(t, 403, `{"message":"API rate limit exceeded"}`)
	_, _, err := Latest(context.Background(), srv.Client(), srv.URL, "janus/test", false)
	if err == nil || !strings.Contains(err.Error(), "rate limit") {
		t.Errorf("err = %v, want it to name the rate limit", err)
	}
}

func TestLatestRefusesAnAnswerThatIsNotAList(t *testing.T) {
	srv := releases(t, 200, `<html>a captive portal</html>`)
	if _, _, err := Latest(context.Background(), srv.Client(), srv.URL, "janus/test", false); err == nil {
		t.Error("a page that is not JSON was taken as a list of releases")
	}
}

// Tag names come from the network, so Parse and Compare must hold up on
// anything: never panic, and order consistently.
func FuzzCompare(f *testing.F) {
	f.Add("v1.0.0-rc.1", "1.0.0")
	f.Add("v0.9.0-rc.12", "v0.9.0-rc.9")
	f.Add("1.0.0-alpha.beta", "1.0.0-alpha.1")
	f.Add("v99999999999999999999.0.0", "1.2.3+build")
	f.Fuzz(func(t *testing.T, a, b string) {
		va, okA := Parse(a)
		vb, okB := Parse(b)
		if !okA || !okB {
			return
		}
		if va.Compare(va) != 0 {
			t.Errorf("%q is not equal to itself", a)
		}
		if va.Compare(vb) != -vb.Compare(va) {
			t.Errorf("Compare(%q, %q) and its reverse disagree", a, b)
		}
	})
}
