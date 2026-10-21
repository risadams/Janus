package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/risadams/janus/internal/guard"
	"github.com/risadams/janus/internal/validate"
)

// DefaultPath is where the config file is looked for when -config is not
// given: next to wherever janus is run from.
const DefaultPath = "janus.config.json"

// Config is the whole of janus.config.json.
type Config struct {
	// Version is the config's format; see Version(). A config without one
	// is version 1.
	Version int `json:"version,omitempty"`
	// Upgraded says what was changed, in memory, to bring an older config
	// up to this janus's version. The file itself is left alone until
	// janus config upgrade rewrites it.
	Upgraded []string `json:"-"`
	// Listen is the host:port janus accepts client connections on.
	Listen string `json:"listen"`
	// AllowRemote must be set before Listen may name a non-loopback
	// address. Janus forwards whatever credentials a client sends, so
	// exposing it beyond this machine is a decision, not a default.
	AllowRemote bool `json:"allowRemote"`

	Upstream Backend `json:"upstream"`
	Local    Local   `json:"local"`
	// OpenAI is the OpenAI API, which can answer Claude Code's requests in
	// Anthropic's place (FR-11). Left out, it is not used.
	OpenAI OpenAI `json:"openai,omitzero"`
	// Routing is the list of rules that decide where a Messages request
	// goes, tried in order; the first that matches wins, and a request no
	// rule matches goes upstream. Rules apply only with features.route on.
	Routing  []Rule   `json:"routing,omitempty"`
	Features Features `json:"features"`
	Logging  Logging  `json:"logging"`
	// Guard sets how secrets are looked for, when features.guard is on.
	Guard Guard `json:"guard,omitzero"`
	// Compress sets how requests going upstream are rewritten to send
	// fewer tokens, when features.compress is on.
	Compress Compress `json:"compress,omitzero"`
	// Pricing adds to, or corrects, the prices janus ships with, keyed by
	// model id. Left out of the starter file because the defaults are
	// usually right; see docs/CONFIG.md.
	Pricing map[string]Price `json:"pricing,omitempty"`
	// Update sets how janus looks for newer releases: when a command asks
	// it to, and, if Check is set, once a day while it serves.
	Update Update `json:"update,omitzero"`
	// Console sets what the console says as janus works.
	Console Console `json:"console,omitzero"`
}

// Console configures the console's notes.
type Console struct {
	// ContextWarnTokens is the prompt size, in tokens, past which the
	// console says, once per conversation, that each turn has grown
	// expensive. Zero means DefaultContextWarnTokens; a negative number
	// turns the note off.
	ContextWarnTokens int `json:"contextWarnTokens,omitempty"`
}

// DefaultContextWarnTokens is where the console's note about a large
// conversation starts: from 400k tokens a turn cost about three times one
// under 200k, in a week of ordinary use (docs/research/baseline.md).
const DefaultContextWarnTokens = 400_000

// ContextWarn is the prompt size the console warns from, or 0 for never.
func (c Console) ContextWarn() int {
	switch {
	case c.ContextWarnTokens < 0:
		return 0
	case c.ContextWarnTokens == 0:
		return DefaultContextWarnTokens
	}
	return c.ContextWarnTokens
}

// Update configures the check for newer releases made by `janus version
// -check` and `janus doctor`, and the notice at startup.
type Update struct {
	// Check looks for a newer release when janus starts, at most once a
	// day, and says on the console if there is one. Off, janus sends
	// nothing to look for releases unless a command asks it to.
	Check bool `json:"check,omitempty"`
	// Prerelease counts pre-releases, such as v1.0.0-rc.2, as newer
	// releases. Without it only stable releases are offered.
	Prerelease bool `json:"prerelease,omitempty"`
}

// Guard configures the search for secrets in requests going upstream.
// With features.guard on, janus looks for them and records what it finds;
// with Redact as well, it keeps them from leaving.
type Guard struct {
	// Patterns are extra regular expressions to treat as secrets. A
	// pattern with a capture group marks the group as the secret.
	Patterns []string `json:"patterns,omitempty"`
	// Allow lists exact values that are never secrets.
	Allow []string `json:"allow,omitempty"`
	// Entropy also looks for long random-looking strings that fit no
	// known format. It finds more and is wrong more often.
	Entropy bool `json:"entropy,omitempty"`
	// Redact replaces each secret with a placeholder before the request is
	// sent, and puts it back in the answer. Without it the guard only
	// reports what it finds.
	Redact bool `json:"redact,omitempty"`
	// LocalOnly names content that must never be sent upstream, whatever
	// is in it.
	LocalOnly LocalOnly `json:"localOnly,omitzero"`
}

// Compress configures the transforms that rewrite requests going upstream.
type Compress struct {
	Digest Digest `json:"digest,omitzero"`
}

// Digest configures the digest of large tool results: the local model
// summarises the result, the summary goes upstream, and Claude can fetch
// the original through janus mcp's tool_output tool.
type Digest struct {
	// Local names the entry of local.models that writes the digests. It
	// may be left out when there is only one.
	Local string `json:"local,omitempty"`
	// MinBytes is the smallest tool result digested. Zero means
	// DefaultDigestMinBytes.
	MinBytes int `json:"minBytes,omitempty"`
	// Tools limits digests to the results of these tools, such as
	// ["Bash"]. Empty means any tool.
	Tools []string `json:"tools,omitempty"`
	// MaxSeconds is how long the local model gets to write one digest.
	// The turn waits all the while; past it the result goes as it is.
	// Zero means DefaultDigestSeconds.
	MaxSeconds int `json:"maxSeconds,omitempty"`
}

// DefaultDigestMinBytes leaves tool results under about 1,300 tokens
// alone: a digest of one would save little.
const DefaultDigestMinBytes = 4000

// DefaultDigestSeconds is how long a digest may hold up a turn.
const DefaultDigestSeconds = 30

// DigestModel is the entry of local.models that writes digests.
func (c Config) DigestModel() (name string, model LocalModel, ok bool) {
	name = c.Compress.Digest.Local
	if name == "" && len(c.Local.Models) == 1 {
		for only := range c.Local.Models {
			name = only
		}
	}
	model, ok = c.Local.Models[name]
	return name, model, ok
}

// LocalOnly is the local-only policy: content that stays on this machine.
type LocalOnly struct {
	// Paths are globs for files whose contents are local-only, matched
	// against the end of a path: "secrets/**", "*.pem", ".env".
	Paths []string `json:"paths,omitempty"`
	// Markers are strings that make any text containing them local-only.
	Markers []string `json:"markers,omitempty"`
	// Action is what happens to a request holding such content: "withhold"
	// (the default) sends the request with the content replaced by a
	// notice; "local" has the local model answer the whole request, or
	// refuses it.
	Action string `json:"action,omitempty"`
	// Local names the local.models entry that answers, for the "local"
	// action. It may be left out when there is only one.
	Local string `json:"local,omitempty"`
}

// On reports whether the policy protects anything.
func (l LocalOnly) On() bool { return len(l.Paths) > 0 || len(l.Markers) > 0 }

// Price is what one model costs, in US dollars per million tokens.
type Price struct {
	Input        float64 `json:"input"`
	Output       float64 `json:"output"`
	CacheRead    float64 `json:"cacheRead"`
	CacheWrite5m float64 `json:"cacheWrite5m"`
	CacheWrite1h float64 `json:"cacheWrite1h"`
}

// Backend is one model server janus can send requests to.
type Backend struct {
	BaseURL string `json:"baseUrl"`
}

// OpenAI is the OpenAI API and how janus reaches it.
type OpenAI struct {
	// BaseURL is the API's address, without /v1. Empty means
	// DefaultOpenAIURL.
	BaseURL string `json:"baseUrl,omitempty"`
	// Key says where the API key comes from: "env:NAME" for the
	// environment variable NAME, or "keychain:NAME" for the entry NAME in
	// the operating system's credential store. The key itself is never
	// written here (ADR-005).
	Key string `json:"key,omitempty"`
	// Models maps what Claude Code asks for to an OpenAI model: a Claude
	// model's full name, its family ("fable", "opus", "sonnet", "haiku"),
	// or "default" for any other. Entries here replace those in
	// DefaultOpenAIModels.
	Models map[string]string `json:"models,omitempty"`
}

// DefaultOpenAIURL is the OpenAI API.
const DefaultOpenAIURL = "https://api.openai.com"

// DefaultOpenAIModels are the models that answer for each Claude family
// unless openai.models says otherwise: the owner chose the smaller model
// where two were close (docs/research/openai.md, Models; #374).
// "default" answers for a model of no family janus knows.
var DefaultOpenAIModels = map[string]string{
	"fable":   "gpt-6.1-sol",
	"opus":    "gpt-6.1-sol",
	"sonnet":  "gpt-6.1-sol",
	"haiku":   "gpt-6-luna",
	"default": "gpt-6.1-sol",
}

// On reports whether the config has an openai section at all.
func (o OpenAI) On() bool { return o.BaseURL != "" || o.Key != "" || len(o.Models) > 0 }

// URL is the API's address, with the default filled in.
func (o OpenAI) URL() string {
	if o.BaseURL == "" {
		return DefaultOpenAIURL
	}
	return o.BaseURL
}

// KeySource splits Key into where the key is kept, "env" or "keychain",
// and the name it is kept under.
func (o OpenAI) KeySource() (kind, name string) {
	kind, name, _ = strings.Cut(o.Key, ":")
	return kind, name
}

// envName is what a POSIX shell accepts as a variable's name.
var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// checkOpenAIKey says what is wrong with openai.key. It never repeats the
// value: a value that is not a source may be the key itself.
func checkOpenAIKey(key string) error {
	const how = `set it to "env:OPENAI_API_KEY" and put the key in that environment variable, or to "keychain:<name>" for an entry in the operating system's credential store`
	kind, name, ok := strings.Cut(key, ":")
	switch {
	case key == "":
		return errors.New("openai.key is not set; " + how)
	case ok && kind == "env" && envName.MatchString(name):
		return nil
	case ok && kind == "env":
		return errors.New(`openai.key names an environment variable that is not a valid name; use letters, digits, and "_", as in "env:OPENAI_API_KEY"`)
	case ok && kind == "keychain" && strings.TrimSpace(name) != "":
		return nil
	case ok && kind == "keychain":
		return errors.New(`openai.key is "keychain:" with no name; name the credential store's entry, as in "keychain:janus-openai"`)
	case strings.HasPrefix(key, "sk-") || !ok:
		return errors.New("openai.key looks like an API key itself, and janus never reads a key from its config file, which gets copied, shared, and backed up; " + how)
	default:
		return errors.New("openai.key must say where the key is kept; " + how)
	}
}

// Local is the LM Studio server and the models on it that janus may use.
type Local struct {
	BaseURL string `json:"baseUrl"`
	// Models maps a model name a client asks for to a local model. A
	// request for one of these names is answered locally when
	// features.route is on. The name can be anything: an alias made up for
	// the purpose, such as "janus-local", or a real model name to divert.
	Models map[string]LocalModel `json:"models,omitempty"`
}

// LocalModel is one entry of local.models.
type LocalModel struct {
	// Model is the model's id in LM Studio.
	Model string `json:"model"`
	// ContextTokens is the largest prompt to send it. Zero means use the
	// context LM Studio has the model loaded with.
	ContextTokens int `json:"contextTokens,omitempty"`
	// TimeoutSeconds bounds one request. Zero means the default.
	TimeoutSeconds int `json:"timeoutSeconds,omitempty"`
	// MaxConcurrent is how many requests routing rules may have in flight
	// to this model at once; a rule sends any more upstream. Zero means
	// DefaultMaxConcurrent.
	MaxConcurrent int `json:"maxConcurrent,omitempty"`
}

// DefaultMaxConcurrent is one request at a time. A local model runs
// requests side by side in the machine's own memory, and on a 36 GB Mac two
// parallel subagents took a 27B model past 31 GB wired within four minutes
// (#128). A machine with room to spare can raise it.
const DefaultMaxConcurrent = 1

// Rule is one routing rule. The fields above Route are conditions, and a
// request must meet every one that is set; a rule with none set matches
// every Messages request.
type Rule struct {
	// Name identifies the rule in the ledger. Optional.
	Name string `json:"name,omitempty"`
	// Class is the request class: main, subagent, status, or unknown.
	Class string `json:"class,omitempty"`
	// Model is the model the client asked for.
	Model string `json:"model,omitempty"`
	// MaxTokens matches requests estimated at no more than this many
	// tokens.
	MaxTokens int `json:"maxTokens,omitempty"`
	// Tools matches requests that do (true) or do not (false) offer the
	// model any tools.
	Tools *bool `json:"tools,omitempty"`

	// Route is where a matching request goes: "upstream"; "local";
	// "shadow", which sends it upstream as usual and afterwards runs it on
	// the local model too, for comparison; or "local-then-escalate", which
	// tries the local model, checks its answer, and asks Claude instead if
	// the answer fails the checks.
	Route string `json:"route"`
	// Local names the entry of local.models to use. Required unless Route
	// is "upstream".
	Local string `json:"local,omitempty"`
	// IfDown names an entry of local.models to answer instead when this
	// rule sends a request upstream and Claude cannot answer it: it is
	// unreachable, rate limited, overloaded, or failing. For the upstream
	// and shadow routes. Left out, the failure reaches the client.
	IfDown string `json:"ifDown,omitempty"`
	// SkipChecks turns off named checks on the local answer, for the
	// local-then-escalate route: empty, truncated, tools, schema, refusal.
	SkipChecks []string `json:"skipChecks,omitempty"`
	// MaxSeconds is how long the local model gets to produce its whole
	// answer on the local-then-escalate route before janus gives up on it
	// and asks Claude. Zero means DefaultTrialSeconds. The client is
	// waiting all the while, so this is the most a poor local answer can
	// cost it.
	MaxSeconds int `json:"maxSeconds,omitempty"`
	// IfUsageAbove makes the rule match only once one of the
	// subscription's usage limits is used past a share, from 0 to 1, as
	// Claude last reported it: {"5h": 0.8} for the five-hour limit, "7d"
	// for the weekly one. It moves work to the local model before a limit
	// is reached (#240). With no report yet, or after a limit's reset, it
	// does not match.
	IfUsageAbove map[string]float64 `json:"ifUsageAbove,omitempty"`
}

// DefaultTrialSeconds is how long a local-then-escalate attempt may take
// when the rule does not say. Claude answers a turn in a few seconds; a
// local model that has not finished in two minutes is not saving anyone
// anything.
const DefaultTrialSeconds = 120

// DefaultLocalTimeoutSeconds is how long a local request may take when the
// model's entry does not say. Local models are slow to start on a large
// prompt and slower still if they must be loaded first.
const DefaultLocalTimeoutSeconds = 900

// Features switches each mechanism on. All are off by default. Route,
// compress, and guard are implemented; Validate rejects the other, so a
// config can never claim a behaviour the binary does not have.
type Features struct {
	Route    bool `json:"route"`
	Escalate bool `json:"escalate"`
	Compress bool `json:"compress"`
	Guard    bool `json:"guard"`
}

// Logging controls what janus writes to disk. Everything goes under Dir:
// the ledger, which is always kept, in Dir/ledger; captures in
// Dir/captures; and, with features.compress on, the originals of digested
// tool results in Dir/originals. Dir defaults to StateDir. A relative Dir
// is relative to the folder holding the config file, so the proxy and
// every command that reads its logs find the same folder wherever they
// are run from.
type Logging struct {
	Dir string `json:"dir"`
	// Capture saves every request and response in full. Captures contain
	// prompts, code, and tool output, so they are pruned by the two limits
	// below; zero for either means no limit.
	Capture      bool `json:"capture"`
	RetainDays   int  `json:"retainDays"`
	MaxMegabytes int  `json:"maxMegabytes"`
}

// StateDir is the default logging.dir: a folder per user, where the
// platform keeps such things, so the ledger is in one place whichever
// folder janus is started in (#242). Before, the default was logs beside
// the config file, and starting janus from two folders split one person's
// use across two ledgers. It is "logs", beside the config, only when the
// user's home cannot be found.
func StateDir() string {
	return stateDir(runtime.GOOS, os.Getenv, os.UserHomeDir)
}

func stateDir(goos string, getenv func(string) string, home func() (string, error)) string {
	if goos == "windows" {
		if dir := getenv("LocalAppData"); dir != "" {
			return filepath.Join(dir, "janus")
		}
	}
	if dir := getenv("XDG_STATE_HOME"); dir != "" && goos != "darwin" && goos != "windows" {
		return filepath.Join(dir, "janus")
	}
	h, err := home()
	if err != nil || h == "" {
		return "logs"
	}
	switch goos {
	case "darwin":
		return filepath.Join(h, "Library", "Application Support", "janus")
	case "windows":
		return filepath.Join(h, "AppData", "Local", "janus")
	default:
		return filepath.Join(h, ".local", "state", "janus")
	}
}

// Default is a pass-through proxy on loopback that keeps a ledger and a
// week of captures in the user's StateDir: the Anthropic API upstream, LM
// Studio addressed by hostname rather than localhost so the same config
// reaches the intended instance from any machine, and every feature off.
func Default() Config {
	return Config{
		Version:  Version(),
		Listen:   "127.0.0.1:8787",
		Upstream: Backend{BaseURL: "https://api.anthropic.com"},
		Local:    Local{BaseURL: "http://studio.local:1234"},
		Logging:  Logging{Dir: StateDir(), Capture: true, RetainDays: 7, MaxMegabytes: 1024},
	}
}

// Load reads path, or writes a starter file holding the defaults and
// returns it with created=true if path does not exist. Fields missing from
// the file keep their defaults; fields janus does not know are an error, so
// a typo is reported rather than silently ignored.
func Load(path string) (cfg Config, created bool, err error) {
	if _, statErr := os.Stat(path); os.IsNotExist(statErr) {
		cfg = Default()
		out, err := json.MarshalIndent(cfg, "", "  ")
		if err != nil {
			return Config{}, false, err
		}
		if err := os.WriteFile(path, append(out, '\n'), 0o600); err != nil {
			return Config{}, false, fmt.Errorf("could not write starter config %s: %w", path, err)
		}
		return resolve(cfg, path), true, nil
	}
	cfg, err = Read(path)
	return cfg, false, err
}

// Read is Load for commands that only look: if path does not exist it
// returns the defaults and writes nothing.
func Read(path string) (Config, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- the config file the user named
	if os.IsNotExist(err) {
		return resolve(Default(), path), nil
	}
	if err != nil {
		return Config{}, err
	}
	upgraded, notes, err := upgrade(data)
	if err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	cfg, err := decode(upgraded, path)
	if err != nil {
		return Config{}, err
	}
	cfg.Upgraded = notes
	return cfg, nil
}

// decode reads a config's JSON over the defaults, refusing settings it
// does not know, so that a typo is reported rather than ignored.
func decode(data []byte, path string) (Config, error) {
	cfg := Default()
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, unknownFieldHint(err))
	}
	return resolve(cfg, path), nil
}

// resolve makes a relative logging.dir relative to the folder holding the
// config file at path, and absolute. Taking it from the current folder
// instead meant a command run from anywhere but where the proxy was started
// read an empty ledger and said nothing was recorded.
func resolve(cfg Config, path string) Config {
	if cfg.Logging.Dir == "" || filepath.IsAbs(cfg.Logging.Dir) {
		return cfg
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return cfg
	}
	cfg.Logging.Dir = filepath.Join(filepath.Dir(abs), cfg.Logging.Dir)
	return cfg
}

// Overrides are the settings that can be changed without editing the file.
// Empty means "not set".
type Overrides struct {
	Listen      string
	UpstreamURL string
	LocalURL    string
}

// Apply layers the environment and then flags on top of cfg, so the order
// of precedence is flag, environment, file, default. getenv is os.Getenv
// outside tests.
func Apply(cfg Config, flags Overrides, getenv func(string) string) Config {
	env := Overrides{
		Listen:      getenv("JANUS_LISTEN"),
		UpstreamURL: getenv("JANUS_UPSTREAM_URL"),
		LocalURL:    getenv("JANUS_LOCAL_URL"),
	}
	for _, o := range []Overrides{env, flags} {
		if o.Listen != "" {
			cfg.Listen = o.Listen
		}
		if o.UpstreamURL != "" {
			cfg.Upstream.BaseURL = o.UpstreamURL
		}
		if o.LocalURL != "" {
			cfg.Local.BaseURL = o.LocalURL
		}
	}
	return cfg
}

// Validate reports every problem at once, each naming the field and what to
// put there, so a broken config is fixed in one pass.
func (c Config) Validate() error {
	var errs []error

	host, _, err := net.SplitHostPort(c.Listen)
	switch {
	case err != nil:
		errs = append(errs, fmt.Errorf("listen %q is not host:port, for example 127.0.0.1:8787", c.Listen))
	case !isLoopback(host) && !c.AllowRemote:
		errs = append(errs, fmt.Errorf("listen %q is not a loopback address; use 127.0.0.1, or set allowRemote to true to accept connections from other machines", c.Listen))
	}

	errs = append(errs, checkURL("upstream.baseUrl", c.Upstream.BaseURL, "https://api.anthropic.com"))
	errs = append(errs, checkURL("local.baseUrl", c.Local.BaseURL, "http://studio.local:1234"))
	backends := []struct{ field, raw string }{
		{"upstream.baseUrl", c.Upstream.BaseURL},
		{"local.baseUrl", c.Local.BaseURL},
	}
	if c.OpenAI.On() {
		errs = append(errs, checkURL("openai.baseUrl", c.OpenAI.URL(), DefaultOpenAIURL))
		errs = append(errs, checkOpenAIKey(c.OpenAI.Key))
		for from, to := range c.OpenAI.Models {
			if strings.TrimSpace(from) == "" || strings.TrimSpace(to) == "" {
				errs = append(errs, fmt.Errorf(`openai.models has an entry %q: %q; both sides must name a model, as in "sonnet": "gpt-6.1-sol"`, from, to))
			}
		}
		backends = append(backends, struct{ field, raw string }{"openai.baseUrl", c.OpenAI.URL()})
	}
	for _, b := range backends {
		if PointsAtListen(c.Listen, b.raw) {
			errs = append(errs, fmt.Errorf("%s %q is janus's own listen address, so janus would send each request back to itself without end; point it at the backend, or move listen to another port", b.field, b.raw))
		}
	}

	for _, f := range []struct {
		name string
		on   bool
	}{
		{"escalate", c.Features.Escalate},
	} {
		if f.on {
			errs = append(errs, fmt.Errorf("features.%s is not available in this version; set it to false", f.name))
		}
	}
	if c.Features.Compress {
		d := c.Compress.Digest
		switch _, _, ok := c.DigestModel(); {
		case ok:
		case d.Local != "":
			errs = append(errs, fmt.Errorf("compress.digest.local is %q, which is not an entry of local.models", d.Local))
		default:
			errs = append(errs, fmt.Errorf("features.compress needs a local model to write digests, and local.models has %d entries; add one, or name the one to use as compress.digest.local", len(c.Local.Models)))
		}
		if d.MinBytes < 0 || d.MaxSeconds < 0 {
			errs = append(errs, errors.New("compress.digest.minBytes and maxSeconds cannot be negative"))
		}
		for i, tool := range d.Tools {
			if strings.TrimSpace(tool) == "" {
				errs = append(errs, fmt.Errorf("compress.digest.tools[%d] is empty; name a tool, such as \"Bash\"", i))
			}
		}
	}
	for i, pattern := range c.Guard.Patterns {
		if _, err := regexp.Compile(pattern); err != nil {
			errs = append(errs, fmt.Errorf("guard.patterns[%d] is not a valid regular expression: %v", i, err))
		}
	}
	if lo := c.Guard.LocalOnly; lo.On() || lo.Action != "" || lo.Local != "" {
		if _, err := guard.NewPolicy(lo.Paths, lo.Markers); err != nil {
			errs = append(errs, fmt.Errorf("guard.localOnly: %v", err))
		}
		switch lo.Action {
		case "", "withhold":
			if lo.Local != "" {
				errs = append(errs, errors.New(`guard.localOnly.local is only used with "action": "local"`))
			}
		case "local":
			switch _, known := c.Local.Models[lo.Local]; {
			case lo.Local != "" && !known:
				errs = append(errs, fmt.Errorf("guard.localOnly.local is %q, which is not an entry of local.models", lo.Local))
			case lo.Local == "" && len(c.Local.Models) != 1:
				errs = append(errs, fmt.Errorf(`guard.localOnly.action is "local" and local.models has %d entries; name the one to use as guard.localOnly.local`, len(c.Local.Models)))
			}
		default:
			errs = append(errs, fmt.Errorf(`guard.localOnly.action is %q; it must be "withhold" or "local"`, lo.Action))
		}
	}
	if c.Features.Route && len(c.Local.Models) == 0 {
		errs = append(errs, errors.New(`features.route is on but local.models is empty; add an entry such as "janus-local": {"model": "<id of a model in LM Studio>"}`))
	}
	for i, rule := range c.Routing {
		where := fmt.Sprintf("routing[%d]", i)
		if rule.Name != "" {
			where += fmt.Sprintf(" (%s)", rule.Name)
		}
		switch rule.Class {
		case "", "main", "subagent", "status", "safety", "unknown":
		default:
			errs = append(errs, fmt.Errorf("%s.class %q is not one of main, subagent, status, safety, unknown", where, rule.Class))
		}
		if rule.MaxTokens < 0 {
			errs = append(errs, fmt.Errorf("%s.maxTokens is negative; leave it out for no limit", where))
		}
		for _, check := range rule.SkipChecks {
			if !validate.Known(check) {
				errs = append(errs, fmt.Errorf("%s.skipChecks has %q, which is not one of %s", where, check, strings.Join(validate.All, ", ")))
			}
		}
		if len(rule.SkipChecks) > 0 && rule.Route != "local-then-escalate" {
			errs = append(errs, fmt.Errorf("%s.skipChecks only applies when route is local-then-escalate", where))
		}
		if rule.IfDown != "" {
			if _, ok := c.Local.Models[rule.IfDown]; !ok {
				errs = append(errs, fmt.Errorf("%s.ifDown %q is not an entry of local.models", where, rule.IfDown))
			}
			if rule.Route != "upstream" && rule.Route != "shadow" {
				errs = append(errs, fmt.Errorf("%s.ifDown only applies when route is upstream or shadow; the other routes already try the local model", where))
			}
		}
		for window, share := range rule.IfUsageAbove {
			if window == "" || share <= 0 || share > 1 {
				errs = append(errs, fmt.Errorf("%s.ifUsageAbove %q: %v is not a share above 0 and at most 1; use, say, {\"5h\": 0.8}", where, window, share))
			}
		}
		if len(rule.IfUsageAbove) > 0 && rule.Route == "upstream" {
			errs = append(errs, fmt.Errorf("%s.ifUsageAbove sends nothing anywhere new with route upstream; use a route that tries the local model", where))
		}
		switch {
		case rule.MaxSeconds < 0:
			errs = append(errs, fmt.Errorf("%s.maxSeconds is negative; leave it out for the default of %d", where, DefaultTrialSeconds))
		case rule.MaxSeconds > 0 && rule.Route != "local-then-escalate":
			errs = append(errs, fmt.Errorf("%s.maxSeconds only applies when route is local-then-escalate; local.models.%s.timeoutSeconds bounds other local requests", where, rule.Local))
		}
		switch rule.Route {
		case "upstream":
		case "local", "shadow", "local-then-escalate":
			if rule.Local == "" {
				errs = append(errs, fmt.Errorf("%s.local is not set; name an entry of local.models", where))
			} else if _, ok := c.Local.Models[rule.Local]; !ok {
				errs = append(errs, fmt.Errorf("%s.local %q is not an entry of local.models", where, rule.Local))
			}
		case "":
			errs = append(errs, fmt.Errorf("%s.route is not set; use upstream, local, shadow, or local-then-escalate", where))
		default:
			errs = append(errs, fmt.Errorf("%s.route %q is not one of upstream, local, shadow, local-then-escalate", where, rule.Route))
		}
	}
	for name, m := range c.Local.Models {
		switch {
		case name == "":
			errs = append(errs, errors.New("local.models has an entry with an empty name"))
		case m.Model == "":
			errs = append(errs, fmt.Errorf("local.models.%s.model is not set; give the id of a model in LM Studio", name))
		case m.ContextTokens < 0 || m.TimeoutSeconds < 0 || m.MaxConcurrent < 0:
			errs = append(errs, fmt.Errorf("local.models.%s has a negative contextTokens, timeoutSeconds, or maxConcurrent; use 0 for the default", name))
		}
	}

	if c.Logging.Dir == "" {
		errs = append(errs, errors.New(`logging.dir is empty; set a directory, for example "logs"`))
	}
	for model, p := range c.Pricing {
		if p.Input < 0 || p.Output < 0 || p.CacheRead < 0 || p.CacheWrite5m < 0 || p.CacheWrite1h < 0 {
			errs = append(errs, fmt.Errorf("pricing.%s has a negative price; prices are dollars per million tokens", model))
		}
	}
	if c.Logging.RetainDays < 0 {
		errs = append(errs, fmt.Errorf("logging.retainDays is %d; use 0 to keep captures forever, or a number of days", c.Logging.RetainDays))
	}
	if c.Logging.MaxMegabytes < 0 {
		errs = append(errs, fmt.Errorf("logging.maxMegabytes is %d; use 0 for no size limit, or a number of megabytes", c.Logging.MaxMegabytes))
	}
	return errors.Join(errs...)
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// PointsAtListen reports whether a URL names janus's own listen address:
// the same port, and the same host or a loopback name for a listen address
// on loopback or on every interface. Validate uses it to refuse a backend
// that is janus itself, where janus would forward each request to itself
// until something gave out (#216); doctor uses it to tell whether Claude
// Code is pointed at janus (#231).
func PointsAtListen(listen, raw string) bool {
	listenHost, listenPort, err := net.SplitHostPort(listen)
	if err != nil {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return false
	}
	port := u.Port()
	if port == "" {
		switch u.Scheme {
		case "http":
			port = "80"
		case "https":
			port = "443"
		}
	}
	if port != listenPort {
		return false
	}
	host := u.Hostname()
	if strings.EqualFold(host, listenHost) {
		return true
	}
	return isLoopback(host) && (isLoopback(listenHost) || isUnspecified(listenHost))
}

// isUnspecified reports whether host means every interface, as in
// ":8787" or "0.0.0.0:8787".
func isUnspecified(host string) bool {
	if host == "" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsUnspecified()
}

func checkURL(field, raw, example string) error {
	if raw == "" {
		return fmt.Errorf("%s is not set; for example %s", field, example)
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("%s %q is not an http or https URL; for example %s", field, raw, example)
	}
	return nil
}
