package ledger

import (
	"fmt"
	"strings"
)

// PricesChecked is when DefaultPrices was last checked against Anthropic's
// published price list. Prices change; janus stats prints this date so a
// stale table is visible, and config can override any entry.
const PricesChecked = "2026-10-06"

// Price is what one model costs, in US dollars per million tokens. Cached
// input is priced separately from uncached input in both directions, and
// that difference is most of what decides whether a change saves money.
type Price struct {
	Input        float64
	Output       float64
	CacheRead    float64
	CacheWrite5m float64
	CacheWrite1h float64
	// MaxPromptTokens, when set, is the largest prompt these rates apply
	// to. A larger request is priced on a different schedule that this
	// table does not hold, so its cost is reported as unknown.
	MaxPromptTokens int
}

// standard builds a price from the three published rates. Cache writes are
// 1.25 times the input rate for a 5-minute lifetime and twice it for an
// hour. Cache reads are given explicitly because their discount differs by
// model: a tenth of input on most, less on the newest.
func standard(input, output, cacheRead float64) Price {
	return Price{
		Input: input, Output: output, CacheRead: cacheRead,
		CacheWrite5m: input * 1.25, CacheWrite1h: input * 2,
	}
}

// Prices maps a model id to its price.
type Prices map[string]Price

// DefaultPrices are Anthropic's first-party list prices as of
// PricesChecked. They are estimates of cost at list price: a subscription
// is not billed per token, and other platforms set their own rates.
var DefaultPrices = Prices{
	"claude-fable-5-1":  standard(10, 50, 0.25),
	"claude-mythos-5-1": standard(10, 50, 0.25),
	"claude-fable-5":    standard(10, 50, 1.00),
	"claude-mythos-5":   standard(10, 50, 1.00),
	"claude-opus-5-5":   standard(4, 20, 0.20),
	"claude-opus-5":     standard(5, 25, 0.50),
	"claude-opus-4-8":   standard(5, 25, 0.50),
	"claude-opus-4-7":   standard(5, 25, 0.50),
	"claude-opus-4-6":   standard(5, 25, 0.50),
	"claude-sonnet-5-5": standard(2, 10, 0.20),
	"claude-sonnet-5":   standard(2, 10, 0.20),
	"claude-sonnet-4-6": standard(3, 15, 0.30),
	"claude-haiku-5-5": {
		Input: 0.10, Output: 0.50, CacheRead: 0.01, CacheWrite5m: 0.125, CacheWrite1h: 0.20,
		MaxPromptTokens: 100_000,
	},
	"claude-haiku-4-5": standard(1, 5, 0.10),
}

// With returns a copy of the table with overrides added or replaced.
func (p Prices) With(overrides Prices) Prices {
	out := make(Prices, len(p)+len(overrides))
	for model, price := range p {
		out[model] = price
	}
	for model, price := range overrides {
		out[model] = price
	}
	return out
}

// lookup finds the price for a model id. An id with a version suffix, such
// as a dated snapshot, takes the price of the longest id it extends.
func (p Prices) lookup(model string) (Price, bool) {
	if price, ok := p[model]; ok {
		return price, true
	}
	best := ""
	for id := range p {
		if len(id) > len(best) && (strings.HasPrefix(model, id+"-") || strings.HasPrefix(model, id+"@")) {
			best = id
		}
	}
	price, ok := p[best]
	return price, ok
}

// Cost is the list-price cost of one response in US dollars. When it cannot
// be worked out, unpriced says why and the cost is zero; callers must
// report such requests as unknown rather than free.
func (p Prices) Cost(model string, u Usage) (usd float64, unpriced string) {
	price, ok := p.lookup(model)
	if !ok {
		if model == "" {
			return 0, "no model reported"
		}
		return 0, "no price for " + model
	}
	if u.Speed == "fast" {
		return 0, "fast mode is priced separately"
	}

	read, write := deref(u.CacheRead), deref(u.CacheWrite)
	if price.MaxPromptTokens > 0 && u.Input+read+write > price.MaxPromptTokens {
		return 0, fmt.Sprintf("%s prompt over %d tokens is priced separately", model, price.MaxPromptTokens)
	}

	// A server that reports cache writes without saying how long they live
	// is priced at the 5-minute rate, which is the API's default lifetime.
	write5m, write1h := write, 0
	if u.CacheWrite5m != nil || u.CacheWrite1h != nil {
		write5m, write1h = deref(u.CacheWrite5m), deref(u.CacheWrite1h)
	}

	const perToken = 1e-6
	return perToken * (float64(u.Input)*price.Input +
		float64(u.Output)*price.Output +
		float64(read)*price.CacheRead +
		float64(write5m)*price.CacheWrite5m +
		float64(write1h)*price.CacheWrite1h), ""
}

func deref(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}
