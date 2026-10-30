# Roadmap and known gaps

Unfinished work, roughly most useful first. Each item says why it matters and where
it lives. Remove an item in the commit that finishes it.

## Replace the stand-ins

- **Embedding model.** `HashEmbedder` matches words and word pieces, not meaning, so
  "annual leave" misses "vacation" (the `paraphrase-synonym` xfail case). First change
  `rag.Embedder` to take a `context.Context` and return an error, since a real model is
  a network call that can fail; then let the CLI choose it (`cmd/plumbline/main.go`
  calls `rag.HashEmbedder{}` directly in `setup` and `cmdStats`).
- **Cross-encoder reranker.** `LexicalReranker` scores keyword overlap. `rag.Reranker`
  has the same missing context and error as `Embedder`.
- **Web search.** Only `StaticWeb`, which reads a fixed JSON file, implements
  `rag.WebSearcher`.
- **Context headers.** Chunks are indexed with title and section heading
  (`ChunkDoc`), not a model-written description of the chunk.

## Answer quality

- **Structured output for the router and grader.** `LLMRouter` and `LLMGrader` take the
  first `{` or `[` in the reply and parse from there; a malformed reply silently falls
  back to the stand-in. Claude's structured outputs and Ollama's JSON mode would
  guarantee the shape.
- **Groundedness is word overlap.** `CheckGrounded` checks that 60% of a sentence's
  content words are in the cited strip and that both agree on negation. It does not
  check meaning; the negation test is a word list (`negated`), so a double negative
  counts as negated and "no later than" as a denial.
- **No model-backed evaluation.** The gate runs offline, where answers quote their
  evidence, so it cannot catch generation bugs such as the negation fix. Needs a gate
  run against a model, or recorded model replies replayed in tests.
- **The threshold sweep overfits.** `eval -sweep` picks thresholds on the same 22 cases
  it reports. Needs more cases from real questions and a held-out split.
- **Summary index and entity graph are shallow.** A summary is the title plus the first
  sentence of up to 6 chunks (`Build`); entities are capitalised runs and letter-digit
  tokens found by a regex (`extractEntities`), linked one hop by co-occurrence.
- **English and ASCII only.** The stemmer removes English plurals, stopwords are
  English, and the entity regex matches ASCII letters.

## Security

- **Groups are self-declared.** `-groups` is whatever the caller types; filtering is
  applied before search, but nothing authenticates the user.
- **Injection screening is pattern-based** (`injectionPatterns`). Fencing and the leak
  check back it up; a new phrasing can still pass the patterns.

## Local model

- **Not yet run against a real Ollama server.** Tested only with a fake server in
  `cmd/plumbline/main_test.go` and `internal/llm/local_test.go`.
- **The 2-minute local deadline is untested.** No test can observe the deadline the CLI
  sets in `setup`.

## Scale and operation

- **Nothing persists.** The index, graph and summaries are rebuilt on every command.
- **Vector search scans every chunk** (`searchDense`). Fine for a few thousand chunks.
- **No server mode.** Each `ask` is a new process, so the answer and retrieval caches
  only help in `eval` or library use.
- **Fallback answers are cached.** `Pipeline.Ask` caches any answer that did not time
  out, including one made by a stand-in after a model error, so a brief outage sticks
  for that question in a long-running process.
