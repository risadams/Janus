package main

import "testing"

func TestVersionLine(t *testing.T) {
	if got, want := versionLine(), "janus dev"; got != want {
		t.Errorf("versionLine() = %q, want %q", got, want)
	}
}
