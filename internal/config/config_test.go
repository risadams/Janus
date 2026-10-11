package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func noEnv(string) string { return "" }

func TestDefaultIsValid(t *testing.T) {
	if err := Default().Validate(); err != nil {
		t.Fatalf("Default().Validate() = %v", err)
	}
}

func TestLoadCreatesStarterFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "janus.config.json")

	cfg, created, err := Load(path)
	if err != nil || !created {
		t.Fatalf("Load() created=%v err=%v, want created with no error", created, err)
	}
	if cfg != Default() {
		t.Errorf("starter config = %+v, want defaults", cfg)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("starter file mode = %o, want 600", perm)
	}

	again, created, err := Load(path)
	if err != nil || created || again != cfg {
		t.Errorf("second Load() = %+v created=%v err=%v, want the same config, not created", again, created, err)
	}
}

func TestLoadKeepsDefaultsForMissingFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.json")
	os.WriteFile(path, []byte(`{"local":{"baseUrl":"http://other.local:9000"}}`), 0o600)

	cfg, _, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Local.BaseURL != "http://other.local:9000" {
		t.Errorf("local.baseUrl = %q, want the file's value", cfg.Local.BaseURL)
	}
	if cfg.Upstream.BaseURL != Default().Upstream.BaseURL || cfg.Listen != Default().Listen || cfg.Logging != Default().Logging {
		t.Errorf("unset fields lost their defaults: %+v", cfg)
	}
}

func TestLoadRejectsUnknownFieldsAndBadJSON(t *testing.T) {
	for name, body := range map[string]string{
		"typo":     `{"upstrem":{"baseUrl":"https://x"}}`,
		"not json": `{"listen":`,
	} {
		path := filepath.Join(t.TempDir(), "c.json")
		os.WriteFile(path, []byte(body), 0o600)
		_, _, err := Load(path)
		if err == nil || !strings.Contains(err.Error(), path) {
			t.Errorf("%s: Load() err = %v, want an error naming the file", name, err)
		}
	}
}

func TestApplyPrecedence(t *testing.T) {
	env := map[string]string{
		"JANUS_LISTEN":       "127.0.0.1:1111",
		"JANUS_UPSTREAM_URL": "https://env.example",
		"JANUS_LOCAL_URL":    "http://env.local:1",
	}
	getenv := func(k string) string { return env[k] }

	got := Apply(Default(), Overrides{Listen: "127.0.0.1:2222", LocalURL: "http://flag.local:2"}, getenv)

	if got.Listen != "127.0.0.1:2222" {
		t.Errorf("Listen = %q, want the flag to beat the environment", got.Listen)
	}
	if got.Upstream.BaseURL != "https://env.example" {
		t.Errorf("Upstream = %q, want the environment to beat the file", got.Upstream.BaseURL)
	}
	if got.Local.BaseURL != "http://flag.local:2" {
		t.Errorf("Local = %q, want the flag", got.Local.BaseURL)
	}
	if untouched := Apply(Default(), Overrides{}, noEnv); untouched != Default() {
		t.Errorf("Apply with nothing set changed the config: %+v", untouched)
	}
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name    string
		edit    func(*Config)
		wantErr string // empty means valid
	}{
		{"localhost is loopback", func(c *Config) { c.Listen = "localhost:8787" }, ""},
		{"ipv6 loopback", func(c *Config) { c.Listen = "[::1]:8787" }, ""},
		{"remote refused by default", func(c *Config) { c.Listen = "0.0.0.0:8787" }, "allowRemote"},
		{"remote allowed when asked", func(c *Config) { c.Listen = "0.0.0.0:8787"; c.AllowRemote = true }, ""},
		{"listen without port", func(c *Config) { c.Listen = "127.0.0.1" }, "host:port"},
		{"upstream missing", func(c *Config) { c.Upstream.BaseURL = "" }, "upstream.baseUrl is not set"},
		{"local not a url", func(c *Config) { c.Local.BaseURL = "studio.tuesday.local:1234" }, "local.baseUrl"},
		{"feature switched on", func(c *Config) { c.Features.Route = true }, "features.route is not available"},
		{"no log dir", func(c *Config) { c.Logging.Dir = "" }, "logging.dir"},
		{"capture off", func(c *Config) { c.Logging.Capture = false }, ""},
		{"no capture limits", func(c *Config) { c.Logging.RetainDays, c.Logging.MaxMegabytes = 0, 0 }, ""},
		{"negative retention", func(c *Config) { c.Logging.RetainDays = -1 }, "logging.retainDays"},
		{"negative size", func(c *Config) { c.Logging.MaxMegabytes = -5 }, "logging.maxMegabytes"},
	}
	for _, tc := range cases {
		cfg := Default()
		tc.edit(&cfg)
		err := cfg.Validate()
		switch {
		case tc.wantErr == "" && err != nil:
			t.Errorf("%s: Validate() = %v, want valid", tc.name, err)
		case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
			t.Errorf("%s: Validate() = %v, want an error containing %q", tc.name, err, tc.wantErr)
		}
	}
}

func TestValidateReportsEveryProblem(t *testing.T) {
	cfg := Default()
	cfg.Listen = "nope"
	cfg.Upstream.BaseURL = ""
	cfg.Features.Guard = true

	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate() = nil, want three problems")
	}
	for _, want := range []string{"listen", "upstream.baseUrl", "features.guard"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Validate() = %v, missing %q", err, want)
		}
	}
}
