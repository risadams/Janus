package route

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/risadams/janus/internal/anthropic"
	"github.com/risadams/janus/internal/backend"
	"github.com/risadams/janus/internal/proxy"
)

// LocalModel is a local model a request can be sent to.
type LocalModel struct {
	// ID is the model's id in LM Studio.
	ID string
	// ContextTokens is the largest prompt to send it. Zero means ask LM
	// Studio what context the model is loaded with.
	ContextTokens int
	// Timeout bounds one request. Zero means no limit.
	Timeout time.Duration
}

// Router decides which backend answers a Messages request.
//
// It has one rule so far: a request whose model is a configured alias goes
// to the local model that alias names. This puts the choice in the
// client's hands. Claude Code can be told to use an alias for its
// subagents or its small, fast model, and everything else stays upstream.
type Router struct {
	Upstream backend.Backend
	Local    *backend.Local
	// Aliases maps a requested model name to a local model.
	Aliases map[string]LocalModel
}

// bytesPerToken converts a request's size to a token estimate. janus has no
// tokenizer, and the real ratio for Claude Code requests was measured
// between 2.75 and 4.1 bytes per token (docs/research/traffic.md). Three
// errs toward over-counting, which leaves the model room to answer.
const bytesPerToken = 3

// EstimateTokens is a deliberately high guess at a request's size.
func EstimateTokens(body []byte) int {
	return (len(body) + bytesPerToken - 1) / bytesPerToken
}

// Plan implements proxy.Planner.
func (r *Router) Plan(ctx context.Context, header http.Header, req *anthropic.Request) (proxy.Plan, *proxy.Refusal) {
	alias := req.Model()
	local, ok := r.Aliases[alias]
	if !ok {
		return proxy.Plan{Backend: r.Upstream, Route: "upstream", Reason: "passthrough"}, nil
	}
	reason := "model alias " + alias

	refuse := func(status int, kind, message string) (proxy.Plan, *proxy.Refusal) {
		return proxy.Plan{}, &proxy.Refusal{Status: status, Type: kind, Message: "janus: " + message, Route: "local", Reason: reason + ": " + message}
	}

	// An alias has no upstream model behind it, so there is nowhere to
	// fall back to: a request that cannot go local is refused, and says why.
	limit := local.ContextTokens
	info, found, err := r.Local.Model(ctx, local.ID)
	switch {
	case err != nil:
		return refuse(http.StatusBadGateway, "api_error",
			fmt.Sprintf("could not reach LM Studio at %s to serve %q: %v", r.Local.URL(), alias, err))
	case !found:
		// LM Studio would not reject this; it would answer with whatever
		// model happens to be loaded.
		return refuse(http.StatusBadRequest, "invalid_request_error",
			fmt.Sprintf("%q is mapped to local model %q, which LM Studio does not have", alias, local.ID))
	case limit == 0:
		limit = info.LoadedContext // zero if not loaded: nothing to check against yet
	}

	stages, err := backend.Normalize(req, local.ID)
	if err != nil {
		return refuse(http.StatusBadRequest, "invalid_request_error", "cannot adapt this request for the local model: "+err.Error())
	}
	body := req.Bytes()

	if estimate := EstimateTokens(body); limit > 0 && estimate > limit {
		return refuse(http.StatusBadRequest, "invalid_request_error",
			fmt.Sprintf("this request is about %d tokens, more than the %d-token context of local model %q", estimate, limit, local.ID))
	}

	return proxy.Plan{
		Backend:       r.Local,
		Route:         "local",
		Reason:        reason,
		Stages:        stages,
		Body:          body,
		BackendModel:  local.ID,
		ResponseModel: alias,
		Timeout:       local.Timeout,
	}, nil
}
