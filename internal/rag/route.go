package rag

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/patlar104/plumbline/internal/llm"
)

// RouteKind is the retrieval strategy for a question.
type RouteKind string

// Route kinds, after Adaptive-RAG's no / single / multi-step split plus a
// corpus-wide kind for questions about the collection as a whole.
const (
	RouteDirect   RouteKind = "direct"   // no retrieval
	RouteSingle   RouteKind = "single"   // hybrid search
	RouteMultiHop RouteKind = "multihop" // hybrid search per sub-query + entity graph
	RouteGlobal   RouteKind = "global"   // hybrid search + summary nodes
)

// Route is the router's plan for one question.
type Route struct {
	Kind       RouteKind `json:"kind"`
	SubQueries []string  `json:"sub_queries,omitempty"`
	Focus      string    `json:"focus"` // what grading and generation should match
	Reason     string    `json:"reason"`
}

// queries returns the focus question followed by its distinct sub-questions.
func (r Route) queries() []string {
	out := []string{r.Focus}
	for _, s := range r.SubQueries {
		if s != r.Focus && !contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

// Router plans retrieval for a question.
type Router interface {
	Route(ctx context.Context, query string) (Route, error)
}

var (
	greetingRe = regexp.MustCompile(`(?i)^\s*(hi|hello|hey|thanks|thank you|good (morning|afternoon|evening))(\s+(there|claude|all|everyone))?\s*[!.?]*\s*$`)
	mathRe     = regexp.MustCompile(`^[\d\s+\-*/().=?^%]+$`)
	globalRe   = regexp.MustCompile(`(?i)\b(overview|summari[sz]e|summary|main (themes|topics|points|policies|ideas)|across (all|the)|list (all|every)|high[- ]level)\b`)
	multiRe    = regexp.MustCompile(`(?i)\b(compare|comparison|versus|vs\.?|difference between|both|used by|that uses|which uses|owned by|led by|leads the)\b|\s+and\s+`)
	splitRe    = regexp.MustCompile(`(?i)\s+(?:and|vs\.?|versus|compared (?:to|with))\s+`)
	metaWords  = map[string]bool{"overview": true, "summarize": true, "summarise": true, "summary": true, "main": true,
		"themes": true, "topics": true, "points": true, "list": true, "high-level": true, "across": true, "ideas": true}
)

// IsGreeting reports small talk that needs no documents.
func IsGreeting(q string) bool { return greetingRe.MatchString(q) }

func isMath(q string) bool {
	return mathRe.MatchString(q) && strings.ContainsAny(q, "0123456789") && strings.ContainsAny(q, "+-*/^%")
}

// focusOf strips routing meta-words so "Summarize the main policies" grades
// against "policies", not against the word "summarize".
func focusOf(q string) string {
	var kept []string
	for _, w := range strings.Fields(q) {
		if !metaWords[strings.ToLower(strings.Trim(w, ".,!?;:"))] {
			kept = append(kept, w)
		}
	}
	if len(kept) == 0 {
		return q
	}
	return strings.Join(kept, " ")
}

// HeuristicRouter is the rule-based stand-in for a trained classifier.
type HeuristicRouter struct{}

// Route implements Router.
func (HeuristicRouter) Route(_ context.Context, q string) (Route, error) {
	switch {
	case IsGreeting(q):
		return Route{Kind: RouteDirect, Focus: q, Reason: "greeting"}, nil
	case isMath(q):
		return Route{Kind: RouteDirect, Focus: q, Reason: "arithmetic"}, nil
	case globalRe.MatchString(q):
		return Route{Kind: RouteGlobal, Focus: focusOf(q), Reason: "corpus-wide wording"}, nil
	case multiRe.MatchString(q):
		var subs []string
		for _, p := range splitRe.Split(q, -1) {
			if p = strings.TrimSpace(p); len(Tokenize(p)) > 0 && p != q {
				subs = append(subs, p)
			}
		}
		return Route{Kind: RouteMultiHop, SubQueries: subs, Focus: q, Reason: "comparison or chained wording"}, nil
	default:
		return Route{Kind: RouteSingle, Focus: q, Reason: "default"}, nil
	}
}

// LLMRouter asks a model to classify the question and falls back to
// Fallback when the output is missing, malformed or over budget.
type LLMRouter struct {
	Model    llm.Model
	Fallback Router
}

const routerSystem = `Classify how to answer a question over an organisation's private documents.
Return only JSON: {"kind": "direct"|"single"|"multihop"|"global", "sub_queries": ["..."]}
- direct: greetings, small talk or arithmetic that need no documents.
- single: one fact likely found in one place.
- multihop: needs facts from several places chained or compared; give 2-4 short sub_queries.
- global: asks about the collection as a whole (themes, overviews, lists of everything).
Questions that could be about the organisation are never direct.`

// Route implements Router.
func (r LLMRouter) Route(ctx context.Context, q string) (Route, error) {
	out, err := callModel(ctx, r.Model, llm.Request{System: routerSystem, Prompt: "Question: " + q, MaxTokens: 200})
	if err == nil {
		var parsed struct {
			Kind       string   `json:"kind"`
			SubQueries []string `json:"sub_queries"`
		}
		i, j := strings.Index(out, "{"), strings.LastIndex(out, "}")
		if i < 0 || j < i {
			err = fmt.Errorf("no JSON object in router output")
		} else if err = json.Unmarshal([]byte(out[i:j+1]), &parsed); err == nil {
			kind := RouteKind(parsed.Kind)
			switch kind {
			case RouteDirect, RouteSingle, RouteMultiHop, RouteGlobal:
				var subs []string
				for _, s := range parsed.SubQueries {
					if s = strings.TrimSpace(s); s != "" && len(subs) < 4 {
						subs = append(subs, s)
					}
				}
				focus := q
				if kind == RouteGlobal {
					focus = focusOf(q)
				}
				return Route{Kind: kind, SubQueries: subs, Focus: focus, Reason: "model"}, nil
			default:
				err = fmt.Errorf("router returned unknown kind %q", parsed.Kind)
			}
		}
	}
	rt, ferr := r.Fallback.Route(ctx, q)
	if ferr != nil {
		return Route{}, ferr
	}
	rt.Reason += " (model router fell back: " + err.Error() + ")"
	return rt, nil
}
