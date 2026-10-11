package config

import (
	"os"
	"path/filepath"
	"reflect"
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
	if !reflect.DeepEqual(cfg, Default()) {
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
	if err != nil || created || !reflect.DeepEqual(again, cfg) {
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
	if untouched := Apply(Default(), Overrides{}, noEnv); !reflect.DeepEqual(untouched, Default()) {
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
		{"local not a url", func(c *Config) { c.Local.BaseURL = "studio.local:1234" }, "local.baseUrl"},
		{"unbuilt feature switched on", func(c *Config) { c.Features.Compress = true }, "features.compress is not available"},
		{"guard on", func(c *Config) { c.Features.Guard = true }, ""},
		{"guard with patterns", func(c *Config) {
			c.Features.Guard = true
			c.Guard = Guard{Patterns: []string{`ACME-[0-9]{6}`}, Allow: []string{"AKIAEXAMPLE"}, Entropy: true}
		}, ""},
		{"a guard pattern that does not compile", func(c *Config) { c.Guard.Patterns = []string{"ok", "(unclosed"} }, "guard.patterns[1] is not a valid regular expression"},
		{"route with nothing to route to", func(c *Config) { c.Features.Route = true }, "local.models is empty"},
		{"route with a local model", func(c *Config) {
			c.Features.Route = true
			c.Local.Models = map[string]LocalModel{"janus-local": {Model: "qwen/qwen3.8-27b"}}
		}, ""},
		{"local models without route", func(c *Config) { c.Local.Models = map[string]LocalModel{"janus-local": {Model: "q"}} }, ""},
		{"a rule", func(c *Config) {
			c.Local.Models = map[string]LocalModel{"janus-local": {Model: "q"}}
			c.Routing = []Rule{{Name: "subagents", Class: "subagent", MaxTokens: 40000, Route: "local", Local: "janus-local"}, {Route: "upstream"}}
		}, ""},
		{"a shadow rule", func(c *Config) {
			c.Local.Models = map[string]LocalModel{"janus-local": {Model: "q"}}
			c.Routing = []Rule{{Class: "subagent", Route: "shadow", Local: "janus-local"}}
		}, ""},
		{"shadow rule naming no model", func(c *Config) { c.Routing = []Rule{{Route: "shadow"}} }, "routing[0].local is not set"},
		{"an escalate rule with a check skipped", func(c *Config) {
			c.Local.Models = map[string]LocalModel{"janus-local": {Model: "q"}}
			c.Routing = []Rule{{Route: "local-then-escalate", Local: "janus-local", SkipChecks: []string{"refusal"}}}
		}, ""},
		{"an escalate rule with a time budget", func(c *Config) {
			c.Local.Models = map[string]LocalModel{"janus-local": {Model: "q"}}
			c.Routing = []Rule{{Route: "local-then-escalate", Local: "janus-local", MaxSeconds: 45}}
		}, ""},
		{"maxSeconds on a route that streams", func(c *Config) {
			c.Local.Models = map[string]LocalModel{"janus-local": {Model: "q"}}
			c.Routing = []Rule{{Route: "local", Local: "janus-local", MaxSeconds: 45}}
		}, "maxSeconds only applies"},
		{"negative maxSeconds", func(c *Config) {
			c.Local.Models = map[string]LocalModel{"janus-local": {Model: "q"}}
			c.Routing = []Rule{{Route: "local-then-escalate", Local: "janus-local", MaxSeconds: -1}}
		}, "maxSeconds is negative"},
		{"an upstream rule with a local fallback", func(c *Config) {
			c.Local.Models = map[string]LocalModel{"janus-local": {Model: "q"}}
			c.Routing = []Rule{{Class: "main", Route: "upstream", IfDown: "janus-local"}}
		}, ""},
		{"ifDown naming a missing model", func(c *Config) { c.Routing = []Rule{{Route: "upstream", IfDown: "nope"}} }, `routing[0].ifDown "nope" is not an entry`},
		{"ifDown on a route that is already local", func(c *Config) {
			c.Local.Models = map[string]LocalModel{"janus-local": {Model: "q"}}
			c.Routing = []Rule{{Route: "local", Local: "janus-local", IfDown: "janus-local"}}
		}, "ifDown only applies"},
		{"an unknown check", func(c *Config) {
			c.Local.Models = map[string]LocalModel{"janus-local": {Model: "q"}}
			c.Routing = []Rule{{Route: "local-then-escalate", Local: "janus-local", SkipChecks: []string{"vibes"}}}
		}, `routing[0].skipChecks has "vibes"`},
		{"skipChecks on a route that does not check", func(c *Config) {
			c.Local.Models = map[string]LocalModel{"janus-local": {Model: "q"}}
			c.Routing = []Rule{{Route: "local", Local: "janus-local", SkipChecks: []string{"refusal"}}}
		}, "skipChecks only applies"},
		{"rule with an unknown class", func(c *Config) { c.Routing = []Rule{{Class: "sidekick", Route: "upstream"}} }, `routing[0].class "sidekick"`},
		{"rule with no route", func(c *Config) { c.Routing = []Rule{{Name: "x", Class: "main"}} }, "routing[0] (x).route is not set"},
		{"rule with an unknown route", func(c *Config) { c.Routing = []Rule{{Route: "sideways"}} }, `routing[0].route "sideways"`},
		{"local rule naming no model", func(c *Config) { c.Routing = []Rule{{Route: "local"}} }, "routing[0].local is not set"},
		{"local rule naming a missing model", func(c *Config) { c.Routing = []Rule{{Route: "local-then-escalate", Local: "nope"}} }, `routing[0].local "nope" is not an entry of local.models`},
		{"local model with no id", func(c *Config) { c.Local.Models = map[string]LocalModel{"janus-local": {}} }, "local.models.janus-local.model is not set"},
		{"local model with a negative limit", func(c *Config) {
			c.Local.Models = map[string]LocalModel{"x": {Model: "q", ContextTokens: -1}}
		}, "local.models.x has a negative"},
		{"no log dir", func(c *Config) { c.Logging.Dir = "" }, "logging.dir"},
		{"capture off", func(c *Config) { c.Logging.Capture = false }, ""},
		{"no capture limits", func(c *Config) { c.Logging.RetainDays, c.Logging.MaxMegabytes = 0, 0 }, ""},
		{"negative retention", func(c *Config) { c.Logging.RetainDays = -1 }, "logging.retainDays"},
		{"negative size", func(c *Config) { c.Logging.MaxMegabytes = -5 }, "logging.maxMegabytes"},
		{"a price override", func(c *Config) { c.Pricing = map[string]Price{"my-model": {Input: 1, Output: 2}} }, ""},
		{"negative price", func(c *Config) { c.Pricing = map[string]Price{"my-model": {Input: -1}} }, "pricing.my-model"},
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
	cfg.Features.Compress = true

	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate() = nil, want three problems")
	}
	for _, want := range []string{"listen", "upstream.baseUrl", "features.compress"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Validate() = %v, missing %q", err, want)
		}
	}
}

func TestReadDoesNotCreateAFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "janus.config.json")
	cfg, err := Read(path)
	if err != nil || !reflect.DeepEqual(cfg, Default()) {
		t.Errorf("Read of a missing file = %+v, %v; want the defaults", cfg, err)
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("Read wrote a config file")
	}

	os.WriteFile(path, []byte(`{"pricing":{"my-model":{"input":1.5,"output":6,"cacheRead":0.15,"cacheWrite5m":1.875,"cacheWrite1h":3}}}`), 0o600)
	cfg, err = Read(path)
	if err != nil || cfg.Pricing["my-model"].CacheWrite1h != 3 {
		t.Errorf("Read = %+v, %v", cfg.Pricing, err)
	}
}

func TestStarterFileLeavesPricingOut(t *testing.T) {
	path := filepath.Join(t.TempDir(), "janus.config.json")
	Load(path)
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "pricing") {
		t.Errorf("starter file mentions pricing:\n%s", data)
	}
}

func TestStarterFileLeavesGuardOut(t *testing.T) {
	path := filepath.Join(t.TempDir(), "janus.config.json")
	Load(path)
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), `"guard": {`) || strings.Contains(string(data), "patterns") {
		t.Errorf("starter file has an empty guard section:\n%s", data)
	}
}
