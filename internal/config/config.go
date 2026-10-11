package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
)

// DefaultPath is where the config file is looked for when -config is not
// given: next to wherever janus is run from.
const DefaultPath = "janus.config.json"

// Config is the whole of janus.config.json.
type Config struct {
	// Listen is the host:port janus accepts client connections on.
	Listen string `json:"listen"`
	// AllowRemote must be set before Listen may name a non-loopback
	// address. Janus forwards whatever credentials a client sends, so
	// exposing it beyond this machine is a decision, not a default.
	AllowRemote bool `json:"allowRemote"`

	Upstream Backend  `json:"upstream"`
	Local    Local    `json:"local"`
	Features Features `json:"features"`
	Logging  Logging  `json:"logging"`
	// Pricing adds to, or corrects, the prices janus ships with, keyed by
	// model id. Left out of the starter file because the defaults are
	// usually right; see docs/CONFIG.md.
	Pricing map[string]Price `json:"pricing,omitempty"`
}

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
}

// DefaultLocalTimeoutSeconds is how long a local request may take when the
// model's entry does not say. Local models are slow to start on a large
// prompt and slower still if they must be loaded first.
const DefaultLocalTimeoutSeconds = 900

// Features switches each mechanism on. All are off by default. Only route
// is implemented; Validate rejects the others, so a config can never claim
// a behaviour the binary does not have.
type Features struct {
	Route    bool `json:"route"`
	Escalate bool `json:"escalate"`
	Compress bool `json:"compress"`
	Guard    bool `json:"guard"`
}

// Logging controls what janus writes to disk. Everything goes under Dir:
// the ledger, which is always kept, in Dir/ledger, and captures in
// Dir/captures.
type Logging struct {
	Dir string `json:"dir"`
	// Capture saves every request and response in full. Captures contain
	// prompts, code, and tool output, so they are pruned by the two limits
	// below; zero for either means no limit.
	Capture      bool `json:"capture"`
	RetainDays   int  `json:"retainDays"`
	MaxMegabytes int  `json:"maxMegabytes"`
}

// Default is a pass-through proxy on loopback that keeps a ledger and a
// week of captures: the Anthropic API upstream, LM Studio addressed by
// hostname rather than localhost so the same config reaches the intended
// instance from any machine, and every feature off.
func Default() Config {
	return Config{
		Listen:   "127.0.0.1:8787",
		Upstream: Backend{BaseURL: "https://api.anthropic.com"},
		Local:    Local{BaseURL: "http://studio.tuesday.local:1234"},
		Logging:  Logging{Dir: "logs", Capture: true, RetainDays: 7, MaxMegabytes: 1024},
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
		return cfg, true, nil
	}
	cfg, err = Read(path)
	return cfg, false, err
}

// Read is Load for commands that only look: if path does not exist it
// returns the defaults and writes nothing.
func Read(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return Default(), nil
	}
	if err != nil {
		return Config{}, err
	}
	cfg := Default()
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
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
	errs = append(errs, checkURL("local.baseUrl", c.Local.BaseURL, "http://studio.tuesday.local:1234"))

	for _, f := range []struct {
		name string
		on   bool
	}{
		{"escalate", c.Features.Escalate},
		{"compress", c.Features.Compress},
		{"guard", c.Features.Guard},
	} {
		if f.on {
			errs = append(errs, fmt.Errorf("features.%s is not available in this version; set it to false", f.name))
		}
	}
	if c.Features.Route && len(c.Local.Models) == 0 {
		errs = append(errs, errors.New(`features.route is on but local.models is empty; add an entry such as "janus-local": {"model": "<id of a model in LM Studio>"}`))
	}
	for name, m := range c.Local.Models {
		switch {
		case name == "":
			errs = append(errs, errors.New("local.models has an entry with an empty name"))
		case m.Model == "":
			errs = append(errs, fmt.Errorf("local.models.%s.model is not set; give the id of a model in LM Studio", name))
		case m.ContextTokens < 0 || m.TimeoutSeconds < 0:
			errs = append(errs, fmt.Errorf("local.models.%s has a negative contextTokens or timeoutSeconds; use 0 for the default", name))
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
