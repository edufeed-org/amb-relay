# Migrate embeddings to multilingual-e5-base — design

**Date:** 2026-06-29
**Status:** Approved (design); implementation plan pending
**Scope:** dev stack only — prod cutover documented as a follow-up, NOT executed

## Problem

Semantic chunk-rerank search for NIP-52 calendar events works end-to-end (relay
window logic, indexer chunking, and data are all correct), but short topic
queries fail to retrieve topically-relevant long passages. The motivating case:
a NIP-50 search "Aktivierung Studierende" should return the e-teaching.org
SCALE-UP event, whose text is "aktivierendes und kooperatives Lernen in
Hochschulveranstaltungen" and never contains the literal token "Studierende".

### Root cause (established by credentialed dev diagnostics, 2026-06-29)

The recall gap is a **model-choice limitation**, not a relay or indexer bug.

- SCALE-UP's chunk exists with a real 200-char snippet and a real normalized
  384-dim embedding. Fresh-embedding the snippet's own text retrieves SCALE-UP
  at rank 1–2 — the stored embedding is faithful, not corrupt.
- Pure-vector KNN rank of SCALE-UP degrades monotonically as the query shortens:
  the snippet's own full text → rank 2; an 8-word near-verbatim query → rank 13;
  "kooperatives Lernen Hochschule" → not in top 50; "Aktivierung Studierende" →
  not in top 50.

This is the classic weakness of `paraphrase-multilingual-MiniLM-L12-v2`: it is a
**symmetric paraphrase** model (trained on similar-length sentence pairs), not an
**asymmetric retrieval** model. A short query embeds in a different region than a
full paragraph, so cosine similarity stays low even when topically on-point.
Calendar chunks compound this: their keyword fields (`text`/`heading`/
`section_path`) are empty, so there is zero BM25 lexical fallback — recall is
100% dependent on the weak vector.

## Goal

Replace the symmetric paraphrase model with the asymmetric
`intfloat/multilingual-e5-base` model (with `query:`/`passage:` prefixes) so that
short topic queries retrieve semantically-related long passages. Validate on the
dev stack that SCALE-UP-style queries now retrieve correctly.

## Decisions (locked during brainstorming)

1. **Model:** `intfloat/multilingual-e5-base` — 768-dim. Chosen over e5-small
   (384-dim, drop-in but lower quality) and bge-m3 (1024-dim, heavier, likely
   overkill). This is a **dimension change**: every Typesense `embedding` field
   must be recreated, not altered in place, and the embed container's memory
   ceiling rises (model ≈1.1GB vs MiniLM ≈0.12GB).
2. **Prefix location:** the embed service owns the `query:`/`passage:` prefix.
   The `/embed` wire contract gains an `input_type` field; the service prepends
   the model-appropriate prefix. Model-specific knowledge stays in the one
   component that owns the model.
3. **Re-chunk trigger:** an embedding-version tag (model name + dim) is folded
   into the indexer's content-direct idempotency hash, so this swap — and every
   future model change — auto-invalidates every event and forces a re-chunk.
   Combined with a one-off drop+recreate of the chunk collection for the
   dimension change.
4. **Scope:** dev implement + validate. Prod cutover is documented (§7) but not
   executed, respecting the standing dev-only constraint.

## Architecture / components

Three repositories are involved. The embed service is the spine; the two Go
repos thread the new `input_type` through their embed call sites.

### Component 1 — Wire contract (`/embed`)

```
POST /embed
  body:    {"texts": ["...", ...], "input_type": "query" | "passage"}
  response:{"embeddings": [[float, ...], ...], "model": str, "dimensions": int}
```

- The embed service prepends the e5 prefix per `input_type`:
  `query: <text>` for `"query"`, `passage: <text>` for `"passage"`.
- `input_type` absent ⇒ default `passage` (index/bulk is the common case). Both
  Go query call sites set `"query"` explicitly, so no path relies on the default
  for a query.

### Component 2 — nostrlib (fork)

The `Embedder` interface is used for **both** query and passage embedding, so the
interface itself must carry `input_type`.

- `eventstore/typesense30142/lib.go:17` — extend the `Embedder` interface to
  `Embed(ctx, texts, inputType)` with typed consts `EmbedQuery` / `EmbedPassage`.
- `eventstore/typesense30142/query.go:383` — AMB hybrid-search query embedding →
  `EmbedQuery`.
- `eventstore/typesense30142/replace.go:43` and `replace.go:177` — AMB index/
  passage embedding → `EmbedPassage`.
- `eventstore/typesense30142/typesense.go:238` and `types.go:257` — AMB
  `embedding` field `num_dim: 384 → 768`; update the README note (line ~205).
- `khatru/semantic` — no prefix change. The chunk-search query is embedded
  inside the indexer's `/search_chunks`; the semantic package only forwards the
  raw search string.

### Component 3 — amb-relay

- `embedding.go` — `EmbeddingClient.Embed` gains the `inputType` parameter to
  satisfy the new interface; `embedRequest` gains an `InputType` field forwarded
  in the request body.
- `embed/embed_service.py` — add `input_type` to the request model and the
  prefix logic; change the default `EMBED_MODEL` to
  `intfloat/multilingual-e5-base`.
- `embed/tests/` — update to expect 768 dimensions and to assert the prefix is
  applied per `input_type`.
- `docker-compose.yml` — raise the embed service `mem_limit` for e5-base; pin
  `EMBED_MODEL`.
- `go.mod` — bump the nostrlib pseudo-version after the nostrlib changes are
  pushed; `GOWORK=off go mod tidy`.

### Component 4 — amb-indexer

- `embed.go` — add `input_type` to the request; the chunk pipeline
  (`worker.go` `processContentDirect`) sends `passage`; the query path
  (`search.go` `/search_chunks`) sends `query`.
- `schema.go` — chunk-collection `embedding` field `num_dim: 384 → 768`.
- Idempotency — fold an embedding-version tag (model name + dim) into the
  `event_hash` value so a model change invalidates all events and forces a
  re-chunk on next delivery / rescan.
- `go.mod` — same nostrlib pseudo-version bump.

## Data flow

- **Index (passage):** relay write-path concatenates AMB fields → `Embed(...,
  EmbedPassage)` → service prepends `passage:` → 768-dim vector → AMB collection.
  Indexer chunk pipeline embeds chunk text as `passage` → chunk collection.
- **Query (query):** relay AMB hybrid search embeds the search string as
  `EmbedQuery`. Chunk-rerank search sends the raw string to the indexer's
  `/search_chunks`, which embeds it as `query`. The asymmetric prefixes are what
  bridge short query → long passage.

## Dev migration sequence

A dimension change means Typesense collections are **recreated**, not altered.
All shared-infra steps are operator actions gated on explicit go-ahead; the plan
lists them as such and does not auto-run them.

1. Push nostrlib changes → bump relay + indexer `go.mod`; verify `GOWORK=off`
   builds in both repos.
2. Deploy the embed service with e5-base (first boot downloads ≈1.1GB; higher
   memory ceiling). Verify `/health` reports 768 dimensions and the new model.
3. **AMB collection:** trigger the relay NIP-86 `reindex` — it drops and
   recreates the AMB schema at 768-dim and re-embeds every kind-30142 event via
   the write-path. (Only the AMB relay collection carries a vector; the
   structured collections — longform/wiki/calendar/shares — are keyword-only and
   need no re-embed.)
4. **Chunk collection:** drop `amb_chunks_30142`; the indexer's
   `EnsureCollection` recreates it at 768-dim; the version-bumped idempotency
   hash makes the indexer re-chunk/re-embed every event. Reset the indexer
   cursor so it rescans all events.

## Error handling / degradation

- During recreate, semantic search has no vectors to match and degrades to
  keyword/lexical fallback (chunk-rerank falls back to plain Typesense search).
  Acceptable on dev.
- A caller that omits `input_type` gets `passage` (safe for the index path).
  Query call sites set `query` explicitly, so a forgotten prefix cannot silently
  regress query recall.
- Mixed-dimension state is avoided by recreating collections rather than
  altering num_dim in place; old 384-dim vectors never coexist with the new
  768-dim field.

## Testing & validation

- **Unit:** embed pytest (768-dim; prefix applied per `input_type`); nostrlib +
  relay `EmbeddingClient` signature tests; indexer embed-wire, hash-version, and
  query-prefix tests.
- **E2E on dev:** re-run the KNN diagnostic — SCALE-UP must rank in the top-K for
  "Aktivierung Studierende" and "kooperatives Lernen Hochschule" (currently not
  in top 50); re-run the `zzraw` topic+time REQ — SCALE-UP returned in-window.

## Prod follow-up (documented, NOT executed)

The same sequence applies to the prod stack, gated on explicit go-ahead later.
Notes: the reindex and re-chunk are heavier on the prod corpus; confirm the
embed service memory ceiling before cutover; expect a degraded-recall window
during recreate.

## Risks

- **Embed memory:** e5-base ≈1.1GB resident vs MiniLM ≈0.12GB — mitigated by the
  `mem_limit` bump; verify the dev host headroom before deploy.
- **Prefix correctness:** e5 gains depend on correct prefixes — mitigated by
  service-side centralization and explicit query callers.
- **Recreate window:** semantic recall degrades to keyword fallback during the
  reindex/re-chunk — acceptable on dev.
