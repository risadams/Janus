package proxy_test

import (
	"github.com/risadams/janus/internal/backend"
	"github.com/risadams/janus/internal/config"
	"github.com/risadams/janus/internal/openai"
	"github.com/risadams/janus/internal/proxy"
	"github.com/risadams/janus/internal/route"
)

// Configuring OpenAI must not change a byte of a request that goes to
// Claude (#409). The router here has an OpenAI backend and a rule listing
// it after Claude, which Claude answering leaves unused.
func init() {
	proxy.FidelityPlanners["OpenAI configured, unused"] = func(upstream backend.Backend) proxy.Planner {
		cfg := config.OpenAI{BaseURL: "http://127.0.0.1:1", Key: "env:OPENAI_API_KEY"}
		oa, err := openai.NewBackend(cfg, openai.Key{}, nil)
		if err != nil {
			panic(err)
		}
		r := &route.Router{Upstream: upstream, OpenAI: oa, Rules: []route.Rule{{
			Name: "all", Route: route.RouteUpstream,
			Fallback: []route.Provider{{Name: route.RouteUpstream}, {Name: route.RouteOpenAI}},
		}}}
		return r.Plan
	}
}
