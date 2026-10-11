package proxy

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// SameMachine wraps a handler so that it only answers requests addressed
// to this machine by a program on it, which is what Claude Code's are.
//
// Listening on loopback keeps other machines out, but not web pages: a
// page in the user's browser can send requests to 127.0.0.1, and by
// pointing a name of its own at 127.0.0.1 it can read the answers too.
// Both are told apart from a local program by what they must send. A page
// that has rebound a name sends that name as Host; a page sending
// cross-site sends its Origin. Either is refused.
//
// refused, if not nil, is told about each request turned away.
func SameMachine(next http.Handler, refused func(r *http.Request, why string)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		why := ""
		if !loopbackHost(r.Host) {
			why = fmt.Sprintf("it is addressed to the host %q, which is not this machine", r.Host)
		} else if origin := r.Header.Get("Origin"); origin != "" {
			if u, err := url.Parse(origin); err != nil || !loopbackHost(u.Host) {
				why = fmt.Sprintf("it comes from a web page at %q", origin)
			}
		}
		if why != "" {
			if refused != nil {
				refused(r, why)
			}
			writeError(w, http.StatusForbidden, "permission_error", "janus: this request was refused because "+why+
				". janus answers programs on this machine only; set allowRemote to change that.")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// loopbackHost reports whether a host, with or without a port, names this
// machine: localhost or a loopback address.
func loopbackHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
