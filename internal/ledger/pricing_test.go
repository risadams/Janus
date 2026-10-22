package ledger

import (
	"math"
	"strings"
	"testing"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// A real session, from the ledger of a Claude Code run on claude-opus-5-5:
// 14 uncached input, 1,601 output, 103,878 read from cache, 20,480 written
// with a 5-minute lifetime and 11,819 with an hour.
//
//	input         14 x $4.00  =      56.0
//	output     1,601 x $20.00 =  32,020.0
//	read     103,878 x $0.20  =  20,775.6
//	write 5m  20,480 x $5.00  = 102,400.0
//	write 1h  11,819 x $8.00  =  94,552.0
//	                            249,803.6 millionths of a dollar
func TestCostWorkedExample(t *testing.T) {
	u := Usage{Input: 14, Output: 1601, CacheRead: n(103878), CacheWrite: n(32299), CacheWrite5m: n(20480), CacheWrite1h: n(11819)}
	usd, unpriced := DefaultPrices.Cost("claude-opus-5-5", u)
	if unpriced != "" || !near(usd, 0.2498036) {
		t.Errorf("Cost = $%.7f (%q), want $0.2498036", usd, unpriced)
	}
}

func TestCacheReadDiscountDiffersByModel(t *testing.T) {
	million := Usage{CacheRead: n(1_000_000)}
	for model, want := range map[string]float64{
		"claude-opus-5-5":   0.20, // a twentieth of input
		"claude-fable-5-1":  0.25, // a fortieth
		"claude-opus-5":     0.50, // a tenth
		"claude-sonnet-5-5": 0.20,
		"claude-haiku-4-5":  0.10,
	} {
		if usd, _ := DefaultPrices.Cost(model, million); !near(usd, want) {
			t.Errorf("%s: a million cached tokens cost $%.4f, want $%.2f", model, usd, want)
		}
	}
}

func TestCacheWritesAreMultiplesOfInput(t *testing.T) {
	for model, p := range DefaultPrices {
		if !strings.HasPrefix(model, "claude-") {
			continue // OpenAI's are its own; see TestOpenAIAnswersArePricedAtOpenAIsRates
		}
		if !near(p.CacheWrite5m, p.Input*1.25) || !near(p.CacheWrite1h, p.Input*2) {
			t.Errorf("%s: cache writes $%.4f / $%.4f are not 1.25x and 2x the input price $%.4f", model, p.CacheWrite5m, p.CacheWrite1h, p.Input)
		}
		if p.CacheRead >= p.Input || p.Output <= p.Input {
			t.Errorf("%s: implausible prices %+v", model, p)
		}
	}
}

func TestCacheWriteWithoutLifetimeUsesTheFiveMinuteRate(t *testing.T) {
	usd, unpriced := DefaultPrices.Cost("claude-opus-5-5", Usage{CacheWrite: n(1_000_000)})
	if unpriced != "" || !near(usd, 5.00) {
		t.Errorf("Cost = $%.2f (%q), want $5.00", usd, unpriced)
	}
	// When the split is given it is used, even where it disagrees with the
	// total: the split is what is billed.
	usd, _ = DefaultPrices.Cost("claude-opus-5-5", Usage{CacheWrite: n(1_000_000), CacheWrite5m: n(0), CacheWrite1h: n(1_000_000)})
	if !near(usd, 8.00) {
		t.Errorf("Cost with a 1h split = $%.2f, want $8.00", usd)
	}
}

func TestLookupHandlesVersionSuffixes(t *testing.T) {
	out := Usage{Output: 1_000_000}
	for model, want := range map[string]float64{
		"claude-haiku-4-5-20251001": 5,  // dated snapshot
		"claude-opus-5-5":           20, // not mistaken for claude-opus-5
		"claude-opus-5-5-20260901":  20, // the longest matching id wins
		"claude-opus-5-20260601":    25,
		"claude-opus-4-8@20260301":  25, // Vertex-style separator
	} {
		usd, unpriced := DefaultPrices.Cost(model, out)
		if unpriced != "" || !near(usd, want) {
			t.Errorf("%s: a million output tokens cost $%.2f (%q), want $%.0f", model, usd, unpriced, want)
		}
	}
}

func TestUnpricedIsReportedNotZeroed(t *testing.T) {
	cases := map[string]struct {
		model string
		usage Usage
		want  string
	}{
		"unknown model":       {"qwen/qwen3.8-27b", Usage{Input: 10, Output: 10}, "no price for qwen/qwen3.8-27b"},
		"a near miss":         {"claude-opus-55", Usage{Output: 10}, "no price for claude-opus-55"},
		"no model":            {"", Usage{Output: 10}, "no model reported"},
		"fast mode":           {"claude-opus-5-5", Usage{Output: 10, Speed: "fast"}, "fast mode"},
		"haiku 5.5 over 100k": {"claude-haiku-5-5", Usage{Input: 1, CacheRead: n(60_000), CacheWrite: n(40_000)}, "over 100000 tokens"},
	}
	for name, c := range cases {
		usd, unpriced := DefaultPrices.Cost(c.model, c.usage)
		if usd != 0 || !strings.Contains(unpriced, c.want) {
			t.Errorf("%s: Cost = $%f, %q; want $0 and a reason containing %q", name, usd, unpriced, c.want)
		}
	}

	// Exactly at the limit is still on the standard schedule.
	if _, unpriced := DefaultPrices.Cost("claude-haiku-5-5", Usage{CacheRead: n(100_000)}); unpriced != "" {
		t.Errorf("a 100,000-token haiku prompt is unpriced: %s", unpriced)
	}
}

func TestWithOverridesWithoutChangingTheDefaults(t *testing.T) {
	prices := DefaultPrices.With(Prices{
		"claude-opus-5-5":  {Input: 1, Output: 2},
		"my-gateway-model": {Input: 3, Output: 4},
	})
	if usd, _ := prices.Cost("claude-opus-5-5", Usage{Input: 1_000_000}); !near(usd, 1) {
		t.Errorf("override not applied: $%.2f", usd)
	}
	if usd, unpriced := prices.Cost("my-gateway-model", Usage{Output: 1_000_000}); unpriced != "" || !near(usd, 4) {
		t.Errorf("added model: $%.2f (%q)", usd, unpriced)
	}
	if DefaultPrices["claude-opus-5-5"].Input != 4 {
		t.Error("With changed the shipped table")
	}
	if _, ok := DefaultPrices["my-gateway-model"]; ok {
		t.Error("With added to the shipped table")
	}
}

func TestOpenAIAnswersArePricedAtOpenAIsRates(t *testing.T) {
	read, write := 90_000, 1_000
	u := Usage{Input: 9_000, Output: 2_000, CacheRead: &read, CacheWrite: &write, CacheWrite5m: &write}
	// OpenAI reports a dated model; the ledger keeps its name.
	usd, unpriced := DefaultPrices.Cost("gpt-6.1-sol-2026-08-01", u)
	want := (9_000*2 + 90_000*0.10 + 1_000*2.5 + 2_000*10) / 1e6
	if unpriced != "" || math.Abs(usd-want) > 1e-9 {
		t.Errorf("cost = %v (%q), want %v", usd, unpriced, want)
	}

	long := 300_000
	u = Usage{Input: 1_000, Output: 1_000, CacheRead: &long}
	usd, _ = DefaultPrices.Cost("gpt-6.1-sol", u)
	want = (2*(1_000*2+300_000*0.10) + 1.5*1_000*10) / 1e6
	if math.Abs(usd-want) > 1e-9 {
		t.Errorf("a prompt over 272K costs %v, want %v at the long-prompt rates", usd, want)
	}
}
