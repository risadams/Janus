package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// migration brings a config from one version to the next. It rewrites the
// file's JSON, as decoded into maps, in place, and says in a sentence for
// each change what it did, so that someone whose config was written years
// of releases ago is told what their old settings now mean (#269).
type migration func(raw map[string]any) (notes []string)

// migrations[i] brings a config from version i+1 to i+2. Every release so
// far has only added settings, so there are none yet: the first setting
// renamed, moved, or changed in meaning adds one here, rather than
// breaking every config that still uses the old form.
var migrations []migration

// Version is the config format this janus writes and reads: one more than
// the migrations it knows. A config with no version is version 1, the
// format every release up to the first migration wrote.
func Version() int { return 1 + len(migrations) }

// upgrade brings data, a config's JSON, up to Version, and returns it with
// a note for each change. A config already current comes back as it was.
// One written for a newer janus is refused, saying so, since this janus
// cannot know what its settings mean.
func upgrade(data []byte) (upgraded []byte, notes []string, err error) {
	var head struct {
		Version int `json:"version"`
	}
	// A config that is not JSON, or not an object, is reported by the
	// strict decode that follows, with its line and column.
	if json.Unmarshal(data, &head) != nil {
		return data, nil, nil
	}
	from := max(head.Version, 1)
	if from > Version() {
		return nil, nil, fmt.Errorf("it is written for a newer janus (config version %d; this janus reads up to %d). Update janus (janus update), or use a config written for this version", from, Version())
	}
	if from == Version() {
		return data, nil, nil
	}
	var raw map[string]any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&raw); err != nil {
		return data, nil, nil
	}
	for v := from; v < Version(); v++ {
		notes = append(notes, migrations[v-1](raw)...)
	}
	raw["version"] = Version()
	upgraded, err = json.MarshalIndent(raw, "", "  ")
	return upgraded, notes, err
}

// Rewrite brings the config file at path up to the current version, after
// copying it to path+".bak", and returns what changed. Janus never does
// this by itself: an old config is upgraded in memory each time it is
// read, and rewritten only when someone runs janus config upgrade.
func Rewrite(path string) (notes []string, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	upgraded, notes, err := upgrade(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if bytes.Equal(upgraded, data) {
		return nil, nil
	}
	// The result must be a config this janus reads, before anything is
	// written.
	if _, err := decode(upgraded, path); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path+".bak", data, 0o600); err != nil {
		return nil, fmt.Errorf("could not keep a copy of %s: %w", path, err)
	}
	if err := os.WriteFile(path, append(upgraded, '\n'), 0o600); err != nil {
		return nil, err
	}
	return notes, nil
}

// unknownFieldHint adds, to the error for a setting janus does not know,
// the likely reason when the setting is spelled correctly: a config
// written for a newer janus, which adds settings without changing the
// config's version.
func unknownFieldHint(err error) error {
	if err != nil && strings.Contains(err.Error(), "unknown field") {
		return fmt.Errorf("%w (a typo, or a setting from a newer janus: if so, update janus with janus update)", err)
	}
	return err
}
