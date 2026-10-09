# Mapping expansion for concept filters — Design

Status: design approved in brainstorming 2026-10-10, pending written-spec review.

## Goal

A search for resources with concept X also finds resources tagged with concepts
that are mapped to X — e.g. a filter on KIM educationalLevel
`https://w3id.org/kim/educationalLevel/level_2` (Sekundarbereich I) also finds
EKW resources tagged `ext:ekw:gradeLevel:id = nostr:39738:<pub>:5-6`. The
mappings are the NIP-VOCAB standalone mappings (kind 39740) grouped in
concordances (kind 39741), published e.g. by the vocab publisher
`d2689e2f…a612` from the `edufeed/vocabs` repo.

## Decisions (from brainstorming)

1. **Trust model:** operator default + per-query override. The operator
   configures trusted concordances; a query can switch expansion off, replace
   the set with its own concordances, or add authors.
2. **Relations counted by default:** `exactMatch`, `closeMatch`, and
   everything **narrower** than the queried concept (via `broadMatch` /
   `narrowMatch` in the narrowing direction and via the in-scheme
   `broader`/`narrower` hierarchy). `relatedMatch` only on request
   (`expand:related`). Broader concepts are never added.
3. **Where:** query-time expansion against a new per-document `concepts`
   field (approach A). Index-time expansion (B) was rejected because it fixes
   trust at index time; a cross-field OR in the nostrlib query builder (C) was
   rejected for grammar complexity.

## Background (current code)

- Tag filters (`#educationalLevel:id`, `#ext:ekw:gradeLevel:id`) and NIP-50
  field filters (`educationalLevel.id:…`) become one Typesense `filter_by` in
  nostrlib `eventstore/typesense30142` (`buildNostrFilterExpression`,
  `ParseSearchQuery`/`parseFieldFilter`/`BuildTypesenseQuery`, joined with
  `equalityFilter`). Values within one field are ORed; different field bases
  are ANDed — so `educationalLevel.id` OR `ext.ekw.gradeLevel.id` cannot be
  expressed today.
- Documents are built by `NostrToAMB` into `AMBMetadata`; live writes go
  through amb-relay `prepareBatch` (buffer.go), the NIP-86 reindex
  (reindex.go) through nostrlib `BatchUpsertEvents`/`PrepareBatch` — two
  separate paths.
- amb-relay already runs background registries (AllowlistManager,
  ProfileManager, CommunityRegistry) with refresh loops and injectable
  sources; configuration is env-based.
- amb-relay uses the nostrlib fork from `git.edufeed.org/edufeed/nostrlib`
  (go.mod replace), not the local checkout.

## 1. Components and data flow

```
read relays ──(39741, 39740, 39738)──▶ MappingRegistry ── graph ──▶ Expander
REQ/COUNT {search, #tags} ──▶ QueryStored/Count ──▶ Expander ──▶ concepts:=[…] ──▶ Typesense
30142 event ──▶ prepareBatch (live + reindex) ──▶ AMB doc incl. concepts[] ──▶ Typesense
```

### 1.1 `concepts` field (nostrlib)

- `AMBMetadata` gets `Concepts []string` (JSON `concepts`); schema field
  `concepts`: `string[]`, filterable, not faceted, not part of full-text or
  embedding input.
- `NostrToAMB` fills it with every concept identifier of the resource, deduped:
  the `id` values of `educationalLevel`, `about`, `learningResourceType`,
  `audience` and of every indexed `ext:*` `id` field.
- Identifiers are taken verbatim (URI or `nostr:39738:<pubkey>:<d>` — the
  NIP-VOCAB concept identifier rule; edufeed-app writes exactly these).
- `EnsureSchemaFields` adds the field on startup; existing documents get it via
  reindex.

### 1.2 `vocabmap` package (amb-relay, pure Go, no I/O)

- Parse kinds 39738 (concept: identifier = `i` tag value if present, else
  `nostr:39738:<pubkey>:<d>`; `inScheme`, `broader`, `narrower` addresses),
  39740 (mapping: `from`, `to[]`, `relation`, `fromScheme`, `toScheme`,
  `partOf`, `d`) and 39741 (concordance: owner, maintainers `p … maintainer`,
  `fromScheme`, `toScheme`).
- Validity rules as in NIP-VOCAB "Standalone Mappings": a mapping's `d` must
  equal its sameness identifier (TS4NFDI, SHA-256 over NFC-normalised, deduped,
  code-point-sorted members); concordance membership only for mappings
  authored by the owner or a listed maintainer whose `partOf` addresses the
  concordance and whose schemes match.
- Graph: for each trusted mapping (1:1 only; 1:n mappings are not used for
  expansion in this revision) edges in both directions with the inverse
  relation (`exact↔exact`, `close↔close`, `related↔related`,
  `broad↔narrow`), plus in-scheme hierarchy edges (`narrower`).
- `Expand(identifier, trust) → []string`, in this order (NIP-VOCAB allows
  transitivity for `exactMatch` only, so `closeMatch`/`relatedMatch` are never
  chained):
  1. **E** = exact closure of the identifier (transitive `exactMatch`).
  2. **C** = E ∪ the direct `closeMatch` neighbours of E (one hop).
  3. **N** = everything narrower than any member of C, transitively: in-scheme
     `narrower` and mappings in the narrowing direction (`A broadMatch X` ⇒ A is
     narrower than X), each newly reached concept again taking its exact
     closure (not its closeMatch neighbours).
  4. If `trust.Related`: add the direct `relatedMatch` neighbours of E (one
     hop, not further expanded).

  Never follows an edge to a broader concept. Depth cap 4 for step 3, result
  cap 200 identifiers ordered by step and distance (the queried identifier
  first). Cycles terminate (visited set).
- Conflict rule as in NIP-VOCAB: a pair that is both `exactMatch` and
  broad/narrow/related within the trust set contributes no `exactMatch` edge.
- Test vectors: the NIP-VOCAB sameness-identifier vectors; graph tests for
  direction, depth/result caps, cycles, conflicts, untrusted mappings ignored.

### 1.3 `MappingRegistry` (amb-relay)

- Modeled on `CommunityRegistry` (Init + StartRefreshLoop + injectable source).
- Loads, from `EXPAND_READ_RELAYS`: the default concordances
  (`EXPAND_CONCORDANCES`), their member mappings
  (`{"kinds":[39740],"#a":["39741:<owner>:<d>"],"authors":[owner, maintainers…]}`),
  and the concepts (39738) of every scheme referenced by those concordances
  (`{"kinds":[39738],"#a":["39737:<pk>:<scheme d>"]}`), paged ≤ 300 per request.
- Additionally on demand (per-query `expand:` sources): concordances given as
  `naddr` and all 39740 mappings by authors given as `npub`, cached with the
  same refresh interval.
- Refresh every `EXPAND_REFRESH_INTERVAL` (default 10 min); on error keep the
  last good graph and log; on cold start (nothing loaded yet) queries run
  unexpanded.

### 1.4 Expander (amb-relay)

- Hooked into `QueryStored` and `Count` before the content registry, so REQ and
  COUNT expand identically.
- Applies to concept filters only:
  - tag filters whose name ends in `:id` and maps to a concept field
    (`educationalLevel:id`, `about:id`, `learningResourceType:id`,
    `audience:id`, `ext:<ns>:<facet>:id`);
  - NIP-50 field filters on the corresponding `*.id` fields.
- For each such filter: compute the union of `Expand(v)` over its values
  (OR semantics preserved) and replace the filter by `concepts:=[…]` (tag filter
  `#concepts` mapped via `TagFields`, or the equivalent search token —
  whichever the nostrlib grammar supports without changes; implementation
  chooses and documents it). Several concept filters remain ANDed.
- If expansion yields only the original values (no mappings), the original
  filter is left untouched (no behaviour change for unmapped concepts).
- Calendar re-ranking (`rerankOwns`) must keep treating the rewritten filter as
  a field filter.

### 1.5 Query syntax (NIP-50 extension)

NIP-50 allows `key:value` extensions in `search`; relays SHOULD ignore
unsupported ones. Tokens:

| Token | Effect |
|---|---|
| (none) | default trust set (`EXPAND_CONCORDANCES`), default relations |
| `expand:off` | no expansion |
| `expand:<naddr1…>` (repeatable) | use these concordances instead of the default set |
| `expand:<npub1…>` (repeatable) | additionally trust all mappings authored by this pubkey |
| `expand:related` | additionally follow `relatedMatch` |

- Values are bech32 only (no colon), so each token stays one `key:value` pair;
  raw addresses (`39741:<pk>:<d>`) are not accepted.
- `ParseSearchQuery` must extract `expand:` tokens **before** field-filter
  parsing (amb-relay's own grammar splits `field.id:value` at the first colon;
  otherwise `expand` would be read as a filter on a non-existent field and fail
  closed). The tokens are removed from the remaining full-text query.
- Relays without the extension ignore the tokens; clients then simply get
  unexpanded results.

## 2. Limits, errors, observability

- Result cap 200 identifiers per filter value (ordered by graph distance), depth
  cap 4; exceeding the cap logs a warning with the queried identifier.
- Unknown / unresolvable / malformed `expand:` value: ignored for that source;
  the query runs with the remaining trust set; optional NIP-01 `NOTICE`.
  Never fail closed.
- Registry not loaded or relays unreachable: expansion skipped, warning logged;
  last good graph retained across refresh errors.
- Debug logging (env `EXPAND_DEBUG`): log original → expanded identifiers per
  query; never in responses.
- NIP-11: document the extension (supported tokens) in the relay info;
  README section "Concept expansion".

## 3. Configuration

| Env | Default | Meaning |
|---|---|---|
| `EXPAND_CONCORDANCES` | empty | comma-separated `naddr`s of default concordances; empty = no default expansion |
| `EXPAND_READ_RELAYS` | `wss://relay.edufeed.org` | relays to load vocab/mapping events from |
| `EXPAND_REFRESH_INTERVAL` | `10m` | registry refresh |
| `EXPAND_DEBUG` | `false` | log expansions |

Runtime changes via NIP-86 are out of scope for this revision.

## 4. Rollout

1. vocabs repo: publish the B1–B3 crosswalks (Klassenstufen → KIM level,
   Schulart → KIM level, remaining ekw-lrt → HCRT) as decided in the mapping
   review doc, via `pnpm plan` / `pnpm apply`.
2. nostrlib: release the `concepts` field; amb-relay bumps the replace.
3. Deploy amb-relay with `EXPAND_CONCORDANCES` empty; run NIP-86 `reindex`;
   verify `concepts` is populated (spot-check documents).
4. Verify with explicit `expand:<naddr>` queries (e.g. `educationalLevel.id`
   level_2 + `expand:<klassenstufen concordance>` finds known EKW resources).
5. Set `EXPAND_CONCORDANCES` to the published concordances, restart, verify
   default expansion and `expand:off`.

## Testing

- `vocabmap`: unit tests (parsing, validity rules incl. sameness id vectors,
  membership rules, graph direction, caps, cycles, conflicts).
- nostrlib: `NostrToAMB` fills `concepts` from all concept fields incl. `ext`;
  schema includes the field.
- amb-relay: expander unit tests (tag + NIP-50 filters, OR preserved, AND across
  filters, untouched when no mappings, `expand:` token parsing incl. the
  colon-split hazard, unknown tokens ignored); registry tests with a fake
  source (refresh, error retention, cold start); reindex path uses
  `prepareBatch`.
- Integration (Typesense test container): resource tagged only with
  `ext:ekw:gradeLevel:id = nostr:…:5-6` is found by an `educationalLevel.id`
  level_2 filter with the Klassenstufen concordance trusted; not found with
  `expand:off`; COUNT matches REQ.

## Out of scope (follow-ups)

- edufeed-app: send `educationalLevel` as a filter; optional expansion toggle.
- amb-mcp: filter by id instead of prefLabel so expansion applies.
- nostrlib bug: `ext` keys with an extra segment (`ext:ekw:konfi:themen:id`)
  are dropped at index time — Konfi facets are unfilterable today; separate fix.
- Runtime configuration via NIP-86; web-of-trust based trust.
- 1:n mappings in expansion.
