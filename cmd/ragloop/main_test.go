package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func runCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	t.Setenv("ANTHROPIC_API_KEY", "") // always test the offline path
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

func TestStats(t *testing.T) {
	code, out, _ := runCLI(t, "stats", "-corpus", corpus)
	if code != 0 || !strings.Contains(out, "documents 10") || !strings.Contains(out, "superseded chunks 1") {
		t.Fatalf("exit %d\n%s", code, out)
	}
}
