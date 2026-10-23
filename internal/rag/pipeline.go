package rag

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"ragloop/internal/llm"
)

// Messages returned in place of an answer.
const (
	LeakAnswer       = "I can't answer that safely: a source contained instructions aimed at the assistant, and the draft answer repeated them."
	UnverifiedPrefix = "Unverified (not every sentence is supported by the sources): "
	directNoModel    = "No model is configured for questions outside the documents."
	greetingReply    = "Hello! Ask me a question about the indexed documents."
)

// Config holds every tunable. Start from DefaultConfig.
type Config struct {
	Upper, Lower    float64       // CRAG grade thresholds, 0 <= Lower < Upper <= 1
	RetrieveK       int           // hits per retrieval list
	TopK            int           // passages kept after reranking
	MinStrip        float64       // coverage a strip needs to become evidence
	MaxEvidence     int           // strips handed to the generator
	MaxSentences    int           // extractive answer length
	MinAnswerScore  float64       // extractive: coverage the best strip needs
	MaxIter         int           // retry loop cap
	MaxLLM          int           // model calls per query
	MaxWeb          int           // web searches per query
	WebResults      int           // pages per web search
	Deadline        time.Duration // wall-clock cap per query
	MinGrounded     float64       // share of supported sentences to count as verified
	SentenceOverlap float64       // share of a sentence's words a cited strip must contain
	CacheSize       int           // entries per cache
}

// DefaultConfig returns the settings the spec documents.
func DefaultConfig() Config {
	return Config{
		Upper: 0.6, Lower: 0.3, RetrieveK: 20, TopK: 8, MinStrip: 0.2, MaxEvidence: 8,
		MaxSentences: 3, MinAnswerScore: 0.3, MaxIter: 3, MaxLLM: 12, MaxWeb: 3, WebResults: 5,
		Deadline: 30 * time.Second, MinGrounded: 1.0, SentenceOverlap: 0.6, CacheSize: 256,
	}
}

// Validate reports the first invalid setting.
func (c Config) Validate() error {
	inUnit := func(name string, v float64) error {
		if v < 0 || v > 1 {
			return fmt.Errorf("%s must be between 0 and 1, got %v", name, v)
		}
		return nil
	}
	for _, chk := range []error{
		inUnit("upper", c.Upper), inUnit("lower", c.Lower), inUnit("min-strip", c.MinStrip),
		inUnit("min-answer-score", c.MinAnswerScore), inUnit("min-grounded", c.MinGrounded),
		inUnit("sentence-overlap", c.SentenceOverlap),
	} {
		if chk != nil {
			return chk
		}
	}
	switch {
	case c.Lower >= c.Upper:
		return fmt.Errorf("lower threshold (%v) must be below upper (%v)", c.Lower, c.Upper)
	case c.RetrieveK < 1 || c.TopK < 1 || c.MaxEvidence < 1 || c.MaxSentences < 1:
		return errors.New("retrieve-k, top-k, max-evidence and max-sentences must be at least 1")
	case c.MaxIter < 1:
		return errors.New("max-iter must be at least 1")
	case c.MaxLLM < 0 || c.MaxWeb < 0 || c.WebResults < 0:
		return errors.New("max-llm, max-web and web-results cannot be negative")
	case c.Deadline <= 0:
		return errors.New("deadline must be positive")
	}
	return nil
}

// Options are the pluggable parts. Nil Model and Web mean offline stand-ins
// and no web fallback.
type Options struct {
	Model llm.Model
	Web   WebSearcher
	Trust TrustPolicy
}

// Pipeline answers questions over one Index. It is safe for concurrent use.
type Pipeline struct {
	Index     *Index
	Cfg       Config
	Router    Router
	Grader    Grader
	Reranker  Reranker
	Generator Generator
	Web       WebSearcher
	Trust     TrustPolicy
	Model     llm.Model // used for direct answers; nil = offline

	answers   *LRU[string, Result]
	retrieval *LRU[string, hybridLists]
}

type hybridLists struct{ bm, vec []Passage }

// NewPipeline wires the default stages: model-backed ones when a model is
// given, each with its offline stand-in as fallback.
func NewPipeline(ix *Index, cfg Config, opt Options) (*Pipeline, error) {
	if ix == nil {
		return nil, errors.New("nil index")
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	lex := LexicalGrader{Index: ix}
	ext := ExtractiveGenerator{Index: ix, MaxSentences: cfg.MaxSentences, MinScore: cfg.MinAnswerScore}
	p := &Pipeline{
		Index: ix, Cfg: cfg, Router: HeuristicRouter{}, Grader: lex, Reranker: LexicalReranker{Index: ix},
		Generator: ext, Web: opt.Web, Trust: opt.Trust, Model: opt.Model,
		answers: NewLRU[string, Result](cfg.CacheSize), retrieval: NewLRU[string, hybridLists](cfg.CacheSize),
	}
	if opt.Model != nil {
		p.Router = LLMRouter{Model: opt.Model, Fallback: HeuristicRouter{}}
		p.Grader = LLMGrader{Model: opt.Model, Fallback: lex}
		p.Generator = LLMGenerator{Model: opt.Model, Fallback: ext}
	}
	return p, nil
}

// Result is the outcome of one question.
type Result struct {
	Query      string       `json:"query"`
	Answer     string       `json:"answer"`
	Route      Route        `json:"route"`
	Grade      Grade        `json:"grade,omitempty"`
	Action     string       `json:"action,omitempty"`
	Evidence   []Evidence   `json:"evidence"`
	Retrieved  []Passage    `json:"retrieved"`
	Grounded   GroundResult `json:"grounded"`
	Verified   bool         `json:"verified"`
	Abstained  bool         `json:"abstained"`
	Leak       bool         `json:"leak_blocked"`
	Iterations int          `json:"iterations"`
	LLMCalls   int          `json:"llm_calls"`
	WebFetches int          `json:"web_fetches"`
	Cached     bool         `json:"cached"`
	TimedOut   bool         `json:"timed_out"`
	Warnings   []string     `json:"warnings,omitempty"`
	Trace      []Event      `json:"trace"`
}

func normalizeQuery(q string) string {
	return strings.Trim(strings.Join(strings.Fields(strings.ToLower(q)), " "), " ?!.")
}

// Ask answers one question within the configured budget and deadline.
func (p *Pipeline) Ask(ctx context.Context, query string, f Filter) (Result, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return Result{}, errors.New("empty question")
	}
	if len(query) > 4000 {
		return Result{}, errors.New("question is longer than 4000 bytes")
	}
	if f.Now.IsZero() {
		f.Now = time.Now()
	}
	key := normalizeQuery(query) + "|" + f.key()
	if r, ok := p.answers.Get(key); ok {
		r.Cached = true
		r.Evidence = slices.Clone(r.Evidence)
		r.Retrieved = slices.Clone(r.Retrieved)
		r.Trace = []Event{{Step: "cache", Detail: "answer cache hit"}}
		return r, nil
	}

	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, p.Cfg.Deadline)
	defer cancel()
	budget := NewBudget(p.Cfg.MaxLLM, p.Cfg.MaxWeb)
	ctx = WithBudget(ctx, budget)
	tr := &tracer{}
	var warnings []string

	t := time.Now()
	route, err := p.Router.Route(ctx, query)
	if err != nil {
		route, _ = HeuristicRouter{}.Route(ctx, query)
		warnings = append(warnings, "router failed: "+err.Error())
	}
	if route.Focus == "" {
		route.Focus = query
	}
	route = p.guardDirect(query, route, f)
	tr.add("route", t, "%s: %s; %d sub-queries", route.Kind, route.Reason, len(route.SubQueries))

	finish := func(r Result) (Result, error) {
		r.Query, r.Route = query, route
		r.LLMCalls, r.WebFetches = budget.Used()
		r.Warnings = append(warnings, r.Warnings...)
		r.TimedOut = errors.Is(ctx.Err(), context.DeadlineExceeded) && parent.Err() == nil
		r.Trace = tr.events
		if err := parent.Err(); err != nil {
			return r, err
		}
		if !r.TimedOut {
			p.answers.Add(key, r)
		}
		return r, nil
	}

	if route.Kind == RouteDirect {
		t = time.Now()
		ans, abstained, warn := p.direct(ctx, query)
		tr.add("direct", t, "answered without retrieval")
		r := Result{Answer: ans, Abstained: abstained, Iterations: 1}
		if warn != "" {
			r.Warnings = append(r.Warnings, warn)
		}
		return finish(r)
	}

	var best *Result
	q, iter := query, 0
	for iter < p.Cfg.MaxIter {
		if ctx.Err() != nil {
			tr.add("stop", time.Now(), "deadline reached before iteration %d", iter+1)
			break
		}
		iter++
		a := p.attempt(ctx, route, q, query, f, tr, iter)
		if best == nil || better(a, *best) {
			best = &a
		}
		if a.Verified || a.Abstained {
			break
		}
		next := p.reformulate(query, a.Evidence)
		if next == q {
			break
		}
		tr.add("retry", time.Now(), "unsupported sentences: %d; reformulated to %q", len(a.Grounded.Unsupported), next)
		q = next
	}
	if best == nil {
		return finish(Result{Answer: NoAnswer, Abstained: true, Iterations: iter})
	}
	r := *best
	r.Iterations = iter
	if !r.Verified && !r.Abstained {
		r.Answer = UnverifiedPrefix + r.Answer
	}
	return finish(r)
}

// guardDirect stops a question that matches the corpus well from skipping
// retrieval, whichever router produced the plan.
func (p *Pipeline) guardDirect(query string, r Route, f Filter) Route {
	if r.Kind != RouteDirect || IsGreeting(query) {
		return r
	}
	terms := Tokenize(query)
	if hits := p.Index.searchBM25(terms, f, 1); len(hits) > 0 {
		c := p.Index.Chunks[hits[0].idx]
		if p.Index.Coverage(terms, c.Header+"\n"+c.Text) >= 0.5 {
			return Route{Kind: RouteSingle, Focus: query, Reason: "direct overridden: matching documents exist"}
		}
	}
	return r
}

func (p *Pipeline) direct(ctx context.Context, query string) (answer string, abstained bool, warning string) {
	if p.Model != nil {
		out, err := callModel(ctx, p.Model, llm.Request{
			System:    "Answer briefly. If the question needs private documents you do not have, say so.",
			Prompt:    query,
			MaxTokens: 300,
		})
		if err == nil && strings.TrimSpace(out) != "" {
			return strings.TrimSpace(out), false, "answered without retrieval; not checked against sources"
		}
		if err != nil {
			warning = "direct model call failed: " + err.Error()
		}
	}
	if IsGreeting(query) {
		return greetingReply, false, warning
	}
	return directNoModel, true, warning
}

func better(a, b Result) bool {
	rank := func(r Result) int {
		switch {
		case r.Verified:
			return 3
		case !r.Abstained:
			return 2
		default:
			return 1
		}
	}
	if rank(a) != rank(b) {
		return rank(a) > rank(b)
	}
	return a.Grounded.Ratio > b.Grounded.Ratio
}

func (p *Pipeline) attempt(ctx context.Context, route Route, q, query string, f Filter, tr *tracer, iter int) Result {
	ps := p.retrieve(ctx, route, q, f, tr)
	top := ps[:min(len(ps), p.Cfg.TopK)]
	r := Result{Retrieved: top}

	t := time.Now()
	queries := route.queries()
	scores, err := p.Grader.Grade(ctx, queries, top)
	if err != nil {
		r.Warnings = append(r.Warnings, err.Error())
	}
	if len(scores) != len(top) {
		if err == nil {
			r.Warnings = append(r.Warnings, fmt.Sprintf("grader returned %d scores for %d passages; used lexical grader", len(scores), len(top)))
		}
		scores, _ = LexicalGrader{Index: p.Index}.Grade(ctx, queries, top)
	}
	r.Grade = Decide(scores, p.Cfg.Upper, p.Cfg.Lower)
	tr.add("grade", t, "iteration %d: %s (best %.2f over %d passages)", iter, r.Grade, maxOf(scores), len(top))

	t = time.Now()
	var used []Passage
	var note string
	r.Action, r.Evidence, used, note = p.correct(ctx, r.Grade, route, top, scores)
	if note != "" {
		r.Warnings = append(r.Warnings, note)
	}
	tr.add("correct", t, "%s: %d evidence strips", r.Action, len(r.Evidence))

	t = time.Now()
	ans, err := p.Generator.Generate(ctx, GenRequest{Query: query, Focus: route.Focus, Kind: route.Kind, Evidence: r.Evidence})
	if err != nil {
		r.Warnings = append(r.Warnings, err.Error())
		if ans == "" {
			ans = NoAnswer
		}
	}
	r.Answer = ans
	tr.add("generate", t, "%d characters", len(ans))

	t = time.Now()
	if ans == NoAnswer {
		r.Abstained = true
		tr.add("verify", t, "abstained")
		return r
	}
	var removed []string
	for _, u := range used {
		removed = append(removed, u.Removed...)
	}
	if LeakCheck(ans, query, r.Evidence, removed) {
		r.Answer, r.Abstained, r.Leak = LeakAnswer, true, true
		tr.add("verify", t, "blocked: answer repeated screened-out instructions")
		return r
	}
	r.Grounded = CheckGrounded(ans, r.Evidence, p.Cfg.SentenceOverlap)
	r.Verified = r.Grounded.Total > 0 && r.Grounded.Ratio >= p.Cfg.MinGrounded
	tr.add("verify", t, "%d of %d sentences supported", r.Grounded.Supported, r.Grounded.Total)
	return r
}

func maxOf(xs []float64) float64 {
	m := 0.0
	for _, x := range xs {
		m = max(m, x)
	}
	return m
}

func (p *Pipeline) retrieve(ctx context.Context, route Route, q string, f Filter, tr *tracer) []Passage {
	t := time.Now()
	queries := []string{q}
	for _, s := range route.SubQueries {
		if s != q {
			queries = append(queries, s)
		}
	}
	var lists [][]Passage
	for _, sq := range queries {
		h := p.hybrid(ctx, sq, f)
		lists = append(lists, h.bm, h.vec)
	}
	extra := ""
	switch route.Kind {
	case RouteMultiHop:
		var seeds []int
		for _, ps := range RRF(lists, 60) {
			if i, ok := p.Index.pos[ps.ID]; ok && len(seeds) < 5 {
				seeds = append(seeds, i)
			}
		}
		var g []Passage
		for _, h := range p.Index.graphExpand(seeds, q, f, p.Cfg.RetrieveK) {
			g = append(g, passageFromChunk(p.Index.Chunks[h.idx], "graph", h.score))
		}
		lists = append(lists, g)
		extra = fmt.Sprintf(", %d from the entity graph", len(g))
	case RouteGlobal:
		var s []Passage
		for _, h := range p.Index.searchSummaries(Tokenize(focusOf(q)), f, p.Cfg.RetrieveK) {
			s = append(s, passageFromChunk(p.Index.Summaries[h.idx], "summary", h.score))
		}
		lists = append(lists, s)
		extra = fmt.Sprintf(", %d summary nodes", len(s))
	}
	fused := RRF(lists, 60)
	reranked := p.Reranker.Rerank(ctx, route.Focus, fused)
	tr.add("retrieve", t, "%d queries, %d fused passages%s", len(queries), len(fused), extra)
	return reranked
}

func (p *Pipeline) hybrid(ctx context.Context, sq string, f Filter) hybridLists {
	key := sq + "|" + f.key()
	if h, ok := p.retrieval.Get(key); ok {
		return h
	}
	var h hybridLists
	for _, x := range p.Index.searchBM25(Tokenize(sq), f, p.Cfg.RetrieveK) {
		h.bm = append(h.bm, passageFromChunk(p.Index.Chunks[x.idx], "bm25", x.score))
	}
	for _, x := range p.Index.searchDense(ctx, sq, f, p.Cfg.RetrieveK) {
		h.vec = append(h.vec, passageFromChunk(p.Index.Chunks[x.idx], "vector", x.score))
	}
	if ctx.Err() == nil {
		p.retrieval.Add(key, h)
	}
	return h
}

// correct applies the CRAG action for the grade and returns the evidence and
// the passages it came from.
func (p *Pipeline) correct(ctx context.Context, g Grade, route Route, ps []Passage, scores []float64) (string, []Evidence, []Passage, string) {
	focus := route.Focus
	var internal []Passage
	for i, s := range scores {
		if s >= p.Cfg.Lower {
			internal = append(internal, ps[i])
		}
	}
	var used []Passage
	var action, note string
	switch g {
	case Correct:
		action, used = "refine", internal
	case Incorrect:
		action = "web"
		used, note = p.webPassages(ctx, focus)
	default:
		action = "refine+web"
		web, n := p.webPassages(ctx, focus)
		used, note = append(internal, web...), n
	}
	queries := append([]string{focus}, route.SubQueries...)
	return action, Refine(p.Index, queries, used, p.Cfg.MinStrip, p.Cfg.MaxEvidence), used, note
}

func (p *Pipeline) webPassages(ctx context.Context, focus string) ([]Passage, string) {
	if p.Web == nil {
		return nil, "web fallback skipped: no web searcher configured"
	}
	if !budgetFrom(ctx).TakeWeb() {
		return nil, "web fallback skipped: web budget exhausted"
	}
	results, err := p.Web.Search(ctx, RewriteForWeb(focus), p.Cfg.WebResults)
	if err != nil {
		return nil, "web search failed: " + err.Error()
	}
	var out []Passage
	dropped := 0
	for _, r := range results {
		trust, ok := p.Trust.Trust(r.URL)
		if !ok {
			dropped++
			continue
		}
		text, removed := Screen(r.Text)
		title, tRemoved := Screen(r.Title)
		removed = append(removed, tRemoved...)
		out = append(out, Passage{
			ID: "web:" + r.URL, DocID: r.URL, Title: title, Header: title, Source: r.URL,
			Text: text, Origin: "web", Trust: trust, Removed: removed,
		})
	}
	note := ""
	if dropped > 0 {
		note = fmt.Sprintf("web fallback dropped %d result(s) from denied or malformed URLs", dropped)
	}
	return out, note
}

// reformulate adds the rarest new terms from the best evidence, so the next
// retrieval round can reach passages the first wording missed.
func (p *Pipeline) reformulate(query string, ev []Evidence) string {
	have := TermSet(query)
	type tw struct {
		t string
		w float64
	}
	seen := map[string]bool{}
	var cands []tw
	for _, e := range ev[:min(2, len(ev))] {
		for _, t := range Tokenize(e.Text) {
			if !have[t] && !seen[t] {
				seen[t] = true
				cands = append(cands, tw{t, p.Index.IDF(t)})
			}
		}
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].w != cands[j].w {
			return cands[i].w > cands[j].w
		}
		return cands[i].t < cands[j].t
	})
	var add []string
	for i := 0; i < len(cands) && i < 3; i++ {
		add = append(add, cands[i].t)
	}
	if len(add) == 0 {
		return query
	}
	return query + " " + strings.Join(add, " ")
}
