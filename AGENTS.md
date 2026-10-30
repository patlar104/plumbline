# Agent instructions

plumbline is a retrieval pipeline prototype in standard-library Go: router, hybrid
search, CRAG-style grading, cited answers, groundedness and leak checks. README.md
describes what it does; this file is how to work on it. Claude Code reads it through
CLAUDE.md, and Codex, Devin and Antigravity read it directly.

## Commands

```sh
mise install    # Go, golangci-lint, GoReleaser, markdownlint-cli2 from mise.toml
make check      # gofmt check, vet, golangci-lint, tests, evaluation gate: what CI runs
make lint-md    # Markdown lint, configured in .markdownlint-cli2.jsonc (not in CI yet)
make race fuzz  # race detector; short fuzz run
go test ./internal/rag -run TestCheckGrounded -v   # one test
```

Run `make check` before calling a change done, and `make lint-md` after editing Markdown.
Tool versions live in `mise.toml`; keep golangci-lint there in step with `ci.yml`.

Claude Code settings: `.claude/settings.json` and `.claude/hooks/` are shared and
committed; `.claude/settings.local.json` is personal and ignored.

## Rules

- **Standard library only.** No new modules in `go.mod` without asking first.
- **Go 1.26 is the minimum.** `go.mod` names the 1.27.3 toolchain, but CI also builds
  with 1.26 and `GOTOOLCHAIN=local`, so do not use standard-library APIs added after 1.26.
- **Every model stage keeps an offline fallback.** The router, grader and generator
  wrap a model with a rule-based stand-in; a model error or exhausted budget falls back
  rather than failing the question.
- **Tests never touch the network.** Model clients are tested against `httptest`
  servers. `runCLI` in `cmd/plumbline/main_test.go` clears `ANTHROPIC_API_KEY` and
  `PLUMBLINE_LOCAL_MODEL`; keep it that way for new model settings.
- **Write the failing test first.** Bug fixes start with a test that shows the bug.
- **Answer-quality changes** (ranking, grading, generation) add a case to
  `testdata/eval.jsonl` that fails before and passes after, or mark a known gap with
  `"xfail"` and a reason. Offline answers only quote evidence, so a bug that needs a
  model to write the answer is covered by a unit test instead; say so in the commit.
- **Treat retrieved text as untrusted.** Passages are screened (`rag.Screen`) and fenced
  in prompts (`defang`); never put document or web text into a prompt outside a fence.
- **Never print or log API keys,** including in error messages.

## Model backends

| Setting | Effect |
| --- | --- |
| `-offline` | Offline stand-ins only; overrides everything below |
| `-local-model` / `PLUMBLINE_LOCAL_MODEL` | Ollama or another OpenAI-compatible server at `PLUMBLINE_LOCAL_URL`; takes priority over Claude |
| `ANTHROPIC_API_KEY` | Claude, model from `-model` / `PLUMBLINE_MODEL` |

`make check`'s evaluation gate uses whichever backend the environment selects. Do not
export a model setting in a shell you run `make check` from: the gate floors are set
for the offline stand-ins.

## Keeping track

- `CHANGELOG.md`: add a line under "Unreleased" for anything a user would notice.
- `docs/ROADMAP.md`: unfinished work and known gaps. Remove an item in the same commit
  that finishes it; add one when you find a gap you are not fixing.
- `README.md`: update "Stand-ins to replace" and "Known limitations" when they change.

## Style

Match the surrounding code: short doc comments that say why, errors wrapped with
`%w` and the operation, table-driven tests, deterministic output (sort map keys
before printing or scoring). Commit messages: a sentence-case summary line, then
a short paragraph on what changed and why.
