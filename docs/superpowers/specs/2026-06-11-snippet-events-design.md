# Ephemeral Search-Snippet Events (kind 21142)

**Date:** 2026-06-11
**Status:** Approved design, pending implementation
**Depends on:** chunk re-ranking (`chunk_rerank.go`, shipped in `a77f580`)

## Problem

Chunk re-ranking orders NIP-50 search results by the best matching fulltext
passage, but the passage itself — the "why this matched" — never reaches the
client. Plain NIP-50 has no channel for per-result relevance metadata, so
clients cannot show highlighted snippets or deep links into the source
document.

## Goal

Deliver the best matching passage, its score, and source locators alongside
each search result, while staying nostr-only: ordinary events over the
existing REQ, no side-channel HTTP, no protocol forks. Legacy clients must be
completely unaffected.

## Design

### Event shape — kind 21142

Kind 21142 sits in the ephemeral range (20000–29999, never stored by relays)
and mirrors the kind 30142 it annotates.

```json
{
  "kind": 21142,
  "pubkey": "<relay signer pubkey>",
  "created_at": "<query time>",
  "content": "…bei stärkerem Licht verdoppelt sich die Photosyntheserate…",
  "tags": [
    ["e", "<parent event id>"],
    ["a", "30142:<author pubkey>:<d-tag>"],
    ["k", "30142"],
    ["score", "0.9312"],
    ["page", "12"],
    ["heading", "Lichtabhängigkeit"],
    ["source_url", "https://…/skript.pdf"]
  ],
  "sig": "<valid schnorr signature>"
}
```

- `content`: the indexer's display snippet (`SearchHit.Snippet`), verbatim.
- `e` + `a`: both included so clients can join by id or by coordinate. Both
  values come directly from the `/search_chunks` hit (`event_id`,
  `event_coord`) — no reconstruction.
- `k`: parent kind. Constant `"30142"` today; kept as the extension hook for
  other content kinds later.
- `score`: best chunk score for this parent, formatted `%.4f`.
- `page`, `heading`, `source_url`: emitted only when the indexer extracted
  them; omitted otherwise.

### Emission rules

A snippet event is emitted iff **both** hold:

1. The chunk-rerank path actually served the query (search term present,
   searcher configured, chunk hits returned). All fallback paths — indexer
   error, empty chunk result, rerank disabled — emit no snippets. Snippets
   are best-effort decoration; clients must not depend on them.
2. The client opted in by including 21142 in the REQ kinds:
   `{"kinds": [30142, 21142], "search": "…"}`. Clients that ask only for
   30142 see exactly today's behavior.

Delivery: interleaved directly after their parent
(`30142, 21142, 30142, 21142, …, EOSE`), written only to the requesting
subscription. Never stored, never broadcast to other subscriptions, never in
Negentropy syncs (those filters carry no search field and bypass re-ranking).

`limit` counts parent events; an opted-in client receives at most `2×limit`
events total.

If a hit has an empty snippet, or signing fails, that snippet event is
skipped (logged) and the parent is still delivered — search never degrades
because of snippet problems.

Client-published 21142 events remain rejected by the existing `OnEvent`
guard (only kinds 30142 and 5 are accepted), so the kind is strictly
relay-originated.

Known protocol caveat: strict clients that post-filter received events
against their own REQ (e.g. with an `authors` constraint the relay-signed
snippet cannot match) will locally discard snippets. That is graceful — they
keep the parents.

### Signing identity

New env var `RELAY_SECKEY` (hex secret key, dedicated to the relay — never
the operator's key). When unset, the relay generates a fresh key at startup
and logs its pubkey; snippets remain validly signed, identity just isn't
stable across restarts. Because of this fallback a signer always exists —
there is no "snippets requested but unsigned" state.

v1 surfaces the signer pubkey via startup log + README only. NIP-11
exposure is deferred until a client needs to pin it.

### Code changes

| File | Change |
|------|--------|
| `chunk_snippet.go` (new) | `kindSearchSnippet = 21142`; `buildSnippetEvent(sk, hit) (nostr.Event, bool)` — pure, leaf, no relay machinery |
| `chunk_snippet_test.go` (new) | Tests 1–4 below |
| `chunk_rerank.go` | `ChunkHit` gains `Snippet, Page, Heading, SourceURL`; best-score map `map[string]float64` becomes best-hit map `map[string]ChunkHit`; `chunkRerankQuery` gains one parameter (signing key) and interleaves snippets per the emission rules |
| `chunk_rerank_test.go` | Tests 5–8 below, reusing existing fakes (`fakeChunkSearcher`, `fakeStore`, `mkEvent`, `collectEvents`) |
| `chunk_searcher_http.go` | Parse `snippet`, `page`, `heading`, `source_url` from the response (already served by amb-indexer — **zero indexer changes**) |
| `chunk_searcher_http_test.go` | Extend existing happy-path test for the new fields (test 9) |
| `main.go` | Load `RELAY_SECKEY` / generate fallback, log signer pubkey, pass key to `chunkRerankQuery` |
| `README.md`, `CLAUDE.md`, `.env.example` | Document kind 21142, opt-in, `RELAY_SECKEY` |

Out of repo: a draft paragraph for the published NIP-AMB spec (naddr), to be
published by the operator.

### Testing (TDD — tests written first, in this order)

1. `BuildSnippetEvent_ValidSignature` — event verifies against signer pubkey
2. `BuildSnippetEvent_TagsCorrect` — e/a/k/score and locator tags
3. `BuildSnippetEvent_OmitsEmptyLocators` — no empty page/heading/source_url tags
4. `BuildSnippetEvent_EmptySnippet_NotEmitted` — returns ok=false
5. `Rerank_InterleavesSnippetsWhenOptedIn` — parent→own-snippet ordering, refs match
6. `Rerank_NoSnippetsWithoutKindOptIn` — kinds:[30142] only → zero 21142 events
7. `Rerank_NoSnippetsOnFallback` — indexer error + opted-in filter → plain results, no snippets
8. `Rerank_LimitCountsParents` — limit 2 → 4 events (2 parents + 2 snippets)
9. `HTTPChunkSearcher` happy path asserts snippet/page/heading/source_url parsing

### Out of scope (v1)

- Top-N passages per parent (single best passage only; clients dedupe by `a` if this changes later)
- NIP-11 signer-pubkey field
- Caching of chunk responses
- Any amb-indexer changes
