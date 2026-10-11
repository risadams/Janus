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
	Local    Backend  `json:"local"`
	Features Features `json:"features"`
	Logging  Logging  `json:"logging"`
}

// Backend is one model server janus can send requests to.
type Backend struct {
	BaseURL string `json:"baseUrl"`
}

// Features switches each mechanism on. All are off by default, and none is
// implemented yet: Validate rejects any that is set, so a config can never
// claim a behaviour the binary does not have.
type Features struct {
	Route    bool `json:"route"`
	Escalate bool `json:"escalate"`
	Compress bool `json:"compress"`
	Guard    bool `json:"guard"`
}

// Logging controls what janus writes to disk.
type Logging struct {
	Dir string `json:"dir"`
}

// Default is a pass-through proxy on loopback: the Anthropic API upstream,
// LM Studio addressed by hostname rather than localhost so the same config
// reaches the intended instance from any machine, and every feature off.
func Default() Config {
	return Config{
		Listen:   "127.0.0.1:8787",
		Upstream: Backend{BaseURL: "https://api.anthropic.com"},
		Local:    Backend{BaseURL: "http://studio.tuesday.local:1234"},
		Logging:  Logging{Dir: "logs"},
	}
}

// Load reads path, or writes a starter file holding the defaults and
// returns it with created=true if path does not exist. Fields missing from
// the file keep their defaults; fields janus does not know are an error, so
// a typo is reported rather than silently ignored.
func Load(path string) (cfg Config, created bool, err error) {
	data, readErr := os.ReadFile(path)
	if os.IsNotExist(readErr) {
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
	if readErr != nil {
		return Config{}, false, readErr
	}

	cfg = Default()
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return Config{}, false, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, false, nil
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
		{"route", c.Features.Route},
		{"escalate", c.Features.Escalate},
		{"compress", c.Features.Compress},
		{"guard", c.Features.Guard},
	} {
		if f.on {
			errs = append(errs, fmt.Errorf("features.%s is not available in this version; set it to false", f.name))
		}
	}

	if c.Logging.Dir == "" {
		errs = append(errs, errors.New(`logging.dir is empty; set a directory, for example "logs"`))
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
