package config

import (
	"os"
	"testing"
)

// TestMain gives the tests a home of their own, so that the default log
// folder, which is under the user's home (config.StateDir), is never the
// real one.
func TestMain(m *testing.M) {
	// A test binary run by a test, as the stand-in for Claude Code is,
	// keeps the home it was given.
	if os.Getenv("JANUS_TEST_HOME") != "" {
		os.Exit(m.Run())
	}
	home, err := os.MkdirTemp("", "janus-test-home-")
	if err != nil {
		panic(err)
	}
	for _, k := range []string{"HOME", "USERPROFILE", "XDG_STATE_HOME", "LocalAppData"} {
		os.Unsetenv(k)
	}
	os.Setenv("HOME", home)
	os.Setenv("USERPROFILE", home)
	os.Setenv("JANUS_TEST_HOME", home)
	code := m.Run()
	os.RemoveAll(home)
	os.Exit(code)
}
