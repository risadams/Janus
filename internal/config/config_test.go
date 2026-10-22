package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func noEnv(string) string { return "" }

func TestDefaultIsValid(t *testing.T) {
	if err := Default().Validate(); err != nil {
		t.Fatalf("Default().Validate() = %v", err)
	}
}

// defaultsFor is Default as Read returns it for a config file at path.
// The default logging.dir is already absolute, so the path does not
// change it.
func defaultsFor(path string) Config {
	return Default()
}

// A relative logging.dir is taken from the config file's folder, not the
// folder a command runs in, so `janus stats` run anywhere reads the ledger
// the proxy writes.
func TestLogDirIsRelativeToTheConfigFile(t *testing.T) {
	home := t.TempDir()
	elsewhere := t.TempDir()
	path := filepath.Join(home, "janus.config.json")
	abs := filepath.Join(t.TempDir(), "kept-here")
	cases := map[string]struct{ file, want string }{
		"default":  {`{}`, StateDir()},
		"relative": {`{"logging":{"dir":"var/janus"}}`, filepath.Join(home, "var", "janus")},
		"absolute": {`{"logging":{"dir":` + strconv.Quote(abs) + `}}`, abs},
	}
	t.Chdir(elsewhere)
	for name, c := range cases {
		os.WriteFile(path, []byte(c.file), 0o600)
		for _, p := range []string{path, mustRel(t, elsewhere, path)} {
			cfg, err := Read(p)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if cfg.Logging.Dir != c.want {
				t.Errorf("%s: Read(%q) logging.dir = %q, want %q", name, p, cfg.Logging.Dir, c.want)
			}
		}
	}
}

func mustRel(t *testing.T, base, target string) string {
	t.Helper()
	rel, err := filepath.Rel(base, target)
	if err != nil {
		t.Skipf("no relative path from %s to %s", base, target)
	}
	return rel
}

func TestStateDirIsPerUserOnEachPlatform(t *testing.T) {
	home := func() (string, error) { return "/home/ris", nil }
	noHome := func() (string, error) { return "", errors.New("no home") }
	env := func(kv ...string) func(string) string {
		return func(k string) string {
			for i := 0; i+1 < len(kv); i += 2 {
				if kv[i] == k {
					return kv[i+1]
				}
			}
			return ""
		}
	}
	cases := []struct {
		goos   string
		getenv func(string) string
		home   func() (string, error)
		want   string
	}{
		{"darwin", env(), home, filepath.Join("/home/ris", "Library", "Application Support", "janus")},
		{"darwin", env("XDG_STATE_HOME", "/x"), home, filepath.Join("/home/ris", "Library", "Application Support", "janus")},
		{"linux", env(), home, filepath.Join("/home/ris", ".local", "state", "janus")},
		{"linux", env("XDG_STATE_HOME", "/x"), home, filepath.Join("/x", "janus")},
		{"windows", env("LocalAppData", `C:\Users\ris\AppData\Local`), home, filepath.Join(`C:\Users\ris\AppData\Local`, "janus")},
		{"windows", env(), home, filepath.Join("/home/ris", "AppData", "Local", "janus")},
		{"linux", env(), noHome, "logs"},
	}
	for _, c := range cases {
		if got := stateDir(c.goos, c.getenv, c.home); got != c.want {
			t.Errorf("stateDir(%s) = %q, want %q", c.goos, got, c.want)
		}
	}
}

func TestLoadCreatesStarterFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "janus.config.json")

	cfg, created, err := Load(path)
	if err != nil || !created {
		t.Fatalf("Load() created=%v err=%v, want created with no error", created, err)
	}
	if !reflect.DeepEqual(cfg, defaultsFor(path)) {
		t.Errorf("starter config = %+v, want defaults", cfg)
	}
	// The file names the per-user folder, so a config written in any
	// folder keeps its logs in the same place.
	want, _ := json.Marshal(StateDir())
	if data, _ := os.ReadFile(path); !strings.Contains(string(data), `"dir": `+string(want)) {
		t.Errorf("starter file does not hold the per-user logging.dir %s:\n%s", want, data)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); runtime.GOOS != "windows" && perm != 0o600 {
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
	if cfg.Upstream.BaseURL != Default().Upstream.BaseURL || cfg.Listen != Default().Listen || cfg.Logging != defaultsFor(path).Logging {
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
		{"upstream is janus itself", func(c *Config) { c.Upstream.BaseURL = "http://127.0.0.1:8787" }, "upstream.baseUrl \"http://127.0.0.1:8787\" is janus's own listen address"},
		{"upstream is janus by another loopback name", func(c *Config) { c.Upstream.BaseURL = "http://localhost:8787/v1" }, "janus's own listen address"},
		{"local is janus listening on every interface", func(c *Config) {
			c.Listen = "0.0.0.0:8787"
			c.AllowRemote = true
			c.Local.BaseURL = "http://[::1]:8787"
		}, "local.baseUrl \"http://[::1]:8787\" is janus's own listen address"},
		{"listen on port 80 and upstream with no port", func(c *Config) {
			c.Listen = "127.0.0.1:80"
			c.Upstream.BaseURL = "http://127.0.0.1"
		}, "janus's own listen address"},
		{"a backend on loopback, another port", func(c *Config) { c.Local.BaseURL = "http://127.0.0.1:1234" }, ""},
		{"a backend on another host, the same port", func(c *Config) { c.Local.BaseURL = "http://studio.local:8787" }, ""},
		{"unbuilt feature switched on", func(c *Config) { c.Features.Escalate = true }, "features.escalate is not available"},
		{"compress with no local model", func(c *Config) { c.Features.Compress = true }, "features.compress needs a local model"},
		{"compress with the only local model", func(c *Config) {
			c.Features.Compress = true
			c.Local.Models = map[string]LocalModel{"janus-local": {Model: "qwen/qwen3.8-27b"}}
		}, ""},
		{"compress naming a model that is not there", func(c *Config) {
			c.Features.Compress = true
			c.Local.Models = map[string]LocalModel{"a": {Model: "x"}, "b": {Model: "y"}}
			c.Compress.Digest.Local = "c"
		}, `compress.digest.local is "c"`},
		{"compress with several models and none named", func(c *Config) {
			c.Features.Compress = true
			c.Local.Models = map[string]LocalModel{"a": {Model: "x"}, "b": {Model: "y"}}
		}, "local.models has 2 entries"},
		{"compress with an empty tool name", func(c *Config) {
			c.Features.Compress = true
			c.Local.Models = map[string]LocalModel{"a": {Model: "x"}}
			c.Compress.Digest.Tools = []string{"Bash", " "}
		}, "compress.digest.tools[1] is empty"},
		{"guard on", func(c *Config) { c.Features.Guard = true }, ""},
		{"openai with a key from the environment", func(c *Config) { c.OpenAI.Key = "env:OPENAI_API_KEY" }, ""},
		{"openai with a key from the keychain", func(c *Config) { c.OpenAI.Key = "keychain:janus-openai" }, ""},
		{"openai with no key", func(c *Config) { c.OpenAI.Models = map[string]string{"haiku": "gpt-6-luna"} }, "openai.key is not set"},
		{"openai key as a literal", func(c *Config) { c.OpenAI.Key = "sk-proj-abc123" }, "looks like an API key itself"},
		{"openai key with no source", func(c *Config) { c.OpenAI.Key = "abc123" }, "looks like an API key itself"},
		{"openai key from an unknown source", func(c *Config) { c.OpenAI.Key = "vault:openai" }, "must say where the key is kept"},
		{"openai key from a badly named variable", func(c *Config) { c.OpenAI.Key = "env:OPENAI-KEY" }, "not a valid name"},
		{"openai key from an unnamed keychain entry", func(c *Config) { c.OpenAI.Key = "keychain: " }, `"keychain:" with no name`},
		{"openai not a url", func(c *Config) {
			c.OpenAI = OpenAI{BaseURL: "api.openai.com", Key: "env:OPENAI_API_KEY"}
		}, "openai.baseUrl"},
		{"openai is janus itself", func(c *Config) {
			c.OpenAI = OpenAI{BaseURL: "http://127.0.0.1:8787", Key: "env:OPENAI_API_KEY"}
		}, `openai.baseUrl "http://127.0.0.1:8787" is janus's own listen address`},
		{"openai model left empty", func(c *Config) {
			c.OpenAI = OpenAI{Key: "env:OPENAI_API_KEY", Models: map[string]string{"opus": ""}}
		}, "openai.models has an entry"},
		{"a rule for status summaries", func(c *Config) {
			c.Routing = []Rule{{Class: "status", Route: "upstream"}}
		}, ""},
		{"guard with patterns", func(c *Config) {
			c.Features.Guard = true
			c.Guard = Guard{Patterns: []string{`ACME-[0-9]{6}`}, Allow: []string{"AKIAEXAMPLE"}, Entropy: true}
		}, ""},
		{"a guard pattern that does not compile", func(c *Config) { c.Guard.Patterns = []string{"ok", "(unclosed"} }, "guard.patterns[1] is not a valid regular expression"},
		{"local-only paths and markers", func(c *Config) {
			c.Features.Guard = true
			c.Guard.LocalOnly = LocalOnly{Paths: []string{"secrets/**", "*.pem"}, Markers: []string{"COMPANY CONFIDENTIAL"}, Action: "withhold"}
		}, ""},
		{"a local-only path that protects everything", func(c *Config) { c.Guard.LocalOnly.Paths = []string{"**"} }, "guard.localOnly: "},
		{"a local-only action that does not exist", func(c *Config) {
			c.Guard.LocalOnly = LocalOnly{Paths: []string{".env"}, Action: "shred"}
		}, `guard.localOnly.action is "shred"`},
		{"an action with nothing to protect", func(c *Config) { c.Guard.LocalOnly.Action = "shred" }, `guard.localOnly.action is "shred"`},
		{"keeping requests local with one local model", func(c *Config) {
			c.Local.Models = map[string]LocalModel{"janus-local": {Model: "q"}}
			c.Guard.LocalOnly = LocalOnly{Paths: []string{".env"}, Action: "local"}
		}, ""},
		{"keeping requests local with no local model", func(c *Config) {
			c.Guard.LocalOnly = LocalOnly{Paths: []string{".env"}, Action: "local"}
		}, "local.models has 0 entries"},
		{"keeping requests local without saying which of two", func(c *Config) {
			c.Local.Models = map[string]LocalModel{"a": {Model: "q"}, "b": {Model: "r"}}
			c.Guard.LocalOnly = LocalOnly{Paths: []string{".env"}, Action: "local"}
		}, "name the one to use as guard.localOnly.local"},
		{"keeping requests local on a model that is not there", func(c *Config) {
			c.Local.Models = map[string]LocalModel{"a": {Model: "q"}}
			c.Guard.LocalOnly = LocalOnly{Paths: []string{".env"}, Action: "local", Local: "b"}
		}, `guard.localOnly.local is "b", which is not an entry`},
		{"naming a local model while withholding", func(c *Config) {
			c.Guard.LocalOnly = LocalOnly{Paths: []string{".env"}, Local: "a"}
		}, `guard.localOnly.local is only used with`},
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
			c.Routing = []Rule{{Class: "main", Route: "upstream", Fallback: []string{"upstream", "janus-local"}}}
		}, ""},
		{"a list that is the route", func(c *Config) {
			c.Local.Models = map[string]LocalModel{"janus-local": {Model: "q"}}
			c.OpenAI = OpenAI{Key: "env:OPENAI_API_KEY"}
			c.Routing = []Rule{{Fallback: []string{"openai", "upstream", "janus-local"}}}
		}, ""},
		{"OpenAI only, with no local model", func(c *Config) {
			c.Local.Models = nil
			c.OpenAI = OpenAI{Key: "env:OPENAI_API_KEY"}
			c.Routing = []Rule{{Fallback: []string{"openai"}}}
		}, ""},
		{"fallback naming a missing model", func(c *Config) { c.Routing = []Rule{{Fallback: []string{"upstream", "nope"}}} }, `routing[0].fallback[1] "nope" is not upstream, openai, or an entry`},
		{"fallback naming openai with no openai section", func(c *Config) { c.Routing = []Rule{{Fallback: []string{"openai"}}} }, "fallback names openai, but there is no openai section"},
		{"fallback naming one twice", func(c *Config) { c.Routing = []Rule{{Fallback: []string{"upstream", "upstream"}}} }, `fallback names "upstream" twice`},
		{"an upstream route whose list starts elsewhere", func(c *Config) {
			c.Local.Models = map[string]LocalModel{"janus-local": {Model: "q"}}
			c.Routing = []Rule{{Route: "upstream", Fallback: []string{"janus-local", "upstream"}}}
		}, `fallback must start with "upstream" when route is upstream`},
		{"fallback on a route that is already local", func(c *Config) {
			c.Local.Models = map[string]LocalModel{"janus-local": {Model: "q"}}
			c.Routing = []Rule{{Route: "local", Local: "janus-local", Fallback: []string{"upstream"}}}
		}, "fallback only applies"},
		{"local beside a list that is the route", func(c *Config) {
			c.Local.Models = map[string]LocalModel{"janus-local": {Model: "q"}}
			c.Routing = []Rule{{Local: "janus-local", Fallback: []string{"janus-local"}}}
		}, "local is not used when fallback is the route"},
		{"a rule that waits for the five-hour limit", func(c *Config) {
			c.Local.Models = map[string]LocalModel{"janus-local": {Model: "q"}}
			c.Routing = []Rule{{Class: "subagent", Route: "local-then-escalate", Local: "janus-local", IfUsageAbove: map[string]float64{"5h": 0.8}}}
		}, ""},
		{"ifUsageAbove past the whole limit", func(c *Config) {
			c.Local.Models = map[string]LocalModel{"janus-local": {Model: "q"}}
			c.Routing = []Rule{{Route: "local", Local: "janus-local", IfUsageAbove: map[string]float64{"5h": 80}}}
		}, `routing[0].ifUsageAbove "5h": 80 is not a share`},
		{"ifUsageAbove on an upstream rule", func(c *Config) {
			c.Routing = []Rule{{Route: "upstream", IfUsageAbove: map[string]float64{"5h": 0.8}}}
		}, "ifUsageAbove sends nothing anywhere new"},
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
	if err != nil || !reflect.DeepEqual(cfg, defaultsFor(path)) {
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

func TestContextWarnDefaultsAndCanBeTurnedOff(t *testing.T) {
	for set, want := range map[int]int{0: DefaultContextWarnTokens, 250_000: 250_000, -1: 0} {
		if got := (Console{ContextWarnTokens: set}).ContextWarn(); got != want {
			t.Errorf("contextWarnTokens %d: %d, want %d", set, got, want)
		}
	}
}

func TestOpenAIKeyIsNeverRepeatedInAnError(t *testing.T) {
	for _, key := range []string{"sk-proj-Q7Rz2Lp9XvB4nKd8", "Q7Rz2Lp9XvB4nKd8Yt3WmHs6", "bearer:Q7Rz2Lp9XvB4nKd8"} {
		c := Default()
		c.OpenAI.Key = key
		err := c.Validate()
		if err == nil {
			t.Fatalf("key %q was accepted", key)
		}
		if strings.Contains(err.Error(), "Q7Rz2Lp9XvB4nKd8") {
			t.Errorf("the error repeats the key: %v", err)
		}
	}
}

func TestOpenAIDefaultsWhenOnlyTheKeyIsSet(t *testing.T) {
	o := OpenAI{Key: "env:OPENAI_API_KEY"}
	if !o.On() || o.URL() != DefaultOpenAIURL {
		t.Fatalf("On() = %v, URL() = %q", o.On(), o.URL())
	}
	if kind, name := o.KeySource(); kind != "env" || name != "OPENAI_API_KEY" {
		t.Fatalf("KeySource() = %q, %q", kind, name)
	}
	if (OpenAI{}).On() {
		t.Fatal("an empty openai section counts as on")
	}
}

func TestConfigWithoutOpenAIIsUnchanged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "janus.config.json")
	Load(path)
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "openai") {
		t.Errorf("starter file has an openai section:\n%s", data)
	}
	if Default().OpenAI.On() {
		t.Error("the default config uses OpenAI")
	}
}

func TestLoadReadsTheOpenAISection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "janus.config.json")
	body := `{"openai": {"key": "env:OPENAI_API_KEY", "models": {"sonnet": "gpt-6-luna"}}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.OpenAI.Key != "env:OPENAI_API_KEY" || c.OpenAI.Models["sonnet"] != "gpt-6-luna" || c.OpenAI.URL() != DefaultOpenAIURL {
		t.Fatalf("openai = %+v", c.OpenAI)
	}
}

func TestKeepFromNamesAProviderAndSomethingToKeep(t *testing.T) {
	for _, c := range []struct {
		name string
		keep map[string]KeepFrom
		want string
	}{
		{"a marker kept from OpenAI", map[string]KeepFrom{"openai": {Markers: []string{"ORION"}}}, ""},
		{"everything kept from Claude", map[string]KeepFrom{"upstream": {Everything: true}}, ""},
		{"a provider that is not one", map[string]KeepFrom{"janus-local": {Markers: []string{"x"}}}, "guard.keepFrom.janus-local: name upstream or openai"},
		{"nothing to keep", map[string]KeepFrom{"openai": {}}, "guard.keepFrom.openai keeps nothing"},
	} {
		cfg := Default()
		cfg.Features.Guard = true
		cfg.Guard.KeepFrom = c.keep
		err := cfg.Validate()
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: %v", c.name, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s: err = %v, want %q", c.name, err, c.want)
		}
	}
	cfg := Default()
	cfg.Guard.KeepFrom = map[string]KeepFrom{"openai": {Everything: true}}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "features.guard is off") {
		t.Errorf("with the guard off: %v", err)
	}
}
