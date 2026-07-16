# Publications (NKBIP-01 kinds 30040/30041) — Design

**Date:** 2026-07-16
**Status:** Approved design, pre-implementation
**Spec:** NKBIP-01 "Curated Publications" (`naddr1qvzqqqrcvypzplfq3m5v3u5r0q9f255fdeyz8nyac6lagssx8zy4wugxjs8ajf7pqqyxu6mzd9cz6vp3tn64lg`, kind 30817, d=`nkbip-01`)
**Producer:** edufeed-app "Wissenschaftliche Publikation" (`src/lib/helpers/publication/publicationTags.js`, `PUBLICATION_KIND = 30040`)

## Goal

Make scientific publications (and curated publications generally) indexable and
searchable on the relay: NIP-50 topical search over metadata **and** article
fulltext (extracted from the linked PDF), facet filtering (type, DOI, authors,
subjects), and the standard multi-content behaviors (deletion, reindex,
community stamping, chunk snippets).

## Decisions made

| Question | Decision |
|---|---|
| Kind scope | **30040 + 30041** (full NKBIP-01, including nested indices) |
| 30040 fulltext | **Metadata + PDF fetch** — amb-indexer fetches `encoding:contentUrl`, Tika-extracts, chunks/embeds, writes back via `setcontent` |
| Collections | **One shared collection** for both kinds (transferkiosk precedent) |
| Validation | **Structural only** — any valid 30040 accepted regardless of `type`; no content policy |
| Extracted text destination | **setcontent kind-routing** — publications doc gets a `content` field, patched by kind-aware `setcontent` (full parity with AMB 30142 recall) |

## Event model (what producers write)

Kind **30040** (publication index): `content` MUST be empty; all metadata in tags.
Core NKBIP-01 tags: `d`, `title` (both required), `type` (academic/book/magazine/…),
`author`, `i` (e.g. `doi:10.x/…`, `isbn:…`), `source`, `version`, `published_on`,
`published_by`, `summary`, `image`, `t`, and ordered `a` tags referencing the
publication's parts. Parts SHOULD be 30041 sections **or nested 30040 indices**
(also 30023/30817/30818 MAY appear).

edufeed-app extensions (AMB-flattened, permitted by NKBIP-01's "additional tags
MAY be included"): `creator:type|name|honorificPrefix|affiliation:name|id` runs
for non-Nostr creators, `p` tags with `creator` marker for Nostr-identity
creators, `about:id` + `about:prefLabel:<lang>` (DeStatis Fachsystematik),
`inLanguage`, `license:id`, `encoding:contentUrl|encodingFormat|contentSize|sha256`
(the uploaded article PDF), `h` (community targeting). edufeed-app deviates from
NKBIP-01 by emitting **no** `a` tags for scientific articles — the body lives
outside Nostr (DOI/source/PDF).

Kind **30041** (publication content): `d` + `title` required; `content` is
display text, MAY be AsciiDoc. **No file/attachment mechanism** — 30041 is the
"chapter written on Nostr" case only.

**The "Reihe" case** (series of separate PDF articles): modeled as nested
indices — a parent 30040 whose `a` tags reference child 30040s, each child
carrying its own `encoding:contentUrl` PDF, DOI, and authors. Each child is
independently fetched and fulltext-indexed; the parent is a metadata-only doc
whose `sections` facet carries the child coords. Two implementation guards
follow from this:

1. The `sections` facet stores **all** `a`-tag coords regardless of referenced
   kind (30041, nested 30040, 30023, 30818) — never assume sections are 30041.
2. `validatePublication` places no constraint on what a 30040's `a` tags
   reference (SHOULD/MAY in the spec).

Known limitation (out of scope): there is no reverse link from a child to its
parent index, so "which Reihe does this article belong to?" is not a single
query. If needed later, edufeed-app can adopt the transferkiosk convention
(`a` tag with `isPartOf` marker on children) and the relay would index it as a
`partOf` facet.

## Relay design

### 1. Content type & gating

- Env gate `PUBLICATIONS_ENABLED` (default `false`); collection
  `TS_COLLECTION_PUBLICATIONS` (default `publications`); backend `tsDB7`.
- Both kinds in ONE shared Typesense collection, one `contentType` registry
  entry with kinds `[30040, 30041]` (transferkiosk precedent).
- Doc id: `GenerateDocumentID(pubkey, d)` — both kinds addressable.

### 2. Validation (`validatePublication`, `publications.go`)

- 30040: require `d` + `title`; **reject non-empty `content`** (spec MUST).
- 30041: require `d` + `title`; content may be any text (the payload).
- No `type` gating, no `a`-tag target constraints.

### 3. Projection (`nostrToPublication`, `publications.go`)

One schema serving both kinds, sparse where a kind doesn't use a field:

- **Shared:** `kind`, `pubkey`, `d`, `title`, `summary`, `searchText`,
  `content`, structured envelope (`community` folded from `h`, tags,
  `created_at`, `eventRaw`, …).
- **30040 facets:** `type`, `author` (string[]), `doi` (from `i` tag, stored
  bare without the `doi:` prefix), `source`, `published_on`, `published_by`,
  `image`, `t` keywords, `about` ids, `inLanguage`, `license`, `sections`
  (all `a`-tag coords), `creatorName` (from `creator:name` runs; `p`-creator
  pubkeys are already covered by the standard pubkey/authors path), `encodingUrl`.
- **30041:** `content` = event content (AsciiDoc body, directly BM25/vector
  searchable at write time — no indexer round-trip needed for recall).
- **`searchText`** folds: author names + creator names + affiliations +
  keywords (`t`) + `about:prefLabel:*` labels + `published_by` — same trick as
  transferkiosk so topical search reaches names and taxonomy labels.

### 4. Writes, deletes, reindex, communities

- StoreEvent/ReplaceEvent route 30040/30041 through the synchronous
  `storeStructured`/`upsertStructuredDoc` path; DeleteEvent targets the
  collection; NIP-09 kind-5 `a`-tag deletion works because the kinds are
  registry-served.
- Reindex: one more `structuredReindexTarget` (drop + reproject via
  `nostrToPublication`), **plus** a content-replay step unique among the
  structured types: 30040 docs carry indexer-written `content`, so after
  reprojection the target replays matching ContentStore entries onto the
  rebuilt docs (scoped to publication event ids).
- Community stamping: add 30040 and 30041 to the CommunityStamper's content
  kinds so member-shared publications match `search:"community:X"`. Model A
  (`h` tag on the event, which edufeed-app writes) works automatically via the
  envelope's `community` field.

### 5. `setcontent` kind-routing

`setcontent` already loads the event from BoltDB before patching, so the kind
is known at patch time. Branch:

- kind 30142 → `tsBuf.QueueContent(event, entry)` (unchanged — buffered path).
- kind 30040 → `contentStore.Put` + **direct PATCH** of `content` /
  `content_fetched_at` / `content_status` on the publications backend. Direct
  is safe here: structured docs are upserted synchronously on write (no
  buffer), so no flush race exists.

`refetchcontent` / `listrefetch` / `acknowledgerefetch` are event-id keyed and
need no changes.

## amb-indexer design (separate repo)

- **30041 — content-direct path:** identical to 30023/30818: chunk
  `event.content`, embed, write to the shared chunk collection. AsciiDoc is
  treated as plain text (headings are not ATX, so like wiki chunks: no heading
  locators). Chunk coords `30041:<pubkey>:<d>`. No fetch, no Tika, no
  setcontent.
- **30040 — external-fetch path:** the 30142-style pipeline with a different
  URL source: fetch `encoding:contentUrl` first (the uploaded PDF); fall back
  to `source` if absent. Tika extracts (PDF or HTML); chunks/embeds land in the
  shared chunk collection (coords `30040:<pubkey>:<d>`, page locators from Tika
  where available); extracted text goes back via `setcontent` (landing per §5).
  Neither URL present → skip quietly (metadata-only doc, like an AMB event with
  a dead link). **No DOI resolution** (YAGNI — publisher landing pages are
  paywalled/inconsistent; `source`/`contentUrl` cover reachable cases).
- Subscription kinds extended to 30040/30041; refetch polling unchanged.
- The snippet layer's `parentKind` already derives kind from the chunk-coord
  prefix, so kind-21142 snippets carry correct `k` tags for free.

## Canonical query shapes (to be added to CLAUDE.md contract)

- Topic: `{"kinds":[30040],"search":"künstliche intelligenz hochschullehre","limit":50}`
  — BM25⊕vector over title/summary/searchText/content (content = extracted PDF text).
- Academic only: `search:"<topic> type:academic"`.
- By DOI: `search:"doi:10.1234/abcd.5678"` (exact facet).
- By author name: via `searchText` (creator names folded in); Nostr-identity
  creators also via the `authors` filter.
- Parts of a publication/Reihe: resolve the 30040 doc's `sections` coords, then
  standard addressable fetch (no reverse facet — see limitation above).
- Community: `search:"community:<pubkey>"` over `kinds:[30040]` per the
  established Model A/B contract.

## Testing

- Unit: `validatePublication` (missing d/title; non-empty 30040 content
  rejected; 30041 content accepted), `nostrToPublication` for both kinds with
  real edufeed-app tag fixtures (including a `creator:*` run, a `p`-creator
  tag, and a nested-30040 `a` reference), setcontent kind-routing.
- Reindex: structured target rebuilds both kinds and replays 30040 content
  from the ContentStore.
- End-to-end on dev: publish a real publication from edufeed-app, verify
  metadata search, PDF-body search, and kind-21142 snippets via nak/amb-mcp.

## Out of scope

- nostrlib changes — none anticipated (schema builder and query path are
  generic); verify during planning.
- edufeed-app changes — it already publishes conforming 30040 events.
- 30041 authoring UI, `partOf` reverse links, DOI resolution.
