package route

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/risadams/janus/internal/proxy"
)

// claudeSaid is an exchange in which Claude answered with the five-hour
// limit used as given, resetting at resets.
func claudeSaid(fiveHour float64, resets time.Time) proxy.Exchange {
	h := http.Header{}
	h.Set("Anthropic-Ratelimit-Unified-5h-Utilization", strconv.FormatFloat(fiveHour, 'f', 2, 64))
	h.Set("Anthropic-Ratelimit-Unified-5h-Reset", strconv.FormatInt(resets.Unix(), 10))
	return proxy.Exchange{Backend: "upstream", Status: 200, ResponseHeader: h}
}

func TestARuleCanWaitForAUsageLimitToFill(t *testing.T) {
	local, _ := lmStudio(t, nil)
	r := router(local, nil)
	r.Rules = []Rule{{Name: "pace", Class: Subagent, Route: RouteLocal, Local: qwen["janus-local"], IfUsageAbove: map[string]float64{"5h": 0.8}}}
	plan := func() (proxy.Plan, string) {
		p, _, trace := r.Explain(context.Background(), subagentHeader(), parse(t, opusRequest))
		if p.Release != nil {
			p.Release()
		}
		return p, strings.Join(trace, "\n")
	}
	later := time.Now().Add(2 * time.Hour)

	if p, trace := plan(); p.Backend.Name() != "upstream" || !strings.Contains(trace, "5h limit use not known yet") {
		t.Errorf("before Claude has said anything: %s\n%s", p.Backend.Name(), trace)
	}
	r.Report(claudeSaid(0.42, later))
	if p, trace := plan(); p.Backend.Name() != "upstream" || !strings.Contains(trace, "5h limit 42% used, not over 80%") {
		t.Errorf("at 42%%: %s\n%s", p.Backend.Name(), trace)
	}
	r.Report(claudeSaid(0.83, later))
	if p, _ := plan(); p.Backend.Name() != "local" || !strings.Contains(p.Reason, "rule pace (5h limit 83% used, over 80%)") {
		t.Errorf("at 83%%: %s, reason %q; want local, saying why", p.Backend.Name(), p.Reason)
	}
	// Once the limit's reset time has passed, its use is unknown again.
	r.Report(claudeSaid(0.95, time.Now().Add(-time.Minute)))
	if p, trace := plan(); p.Backend.Name() != "upstream" || !strings.Contains(trace, "not known yet") {
		t.Errorf("after the reset: %s\n%s", p.Backend.Name(), trace)
	}
	// A local answer says nothing about Claude's limits.
	r.Report(claudeSaid(0.90, later))
	r.Report(proxy.Exchange{Backend: "local", Status: 200})
	if p, _ := plan(); p.Backend.Name() != "local" {
		t.Errorf("a local answer cleared what Claude last said: %s", p.Backend.Name())
	}
}
