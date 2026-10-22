package route

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
)

// janus use (#379) moves a session, or every session, to a provider by
// hand, without restarting Claude Code. The choice is kept here, by
// session id ("" for every session), as the route header's value would
// be: "upstream", "openai", or "local". It holds until it is cleared or
// until its provider cannot answer, and a session's own choice beats the
// one for every session.
type manual struct {
	mu     sync.Mutex
	chosen map[string]string
}

func (m *manual) get(session string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c, ok := m.chosen[session]; ok && session != "" {
		return c
	}
	return m.chosen[""]
}

func (m *manual) set(session, choice string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.chosen == nil {
		m.chosen = map[string]string{}
	}
	m.chosen[session] = choice
}

// clear drops the choice that applies to session: its own if it has one,
// else the one for every session.
func (m *manual) clear(session string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.chosen[session]; ok {
		delete(m.chosen, session)
		return
	}
	delete(m.chosen, "")
}

func (m *manual) all() map[string]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]string, len(m.chosen))
	for k, v := range m.chosen {
		out[k] = v
	}
	return out
}

// ControlPath is where janus use reaches the running janus, on its own
// listener: Claude Code never sends there, and it is answered by janus,
// never forwarded.
const ControlPath = "/janus/use"

// Use is what janus use sends: a provider for a session, or every session
// with Session empty; or Clear to drop the choice.
type Use struct {
	Provider string `json:"provider,omitempty"`
	Session  string `json:"session,omitempty"`
	Clear    bool   `json:"clear,omitempty"`
}

// UseProviders maps what janus use accepts to the route it means.
var UseProviders = map[string]string{"claude": RouteUpstream, "codex": RouteOpenAI, "openai": RouteOpenAI, "local": RouteLocal}

// Control answers janus use. It takes requests from this machine only,
// and none from a web page: a page in a browser here could otherwise move
// a session to a provider its owner did not choose. GET lists the choices
// in force; POST makes or clears one.
func (r *Router) Control() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		host, _, err := net.SplitHostPort(req.RemoteAddr)
		if ip := net.ParseIP(host); err != nil || ip == nil || !ip.IsLoopback() || req.Header.Get("Origin") != "" {
			http.Error(w, "janus: janus use is answered for programs on this machine only", http.StatusForbidden)
			return
		}
		switch req.Method {
		case http.MethodGet:
		case http.MethodPost:
			var u Use
			if err := json.NewDecoder(io.LimitReader(req.Body, 4096)).Decode(&u); err != nil {
				http.Error(w, "janus: "+err.Error(), http.StatusBadRequest)
				return
			}
			if u.Clear {
				r.manual.clear(u.Session)
				break
			}
			route, ok := UseProviders[u.Provider]
			switch {
			case !ok:
				http.Error(w, fmt.Sprintf("janus: %q is not claude, codex, openai, or local", u.Provider), http.StatusBadRequest)
				return
			case route == RouteOpenAI && r.OpenAI == nil:
				http.Error(w, "janus: there is no openai section in janus's config", http.StatusBadRequest)
				return
			case route == RouteUpstream && r.NoUpstream:
				http.Error(w, "janus: upstream is off in janus's config", http.StatusBadRequest)
				return
			case route == RouteLocal:
				if _, problem := r.named(""); problem != "" {
					http.Error(w, "janus: "+problem, http.StatusBadRequest)
					return
				}
			}
			r.manual.set(u.Session, route)
		default:
			http.Error(w, "janus: GET or POST", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(r.manual.all()) // a client that left gets nothing either way
	})
}

// endManual drops a janus use choice whose provider could not answer this
// exchange: tried and failed, passed over, or answering with an error.
func (r *Router) endManual(session, reason string, backendName string, tried []string, failed bool) {
	chosen, ok := strings.CutPrefix(reason, "janus use ")
	if !ok {
		return
	}
	chosen, _, _ = strings.Cut(chosen, ":")
	chosen, _, _ = strings.Cut(chosen, ",")
	for _, t := range tried {
		if t == chosen {
			r.manual.clear(session)
			return
		}
	}
	if failed && backendName == chosen {
		r.manual.clear(session)
	}
}

// Choices describes the janus use choices in force, for a person.
func Choices(chosen map[string]string) []string {
	var out []string
	for session, route := range chosen {
		who := "every session"
		if session != "" {
			who = "session " + session
		}
		out = append(out, fmt.Sprintf("%s: %s", who, route))
	}
	sort.Strings(out)
	return out
}
