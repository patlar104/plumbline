package rag

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
)

// Passage is one retrieved unit handed between stages.
type Passage struct {
	ID      string    `json:"id"`
	DocID   string    `json:"doc_id"`
	Title   string    `json:"title"`
	Header  string    `json:"-"`
	Source  string    `json:"source"`
	Text    string    `json:"-"`
	Updated time.Time `json:"-"`
	Origin  string    `json:"origin"` // bm25, vector, graph, summary, web
	Score   float64   `json:"score"`
	Trust   float64   `json:"trust"` // 0..1; web passages below 1 unless allow-listed
	Removed []string  `json:"-"`     // sentences removed by Screen
}

// Flagged reports whether screening removed anything from the passage.
func (p Passage) Flagged() bool { return len(p.Removed) > 0 }

func passageFromChunk(c Chunk, origin string, score float64) Passage {
	return Passage{
		ID: c.ID, DocID: c.DocID, Title: c.Title, Header: c.Header, Source: c.Source,
		Text: c.Text, Updated: c.Updated, Origin: origin, Score: score, Trust: 1, Removed: c.Removed,
	}
}

// RRF merges ranked lists with reciprocal rank fusion: each passage scores
// the sum of 1/(k+rank) over the lists it appears in. The first copy of a
// passage is kept; its Origin lists every source that found it.
func RRF(lists [][]Passage, k float64) []Passage {
	type acc struct {
		p       Passage
		score   float64
		origins []string
		first   int
	}
	byID := map[string]*acc{}
	order := 0
	for _, list := range lists {
		for rank, p := range list {
			a, ok := byID[p.ID]
			if !ok {
				a = &acc{p: p, first: order}
				order++
				byID[p.ID] = a
			}
			a.score += 1 / (k + float64(rank+1))
			if !contains(a.origins, p.Origin) {
				a.origins = append(a.origins, p.Origin)
			}
		}
	}
	out := make([]Passage, 0, len(byID))
	firsts := map[string]int{}
	for _, a := range byID {
		p := a.p
		p.Score = a.score
		p.Origin = strings.Join(a.origins, "+")
		firsts[p.ID] = a.first
		out = append(out, p)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return firsts[out[i].ID] < firsts[out[j].ID]
	})
	return out
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

// Reranker reorders fused passages. A cross-encoder implements the same
// interface; LexicalReranker is the offline stand-in.
type Reranker interface {
	Rerank(ctx context.Context, query string, ps []Passage) []Passage
}

// LexicalReranker blends IDF-weighted query coverage with the fused rank,
// then discounts passages that are untrusted or had content screened out.
type LexicalReranker struct{ Index *Index }

// Rerank implements Reranker. It never mutates its input.
func (r LexicalReranker) Rerank(_ context.Context, query string, ps []Passage) []Passage {
	out := append([]Passage(nil), ps...)
	if len(out) == 0 {
		return out
	}
	terms := Tokenize(query)
	maxFused := 0.0
	for _, p := range out {
		maxFused = max(maxFused, p.Score)
	}
	for i := range out {
		cov := r.Index.Coverage(terms, out[i].Header+"\n"+out[i].Text)
		fused := 0.0
		if maxFused > 0 {
			fused = out[i].Score / maxFused
		}
		s := 0.7*cov + 0.3*fused
		if out[i].Flagged() {
			s *= 0.8
		}
		out[i].Score = s * out[i].Trust
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	return out
}

// WebResult is one page returned by a web searcher.
type WebResult struct {
	URL   string `json:"url"`
	Title string `json:"title"`
	Text  string `json:"text"`
}

// WebSearcher finds pages for the corrective web fallback.
type WebSearcher interface {
	Search(ctx context.Context, query string, n int) ([]WebResult, error)
}

// StaticWeb searches a fixed set of pages. It stands in for a real search
// API in tests and offline demos.
type StaticWeb struct{ Pages []WebResult }

// LoadStaticWeb reads a JSON array of {url, title, text} pages.
func LoadStaticWeb(path string) (*StaticWeb, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var pages []WebResult
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&pages); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	for i, p := range pages {
		if u, err := url.Parse(p.URL); err != nil || u.Host == "" {
			return nil, fmt.Errorf("%s: page %d has an invalid url %q", path, i, p.URL)
		}
	}
	return &StaticWeb{Pages: pages}, nil
}

// Search implements WebSearcher by term overlap.
func (w *StaticWeb) Search(ctx context.Context, query string, n int) ([]WebResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	q := Unique(Tokenize(query))
	type scored struct {
		r WebResult
		s int
	}
	var hits []scored
	for _, p := range w.Pages {
		have := TermSet(p.Title + " " + p.Text)
		s := 0
		for _, t := range q {
			if have[t] {
				s++
			}
		}
		if s > 0 {
			hits = append(hits, scored{p, s})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].s > hits[j].s })
	var out []WebResult
	for i := 0; i < len(hits) && i < n; i++ {
		out = append(out, hits[i].r)
	}
	return out, nil
}

// TrustPolicy scores web sources by domain. Deny wins over Allow.
type TrustPolicy struct {
	Allow []string // domain suffixes trusted fully
	Deny  []string // domain suffixes never used
}

// Trust returns the trust score for a URL and whether it may be used at all.
// Unknown domains get 0.6; malformed URLs are rejected.
func (t TrustPolicy) Trust(raw string) (float64, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return 0, false
	}
	host := strings.ToLower(u.Hostname())
	match := func(suffix string) bool {
		suffix = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(suffix), "."))
		return suffix != "" && (host == suffix || strings.HasSuffix(host, "."+suffix))
	}
	for _, d := range t.Deny {
		if match(d) {
			return 0, false
		}
	}
	for _, a := range t.Allow {
		if match(a) {
			return 1, true
		}
	}
	return 0.6, true
}

// RewriteForWeb turns a question into a keyword query, as CRAG does before
// its web search: stopwords and punctuation go, content words stay in order.
func RewriteForWeb(query string) string {
	return strings.Join(Unique(Tokenize(query)), " ")
}
