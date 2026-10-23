# ragloop

A retrieval pipeline prototype: an adaptive router, hybrid search, an optional entity
graph and summary index, fusion and reranking, a CRAG-style relevance grader with
corrective actions, a cited answer, a groundedness check and a bounded retry loop.
It is standard-library Go with no dependencies.

Every model-backed stage has an offline stand-in, so it builds, runs and passes its
tests with no network and no API key. Set `ANTHROPIC_API_KEY` to have Claude do the
routing, grading and writing; each model stage falls back to its stand-in on any error
or when the per-question budget runs out.

## Run it

Requires Go 1.24 or newer.

```sh
go build -o ragloop ./cmd/ragloop

./ragloop stats -corpus testdata/corpus
./ragloop ask   -corpus testdata/corpus -trace "How many vacation days do employees get per year?"
./ragloop ask   -corpus testdata/corpus -groups security "Where are the production signing keys stored?"
./ragloop eval  -corpus testdata/corpus -cases testdata/eval.jsonl \
                -web testdata/web.json -deny-domains evil.example.com -allow-domains iso.org
./ragloop eval  ... -sweep      # also tune the grader thresholds
```

Exit codes: `0` success, `1` the evaluation gate failed, `2` usage or setup error.

Environment: `ANTHROPIC_API_KEY` (optional), `RAGLOOP_MODEL` (default `claude-haiku-5-5`),
`ANTHROPIC_BASE_URL` (default `https://api.anthropic.com`).

## Documents

`.md` and `.txt` files, read recursively; hidden folders and symlinks are skipped.
Optional front matter:

```yaml
---
id: travel-policy            # defaults to the file name
title: Travel policy         # defaults to the first heading
updated: 2026-02-10          # used by -max-age-days
groups: hr, managers         # only these groups may see it; empty = everyone
supersedes: travel-policy-2019   # hides the older document unless -include-superseded
---
```

## Layout

| Path | What it holds |
| --- | --- |
| `internal/rag` | The pipeline and every stage |
| `internal/llm` | Claude Messages API client with retries |
| `internal/eval` | Evaluation harness, metric floors, threshold sweep |
| `cmd/ragloop` | Command-line tool |
| `testdata` | Fictional corpus, static web pages, 22 test cases |

## Stand-ins to replace for real use

| Stage | Offline stand-in | Replace with |
| --- | --- | --- |
| Vectors | Hashed words and trigrams (`HashEmbedder`) | An embedding model behind `rag.Embedder` |
| Reranker | Weighted term coverage (`LexicalReranker`) | A cross-encoder behind `rag.Reranker` |
| Web search | Fixed pages from a JSON file (`StaticWeb`) | A search API behind `rag.WebSearcher` |
| Context headers | Title and section heading | Model-written context per chunk |
| Router, grader, generator | Rules, coverage, extractive quotes | Claude, already wired: set the API key |

## Known limitations

- Without a real embedding model, synonyms are not matched: "annual leave" does not
  find "vacation". The test set keeps this as an expected failure (`xfail`).
- The extractive generator only quotes, so offline answers are grounded by construction;
  the groundedness check only earns its keep once a model writes the answers.
- Injection screening is pattern-based. Fencing, the output leak check and the
  injection test cases back it up, but a novel phrasing can get past the patterns.
- Supersession needs the `supersedes` field; the system does not guess which of two
  similar documents is current.
- The graph and summary indexes are rebuilt from scratch at load time.
- The test cases were written alongside the code, so a perfect score shows the known
  failure modes are handled, not that the system is good in general. Add cases from
  real questions before trusting the numbers.

## Development

```sh
make check      # format check, vet, lint, tests, evaluation gate (same as CI)
make race fuzz  # race detector; short fuzz run
make help       # every target
```

See [CONTRIBUTING.md](CONTRIBUTING.md) for the commands without `make`.

## Releases

Push a tag such as `v0.2.0`. The release workflow tests, builds Linux, macOS and
Windows binaries, publishes the GitHub Release with checksums, and attaches signed
build provenance. To check a downloaded archive came from this repository's workflow:

```sh
gh attestation verify ragloop_<version>_<os>_<arch>.tar.gz --repo <owner>/ragloop
```

## Repository settings

Branch protection, private vulnerability reporting and security alerts are settings,
not files. After the first push, run once (it prints the changes; `--apply` makes them):

```sh
scripts/github-setup.sh            # dry run
scripts/github-setup.sh --apply    # add --require-checks to require PRs and green CI
```

## License

MIT. See [LICENSE](LICENSE).
