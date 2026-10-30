// Package rag implements the retrieval pipeline: routing, hybrid retrieval,
// fusion, CRAG-style grading and correction, generation, groundedness
// checking and a bounded retry loop. Every model-backed stage has an offline
// stand-in so the pipeline runs and tests without network access.
package rag

import (
	"strings"
	"unicode"
)

var stopwords = func() map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(`a an the and or but if then else of to in on at by for with from into onto
	about as is are was were be been being am do does did doing done have has had having can could should would
	will shall may might must not no nor so than too very just also only what which who whom whose when where why
	how many much more most some any all each every both either neither other such own same few i me my mine we us
	our ours you your yours he him his she her hers it its they them their theirs this that these those there here
	get gets got give gives gave tell told please use uses used using make makes made let lets`) {
		m[w] = true
	}
	return m
}()

// IsStopword reports whether the lowercase word w carries no retrieval signal.
func IsStopword(w string) bool { return stopwords[w] }

// Tokenize lowercases s, splits it on anything that is not a letter or digit,
// drops stopwords and single letters, and applies a light plural stemmer.
func Tokenize(s string) []string {
	fields := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if stopwords[f] {
			continue
		}
		if len([]rune(f)) < 2 && !isDigits(f) {
			continue
		}
		out = append(out, stem(f))
	}
	return out
}

// Unique returns the distinct items of xs in first-seen order.
func Unique(xs []string) []string {
	seen := make(map[string]bool, len(xs))
	out := make([]string, 0, len(xs))
	for _, x := range xs {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

// TermSet returns the set of tokens in s.
func TermSet(s string) map[string]bool {
	set := map[string]bool{}
	for _, t := range Tokenize(s) {
		set[t] = true
	}
	return set
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}

// stem strips English plural endings. It is deliberately conservative: a
// wrong merge hurts precision more than a missed one hurts recall.
func stem(w string) string {
	n := len(w)
	switch {
	case n > 4 && strings.HasSuffix(w, "ies"):
		return w[:n-3] + "y"
	case n > 3 && strings.HasSuffix(w, "s") && !strings.HasSuffix(w, "ss") &&
		!strings.HasSuffix(w, "us") && !strings.HasSuffix(w, "is"):
		return w[:n-1]
	}
	return w
}

// Sentences splits s at newlines and at . ! ? followed by whitespace or the
// end of the text, so decimals such as "3.2" stay intact.
func Sentences(s string) []string {
	var out []string
	var b strings.Builder
	flush := func() {
		if t := strings.TrimSpace(b.String()); t != "" {
			out = append(out, t)
		}
		b.Reset()
	}
	rs := []rune(s)
	for i, r := range rs {
		if r == '\n' {
			flush()
			continue
		}
		b.WriteRune(r)
		if (r == '.' || r == '!' || r == '?') && (i+1 == len(rs) || unicode.IsSpace(rs[i+1])) {
			flush()
		}
	}
	flush()
	return out
}
