package llm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func client(url string) *Anthropic {
	return &Anthropic{APIKey: "test-key", Model: "test-model", BaseURL: url, Version: DefaultVersion,
		HTTP: &http.Client{Timeout: 5 * time.Second}, MaxRetries: 2, Backoff: time.Millisecond}
}

func TestCompleteSendsDocumentedRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/messages" {
			t.Errorf("got %s %s", r.Method, r.URL.Path)
		}
		for h, want := range map[string]string{"x-api-key": "test-key", "anthropic-version": "2023-06-01", "content-type": "application/json"} {
			if got := r.Header.Get(h); got != want {
				t.Errorf("header %s = %q, want %q", h, got, want)
			}
		}
		var body apiRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body.Model != "test-model" || body.MaxTokens != 77 || body.System != "sys" ||
			len(body.Messages) != 1 || body.Messages[0].Role != "user" || body.Messages[0].Content != "hi" {
			t.Errorf("bad body: %+v", body)
		}
		w.Write([]byte(`{"content":[{"type":"text","text":"Hello "},{"type":"tool_use"},{"type":"text","text":"there"}]}`))
	}))
	defer srv.Close()
	got, err := client(srv.URL+"/").Complete(context.Background(), Request{System: "sys", Prompt: "hi", MaxTokens: 77})
	if err != nil || got != "Hello there" {
		t.Fatalf("Complete = %q, %v", got, err)
	}
}

func TestRetriesOverloadThenSucceeds(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if n.Add(1) == 1 {
			w.WriteHeader(529)
			w.Write([]byte(`{"type":"error","error":{"type":"overloaded_error","message":"busy"}}`))
			return
		}
		w.Write([]byte(`{"content":[{"type":"text","text":"ok"}]}`))
	}))
	defer srv.Close()
	got, err := client(srv.URL).Complete(context.Background(), Request{Prompt: "x"})
	if err != nil || got != "ok" || n.Load() != 2 {
		t.Fatalf("got %q, %v after %d calls", got, err, n.Load())
	}
}

func TestClientErrorIsNotRetried(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n.Add(1)
		w.WriteHeader(400)
		w.Write([]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"bad model"}}`))
	}))
	defer srv.Close()
	_, err := client(srv.URL).Complete(context.Background(), Request{Prompt: "x"})
	var se *StatusError
	if !errors.As(err, &se) || se.Status != 400 || se.Type != "invalid_request_error" || n.Load() != 1 {
		t.Fatalf("err = %v after %d calls", err, n.Load())
	}
	if strings.Contains(err.Error(), "test-key") {
		t.Fatal("error text must never contain the API key")
	}
}

func TestRetriesExhausted(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n.Add(1)
		w.WriteHeader(500)
		w.Write([]byte("plain text failure"))
	}))
	defer srv.Close()
	_, err := client(srv.URL).Complete(context.Background(), Request{Prompt: "x"})
	if err == nil || n.Load() != 3 || !strings.Contains(err.Error(), "plain text failure") {
		t.Fatalf("err = %v after %d calls (want 1 + 2 retries)", err, n.Load())
	}
}

func TestEmptyContentAndCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"content":[]}`))
	}))
	defer srv.Close()
	if _, err := client(srv.URL).Complete(context.Background(), Request{Prompt: "x"}); err == nil {
		t.Fatal("a response without text must be an error")
	}

	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(503)
	}))
	defer slow.Close()
	c := client(slow.URL)
	c.Backoff = time.Hour
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := c.Complete(ctx, Request{Prompt: "x"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want deadline exceeded", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("cancellation did not interrupt the backoff")
	}
}

func TestFromEnv(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	if _, ok := FromEnv(); ok {
		t.Fatal("no key must mean no client")
	}
	t.Setenv("ANTHROPIC_API_KEY", " k ")
	t.Setenv("RAGLOOP_MODEL", "")
	t.Setenv("ANTHROPIC_BASE_URL", "")
	c, ok := FromEnv()
	if !ok || c.APIKey != "k" || c.Model != DefaultModel || c.BaseURL != DefaultBaseURL {
		t.Fatalf("FromEnv = %+v, %t", c, ok)
	}
}
