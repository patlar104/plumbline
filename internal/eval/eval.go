// Package eval runs a fixed question set through the pipeline, scores it,
// and gates changes on metric floors.
package eval

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"ragloop/internal/rag"
)

// Case is one test question.
type Case struct {
	ID            string   `json:"id"`
	Category      string   `json:"category"`
	Question      string   `json:"question"`
	ExpectDocs    []string `json:"expect_docs,omitempty"`
	ExpectTerms   []string `json:"expect_terms,omitempty"`
	ExpectAbstain bool     `json:"expect_abstain,omitempty"`
	ExpectRoute   string   `json:"expect_route,omitempty"`
	Forbid        []string `json:"forbid,omitempty"`     // wrong content: fails the case
	LeakTerms     []string `json:"leak_terms,omitempty"` // injected or restricted text: fails and counts as a leak
	Groups        []string `json:"groups,omitempty"`
	MaxAgeDays    int      `json:"max_age_days,omitempty"`
	// IncludeSuperseded searches documents that a newer one replaces.
	IncludeSuperseded bool   `json:"include_superseded,omitempty"`
	XFail             string `json:"xfail,omitempty"` // reason this case is expected to fail
}

// LoadCases reads JSON Lines; blank lines and lines starting with # are
// skipped. Unknown fields and duplicate ids are errors.
func LoadCases(path string) ([]Case, error) {
	f, err := os.Open(path) //nolint:gosec // G304: the path is the -cases argument; reading it is the point
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var cases []Case
	seen := map[string]int{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	line := 0
	for sc.Scan() {
		line++
		raw := bytes.TrimSpace(sc.Bytes())
		if len(raw) == 0 || raw[0] == '#' {
			continue
		}
		var c Case
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&c); err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, line, err)
		}
		switch {
		case strings.TrimSpace(c.ID) == "":
			return nil, fmt.Errorf("%s:%d: missing id", path, line)
		case strings.TrimSpace(c.Question) == "":
			return nil, fmt.Errorf("%s:%d: case %q has no question", path, line, c.ID)
		case c.MaxAgeDays < 0:
			return nil, fmt.Errorf("%s:%d: case %q has a negative max_age_days", path, line, c.ID)
		case c.ExpectAbstain && (len(c.ExpectTerms) > 0 || len(c.ExpectDocs) > 0):
			return nil, fmt.Errorf("%s:%d: case %q expects an abstention and an answer", path, line, c.ID)
		}
		if prev, dup := seen[c.ID]; dup {
			return nil, fmt.Errorf("%s:%d: duplicate id %q (first on line %d)", path, line, c.ID, prev)
		}
		seen[c.ID] = line
		if c.Category == "" {
			c.Category = "uncategorized"
		}
		cases = append(cases, c)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(cases) == 0 {
		return nil, fmt.Errorf("%s: no cases", path)
	}
	return cases, nil
}

// CaseResult is one scored case.
type CaseResult struct {
	ID        string      `json:"id"`
	Category  string      `json:"category"`
	Pass      bool        `json:"pass"`
	XFail     string      `json:"xfail,omitempty"`
	Reasons   []string    `json:"reasons,omitempty"`
	Recall    float64     `json:"recall"`
	RR        float64     `json:"reciprocal_rank"`
	Coverage  float64     `json:"coverage"`
	Forbidden bool        `json:"forbidden_text"`
	Leaked    bool        `json:"leaked"`
	Result    rag.Result  `json:"result"`
	scored    scoredFlags // which metrics apply to this case
}

type scoredFlags struct{ docs, terms, answered, route bool }

// Metrics are averages over the cases each metric applies to.
type Metrics struct {
	Cases      int     `json:"cases"`
	Passed     int     `json:"passed"`
	Recall     float64 `json:"recall"`
	MRR        float64 `json:"mrr"`
	Coverage   float64 `json:"answer_coverage"`
	AbstainAcc float64 `json:"abstention_accuracy"`
	Grounded   float64 `json:"groundedness"`
	LeakRate   float64 `json:"leak_rate"`
	RouteAcc   float64 `json:"route_accuracy"`
}

// Floors are the minimum metrics a change must keep. LeakRate is a ceiling.
type Floors struct {
	Recall, MRR, Coverage, AbstainAcc, Grounded, MaxLeakRate float64
}

// DefaultFloors match the spec.
func DefaultFloors() Floors {
	return Floors{Recall: 0.85, MRR: 0.6, Coverage: 0.75, AbstainAcc: 0.9, Grounded: 0.9, MaxLeakRate: 0}
}

// Report is a full evaluation run.
type Report struct {
	Cases      []CaseResult       `json:"cases"`
	Overall    Metrics            `json:"overall"`
	ByCategory map[string]Metrics `json:"by_category"`
	// UnexpectedPasses are xfail cases that now pass: drop their xfail.
	UnexpectedPasses []string `json:"unexpected_passes,omitempty"`
}

// Run asks every case and scores it. Expected-failure cases run and are
// reported but do not count toward the metrics.
func Run(ctx context.Context, p *rag.Pipeline, cases []Case, now time.Time) (Report, error) {
	var rep Report
	for _, c := range cases {
		f := rag.Filter{Groups: c.Groups, Now: now, MaxAge: time.Duration(c.MaxAgeDays) * 24 * time.Hour,
			IncludeSuperseded: c.IncludeSuperseded}
		res, err := p.Ask(ctx, c.Question, f)
		if err != nil {
			return rep, fmt.Errorf("case %s: %w", c.ID, err)
		}
		cr := score(c, res)
		if c.XFail != "" && cr.Pass {
			rep.UnexpectedPasses = append(rep.UnexpectedPasses, c.ID)
		}
		rep.Cases = append(rep.Cases, cr)
	}
	var counted []CaseResult
	byCat := map[string][]CaseResult{}
	for _, cr := range rep.Cases {
		if cr.XFail != "" {
			continue
		}
		counted = append(counted, cr)
		byCat[cr.Category] = append(byCat[cr.Category], cr)
	}
	rep.Overall = aggregate(counted)
	rep.ByCategory = map[string]Metrics{}
	for k, v := range byCat {
		rep.ByCategory[k] = aggregate(v)
	}
	return rep, nil
}

func score(c Case, r rag.Result) CaseResult {
	cr := CaseResult{ID: c.ID, Category: c.Category, XFail: c.XFail, Result: r, Pass: true}
	fail := func(format string, args ...any) {
		cr.Pass = false
		cr.Reasons = append(cr.Reasons, fmt.Sprintf(format, args...))
	}
	answerLower := strings.ToLower(r.Answer)

	if r.Abstained != c.ExpectAbstain {
		if c.ExpectAbstain {
			fail("answered but should have declined")
		} else {
			fail("declined but should have answered")
		}
	}
	if c.ExpectRoute != "" {
		cr.scored.route = true
		if string(r.Route.Kind) != c.ExpectRoute {
			fail("route %s, expected %s", r.Route.Kind, c.ExpectRoute)
		}
	}
	if len(c.ExpectDocs) > 0 {
		cr.scored.docs = true
		evDocs := map[string]bool{}
		for _, e := range r.Evidence {
			evDocs[e.DocID] = true
		}
		found := 0
		for _, d := range c.ExpectDocs {
			if evDocs[d] {
				found++
			}
		}
		cr.Recall = float64(found) / float64(len(c.ExpectDocs))
		if found < len(c.ExpectDocs) {
			fail("evidence has %d of %d expected documents", found, len(c.ExpectDocs))
		}
		want := map[string]bool{}
		for _, d := range c.ExpectDocs {
			want[d] = true
		}
		for i, p := range r.Retrieved {
			if want[p.DocID] {
				cr.RR = 1 / float64(i+1)
				break
			}
		}
	}
	if len(c.ExpectTerms) > 0 {
		cr.scored.terms = true
		got := 0
		var missing []string
		for _, t := range c.ExpectTerms {
			if strings.Contains(answerLower, strings.ToLower(t)) {
				got++
			} else {
				missing = append(missing, t)
			}
		}
		cr.Coverage = float64(got) / float64(len(c.ExpectTerms))
		if len(missing) > 0 {
			fail("answer is missing %q", missing)
		}
	}
	for _, f := range c.Forbid {
		if strings.Contains(answerLower, strings.ToLower(f)) {
			cr.Forbidden = true
			fail("answer contains wrong content %q", f)
		}
	}
	for _, t := range c.LeakTerms {
		if strings.Contains(answerLower, strings.ToLower(t)) {
			cr.Leaked = true
			fail("answer leaked %q", t)
		}
	}
	if !r.Abstained && r.Route.Kind != rag.RouteDirect {
		cr.scored.answered = true
		if !r.Verified {
			fail("answer is not fully supported by its sources")
		}
	}
	return cr
}

func aggregate(rs []CaseResult) Metrics {
	m := Metrics{Cases: len(rs)}
	var nDocs, nTerms, nAns, nRoute, abstainOK, grounded, leaks, routeOK int
	for _, r := range rs {
		if r.Pass {
			m.Passed++
		}
		if r.scored.docs {
			nDocs++
			m.Recall += r.Recall
			m.MRR += r.RR
		}
		if r.scored.terms {
			nTerms++
			m.Coverage += r.Coverage
		}
		if r.scored.answered {
			nAns++
			if r.Result.Verified {
				grounded++
			}
		}
		if r.scored.route {
			nRoute++
			if !hasReasonPrefix(r.Reasons, "route ") {
				routeOK++
			}
		}
		if !hasReasonPrefix(r.Reasons, "answered but should") && !hasReasonPrefix(r.Reasons, "declined but should") {
			abstainOK++
		}
		if r.Leaked {
			leaks++
		}
	}
	div := func(a float64, n int) float64 {
		if n == 0 {
			return 1
		}
		return a / float64(n)
	}
	m.Recall = div(m.Recall, nDocs)
	m.MRR = div(m.MRR, nDocs)
	m.Coverage = div(m.Coverage, nTerms)
	m.Grounded = div(float64(grounded), nAns)
	m.RouteAcc = div(float64(routeOK), nRoute)
	if len(rs) > 0 {
		m.AbstainAcc = float64(abstainOK) / float64(len(rs))
		m.LeakRate = float64(leaks) / float64(len(rs))
	}
	return m
}

func hasReasonPrefix(rs []string, prefix string) bool {
	for _, r := range rs {
		if strings.HasPrefix(r, prefix) {
			return true
		}
	}
	return false
}

// Check returns every floor the report misses.
func (r Report) Check(f Floors) []string {
	m := r.Overall
	var v []string
	below := func(name string, got, floor float64) {
		if got < floor {
			v = append(v, fmt.Sprintf("%s %.2f is below the floor %.2f", name, got, floor))
		}
	}
	below("recall", m.Recall, f.Recall)
	below("mrr", m.MRR, f.MRR)
	below("answer coverage", m.Coverage, f.Coverage)
	below("abstention accuracy", m.AbstainAcc, f.AbstainAcc)
	below("groundedness", m.Grounded, f.Grounded)
	if m.LeakRate > f.MaxLeakRate {
		v = append(v, fmt.Sprintf("leak rate %.2f is above the ceiling %.2f", m.LeakRate, f.MaxLeakRate))
	}
	return v
}

// SweepRow is one threshold pair's result.
type SweepRow struct {
	Upper, Lower float64
	Score        float64 // mean of recall, coverage, abstention and groundedness, minus leak rate
	Metrics      Metrics
}

// Sweep runs the cases once per (upper, lower) pair with lower < upper and
// returns the rows best first. build makes a fresh pipeline for each pair,
// so caches never leak results between settings.
func Sweep(ctx context.Context, build func(upper, lower float64) (*rag.Pipeline, error), cases []Case, now time.Time, uppers, lowers []float64) ([]SweepRow, error) {
	var rows []SweepRow
	for _, u := range uppers {
		for _, l := range lowers {
			if l >= u {
				continue
			}
			p, err := build(u, l)
			if err != nil {
				return nil, err
			}
			rep, err := Run(ctx, p, cases, now)
			if err != nil {
				return nil, err
			}
			m := rep.Overall
			rows = append(rows, SweepRow{Upper: u, Lower: l, Metrics: m,
				Score: (m.Recall+m.Coverage+m.AbstainAcc+m.Grounded)/4 - m.LeakRate})
		}
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Score > rows[j].Score })
	return rows, nil
}
