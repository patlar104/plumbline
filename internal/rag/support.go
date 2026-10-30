package rag

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/patlar104/plumbline/internal/llm"
)

// ErrBudget is returned when a model call or web fetch would exceed the
// per-query budget. Callers fall back to their offline stand-in.
var ErrBudget = errors.New("per-query budget exhausted")

// Budget caps model calls and web fetches for one query. A nil *Budget is
// unlimited. It is safe for concurrent use.
type Budget struct {
	mu             sync.Mutex
	maxLLM, maxWeb int
	llm, web       int
}

// NewBudget returns a budget with the given caps.
func NewBudget(maxLLM, maxWeb int) *Budget { return &Budget{maxLLM: maxLLM, maxWeb: maxWeb} }

// TakeLLM reserves one model call, reporting false when none are left.
func (b *Budget) TakeLLM() bool {
	if b == nil {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.llm >= b.maxLLM {
		return false
	}
	b.llm++
	return true
}

// TakeWeb reserves one web search, reporting false when none are left.
func (b *Budget) TakeWeb() bool {
	if b == nil {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.web >= b.maxWeb {
		return false
	}
	b.web++
	return true
}

// Used reports the model calls and web fetches taken so far.
func (b *Budget) Used() (llmCalls, webFetches int) {
	if b == nil {
		return 0, 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.llm, b.web
}

type budgetKey struct{}

// WithBudget attaches a budget to ctx.
func WithBudget(ctx context.Context, b *Budget) context.Context {
	return context.WithValue(ctx, budgetKey{}, b)
}

func budgetFrom(ctx context.Context) *Budget {
	b, _ := ctx.Value(budgetKey{}).(*Budget)
	return b
}

// callModel spends one unit of the context's budget, then calls the model.
func callModel(ctx context.Context, m llm.Model, req llm.Request) (string, error) {
	if m == nil {
		return "", errors.New("no model configured")
	}
	if !budgetFrom(ctx).TakeLLM() {
		return "", ErrBudget
	}
	return m.Complete(ctx, req)
}

// LRU is a size-bounded least-recently-used cache, safe for concurrent use.
type LRU[K comparable, V any] struct {
	mu  sync.Mutex
	cap int
	ll  *list.List
	m   map[K]*list.Element
}

type lruEntry[K comparable, V any] struct {
	k K
	v V
}

// NewLRU returns a cache holding at most capacity entries (minimum 1).
func NewLRU[K comparable, V any](capacity int) *LRU[K, V] {
	return &LRU[K, V]{cap: max(1, capacity), ll: list.New(), m: map[K]*list.Element{}}
}

// Get returns the value for k and marks it recently used.
func (c *LRU[K, V]) Get(k K) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.m[k]; ok {
		c.ll.MoveToFront(e)
		return e.Value.(*lruEntry[K, V]).v, true
	}
	var zero V
	return zero, false
}

// Add stores v under k, evicting the least recently used entry when full.
func (c *LRU[K, V]) Add(k K, v V) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.m[k]; ok {
		e.Value.(*lruEntry[K, V]).v = v
		c.ll.MoveToFront(e)
		return
	}
	c.m[k] = c.ll.PushFront(&lruEntry[K, V]{k, v})
	if c.ll.Len() > c.cap {
		last := c.ll.Back()
		c.ll.Remove(last)
		delete(c.m, last.Value.(*lruEntry[K, V]).k)
	}
}

// Len reports the number of cached entries.
func (c *LRU[K, V]) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ll.Len()
}

// Event is one traced pipeline step.
type Event struct {
	Step       string  `json:"step"`
	Detail     string  `json:"detail"`
	DurationMS float64 `json:"duration_ms"`
}

type tracer struct{ events []Event }

func (t *tracer) add(step string, start time.Time, format string, args ...any) {
	t.events = append(t.events, Event{
		Step:       step,
		Detail:     fmt.Sprintf(format, args...),
		DurationMS: float64(time.Since(start).Microseconds()) / 1000,
	})
}
