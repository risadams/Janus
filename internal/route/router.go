package route

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/risadams/janus/internal/anthropic"
	"github.com/risadams/janus/internal/backend"
	"github.com/risadams/janus/internal/proxy"
	"github.com/risadams/janus/internal/validate"
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
	// RouteEscalate is local first with Claude as the fallback: the local
	// model's whole answer is checked before the client sees any of it, and
	// if it fails the checks the request goes upstream instead.
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
	// SkipChecks names validators to leave out when judging a local answer
	// on the escalate route.
	SkipChecks []string
	// TrialTimeout is how long the local model gets to produce its whole
	// answer on the escalate route. It applies as well as the model's own
	// Timeout; whichever is shorter wins.
	TrialTimeout time.Duration
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

	health breaker
}

// A breaker stops rules from sending requests to a local backend that is
// plainly down, so that each request does not have to find that out for
// itself. After threshold failures in a row it opens for cooldown; the
// first request after that is let through to see whether things are
// better.
//
// It counts availability only: the backend could not be reached, timed
// out, or returned a server error. An answer that was merely not good
// enough is the backend working.
type breaker struct {
	mu        sync.Mutex
	failures  int
	openUntil time.Time
	now       func() time.Time // time.Now, except in tests
}

const (
	breakerThreshold = 3
	breakerCooldown  = 30 * time.Second
)

func (b *breaker) clock() time.Time {
	if b.now != nil {
		return b.now()
	}
	return time.Now()
}

// open reports whether the backend should be skipped right now.
func (b *breaker) open() (skip bool, failures int, until time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.clock().Before(b.openUntil), b.failures, b.openUntil
}

func (b *breaker) record(ok bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if ok {
		b.failures, b.openUntil = 0, time.Time{}
		return
	}
	b.failures++
	if b.failures >= breakerThreshold {
		b.openUntil = b.clock().Add(breakerCooldown)
	}
}

// Report tells the router how a finished exchange went, so that it can
// notice the local backend failing. Call it for every exchange.
func (r *Router) Report(ex proxy.Exchange) {
	switch {
	case ex.Attempt != nil && ex.Attempt.Backend == "local":
		r.health.record(!ex.Attempt.Fault)
	case ex.Backend == "local":
		r.health.record(!ex.BackendFault && ex.Status < 500)
	}
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
	upstreamPlan := func(reason string) (plan proxy.Plan, repaired bool) {
		plan = proxy.Plan{Backend: r.Upstream, Route: RouteUpstream, Reason: reason}
		if backend.RepairForUpstream(req) {
			plan.Body = req.Bytes()
			plan.Stages = []string{backend.StageDropThinking}
			return plan, true
		}
		return plan, false
	}
	const repairNote = "removed the unsigned thinking a local answer left in the history, which the API would reject"
	upstream := func(reason string) proxy.Plan {
		plan, repaired := upstreamPlan(reason)
		if repaired {
			trace = append(trace, repairNote)
		}
		return plan
	}
	facts := FactsOf(header, req)

	// An alias has no upstream model behind it, so there is nowhere to
	// fall back to: a request that cannot go local is refused, with why.
	if local, ok := r.Aliases[facts.Model]; ok {
		reason := "model alias " + facts.Model
		plan, _, problem := r.local(ctx, req, local, facts.Model, reason)
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
		case RouteLocal, RouteEscalate:
			// The client asked for a real model, so whatever goes wrong
			// with the local one, the request goes to that model instead
			// of failing.
			if skip, failures, until := r.health.open(); skip {
				reason := fmt.Sprintf("%s: the local backend has failed %d times in a row and is being skipped until %s; sent upstream",
					label, failures, until.Format("15:04:05"))
				trace = append(trace, reason)
				return upstream(reason), nil, trace
			}
			plan, adapted, problem := r.local(ctx, req, rule.Local, facts.Model, label)
			if problem != nil {
				if problem.status == http.StatusBadGateway {
					r.health.record(false) // LM Studio could not be reached
				}
				reason := fmt.Sprintf("%s: %s; sent upstream", label, problem.message)
				trace = append(trace, reason)
				return upstream(reason), nil, trace
			}

			// The fallback is planned now, from the request as it arrived,
			// in case the local attempt fails once it is under way.
			fallback, _ := upstreamPlan(label)
			plan.Else = &fallback
			if rule.Route == RouteEscalate {
				if rule.TrialTimeout > 0 && (plan.Timeout == 0 || rule.TrialTimeout < plan.Timeout) {
					plan.Timeout = rule.TrialTimeout
				}
				skip := rule.SkipChecks
				plan.Accept = func(got proxy.Fetched) (bool, string) {
					answer, readable := anthropic.Assemble(got.Body, got.Header.Get("Content-Type"))
					if _, why := validate.Check(adapted, got.Summary, answer, readable, skip); why != "" {
						return false, why
					}
					return true, ""
				}
				trace = append(trace, fmt.Sprintf("%s: local first, on %s; its answer is checked, and Claude answers instead if it fails", label, rule.Local.ID))
			} else {
				trace = append(trace, fmt.Sprintf("%s: local, to %s", label, rule.Local.ID))
			}
			return plan, nil, trace
		case RouteShadow:
			plan := upstream(label)
			plan.Shadow = &proxy.Shadow{Model: rule.Local.ID, ContextTokens: rule.Local.ContextTokens, Timeout: rule.Local.Timeout, Reason: label}
			trace = append(trace, fmt.Sprintf("%s: upstream, and shadowed on %s", label, rule.Local.ID))
			return plan, nil, trace
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
// It works on a copy of the request, which it also returns, so that a
// request which turns out not to fit can still go upstream exactly as it
// arrived.
func (r *Router) local(ctx context.Context, req *anthropic.Request, local LocalModel, reportAs, reason string) (proxy.Plan, *anthropic.Request, *problem) {
	limit := local.ContextTokens
	info, found, err := r.Local.Model(ctx, local.ID)
	switch {
	case err != nil:
		return proxy.Plan{}, nil, &problem{http.StatusBadGateway, "api_error",
			fmt.Sprintf("could not reach LM Studio at %s: %v", r.Local.URL(), err)}
	case !found:
		// LM Studio would not reject this; it would answer with whatever
		// model happens to be loaded.
		return proxy.Plan{}, nil, &problem{http.StatusBadRequest, "invalid_request_error",
			fmt.Sprintf("LM Studio does not have local model %q", local.ID)}
	case limit == 0:
		limit = info.LoadedContext // zero if not loaded: nothing to check against yet
	}

	adapted, err := anthropic.ParseRequest(req.Bytes())
	if err != nil {
		return proxy.Plan{}, nil, &problem{http.StatusBadRequest, "invalid_request_error", "cannot read this request: " + err.Error()}
	}
	stages, err := backend.Normalize(adapted, local.ID)
	if err != nil {
		return proxy.Plan{}, nil, &problem{http.StatusBadRequest, "invalid_request_error", "cannot adapt this request for the local model: " + err.Error()}
	}
	body := adapted.Bytes()

	if estimate := EstimateTokens(body); limit > 0 && estimate > limit {
		return proxy.Plan{}, nil, &problem{http.StatusBadRequest, "invalid_request_error",
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
	}, adapted, nil
}
