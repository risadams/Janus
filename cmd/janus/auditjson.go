// auditjson.go is janus audit -json: the same tally the report prints, for
// scripts. Its fields are kept within a major version (docs/RELEASES.md);
// docs/GUARD.md describes them.

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/risadams/janus/internal/ledger"
)

// auditReport is what janus audit -json prints.
type auditReport struct {
	Dir string `json:"dir"`
	// From and To are the first and last request counted, left out when
	// there were none.
	From     *time.Time `json:"from,omitempty"`
	To       *time.Time `json:"to,omitempty"`
	Requests int        `json:"requests"`
	// Where the requests carrying content went.
	Anthropic auditSent `json:"anthropic"`
	LocalOnly int       `json:"local_only"`
	Stopped   int       `json:"stopped"`
	// KeptLocal counts requests the local-only policy kept from Anthropic
	// altogether.
	KeptLocal int          `json:"kept_local"`
	Secrets   auditSecrets `json:"secrets"`
	Blocked   []auditStop  `json:"blocked"`
	// Verify is there only with -verify.
	Verify *auditVerify `json:"verify,omitempty"`
}

// auditSent is the requests that reached Anthropic: as the client sent
// them, or changed by janus on the way, and how.
type auditSent struct {
	Untouched int `json:"untouched"`
	Changed   int `json:"changed"`
	Redacted  int `json:"redacted"`
	Withheld  int `json:"withheld"`
	Repaired  int `json:"repaired"`
}

// auditSecrets is what the guard found in requests bound for Anthropic.
type auditSecrets struct {
	Requests int            `json:"requests"`
	Replaced int            `json:"replaced"`
	Kinds    map[string]int `json:"kinds"`
	// Exposed are the requests sent with the secrets in them.
	Exposed []auditFinding `json:"exposed"`
}

// auditFinding is one request and what was found in it, and where.
type auditFinding struct {
	ID    string   `json:"id"`
	Kinds []string `json:"kinds"`
	Where []string `json:"where"`
}

type auditStop struct {
	ID  string `json:"id"`
	Why string `json:"why"`
}

// auditVerify is what -verify found on re-reading what was sent.
type auditVerify struct {
	Checked   int            `json:"checked"`
	Clean     int            `json:"clean"`
	Unchecked int            `json:"unchecked"`
	Leaks     []auditFinding `json:"leaks"`
}

func auditJSON(dir string, a audit, v *verification) auditReport {
	r := auditReport{
		Dir:      dir,
		Requests: a.requests,
		Anthropic: auditSent{
			Untouched: a.untouched, Changed: a.changed,
			Redacted: a.redacted, Withheld: a.withheld, Repaired: a.repaired,
		},
		LocalOnly: a.localOnly,
		Stopped:   a.stopped,
		KeptLocal: a.keptLocal,
		Secrets: auditSecrets{
			Requests: a.carried, Replaced: a.replaced, Kinds: a.kinds,
			Exposed: []auditFinding{},
		},
		Blocked: []auditStop{},
	}
	if a.requests > 0 {
		r.From, r.To = &a.from, &a.to
	}
	for _, rec := range a.exposed {
		r.Secrets.Exposed = append(r.Secrets.Exposed, auditFinding{ID: rec.ID, Kinds: sortedKinds(rec.Guard.Kinds), Where: rec.Guard.Where})
	}
	for _, rec := range a.blocked {
		r.Blocked = append(r.Blocked, auditStop{ID: rec.ID, Why: rec.Guard.Blocked})
	}
	if v != nil {
		r.Verify = &auditVerify{Checked: v.checked, Clean: v.clean, Unchecked: v.unchecked, Leaks: []auditFinding{}}
		for _, l := range v.leaks {
			r.Verify.Leaks = append(r.Verify.Leaks, auditFinding{ID: l.id, Kinds: sortedKinds(l.report.Kinds), Where: l.report.Where})
		}
	}
	return r
}

func sortedKinds(kinds map[string]int) []string {
	names := make([]string, 0, len(kinds))
	for k := range kinds {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// auditOneJSON prints one request's ledger record, as the ledger holds it
// (docs/LEDGER.md).
func auditOneJSON(stdout, stderr io.Writer, records []ledger.Record, id string) int {
	for _, rec := range records {
		if rec.ID != id {
			continue
		}
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rec); err != nil {
			fmt.Fprintf(stderr, "janus audit: %v\n", err)
			return 1
		}
		return 0
	}
	fmt.Fprintf(stderr, "janus audit: no request %q in the ledger for this range\n", id)
	return 1
}
