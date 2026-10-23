# Contributing

Thanks for helping. Small, focused pull requests are easiest to review.

## Setup

You need Go 1.24 or newer. CI also tests the current stable release.
[golangci-lint](https://golangci-lint.run/welcome/install/) v2 is needed for `make lint`.

```sh
make check   # format check, vet, lint, tests, evaluation gate
make race    # tests with the race detector
make fuzz    # short fuzz run
```

Without `make`, run the commands directly:

```sh
gofmt -l .
go vet ./...
golangci-lint run ./...
go test ./...
go run ./cmd/ragloop eval -corpus testdata/corpus -cases testdata/eval.jsonl -web testdata/web.json -deny-domains evil.example.com -allow-domains iso.org
```

## Changes to answer quality

Retrieval changes can fix one question and quietly break another. Before
opening a pull request that touches ranking, grading or generation:

1. Add a case to `testdata/eval.jsonl` that fails before your change and passes after.
2. Run the evaluation gate and include its summary line in the pull request.
3. If a case is expected to fail for a known reason, mark it with `"xfail"` and say why.

## Pull requests

- Keep the standard-library-only rule: no new dependencies without discussion first.
- Add a line to `CHANGELOG.md` under "Unreleased" if users would notice the change.
- CI must pass on Linux, macOS and Windows.

## Releases

Maintainers release by pushing a tag such as `v0.2.0`. The release workflow
tests, builds every platform, publishes the GitHub Release and attaches build
provenance. Move the "Unreleased" changelog entries under the new version first.
