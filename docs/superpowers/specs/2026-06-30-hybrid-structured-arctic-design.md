# Hybrid Search on Structured Collections + Arctic Model Swap — Design

**Date:** 2026-06-30
**Status:** Approved (brainstorm)
**Scope:** dev-only implementation + validation; prod cutover documented but operator-gated.

## Goal

Make short German topic queries retrieve semantically-related events from the
structured collections — concretely, make the e-teaching.org **SCALE-UP**
calendar event surface on page 1 for queries like "Aktivierung Studierende" and
"kooperatives Lernen Hochschule", where today it is absent.

## Background — why two levers

A prior de-risk experiment (2026-06-30, full live `calendar_31922` corpus, 2591
deduped events) established that **both** of the following are required; neither
alone reaches page 1:

| query | e5-base rank | arctic-m-v2.0 rank |
|---|---|---|
| kooperatives Lernen Hochschule | 200 | 30 |
| Aktivierung Studierende | 63 | 27 |

1. **Recall architecture.** The structured collections (calendar, long-form,
   wiki) do **BM25-only** search; the relay's external chunk-rerank only
   *reorders* the BM25 candidate set, so an event whose own fields don't
   lexically match the query never enters the set and can't be promoted. The AMB
   kind-30142 collection already does Typesense-native hybrid search (BM25 ⊕
   dense vector, RRF fusion) — which is why the SCALE-UP *resource* (30142)
   surfaces while its calendar twin (31923) does not.
2. **Model quality.** `intfloat/multilingual-e5-base` packs German educational
   text into a narrow ~0.80–0.85 cosine band (low discrimination). Even with
   hybrid, e5 ranks the target too deep (63–200) to enter the vector candidate
   set. `Snowflake/snowflake-arctic-embed-m-v2.0` (768-dim, same CPU class)
   ranks it 27–30 — comfortably inside the candidate set, so RRF fusion lands it
   on page 1.

This project bundles both levers. It builds on the already-shipped e5 migration
(768-dim everywhere) as its foundation.

## Architecture

**Key leverage — the query path is already built.** All structured collections
are `typesense30142.TSBackend` instances routing through the same
`QueryEvents`/`query.go`. At `query.go:379` the code performs hybrid search
(`vector_query: "embedding:([...], alpha:0.3)"`, RRF) **whenever
`ts.Embedder != nil` and the collection has an `embedding` field**. The
structured collections are BM25-only today only because three things are
missing: no `Embedder` wired, no `embedding` schema field, and the structured
write path doesn't compute a vector. **No query-path / nostrlib change is
needed** — the hybrid path is reused as-is.

The two levers are mostly independent in code and share only the embed service:

- **Lever A (Arctic swap):** embed service (model-aware prompting) + an
  `EMBED_MODEL` env flip. Dimension stays **768**, so no Typesense `num_dim`
  change anywhere; every stored vector is recomputed by reindex/re-chunk.
- **Lever B (hybrid on structured):** add an `embedding` field to the
  calendar/long-form/wiki schemas; embed text on write via a shared helper; wire
  `tsDB2/3/4.Embedder`; reindex re-embeds.

**Out of scope:** the shares + profiles collections (no semantic value), the
bge-reranker precision stage (deferred — add only if measurement shows page-1
ranking is insufficient), and prod cutover.

## Lever A — Arctic model swap

### Embed service (`embed/embed_service.py`) — model-aware prompting

Today the service hard-codes e5 string prefixes (`"query: "` / `"passage: "`).
Arctic v2.0 prompts differently: queries use the model's built-in prompt
(`prompt_name="query"`), passages get **no** prefix. The change keeps
"model-specific knowledge lives in exactly one place" by selecting a strategy
from the model name:

- **Load:** Arctic →
  `SentenceTransformer(name, trust_remote_code=True,
  model_kwargs={"attn_implementation": "eager"},
  config_kwargs={"use_memory_efficient_attention": False, "unpad_inputs": False})`
  (the de-risk-proven, xformers-free load); otherwise the plain
  `SentenceTransformer(name)`.
- **Encode query:** Arctic → `encode(texts, prompt_name="query",
  normalize_embeddings=True)`; e5 → prepend `"query: "`.
- **Encode passage:** Arctic → `encode(texts, normalize_embeddings=True)` bare;
  e5 → prepend `"passage: "`.

Both paths keep `normalize_embeddings=True` (cosine = dot). `/health` keeps
reporting `model` + `dimensions` (768 for both). The model family is detected by
checking whether the model name contains `arctic` (case-insensitive); this is
the single branch point.

### Stack config

Set `EMBED_MODEL=Snowflake/snowflake-arctic-embed-m-v2.0` for both the relay and
the indexer. The `embed_model_cache` volume holds the ~450 MB download.

### Auto re-embed via hash invalidation

The indexer already folds `EmbedModel` into its content hash (e5 migration), so
bumping the model name invalidates every chunk → full re-chunk/re-embed with no
manual cursor reset. The relay's AMB write-path re-embeds on reindex.

### No dimension change

Arctic-m-v2.0 is 768-dim, same as e5 — so there is **no** `num_dim` schema reset
for the AMB collection (unlike the 384→768 e5 migration). Stored e5 vectors
become stale and are overwritten by reindex/re-chunk.

## Lever B — hybrid on structured collections

### Schema (`calendar.go` / `longform.go` / `wiki.go`)

Each schema builder gains one field, identical to the AMB collection's:

```go
{Name: "embedding", Type: "float[]", NumDim: 768, VecDistMetric: "cosine", Optional: true}
```

The shared query path already sets `exclude_fields: "embedding"`
(`query.go:375`), so vectors never bloat responses, and `vector_query` excludes
it from `query_by`.

### Doc structs + embeddable interface

Each projection's output struct (calendar/long-form/wiki doc) gains
`Embedding []float32 \`json:"embedding,omitempty"\``. To keep the pure
projections (`nostrToCalendar`/`nostrToLongform`/`nostrToWiki`) free of I/O,
embedding happens in the store step via a small interface:

```go
type embeddable interface {
    EmbedText() string       // title+summary+content (+location for calendar)
    SetEmbedding([]float32)
}
```

Each doc implements it. A shared helper `embedAndAttach(embedder, doc)` calls
`embedder.Embed(ctx, []string{doc.EmbedText()}, EmbedPassage)` and sets the
vector. `storeStructured` / `reprojectStructured` call it after projecting **only
when an embedder is present**; with semantic disabled the docs project exactly as
today (vector omitted via `omitempty`). Because reindex flows through
`reprojectStructured`, the drop+reproject pass re-embeds for free.

`EmbedText()` concatenates non-empty fields:
- calendar: `title`, `summary`, `location`, `content`
- long-form / wiki: `title`, `summary`, `content`

### Wiring (`main.go`)

Under the same `semanticCfg.Enabled && embedder != nil` guard that wires the AMB
collection, set `tsDB2.Embedder = embedder`, `tsDB3.Embedder = embedder`,
`tsDB4.Embedder = embedder`. That single assignment per backend activates the
hybrid branch in `query.go:379`. No `EmbedFields` is needed on these backends —
the write path embeds via `EmbedText()`, and the query path keys only off
`ts.Embedder != nil`.

### Fusion weight

The shared path uses a fixed `alpha:0.3` (30% vector / 70% keyword) for every
collection; structured ones inherit it, matching AMB and the de-risk. Not
parameterized per-collection (YAGNI; 0.3 is the validated value).

### Calendar's two fetch paths are unaffected

`calendarFetch` still routes range/geo queries to the Bolt index (exact, no
semantics) and full-text to Typesense. The "topic + time" case
(`{kinds:[31923], search:"...", #start_after, #start_before}`) already routes to
Typesense — so it now gets hybrid recall, exactly the "events about X next week"
shape. Plain wildcard listings (`q="*"`) skip the hybrid branch
(`query.go:379` requires `mainQuery != "*"`), so non-search queries are
unchanged.

## Migration (dev-only; each shared-infra step operator-gated)

1. Land code on a worktree branch; `GOWORK=off go build ./... && go vet ./...`
   green for the relay; embed pytest green. (No nostrlib or amb-indexer source
   change.)
2. Deploy the dev stack with `EMBED_MODEL=Snowflake/snowflake-arctic-embed-m-v2.0`,
   embed service rebuilt `--no-cache`; verify `/health` → arctic, dim 768.
3. Re-embed everything (768→768, **no schema reset**): relay `reindex`
   re-embeds the AMB collection with Arctic and drops+reprojects the structured
   targets (populating the new `embedding` field via `embedAndAttach`); the
   indexer auto-re-chunks `amb_chunks_30142` via hash invalidation.
4. Validate after re-embed completes.

## Validation (acceptance test)

Re-run the de-risk probes against the **live relay** NIP-50 search (not raw
cosine): `search:"kooperatives Lernen Hochschule"` and
`search:"Aktivierung Studierende"` over `kinds:[31923]` (and `[30142]`).

- **Pass:** the SCALE-UP event appears on page 1 (within `limit:20`), where
  today it is absent.
- **No regression:** plain kind/range/`#h` queries and existing AMB search
  behave as before.

## Testing

- **embed service (pytest):** Arctic path — query uses `prompt_name="query"`,
  passage encoded bare; e5 path retained; both normalized. Guarded skip when the
  model isn't downloaded (matches current ergonomics).
- **Lever B (Go unit):** each schema includes the `embedding` field; each doc
  implements `embeddable` and `EmbedText()` concatenates the right fields
  (calendar includes `location`, empties skipped); `embedAndAttach` sets the
  vector from a fake embedder; `storeStructured`/`reprojectStructured` embed when
  an embedder is present and skip (no `embedding` key, via `omitempty`) when nil
  — proving back-compat.
- **Build:** `GOWORK=off go build ./... && go vet ./...` for the relay.

## Error handling / risks

- **Write-time embed failure:** `embedAndAttach` error → log and upsert
  **without** the vector (doc still BM25-searchable); a TS/embed blip must never
  reject the durable BoltDB write (mirrors existing fire-and-forget
  `storeStructured`). The next reindex re-embeds.
- **Query-time embed failure:** already handled in `query.go` (logs, falls back
  to keyword) — recall never worse than today.
- **Transient mixed-vector window:** between the model swap and reindex
  completion, stored vectors are stale e5 while queries embed with Arctic →
  transiently poor hybrid scores. The runbook reindexes immediately after the
  swap and validation waits for completion — acceptable on dev (same as the e5
  migration).
- **Arctic load:** the de-risk-proven `trust_remote_code` + eager-attention
  kwargs avoid the xformers dependency; the service fails fast at startup if the
  model can't load, caught by the deploy health check.
- **Latency:** one synchronous embed per structured write (low volume, fine);
  reindex embeds per doc (slower, bounded, same as chunking).
