package ledger

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/risadams/janus/internal/route"
)

// OutageGap is how far apart two requests Claude could not answer may be
// and still count as one outage. Claude Code retries a failed request
// within seconds and a person carries on within minutes, so a quarter of
// an hour joins one rate limit or outage without joining two.
const OutageGap = 15 * time.Minute

// Outage is a run of requests Claude could not answer, none more than
// OutageGap after the one before (#230).
type Outage struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
	// Cause is the most common cause in the run: rate_limited,
	// overloaded, server_error, or unreachable.
	Cause string `json:"cause"`
	// Requests is how many Claude could not answer; StoodIn is how many
	// of them the local model answered instead. The rest reached the
	// client as errors.
	Requests int `json:"requests"`
	StoodIn  int `json:"stood_in"`
	// Limit is, for a rate limit, which of the subscription's limits was
	// reached, as the API named it (five_hour, seven_day), and Lifts is
	// when the API said it would answer again: the latest it gave.
	Limit string    `json:"limit,omitempty"`
	Lifts time.Time `json:"lifts,omitzero"`
}

// Causes of an outage, as Outage.Cause names them.
const (
	OutageRateLimited = "rate_limited"
	OutageOverloaded  = "overloaded"
	OutageServerError = "server_error"
	OutageUnreachable = "unreachable"
)

// upstreamFailure says whether Claude could not answer rec, why, whether
// the local model stood in, and the headers the API sent about when it
// would answer again.
func upstreamFailure(rec Record) (cause string, stoodIn bool, limits map[string]string) {
	if a := rec.Attempt; a != nil && a.Backend == "upstream" {
		var status int
		if _, err := fmt.Sscanf(a.Why, "upstream answered HTTP %d", &status); err == nil {
			return statusCause(status, ""), true, a.Limits
		}
		return OutageUnreachable, true, a.Limits
	}
	// Claude Code's start-up quota check is normally answered with a 429,
	// which says nothing about whether Claude is answering.
	if rec.Backend != "upstream" || rec.ShadowOf != "" || rec.Worker != nil || rec.Class == string(route.Quota) {
		return "", false, nil
	}
	if route.UpstreamDown(rec.Status) {
		return statusCause(rec.Status, rec.Error), false, rec.Limits
	}
	if rec.Status == 0 && rec.Outcome == OutcomeNoResponse {
		return OutageUnreachable, false, nil
	}
	return "", false, nil
}

func statusCause(status int, apiError string) string {
	switch {
	case status == http.StatusTooManyRequests:
		return OutageRateLimited
	case status == 529, apiError == "overloaded_error":
		return OutageOverloaded
	default:
		return OutageServerError
	}
}

// Outages finds the runs of requests Claude could not answer, oldest
// first. The records must be in time order, as Read returns them.
func Outages(records []Record) []Outage {
	var out []Outage
	var causes map[string]int
	finish := func() {
		if len(out) == 0 {
			return
		}
		o := &out[len(out)-1]
		names := make([]string, 0, len(causes))
		for c := range causes {
			names = append(names, c)
		}
		sort.Slice(names, func(i, j int) bool {
			if causes[names[i]] != causes[names[j]] {
				return causes[names[i]] > causes[names[j]]
			}
			return names[i] < names[j]
		})
		o.Cause = names[0]
	}
	for _, rec := range records {
		cause, stoodIn, limits := upstreamFailure(rec)
		if cause == "" {
			continue
		}
		if len(out) == 0 || rec.Time.Sub(out[len(out)-1].End) > OutageGap {
			finish()
			out = append(out, Outage{Start: rec.Time})
			causes = map[string]int{}
		}
		o := &out[len(out)-1]
		o.End = rec.Time
		o.Requests++
		causes[cause]++
		if stoodIn {
			o.StoodIn++
		}
		if claim := limits["anthropic-ratelimit-unified-representative-claim"]; claim != "" {
			o.Limit = claim
		}
		if lifts := liftsAt(rec.Time, limits); lifts.After(o.Lifts) {
			o.Lifts = lifts
		}
	}
	finish()
	return out
}

// liftsAt is when the API said it would answer again: the reset time of
// the subscription limit that was reached, or else the request's time
// plus retry-after. It is zero when the API did not say.
func liftsAt(at time.Time, limits map[string]string) time.Time {
	if s := limits["anthropic-ratelimit-unified-reset"]; s != "" {
		if epoch, err := strconv.ParseInt(s, 10, 64); err == nil {
			return time.Unix(epoch, 0).UTC()
		}
	}
	if s := strings.TrimSpace(limits["retry-after"]); s != "" {
		if secs, err := strconv.Atoi(s); err == nil {
			return at.Add(time.Duration(secs) * time.Second)
		}
	}
	return time.Time{}
}
