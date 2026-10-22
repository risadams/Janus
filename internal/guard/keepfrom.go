package guard

import (
	"fmt"
	"sort"
	"strings"

	"github.com/risadams/janus/internal/anthropic"
)

// KeepFrom keeps named content from some providers and not others
// (guard.keepFrom, #406): an employer may approve Anthropic and not
// OpenAI, or the reverse. Each provider has a policy, read as the
// local-only policy is; a request holding what it names may not be sent
// there, though it may go elsewhere.
type KeepFrom struct {
	policies   map[string]*Policy
	everything map[string]bool
}

// NewKeepFrom builds the check from each provider's paths and markers, or
// all of its requests when everything is set.
func NewKeepFrom() *KeepFrom {
	return &KeepFrom{policies: map[string]*Policy{}, everything: map[string]bool{}}
}

// Add keeps what paths and markers name from provider, or everything.
func (k *KeepFrom) Add(provider string, paths, markers []string, everything bool) error {
	p, err := NewPolicy(paths, markers)
	if err != nil {
		return err
	}
	k.policies[provider], k.everything[provider] = p, everything
	return nil
}

// Barred says why req may not be sent to provider, naming the policy and
// what in the request it matched, never the content; "" if it may. A
// line that only quotes a file the policy names counts too: the request
// would carry it.
func (k *KeepFrom) Barred(provider string, req *anthropic.Request) string {
	if k == nil {
		return ""
	}
	if k.everything[provider] {
		return fmt.Sprintf("guard.keepFrom.%s keeps every request from it", provider)
	}
	p := k.policies[provider]
	if p == nil {
		return ""
	}
	m := p.Check(req)
	reasons := append(append([]string(nil), m.Reasons...), m.Quoted...)
	if len(reasons) == 0 {
		return ""
	}
	sort.Strings(reasons)
	return fmt.Sprintf("guard.keepFrom.%s keeps this from it (%s)", provider, strings.Join(reasons, "; "))
}
