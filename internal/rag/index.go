package rag

import (
	"context"
	"fmt"
	"hash/fnv"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Embedder turns text into a unit vector. HashEmbedder is the offline
// stand-in; a real embedding model implements the same interface.
type Embedder interface {
	Embed(text string) []float32
}

// HashEmbedder hashes words and character trigrams into a fixed number of
// dimensions. It catches spelling variants and shared word pieces, not
// synonyms: "vacation" and "time off" stay far apart.
type HashEmbedder struct{ Dim int }

// Embed implements Embedder.
func (h HashEmbedder) Embed(text string) []float32 {
	dim := h.Dim
	if dim <= 0 {
		dim = 512
	}
	v := make([]float32, dim)
	for _, tok := range Tokenize(text) {
		hashAdd(v, "w:"+tok, 1)
		rs := []rune("<" + tok + ">")
		for i := 0; i+3 <= len(rs); i++ {
			hashAdd(v, "g:"+string(rs[i:i+3]), 0.5)
		}
	}
	var norm float64
	for _, x := range v {
		norm += float64(x) * float64(x)
	}
	if norm > 0 {
		inv := float32(1 / math.Sqrt(norm))
		for i := range v {
			v[i] *= inv
		}
	}
	return v
}

func hashAdd(v []float32, feature string, w float32) {
	h := fnv.New64a()
	h.Write([]byte(feature))
	s := h.Sum64()
	if s>>63 == 1 {
		w = -w
	}
	v[s%uint64(len(v))] += w
}

func dot(a, b []float32) float64 {
	var s float64
	for i := range a {
		s += float64(a[i]) * float64(b[i])
	}
	return s
}

type posting struct{ doc, tf int }

type bm25 struct {
	k1, b    float64
	postings map[string][]posting
	lens     []int
	avg      float64
	n        int
}

func newBM25(docs [][]string) *bm25 {
	bm := &bm25{k1: 1.2, b: 0.75, postings: map[string][]posting{}, lens: make([]int, len(docs)), n: len(docs)}
	total := 0
	for i, toks := range docs {
		bm.lens[i] = len(toks)
		total += len(toks)
		tf := map[string]int{}
		for _, t := range toks {
			tf[t]++
		}
		keys := make([]string, 0, len(tf))
		for t := range tf {
			keys = append(keys, t)
		}
		sort.Strings(keys)
		for _, t := range keys {
			bm.postings[t] = append(bm.postings[t], posting{i, tf[t]})
		}
	}
	if len(docs) > 0 {
		bm.avg = float64(total) / float64(len(docs))
	}
	return bm
}

func (bm *bm25) idf(t string) float64 {
	df := float64(len(bm.postings[t]))
	return math.Log(1 + (float64(bm.n)-df+0.5)/(df+0.5))
}

func (bm *bm25) search(q []string, allow func(int) bool, k int) []hit {
	scores := map[int]float64{}
	for _, t := range Unique(q) {
		idf := bm.idf(t)
		for _, p := range bm.postings[t] {
			if allow != nil && !allow(p.doc) {
				continue
			}
			tf := float64(p.tf)
			norm := 1 - bm.b + bm.b*float64(bm.lens[p.doc])/bm.avg
			scores[p.doc] += idf * tf * (bm.k1 + 1) / (tf + bm.k1*norm)
		}
	}
	return topK(scores, k)
}

type hit struct {
	idx   int
	score float64
}

func topK(m map[int]float64, k int) []hit {
	hs := make([]hit, 0, len(m))
	for i, s := range m {
		hs = append(hs, hit{i, s})
	}
	sort.Slice(hs, func(a, b int) bool {
		if hs[a].score != hs[b].score {
			return hs[a].score > hs[b].score
		}
		return hs[a].idx < hs[b].idx
	})
	if k > 0 && len(hs) > k {
		hs = hs[:k]
	}
	return hs
}

// Filter restricts what a query may see. It runs before scoring, so a
// restricted chunk never reaches the reranker, the grader or a model.
type Filter struct {
	Groups            []string      // the user's groups
	MaxAge            time.Duration // 0 = no age limit; documents with no date always pass
	Now               time.Time
	IncludeSuperseded bool // also search documents a newer one replaces
}

// AllowsChunk applies every filter rule to one chunk.
func (f Filter) AllowsChunk(c Chunk) bool {
	if c.Superseded && !f.IncludeSuperseded {
		return false
	}
	return f.Allows(c.Groups, c.Updated)
}

// Allows reports whether content with these groups and date is visible.
func (f Filter) Allows(groups []string, updated time.Time) bool {
	if len(groups) > 0 {
		ok := false
		for _, g := range groups {
			for _, ug := range f.Groups {
				if strings.EqualFold(g, ug) {
					ok = true
				}
			}
		}
		if !ok {
			return false
		}
	}
	if f.MaxAge > 0 && !updated.IsZero() && f.Now.Sub(updated) > f.MaxAge {
		return false
	}
	return true
}

func (f Filter) key() string {
	gs := make([]string, 0, len(f.Groups))
	for _, g := range f.Groups {
		gs = append(gs, strings.ToLower(strings.TrimSpace(g)))
	}
	sort.Strings(gs)
	return fmt.Sprintf("%s|%s|%t", strings.Join(Unique(gs), ","), f.MaxAge, f.IncludeSuperseded)
}

// Index holds every retrieval structure for one corpus. It is read-only after
// Build and safe for concurrent use.
type Index struct {
	Chunks    []Chunk
	Summaries []Chunk  // one extractive summary node per document
	Warnings  []string // build problems that did not stop indexing

	emb       Embedder
	bm        *bm25
	sumBM     *bm25
	vecs      [][]float32
	chunkEnts [][]string
	entIdx    map[string][]int
	pos       map[string]int // chunk id -> index
}

// Build indexes docs. maxWords bounds the chunk size.
func Build(docs []Doc, emb Embedder, maxWords int) *Index {
	if emb == nil {
		emb = HashEmbedder{}
	}
	ix := &Index{emb: emb, entIdx: map[string][]int{}, pos: map[string]int{}}
	known := map[string]bool{}
	for _, d := range docs {
		known[d.ID] = true
	}
	superseded := map[string]bool{}
	for _, d := range docs {
		for _, old := range d.Supersedes {
			switch {
			case old == d.ID:
				ix.Warnings = append(ix.Warnings, fmt.Sprintf("%s lists itself in supersedes; ignored", d.ID))
			case !known[old]:
				ix.Warnings = append(ix.Warnings, fmt.Sprintf("%s supersedes unknown document %q", d.ID, old))
			default:
				superseded[old] = true
			}
		}
	}
	byDoc := map[string][]Chunk{}
	for _, d := range docs {
		cs := ChunkDoc(d, maxWords)
		for i := range cs {
			cs[i].Superseded = superseded[d.ID]
		}
		ix.Chunks = append(ix.Chunks, cs...)
		byDoc[d.ID] = cs
	}
	toks := make([][]string, len(ix.Chunks))
	ix.vecs = make([][]float32, len(ix.Chunks))
	ix.chunkEnts = make([][]string, len(ix.Chunks))
	for i, c := range ix.Chunks {
		ix.pos[c.ID] = i
		toks[i] = Tokenize(c.Header + "\n" + c.Text)
		ix.vecs[i] = emb.Embed(c.Header + "\n" + c.Text)
		ix.chunkEnts[i] = extractEntities(c.Text)
		for _, e := range ix.chunkEnts[i] {
			ix.entIdx[e] = append(ix.entIdx[e], i)
		}
	}
	ix.bm = newBM25(toks)

	// Entities in more than half the chunks link everything to everything.
	limit := max(2, len(ix.Chunks)/2)
	for e, cs := range ix.entIdx {
		if len(cs) > limit {
			delete(ix.entIdx, e)
		}
	}

	sumToks := [][]string{}
	for _, d := range docs {
		cs := byDoc[d.ID]
		if len(cs) == 0 {
			continue
		}
		var firsts []string
		for _, c := range cs {
			if ss := Sentences(c.Text); len(ss) > 0 && len(firsts) < 6 {
				firsts = append(firsts, ss[0])
			}
		}
		s := Chunk{
			ID: d.ID + "#summary", DocID: d.ID, Title: d.Title, Header: d.Title, Source: d.Source,
			Updated: d.Updated, Groups: d.Groups, Superseded: superseded[d.ID],
			Text: d.Title + ": " + strings.Join(firsts, " "),
		}
		ix.Summaries = append(ix.Summaries, s)
		sumToks = append(sumToks, Tokenize(s.Text))
	}
	ix.sumBM = newBM25(sumToks)
	return ix
}

// IDF is the corpus inverse document frequency of a token. Tokens the corpus
// has never seen get the highest weight: a question about something absent
// should not look answered.
func (ix *Index) IDF(term string) float64 { return ix.bm.idf(term) }

// Coverage is the IDF-weighted share of the query terms found in text.
func (ix *Index) Coverage(queryTerms []string, text string) float64 {
	q := Unique(queryTerms)
	if len(q) == 0 {
		return 0
	}
	have := TermSet(text)
	var tot, got float64
	for _, t := range q {
		w := ix.IDF(t)
		tot += w
		if have[t] {
			got += w
		}
	}
	if tot == 0 {
		return 0
	}
	return got / tot
}

func (ix *Index) allowChunk(f Filter) func(int) bool {
	return func(i int) bool { return f.AllowsChunk(ix.Chunks[i]) }
}

// searchBM25 returns chunk hits for the query terms.
func (ix *Index) searchBM25(terms []string, f Filter, k int) []hit {
	return ix.bm.search(terms, ix.allowChunk(f), k)
}

// searchDense returns chunk hits by cosine similarity above a noise floor.
func (ix *Index) searchDense(ctx context.Context, text string, f Filter, k int) []hit {
	q := ix.emb.Embed(text)
	allow := ix.allowChunk(f)
	scores := map[int]float64{}
	for i, v := range ix.vecs {
		if i%256 == 0 && ctx.Err() != nil {
			break
		}
		if !allow(i) {
			continue
		}
		if s := dot(q, v); s > 0.1 {
			scores[i] = s
		}
	}
	return topK(scores, k)
}

// searchSummaries returns summary-node hits for corpus-wide questions.
func (ix *Index) searchSummaries(terms []string, f Filter, k int) []hit {
	return ix.sumBM.search(terms, func(i int) bool { return f.AllowsChunk(ix.Summaries[i]) }, k)
}

// graphExpand returns chunks that share entities with the seed chunks or the
// query: one hop across the entity graph, weighted by entity rarity.
func (ix *Index) graphExpand(seeds []int, query string, f Filter, k int) []hit {
	allow := ix.allowChunk(f)
	isSeed := map[int]bool{}
	ents := map[string]bool{}
	for _, s := range seeds {
		isSeed[s] = true
		for _, e := range ix.chunkEnts[s] {
			ents[e] = true
		}
	}
	for _, e := range extractEntities(query) {
		ents[e] = true
	}
	lq := strings.ToLower(query)
	for e := range ix.entIdx {
		if strings.Contains(lq, e) {
			ents[e] = true
		}
	}
	n := float64(len(ix.Chunks))
	scores := map[int]float64{}
	keys := make([]string, 0, len(ents))
	for e := range ents {
		keys = append(keys, e)
	}
	sort.Strings(keys)
	for _, e := range keys {
		cs := ix.entIdx[e]
		if len(cs) == 0 {
			continue
		}
		w := math.Log(1 + n/float64(len(cs)))
		for _, c := range cs {
			if !isSeed[c] && allow(c) {
				scores[c] += w
			}
		}
	}
	return topK(scores, k)
}

var entityRe = regexp.MustCompile(`[A-Z][A-Za-z0-9]*(?:[- ][A-Z][A-Za-z0-9]*)*|[A-Za-z]*[0-9][A-Za-z0-9-]*`)

// extractEntities finds names and product codes: capitalised word runs and
// tokens mixing letters with digits. A lone capitalised word that starts a
// sentence is skipped, since capitalisation there says nothing.
func extractEntities(text string) []string {
	var out []string
	for _, s := range Sentences(text) {
		for _, loc := range entityRe.FindAllStringIndex(s, -1) {
			m := s[loc[0]:loc[1]]
			hasDigit := strings.ContainsAny(m, "0123456789")
			hasLetter := strings.IndexFunc(m, func(r rune) bool { return r >= 'A' && r <= 'z' && (r <= 'Z' || r >= 'a') }) >= 0
			if !hasLetter {
				continue
			}
			words := strings.Fields(strings.ToLower(m))
			stripped := 0
			for len(words) > 0 && stopwords[words[0]] {
				words = words[1:]
				stripped++
			}
			if len(words) == 0 {
				continue
			}
			// A lone capitalised word opening a sentence says nothing about
			// whether it is a name ("Requests go through...").
			if !hasDigit && loc[0] == 0 && stripped == 0 && len(words) == 1 {
				continue
			}
			lm := strings.Join(words, " ")
			if len(lm) < 2 {
				continue
			}
			out = append(out, lm)
		}
	}
	return Unique(out)
}
