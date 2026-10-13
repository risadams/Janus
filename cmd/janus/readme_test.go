package main

import (
	"os"
	"regexp"
	"testing"
)

// releaseNamed matches a specific release of Janus: a version such as
// v0.9.0-rc.6, a versioned archive, or a download link for one tag.
var releaseNamed = regexp.MustCompile(`\bv\d+\.\d+\.\d+|janus_\d+\.\d+|/releases/download/v`)

// The README's install steps find the newest release themselves, so that a
// release needs no change to the README (#134). This keeps a specific
// version from creeping back in.
func TestReadmeNamesNoRelease(t *testing.T) {
	data, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range releaseNamed.FindAllString(string(data), -1) {
		t.Errorf("README.md names a specific release (%q); point it at the newest release instead", m)
	}
}

func TestReleaseNamedPattern(t *testing.T) {
	for s, want := range map[string]bool{
		"v0.9.0-rc.6": true, "janus_0.9.0-rc.6_darwin_arm64.tar.gz": true, "/releases/download/v1.0.0/": true,
		"127.0.0.1:8787": false, "Go 1.27": false, "janus_*_darwin_arm64": false, "@latest": false,
	} {
		if got := releaseNamed.MatchString(s); got != want {
			t.Errorf("releaseNamed.MatchString(%q) = %v, want %v", s, got, want)
		}
	}
}
