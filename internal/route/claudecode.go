package route

import (
	"net/http"
	"strings"
)

// TestedClaudeCode is the range of Claude Code releases janus has been
// tested with. Oldest is the release the hand-built fixtures are shaped
// after; Newest is the newest in the captures TestRealCapturesAreClassified
// last passed over, and that test fails on a newer one until Newest is
// raised, so the range moves when the captures are refreshed (NFR-5).
// janus doctor warns when Claude Code is outside it (#267), since a change
// in what Claude Code sends can quietly break classification.
var TestedClaudeCode = struct{ Oldest, Newest string }{"2.1.296", "2.1.301"}

// ClaudeCodeVersion is the Claude Code release a request's User-Agent
// names, such as 2.1.301 from "claude-cli/2.1.301 (external, cli)", or ""
// when the request is from some other client.
func ClaudeCodeVersion(header http.Header) string {
	rest, ok := strings.CutPrefix(header.Get("User-Agent"), "claude-cli/")
	if !ok {
		return ""
	}
	v, _, _ := strings.Cut(rest, " ")
	// Only a plain version is kept: the ledger is no place for whatever
	// else a User-Agent might carry.
	if v == "" || len(v) > 32 || strings.Trim(v, "0123456789.") != "" {
		return ""
	}
	return v
}
