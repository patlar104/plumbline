# Changelog

All notable changes are recorded here. Versions follow semantic versioning.

## Unreleased

### Added

- Free local models: `-local-model` or `PLUMBLINE_LOCAL_MODEL` runs routing, grading
  and writing on a model served by Ollama (or any OpenAI-compatible server at
  `PLUMBLINE_LOCAL_URL`), with no API key. Startup checks that the server is up and
  the model downloaded, and falls back to the offline stand-ins with the command to
  fix it. The default deadline per question is 2 minutes in this mode.

### Fixed

- The groundedness check accepted a sentence that reversed its evidence ("do not
  get 25 days" citing "get 25 days"), because "not" is a stopword.

### Changed

- `go.mod` names the Go 1.27.3 toolchain, so builds from source use it and its
  security fixes by default. The minimum stays Go 1.26.
- CI's minimum-version jobs run with `GOTOOLCHAIN=local` and print `go version`, so
  they test Go 1.26 rather than switching to the newer toolchain.

## 0.1.0 — 2026-10-23

First version.

### Added

- Pipeline: adaptive router, hybrid BM25 and vector search, entity graph and summary
  index for multi-hop and corpus-wide questions, reciprocal rank fusion, reranking,
  CRAG-style grading with refine / web / refine+web actions, cited answers,
  groundedness and leak checks, and a retry loop capped by iterations, model calls,
  web searches and a deadline.
- Injection screening of every passage, fenced evidence in model prompts, domain
  trust for web results, group-based permissions applied before search.
- `supersedes` front-matter field; replaced documents are hidden unless asked for.
- Claude Messages API client with retries; offline stand-ins for every model stage.
- Evaluation harness with metric floors, per-category results, expected failures
  and a grader-threshold sweep; 22 test cases on a fictional corpus.
- Command-line tool: `ask`, `eval`, `stats`, `version`.
- Requires Go 1.26 or newer, the oldest Go release still receiving security fixes.
- CI on Linux, macOS and Windows against both that minimum and current stable Go:
  format, vet, lint (golangci-lint v2), tests, evaluation gate, fuzzing,
  govulncheck. CodeQL scanning. Every action pinned to a commit SHA; Dependabot
  keeps the pins current.
- Releases: GoReleaser builds Linux, macOS and Windows (amd64, arm64) with current
  stable Go, with checksums, a GitHub Release and signed build provenance on
  every `v*` tag.
- Repository files: issue forms, pull request template, CONTRIBUTING, SECURITY,
  `.editorconfig`, Makefile, and `scripts/github-setup.sh` for the settings that
  cannot live in files (branch ruleset, private vulnerability reporting, alerts).
- MIT license.

### Fixed during review

- Superseded documents outranked current ones.
- Compound questions lost half their answer: grading and refinement now score each
  passage against every sub-question.
- Wrong content was counted as a security leak; `forbid` and `leak_terms` are now separate.
- Injected text hidden behind an HTML tag passed screening; plain `<` and `>` in text
  were stripped as if they were tags.
- Citations placed after a sentence's full stop were attributed to the next sentence.
- Entity names picked up a leading "the".
- Errors writing report tables to the terminal were ignored (found by the linter).

### Known limitations

- Offline vectors match words, not synonyms (one expected-failure case).
- The test set is too small to tune the grader thresholds: most settings tie.
- The Claude client is tested against a local fake server, not the live API.
