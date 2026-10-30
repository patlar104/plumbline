package llm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func localClient(url, model string) *Local {
	return &Local{BaseURL: url + "/v1", Model: model, HTTP: &http.Client{Timeout: 5 * time.Second}}
}

func TestLocalSendsChatCompletion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
			t.Errorf("got %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("content-type"); got != "application/json" {
			t.Errorf("content-type = %q", got)
		}
		var body chatRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body.Model != "gemma3:12b" || body.MaxTokens != 77 || body.Stream || len(body.Messages) != 2 ||
			body.Messages[0] != (apiMessage{Role: "system", Content: "sys"}) ||
			body.Messages[1] != (apiMessage{Role: "user", Content: "hi"}) {
			t.Errorf("bad body: %+v", body)
		}
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":" Hello there "}}]}`))
	}))
	defer srv.Close()
	got, err := localClient(srv.URL, "gemma3:12b").Complete(context.Background(), Request{System: "sys", Prompt: "hi", MaxTokens: 77})
	if err != nil || got != "Hello there" {
		t.Fatalf("Complete = %q, %v", got, err)
	}
}

func TestLocalOmitsEmptySystem(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body chatRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if len(body.Messages) != 1 || body.Messages[0].Role != "user" || body.MaxTokens != 1024 {
			t.Errorf("bad body: %+v", body)
		}
		w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer srv.Close()
	if _, err := localClient(srv.URL, "m").Complete(context.Background(), Request{Prompt: "hi"}); err != nil {
		t.Fatal(err)
	}
}

// Reasoning models put their thinking inline; the router and grader read the
// first { or [ in the reply, so thinking must not reach them.
func TestLocalStripsThinking(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"choices":[{"message":{"content":"<think>Maybe [0.1] or {\"kind\":\"direct\"}?</think>\n{\"kind\":\"single\"}"}}]}`))
	}))
	defer srv.Close()
	got, err := localClient(srv.URL, "m").Complete(context.Background(), Request{Prompt: "x"})
	if err != nil || got != `{"kind":"single"}` {
		t.Fatalf("Complete = %q, %v", got, err)
	}
}

func TestLocalErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(404)
		w.Write([]byte(`{"error":{"message":"model \"nope\" not found, try pulling it first","type":"api_error"}}`))
	}))
	defer srv.Close()
	_, err := localClient(srv.URL, "nope").Complete(context.Background(), Request{Prompt: "x"})
	var se *StatusError
	if !errors.As(err, &se) || se.Status != 404 || !strings.Contains(se.Message, "not found") {
		t.Fatalf("err = %v", err)
	}

	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"choices":[{"message":{"content":"<think>only thinking</think>"}}]}`))
	}))
	defer empty.Close()
	if _, err := localClient(empty.URL, "m").Complete(context.Background(), Request{Prompt: "x"}); err == nil {
		t.Fatal("a reply with no answer text must be an error")
	}
}

func TestLocalCheck(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/models" {
			t.Errorf("got %s %s", r.Method, r.URL.Path)
		}
		w.Write([]byte(`{"object":"list","data":[{"id":"gemma3:12b"},{"id":"qwen3:latest"}]}`))
	}))
	defer srv.Close()
	ctx := context.Background()
	for _, m := range []string{"gemma3:12b", "qwen3", "qwen3:latest"} {
		if err := localClient(srv.URL, m).Check(ctx); err != nil {
			t.Errorf("Check(%q) = %v", m, err)
		}
	}
	err := localClient(srv.URL, "llama3.2").Check(ctx)
	if err == nil || !strings.Contains(err.Error(), "ollama pull llama3.2") {
		t.Errorf("missing model: err = %v", err)
	}

	down := httptest.NewServer(http.NotFoundHandler())
	url := down.URL
	down.Close()
	if err := localClient(url, "m").Check(ctx); err == nil || !strings.Contains(err.Error(), "ollama serve") {
		t.Errorf("server down: err = %v", err)
	}
}

func TestLocalFromEnv(t *testing.T) {
	t.Setenv("PLUMBLINE_LOCAL_MODEL", "")
	t.Setenv("PLUMBLINE_LOCAL_URL", "")
	if _, ok := LocalFromEnv(""); ok {
		t.Fatal("no model must mean no client")
	}
	t.Setenv("PLUMBLINE_LOCAL_MODEL", " gemma3:12b ")
	c, ok := LocalFromEnv("")
	if !ok || c.Model != "gemma3:12b" || c.BaseURL != DefaultLocalURL {
		t.Fatalf("LocalFromEnv = %+v, %t", c, ok)
	}
	t.Setenv("PLUMBLINE_LOCAL_URL", "http://127.0.0.1:1234/v1/")
	if c, _ := LocalFromEnv("qwen3"); c.Model != "qwen3" || c.BaseURL != "http://127.0.0.1:1234/v1" {
		t.Fatalf("flag and URL override: %+v", c)
	}
}
