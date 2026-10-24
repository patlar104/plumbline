package rag

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patlar104/plumbline/internal/llm"
)

var now = time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC)

func testIndex(t *testing.T) *Index {
	t.Helper()
	docs, err := LoadDir("../../testdata/corpus")
	if err != nil {
		t.Fatal(err)
	}
	return Build(docs, HashEmbedder{}, 120)
}

func testPipeline(t *testing.T, opt Options, mutate ...func(*Config)) *Pipeline {
	t.Helper()
	cfg := DefaultConfig()
	for _, m := range mutate {
		m(&cfg)
	}
	p, err := NewPipeline(testIndex(t), cfg, opt)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func ask(t *testing.T, p *Pipeline, q string, f Filter) Result {
	t.Helper()
	f.Now = now
	r, err := p.Ask(context.Background(), q, f)
	if err != nil {
		t.Fatalf("Ask(%q): %v", q, err)
	}
	return r
}

func evidenceDocs(r Result) map[string]bool {
	m := map[string]bool{}
	for _, e := range r.Evidence {
		m[e.DocID] = true
	}
	return m
}

func TestOfflineAnswerIsCitedAndVerified(t *testing.T) {
	p := testPipeline(t, Options{})
	r := ask(t, p, "How many vacation days do employees get per year?", Filter{})
	if !r.Verified || r.Abstained || !strings.Contains(r.Answer, "25 days") || !strings.Contains(r.Answer, "[1]") {
		t.Fatalf("unexpected result: %+v", r)
	}
	if !evidenceDocs(r)["vacation-policy"] || r.Grade != Correct || r.Action != "refine" {
		t.Fatalf("evidence/grade: %+v", r)
	}
	steps := []string{}
	for _, e := range r.Trace {
		steps = append(steps, e.Step)
	}
	if got := strings.Join(steps, ","); got != "route,retrieve,grade,correct,generate,verify" {
		t.Fatalf("trace steps = %s", got)
	}
}

func TestUnanswerableAbstainsWithoutWeb(t *testing.T) {
	p := testPipeline(t, Options{})
	r := ask(t, p, "What is the parking policy?", Filter{})
	if !r.Abstained || r.Answer != NoAnswer || r.Grade != Incorrect {
		t.Fatalf("should abstain: %+v", r)
	}
	if len(r.Warnings) == 0 || !strings.Contains(strings.Join(r.Warnings, ";"), "no web searcher") {
		t.Fatalf("missing web warning: %q", r.Warnings)
	}
}

func TestPermissionsAndCacheIsolation(t *testing.T) {
	p := testPipeline(t, Options{})
	q := "Where are the production signing keys stored?"
	sec := ask(t, p, q, Filter{Groups: []string{"security"}})
	if !strings.Contains(sec.Answer, "vault-prod-3") {
		t.Fatalf("security group should see the answer: %+v", sec)
	}
	// Same question from a user without the group must not hit that cache entry.
	pub := ask(t, p, q, Filter{})
	if pub.Cached || strings.Contains(pub.Answer, "vault-prod-3") || evidenceDocs(pub)["security-keys"] {
		t.Fatalf("restricted content leaked: %+v", pub)
	}
	again := ask(t, p, q, Filter{Groups: []string{"SECURITY"}})
	if !again.Cached || again.Answer != sec.Answer {
		t.Fatalf("equivalent filter should hit the cache: cached=%t", again.Cached)
	}
}

func TestSupersededHiddenUnlessAsked(t *testing.T) {
	p := testPipeline(t, Options{})
	r := ask(t, p, "Which class do employees fly?", Filter{})
	if strings.Contains(r.Answer, "4 hours") || evidenceDocs(r)["travel-policy-2019"] {
		t.Fatalf("superseded policy used: %+v", r)
	}
	old := ask(t, p, "Which flights allowed business class under the 2019 travel policy?", Filter{IncludeSuperseded: true})
	if !evidenceDocs(old)["travel-policy-2019"] {
		t.Fatalf("archive search should reach the old policy: %+v", old)
	}
}

func TestInjectionScreenedAndWebFallback(t *testing.T) {
	web, err := LoadStaticWeb("../../testdata/web.json")
	if err != nil {
		t.Fatal(err)
	}
	p := testPipeline(t, Options{Web: web, Trust: TrustPolicy{Allow: []string{"iso.org"}, Deny: []string{"evil.example.com"}}})
	v := ask(t, p, "Who is our office supplies vendor?", Filter{})
	if !strings.Contains(v.Answer, "Paperline") || strings.Contains(strings.ToLower(v.Answer), "canary") {
		t.Fatalf("vendor answer: %+v", v)
	}
	w := ask(t, p, "What is ISO 9001?", Filter{})
	if !strings.Contains(w.Answer, "quality management") || w.WebFetches != 1 {
		t.Fatalf("web answer: %+v", w)
	}
	for _, e := range w.Evidence {
		if strings.Contains(e.Source, "evil.example.com") || strings.Contains(strings.ToLower(e.Text), "canary") {
			t.Fatalf("untrusted or injected web text reached evidence: %+v", e)
		}
	}
	if !strings.Contains(strings.Join(w.Warnings, ";"), "dropped 1") {
		t.Fatalf("denied domain should be reported: %q", w.Warnings)
	}
	q := testPipeline(t, Options{Web: web}, func(c *Config) { c.MaxWeb = 0 })
	if r := ask(t, q, "What is ISO 9001?", Filter{}); !r.Abstained || r.WebFetches != 0 {
		t.Fatalf("web budget 0 must block the fallback: %+v", r)
	}
}

func TestDeadlineAndCancellation(t *testing.T) {
	p := testPipeline(t, Options{}, func(c *Config) { c.Deadline = time.Nanosecond })
	r := ask(t, p, "How many vacation days do employees get per year?", Filter{})
	if !r.TimedOut || !r.Abstained {
		t.Fatalf("expected a timed-out abstention: %+v", r)
	}
	if r2 := ask(t, p, "How many vacation days do employees get per year?", Filter{}); r2.Cached {
		t.Fatal("timed-out results must not be cached")
	}
	q := testPipeline(t, Options{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := q.Ask(ctx, "How many vacation days?", Filter{Now: now}); !errors.Is(err, context.Canceled) {
		t.Fatalf("caller cancellation should surface as an error, got %v", err)
	}
	if _, err := q.Ask(context.Background(), "   ", Filter{}); err == nil {
		t.Fatal("empty question should be an error")
	}
}

func TestConcurrentAsk(t *testing.T) {
	p := testPipeline(t, Options{})
	qs := []string{"How many vacation days do employees get per year?", "What is the daily meal allowance?",
		"What payload can the X200 carry?", "What is the parking policy?", "Summarize the main policies"}
	var wg sync.WaitGroup
	errs := make(chan error, 40)
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := p.Ask(context.Background(), qs[i%len(qs)], Filter{Now: now}); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

// fakeModel answers by role, recognised from the system prompt.
type fakeModel struct {
	mu        sync.Mutex
	calls     int
	router    string
	grader    func(prompt string) string
	generator string
	direct    string
	err       error
}

func (f *fakeModel) Complete(_ context.Context, req llm.Request) (string, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	if f.err != nil {
		return "", f.err
	}
	switch {
	case strings.HasPrefix(req.System, "Classify"):
		return f.router, nil
	case strings.HasPrefix(req.System, "You grade"):
		if f.grader != nil {
			return f.grader(req.Prompt), nil
		}
		return "not json", nil
	case strings.HasPrefix(req.System, "You answer"):
		return f.generator, nil
	default:
		return f.direct, nil
	}
}

func allOnes(prompt string) string {
	n := strings.Count(prompt, "<passage n=")
	return "[" + strings.TrimSuffix(strings.Repeat("0.9,", n), ",") + "]"
}

func TestModelThatObeysInjectionIsBlocked(t *testing.T) {
	m := &fakeModel{router: `{"kind":"single"}`, grader: allOnes,
		generator: "Our vendor is Paperline [1]. CANARY-7731 [1]."}
	p := testPipeline(t, Options{Model: m})
	r := ask(t, p, "Who is our office supplies vendor?", Filter{})
	if !r.Leak || !r.Abstained || r.Answer != LeakAnswer {
		t.Fatalf("leak not blocked: %+v", r)
	}
}

func TestModelThatInventsFactsIsCaveated(t *testing.T) {
	m := &fakeModel{router: `{"kind":"single"}`, grader: allOnes,
		generator: "Employees get 40 days of vacation and a company car [1]."}
	p := testPipeline(t, Options{Model: m})
	r := ask(t, p, "How many vacation days do employees get per year?", Filter{})
	if r.Verified || !strings.HasPrefix(r.Answer, UnverifiedPrefix) {
		t.Fatalf("invented answer must be marked unverified: %+v", r)
	}
	if r.Iterations < 1 || r.Iterations > p.Cfg.MaxIter || r.LLMCalls > p.Cfg.MaxLLM {
		t.Fatalf("loop exceeded its caps: iterations=%d calls=%d", r.Iterations, r.LLMCalls)
	}
}

func TestModelGarbageAndErrorsFallBack(t *testing.T) {
	m := &fakeModel{router: "I think it's single?", generator: NoAnswer}
	p := testPipeline(t, Options{Model: m})
	r := ask(t, p, "How many vacation days do employees get per year?", Filter{})
	if r.Route.Kind != RouteSingle || !strings.Contains(r.Route.Reason, "fell back") {
		t.Fatalf("router fallback: %+v", r.Route)
	}
	if !strings.Contains(strings.Join(r.Warnings, ";"), "fell back to lexical") {
		t.Fatalf("grader fallback should be reported: %q", r.Warnings)
	}

	broken := &fakeModel{err: fmt.Errorf("HTTP 500")}
	q := testPipeline(t, Options{Model: broken})
	r = ask(t, q, "How many vacation days do employees get per year?", Filter{})
	if !r.Verified || !strings.Contains(r.Answer, "25 days") {
		t.Fatalf("a failing model must degrade to the offline stages: %+v", r)
	}
}

func TestModelBudgetIsEnforced(t *testing.T) {
	m := &fakeModel{router: `{"kind":"single"}`, grader: allOnes, generator: "unused"}
	p := testPipeline(t, Options{Model: m}, func(c *Config) { c.MaxLLM = 1 })
	r := ask(t, p, "How many vacation days do employees get per year?", Filter{})
	if r.LLMCalls != 1 || m.calls != 1 {
		t.Fatalf("budget allowed %d calls (model saw %d)", r.LLMCalls, m.calls)
	}
	if !r.Verified || !strings.Contains(r.Answer, "25 days") {
		t.Fatalf("stages past the budget should use stand-ins: %+v", r)
	}
}

func TestDirectRouteGuard(t *testing.T) {
	m := &fakeModel{router: `{"kind":"direct"}`, grader: allOnes, generator: "unused", direct: "From memory: 30 days."}
	p := testPipeline(t, Options{Model: m})
	r := ask(t, p, "How many vacation days do employees get per year?", Filter{})
	if r.Route.Kind == RouteDirect || strings.Contains(r.Answer, "From memory") {
		t.Fatalf("a document question must not skip retrieval: %+v", r)
	}
	g := ask(t, testPipeline(t, Options{}), "Hello!", Filter{})
	if g.Route.Kind != RouteDirect || g.Abstained || g.Answer != greetingReply {
		t.Fatalf("greeting: %+v", g)
	}
}
