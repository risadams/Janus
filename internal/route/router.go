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

// Routes a rule can choose.
const (
	RouteUpstream = "upstream"
	RouteLocal    = "local"
	// RouteShadow sends the request upstream as usual and asks for it to be
	// run on the local model afterwards, so the two answers can be compared
	// without the local one ever reaching the client.
	RouteShadow = "shadow"
	// RouteEscalate is local first with Claude as the fallback. It is
	// accepted so that configs can be written ahead of it, and until it is
	// built it sends the request upstream.
	RouteEscalate = "local-then-escalate"
)

// Rule sends the requests it matches somewhere. A request must meet every
// condition that is set; a rule with none set matches everything.
type Rule struct {
	Name string
	// Conditions.
	Class     Class // empty matches any class
	Model     string
	MaxTokens int   // zero means no limit
	Tools     *bool // nil matches either way
	// Outcome.
	Route string
	// Local is the model to use, for every route but upstream.
	Local LocalModel
}

// Facts is what a rule is matched against.
type Facts struct {
	Class  Class
	Model  string
	Tokens int // estimated
	Tools  bool
}

// FactsOf gathers the facts about a request.
func FactsOf(header http.Header, req *anthropic.Request) Facts {
	return Facts{
		Class:  Classify(http.MethodPost, "/v1/messages", header),
		Model:  req.Model(),
		Tokens: EstimateTokens(req.Bytes()),
		Tools:  len(req.Tools()) > 0,
	}
}

// mismatch says why a rule does not match, or returns "" if it does.
func (rule Rule) mismatch(f Facts) string {
	switch {
	case rule.Class != "" && rule.Class != f.Class:
		return fmt.Sprintf("class is %s, not %s", f.Class, rule.Class)
	case rule.Model != "" && rule.Model != f.Model:
		return fmt.Sprintf("model is %s, not %s", f.Model, rule.Model)
	case rule.MaxTokens > 0 && f.Tokens > rule.MaxTokens:
		return fmt.Sprintf("about %d tokens, over the rule's %d", f.Tokens, rule.MaxTokens)
	case rule.Tools != nil && *rule.Tools != f.Tools:
		if f.Tools {
			return "the request offers tools"
		}
		return "the request offers no tools"
	}
	return ""
}

func (rule Rule) label(index int) string {
	if rule.Name != "" {
		return "rule " + rule.Name
	}
	return fmt.Sprintf("rule %d", index+1)
}

// Router decides which backend answers a Messages request.
//
// Two things can send a request to the local model. A request whose model
// is an alias always goes local: that puts the choice in the client's
// hands, and has no upstream to fall back to. Otherwise the rules are
// tried in order and the first match decides; a request that matches none
// goes upstream untouched.
type Router struct {
	Upstream backend.Backend
	Local    *backend.Local
	// Aliases maps a requested model name to a local model.
	Aliases map[string]LocalModel
	Rules   []Rule
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
	plan, refusal, _ := r.Explain(ctx, header, req)
	return plan, refusal
}

// Explain is Plan with its reasoning: one line per step considered, ending
// with the decision.
func (r *Router) Explain(ctx context.Context, header http.Header, req *anthropic.Request) (plan proxy.Plan, refusal *proxy.Refusal, trace []string) {
	// Whatever sends a request upstream, a history that a local model had a
	// turn in is repaired first, or the API would reject it. For a
	// conversation that only ever went to Claude this finds nothing and
	// the request goes exactly as it arrived.
	upstream := func(reason string) proxy.Plan {
		plan := proxy.Plan{Backend: r.Upstream, Route: RouteUpstream, Reason: reason}
		if backend.RepairForUpstream(req) {
			plan.Body = req.Bytes()
			plan.Stages = []string{backend.StageDropThinking}
			trace = append(trace, "removed the unsigned thinking a local answer left in the history, which the API would reject")
		}
		return plan
	}
	facts := FactsOf(header, req)

	// An alias has no upstream model behind it, so there is nowhere to
	// fall back to: a request that cannot go local is refused, with why.
	if local, ok := r.Aliases[facts.Model]; ok {
		reason := "model alias " + facts.Model
		plan, problem := r.local(ctx, req, local, facts.Model, reason)
		if problem != nil {
			trace = append(trace, fmt.Sprintf("%s: refused: %s", reason, problem.message))
			return proxy.Plan{}, &proxy.Refusal{
				Status: problem.status, Type: problem.kind, Message: "janus: " + problem.message,
				Route: RouteLocal, Reason: reason + ": " + problem.message,
			}, trace
		}
		trace = append(trace, fmt.Sprintf("%s: local, to %s", reason, local.ID))
		return plan, nil, trace
	}

	for i, rule := range r.Rules {
		label := rule.label(i)
		if why := rule.mismatch(facts); why != "" {
			trace = append(trace, fmt.Sprintf("%s: no match (%s)", label, why))
			continue
		}
		switch rule.Route {
		case RouteLocal:
			// The client asked for a real model, so a request that
			// cannot go local goes to that model instead of failing.
			plan, problem := r.local(ctx, req, rule.Local, facts.Model, label)
			if problem != nil {
				reason := fmt.Sprintf("%s: %s; sent upstream", label, problem.message)
				trace = append(trace, reason)
				return upstream(reason), nil, trace
			}
			trace = append(trace, fmt.Sprintf("%s: local, to %s", label, rule.Local.ID))
			return plan, nil, trace
		case RouteShadow:
			plan := upstream(label)
			plan.Shadow = &proxy.Shadow{Model: rule.Local.ID, ContextTokens: rule.Local.ContextTokens, Timeout: rule.Local.Timeout, Reason: label}
			trace = append(trace, fmt.Sprintf("%s: upstream, and shadowed on %s", label, rule.Local.ID))
			return plan, nil, trace
		case RouteEscalate:
			reason := label + ": local-then-escalate is not available yet; sent upstream"
			trace = append(trace, reason)
			return upstream(reason), nil, trace
		default:
			trace = append(trace, label+": upstream")
			return upstream(label), nil, trace
		}
	}

	trace = append(trace, "no rule matched: upstream")
	return upstream("passthrough"), nil, trace
}

// problem is why a request cannot go to the local model.
type problem struct {
	status  int
	kind    string
	message string
}

// local builds the plan for sending req to a local model. The response
// reports reportAs as its model, which is the name the client asked for.
//
// It works on a copy of the request, so that a request which turns out not
// to fit can still go upstream exactly as it arrived.
func (r *Router) local(ctx context.Context, req *anthropic.Request, local LocalModel, reportAs, reason string) (proxy.Plan, *problem) {
	limit := local.ContextTokens
	info, found, err := r.Local.Model(ctx, local.ID)
	switch {
	case err != nil:
		return proxy.Plan{}, &problem{http.StatusBadGateway, "api_error",
			fmt.Sprintf("could not reach LM Studio at %s: %v", r.Local.URL(), err)}
	case !found:
		// LM Studio would not reject this; it would answer with whatever
		// model happens to be loaded.
		return proxy.Plan{}, &problem{http.StatusBadRequest, "invalid_request_error",
			fmt.Sprintf("LM Studio does not have local model %q", local.ID)}
	case limit == 0:
		limit = info.LoadedContext // zero if not loaded: nothing to check against yet
	}

	adapted, err := anthropic.ParseRequest(req.Bytes())
	if err != nil {
		return proxy.Plan{}, &problem{http.StatusBadRequest, "invalid_request_error", "cannot read this request: " + err.Error()}
	}
	stages, err := backend.Normalize(adapted, local.ID)
	if err != nil {
		return proxy.Plan{}, &problem{http.StatusBadRequest, "invalid_request_error", "cannot adapt this request for the local model: " + err.Error()}
	}
	body := adapted.Bytes()

	if estimate := EstimateTokens(body); limit > 0 && estimate > limit {
		return proxy.Plan{}, &problem{http.StatusBadRequest, "invalid_request_error",
			fmt.Sprintf("this request is about %d tokens, more than the %d-token context of local model %q", estimate, limit, local.ID)}
	}

	return proxy.Plan{
		Backend:       r.Local,
		Route:         RouteLocal,
		Reason:        reason,
		Stages:        stages,
		Body:          body,
		BackendModel:  local.ID,
		ResponseModel: reportAs,
		Timeout:       local.Timeout,
	}, nil
}
