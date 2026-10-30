package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func runCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	t.Setenv("ANTHROPIC_API_KEY", "") // always test the offline path
	t.Setenv("PLUMBLINE_LOCAL_MODEL", "")
	var out, errb bytes.Buffer
	code := run(context.Background(), args, &out, &errb)
	return code, out.String(), errb.String()
}

const corpus = "../../testdata/corpus"

func TestUsageErrors(t *testing.T) {
	for _, args := range [][]string{nil, {"nope"}, {"ask"}, {"ask", "-corpus", corpus}, {"eval", "-corpus", corpus},
		{"ask", "-corpus", corpus, "-upper", "0.2", "-lower", "0.5", "q"}, {"ask", "-corpus", "/does/not/exist", "q"}} {
		if code, _, _ := runCLI(t, args...); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
	if code, out, _ := runCLI(t, "help"); code != 0 || !strings.Contains(out, "usage") {
		t.Errorf("help: exit %d", code)
	}
}

func TestAsk(t *testing.T) {
	code, out, errOut := runCLI(t, "ask", "-corpus", corpus, "-trace", "How many vacation days do employees get per year?")
	if code != 0 || !strings.Contains(out, "25 days") || !strings.Contains(out, "Sources:") ||
		!strings.Contains(out, "verified=true") || !strings.Contains(out, "Trace:") {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	if !strings.Contains(errOut, "offline") {
		t.Errorf("mode line missing: %q", errOut)
	}
	code, out, _ = runCLI(t, "ask", "-corpus", corpus, "-json", "-groups", "security", "Where are the production signing keys stored?")
	var res struct {
		Answer   string `json:"answer"`
		Verified bool   `json:"verified"`
	}
	if code != 0 || json.Unmarshal([]byte(out), &res) != nil || !strings.Contains(res.Answer, "vault-prod-3") || !res.Verified {
		t.Fatalf("json ask: exit %d %q", code, out)
	}
}

// fakeOllama answers like a local model server: it routes every question as
// single, grades every passage relevant and answers by quoting evidence [1].
func fakeOllama(t *testing.T, model string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			fmt.Fprintf(w, `{"data":[{"id":%q}]}`, model)
			return
		}
		var req struct {
			Messages []struct{ Content string } `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Messages) != 2 {
			t.Errorf("bad request: %v", err)
			return
		}
		system, prompt := req.Messages[0].Content, req.Messages[1].Content
		var reply string
		switch {
		case strings.HasPrefix(system, "Classify"):
			reply = `{"kind":"single"}`
		case strings.HasPrefix(system, "You grade"):
			reply = "[" + strings.TrimSuffix(strings.Repeat("0.9,", strings.Count(prompt, "<passage ")), ",") + "]"
		default:
			_, ev, _ := strings.Cut(prompt, "<evidence n=\"1\"")
			_, ev, _ = strings.Cut(ev, ">\n")
			ev, _, _ = strings.Cut(ev, "\n</evidence>")
			reply = strings.TrimRight(ev, ".") + " [1]."
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": reply}}}})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestAskLocalModel(t *testing.T) {
	srv := fakeOllama(t, "tiny:1b")
	t.Setenv("PLUMBLINE_LOCAL_URL", srv.URL+"/v1")
	code, out, errOut := runCLI(t, "ask", "-corpus", corpus, "-local-model", "tiny:1b", "How many vacation days do employees get per year?")
	if code != 0 || !strings.Contains(out, "25 days") || strings.Contains(out, "model_calls=0") ||
		!strings.Contains(errOut, "mode: local model tiny:1b") {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
}

func TestAskLocalModelUnavailable(t *testing.T) {
	srv := fakeOllama(t, "tiny:1b")
	t.Setenv("PLUMBLINE_LOCAL_URL", srv.URL+"/v1")
	code, out, errOut := runCLI(t, "ask", "-corpus", corpus, "-local-model", "other:7b", "How many vacation days do employees get per year?")
	if code != 0 || !strings.Contains(out, "25 days") || !strings.Contains(out, "model_calls=0") ||
		!strings.Contains(errOut, "ollama pull other:7b") || !strings.Contains(errOut, "offline") {
		t.Fatalf("missing model: exit %d\n%s\n%s", code, out, errOut)
	}

	t.Setenv("PLUMBLINE_LOCAL_URL", "http://127.0.0.1:1/v1")
	code, _, errOut = runCLI(t, "ask", "-corpus", corpus, "-local-model", "tiny:1b", "How many vacation days do employees get per year?")
	if code != 0 || !strings.Contains(errOut, "ollama serve") {
		t.Fatalf("server down: exit %d\n%s", code, errOut)
	}
}

func TestEvalGate(t *testing.T) {
	base := []string{"eval", "-corpus", corpus, "-cases", "../../testdata/eval.jsonl"}
	code, out, errOut := runCLI(t, append(base, "-web", "../../testdata/web.json", "-deny-domains", "evil.example.com")...)
	if code != 0 || !strings.Contains(errOut, "Gate passed") || !strings.Contains(out, "XFAIL paraphrase-synonym") {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	// Without the web searcher the web case fails; coverage stays above its floor
	// but the case is reported, and -no-gate keeps the exit code at 0.
	code, out, _ = runCLI(t, append(base, "-no-gate")...)
	if code != 0 || !strings.Contains(out, "FAIL  web-fallback-iso") {
		t.Fatalf("no-gate run: exit %d\n%s", code, out)
	}
}

func TestVersion(t *testing.T) {
	code, out, _ := runCLI(t, "version")
	if code != 0 || !strings.HasPrefix(out, "plumbline ") {
		t.Fatalf("exit %d: %q", code, out)
	}
	defer func(v, c, d string) { version, commit, date = v, c, d }(version, commit, date)
	version, commit, date = "v1.2.3", "0123456789abcdef0123", "2026-10-23T00:00:00Z"
	if got := versionString(); !strings.HasPrefix(got, "plumbline v1.2.3 (0123456789ab, 2026-10-23T00:00:00Z) ") {
		t.Fatalf("stamped version = %q", got)
	}
}

func TestStats(t *testing.T) {
	code, out, _ := runCLI(t, "stats", "-corpus", corpus)
	if code != 0 || !strings.Contains(out, "documents 10") || !strings.Contains(out, "superseded chunks 1") {
		t.Fatalf("exit %d\n%s", code, out)
	}
}
