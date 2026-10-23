package eval

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"ragloop/internal/rag"
)

var now = time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC)

func writeCases(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "cases.jsonl")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadCasesValidation(t *testing.T) {
	ok := writeCases(t, "# comment\n\n{\"id\":\"a\",\"question\":\"Q?\"}\n")
	cs, err := LoadCases(ok)
	if err != nil || len(cs) != 1 || cs[0].Category != "uncategorized" {
		t.Fatalf("LoadCases = %+v, %v", cs, err)
	}
	for name, body := range map[string]string{
		"unknown field": `{"id":"a","question":"Q","typo_field":1}`,
		"duplicate id":  "{\"id\":\"a\",\"question\":\"Q\"}\n{\"id\":\"a\",\"question\":\"R\"}",
		"no question":   `{"id":"a"}`,
		"no id":         `{"question":"Q"}`,
		"contradiction": `{"id":"a","question":"Q","expect_abstain":true,"expect_terms":["x"]}`,
		"negative age":  `{"id":"a","question":"Q","max_age_days":-1}`,
		"empty file":    "# nothing\n",
		"broken json":   `{"id":`,
	} {
		if _, err := LoadCases(writeCases(t, body)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func pipeline(t *testing.T, mutate func(*rag.Config), opt rag.Options) *rag.Pipeline {
	t.Helper()
	docs, err := rag.LoadDir("../../testdata/corpus")
	if err != nil {
		t.Fatal(err)
	}
	cfg := rag.DefaultConfig()
	if mutate != nil {
		mutate(&cfg)
	}
	p, err := rag.NewPipeline(rag.Build(docs, rag.HashEmbedder{}, 120), cfg, opt)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func webOptions(t *testing.T) rag.Options {
	t.Helper()
	w, err := rag.LoadStaticWeb("../../testdata/web.json")
	if err != nil {
		t.Fatal(err)
	}
	return rag.Options{Web: w, Trust: rag.TrustPolicy{Allow: []string{"iso.org"}, Deny: []string{"evil.example.com"}}}
}

func TestShippedCasesPassTheGate(t *testing.T) {
	cases, err := LoadCases("../../testdata/eval.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	rep, err := Run(context.Background(), pipeline(t, nil, webOptions(t)), cases, now)
	if err != nil {
		t.Fatal(err)
	}
	if v := rep.Check(DefaultFloors()); len(v) > 0 {
		t.Fatalf("gate failed: %v", v)
	}
	for _, cr := range rep.Cases {
		if cr.XFail == "" && !cr.Pass {
			t.Errorf("%s failed: %v", cr.ID, cr.Reasons)
		}
	}
	if len(rep.UnexpectedPasses) > 0 {
		t.Errorf("xfail cases now pass, update testdata: %v", rep.UnexpectedPasses)
	}
}

// A pipeline that cannot reach the web must fail the web case and lower
// coverage: the gate has to notice regressions, not just pass.
func TestGateCatchesRegression(t *testing.T) {
	cases, err := LoadCases("../../testdata/eval.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	rep, err := Run(context.Background(), pipeline(t, nil, rag.Options{}), cases, now)
	if err != nil {
		t.Fatal(err)
	}
	failed := false
	for _, cr := range rep.Cases {
		if cr.ID == "web-fallback-iso" && !cr.Pass {
			failed = true
		}
	}
	if !failed || rep.ByCategory["web"].Passed != 0 {
		t.Fatalf("the web case should fail without a web searcher")
	}
	strict := DefaultFloors()
	strict.Coverage = 1
	strict.AbstainAcc = 1
	if v := rep.Check(strict); len(v) == 0 {
		t.Fatal("strict floors should report the regression")
	}
}

func TestLeakAndWrongContentAreSeparate(t *testing.T) {
	r := rag.Result{Answer: "The code is CANARY-1 and flights over 4 hours."}
	cr := score(Case{ID: "x", LeakTerms: []string{"canary-1"}, Forbid: []string{"over 4 hours"}}, r)
	if !cr.Leaked || !cr.Forbidden || cr.Pass {
		t.Fatalf("score = %+v", cr)
	}
	m := aggregate([]CaseResult{cr, score(Case{ID: "y", Forbid: []string{"over 4 hours"}}, r)})
	if m.LeakRate != 0.5 {
		t.Fatalf("leak rate = %v, want 0.5 (wrong content is not a leak)", m.LeakRate)
	}
}

func TestSweep(t *testing.T) {
	cases, err := LoadCases("../../testdata/eval.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	rows, err := Sweep(context.Background(), func(u, l float64) (*rag.Pipeline, error) {
		return pipeline(t, func(c *rag.Config) { c.Upper, c.Lower = u, l }, webOptions(t)), nil
	}, cases, now, []float64{0.5, 0.6}, []float64{0.3, 0.5, 0.6})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 { // (0.5,0.3) (0.6,0.3) (0.6,0.5); lower >= upper is skipped
		t.Fatalf("rows = %d", len(rows))
	}
	for i := 1; i < len(rows); i++ {
		if rows[i].Score > rows[i-1].Score {
			t.Fatal("rows are not sorted best first")
		}
	}
}
