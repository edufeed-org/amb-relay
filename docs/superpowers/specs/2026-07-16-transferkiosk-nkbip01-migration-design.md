# Transferkiosk Publikationen → NKBIP-01 (retire kind 30145) — Design

**Date:** 2026-07-16
**Status:** Approved decisions, pre-implementation
**Depends on:** Publications content type (spec `2026-07-16-publications-30040-design.md`, merged: relay dev `db88f80`, indexer main `a488ced`) — NOT yet deployed.

## Goal

Stop maintaining a parallel publication event schema. Transferkiosk
publications are re-published as NKBIP-01 kind-30040 publication indices;
kind 30145 is retired everywhere (converter, relay, indexer, spec). Projekt
(30143) and Maßnahme (30144) remain NIP-DIDACTIC — they have no NKBIP
equivalent. After this migration there is exactly ONE publication event
schema in the ecosystem, produced by both edufeed-app and the Transferkiosk
importer in the same dialect.

**Why now:** nothing consumes 30145 except our own relay + amb-mcp (verified:
stil-finder is data dumps, edufeed-app has no transferkiosk kinds);
transferkiosk is dev-only (`amb_relay_transferkiosk_enabled: false` in prod
homelab defaults); the 186 dev events are importer-owned and fully
re-publishable. The naming collision this dissolves also unblocks the
amb-mcp publications work (follow-up project, out of scope here).

## Decisions (user-approved 2026-07-16)

| Question | Decision |
|---|---|
| New d-tag identity | Deterministic `tk-p{projekt_id}-pub{publikation_id}` slug (NKBIP-conformant: lowercase letters/digits/hyphens; stable across re-imports) |
| `type` mapping | ScholarlyArticle/Chapter → `academic`, Book → `book`; original schema.org type preserved as `additionalType` extension tag |
| Old 30145 events | Proper NIP-09 kind-5 deletions signed by the importer key (`EDUFEED_PUBLISHER_NSEC`) |
| NIP-DIDACTIC spec | `nips/DIDACTIC.md` updated: 30145 section replaced by a pointer to NKBIP-01 (`naddr1qvzqqqrcvypzplfq3m5v3u5r0q9f255fdeyz8nyac6lagssx8zy4wugxjs8ajf7pqqyxu6mzd9cz6vp3tn64lg`, kind 30817, d=`nkbip-01`) plus documentation of the transferkiosk extension tags |

## Repos touched

1. `edufeed-data/transferkiosk` — converter + deletion script (the root fix)
2. `amb-relay` — publications projection gains `partOf`; transferkiosk type drops 30145
3. `amb-indexer` — drop 30145 from subscription/dispatch
4. `nips` — DIDACTIC.md rewrite of the publication section
5. NOT touched: homelab (no new vars), amb-mcp (follow-up), edufeed-app (already NKBIP-conformant)

## 1. Converter (`edufeed-data/transferkiosk/convert.py`, `build_publikation`)

Emits kind **30040** with **empty content** (30145 put the description in
content; the relay's NKBIP validation rejects that — the description moves
entirely into `summary`).

Tag mapping (old → new):

| 30145 tag | 30040 tag |
|---|---|
| `d` = DOI-URL or tk URL | `d` = `tk-p{projekt_id}-pub{publikation_id}` |
| `name` | `title` |
| `type` = ScholarlyArticle/Book/Chapter | `type` = `academic`/`book`/`academic` + `additionalType` = original schema.org type |
| `description` + content | `summary` (content empty) |
| `author:name/type/honorificPrefix` runs | **both** plain `author` (one per person, edufeed-app convention) **and** `creator:type/name/honorificPrefix` runs |
| `editor:*` runs | kept verbatim (extension) |
| `datePublished` | `published_on` (same `YYYY-01-01` value) |
| `publisher:name` (+`publisher:type` dropped) | `published_by` |
| `i` = `https://doi.org/10.x` | `i` = `doi:10.x` (NKBIP code form); non-DOI publikationsnummer stays raw `i` |
| `r` = transferkiosk page URL | `source` = same URL (`r` dropped; `source` is the NKBIP tag and the indexer's fetch fallback — converted publications get real fetched fulltext instead of a chunked two-line description) |
| `publicationLocation`, `pageRange`, `publicationType:*` triple | kept verbatim (extensions; NKBIP-01 explicitly allows additional tags) |
| `a` → 30143 with `isOutputOf` marker | unchanged |
| `license:id`, `inLanguage` | unchanged |

`PUBLIKATION_KIND = 30040`; `d_publikation()` returns the new slug. Events
land in `events/publikation.jsonl` as before; `publish-events.py` is
kind-agnostic and needs no change.

**New script `delete-30145.py`** (same style as publish-events.py): queries
the target relay for `{"kinds":[30145],"authors":[<importer pubkey>]}` via
nak, emits one kind-5 per event carrying both the `e` (event id) and `a`
(`30145:<pubkey>:<d>`) tags, signs with `EDUFEED_PUBLISHER_NSEC`, publishes.
Supports `--dry-run` and `--relays`. (Relay side: NIP-09 removes each event
from BoltDB + Typesense and blocks exact re-publication — harmless, since
replacements are a different kind and d.)

## 2. amb-relay

**publications.go — `partOf` support (fixes a latent bug):** the projection
currently folds ALL `a` tags into `sections`; a converted publication's
`isOutputOf` project link would pollute the section list. Change: an `a` tag
whose marker (`tag[3]`) is `isPartOf` or `isOutputOf` goes to a new
`partOf` string[] facet (same convention as transferkiosk) and is EXCLUDED
from `sections`; unmarked `a` tags remain sections. Additionally fold
`editor:name` values into `searchText` (people search reaches editors).
Schema gains `partOf` (faceted string[]). `additionalType`,
`publicationLocation`, `pageRange` stay eventRaw-only (no query need).

**transferkiosk retirement of 30145:** remove 30145 from the contentType
kinds, retention list, reindex target, stamp targets, and the docs; remove
the publication-only projection fields (`Author`, `Publisher`,
`DatePublished`, `PublicationType`) and their schema entries + parsing
cases. `partOf` stays (30144 uses `isPartOf`). `validateTransferkiosk`
unchanged (generic d+name).

**CLAUDE.md / canonical queries:** transferkiosk section becomes
30143/30144-only; publications section documents `partOf` and the
transferkiosk-born extension tags; "Measures of a project" query drops
30145; new shape "Publications of a project":
`{"kinds":[30040],"search":"partOf:30143:<pub>:<d>","limit":50}` (NIP-50
field syntax — plain `#partOf` tag filters are skipped by the query
builder).

**Post-deploy:** one NIP-86 reindex rebuilds both collections (drops the
stale 30145 docs from the transferkiosk collection and projects the new
`partOf` facet).

## 3. amb-indexer

Remove 30145 from the subscription kind list (`main.go`) and the
content-direct dispatch condition (`worker.go`). Converted publications
arrive as 30040 → external-fetch path via their `source` URL (already
shipped, incl. the source-fallback tests and the license exemption). 30143
and 30144 stay content-direct. Chunks for retired 30145 coords become
orphans in the chunk collection; acceptable residue (they stop matching any
parent on the ID-only parent fetch) — optionally cleaned with a one-off
`coord_hash` filter delete, not required.

## 4. NIP-DIDACTIC spec (`nips/DIDACTIC.md`)

- Kind table: 30145 row replaced — "Publikation: uses NKBIP-01 kind 30040
  (Curated Publications), see the NKBIP-01 spec" with the naddr above.
- `## Kind 30145 — Publikation` section replaced by `## Publikationen
  (NKBIP-01 kind 30040)`: states that publications are standard NKBIP-01
  indices; documents the transferkiosk-specific extension tags
  (`additionalType`, `editor:*`, `publicationLocation`, `pageRange`,
  `publicationType:*` concept triple, `a`+`isOutputOf` project link) and
  the field mapping table from §1.
- Link-marker table: `isOutputOf` / `documents` rows now reference kind
  30040 events.
- Query examples updated to kind 30040 + NIP-50 field syntax.

## 5. Migration runbook (dev)

Order matters twice: the relay must accept 30040 BEFORE the importer
publishes, and the old 30145s must be enumerated + deleted BEFORE the
retirement deploy (once 30145 leaves the registry, REQs for it return
nothing and the deletion script has nothing to enumerate against).

1. Push amb-relay dev + amb-indexer main incl. the `partOf` projection
   change (but NOT yet the 30145 retirement); deploy dev with
   `PUBLICATIONS_ENABLED=true` (homelab role env — gate on CI image per
   `ops_dev_deploy_stale_binary`).
2. Run the updated converter; `publish-events.py --type publikation`
   against dev → new 30040s (verify count ≈ old 30145 count).
3. Run `delete-30145.py` against dev while 30145 is still served → old
   events enumerated and deleted (kind-5 with `e` + `a` tags each).
4. Merge + deploy the 30145 retirement (relay + indexer).
5. Trigger NIP-86 reindex; verify: `search:"partOf:30143:..."` returns
   publications; kind-30145 REQ returns nothing; a converted publication's
   fulltext lands after the indexer fetches its `source` page; DOI query
   `search:"doi:10.x"` hits.

(Consequence for branch mechanics: the relay/indexer work lands as TWO
separable changes — `partOf`/searchText projection first, 30145 retirement
second — so step 1 can deploy without the retirement. The plan should keep
them as separate commits or branches.)

## Testing

- Converter: extend the existing converter check pattern (dry-run +
  assertions on `events/publikation.jsonl`): kind 30040, empty content,
  slug d, both author shapes, `i` code form, `additionalType`, `source`.
- Relay: publications_test.go — marked `a` → `partOf` not `sections`;
  editor:name in searchText; transferkiosk_test.go — publication cases
  removed, Maßnahme `isPartOf` unchanged; schema field test updated.
- Indexer: dispatch/subscription list assertions updated (no 30145).
- Dev smoke per runbook step 5.

## Out of scope

- amb-mcp publications support (follow-up project; after this migration the
  `publication` type name is unambiguous: kinds 30040/30041 only).
- Prod (transferkiosk was never enabled there).
- Republishing 30143/30144 (unchanged), the `documents` link marker
  (converter never emitted it), DOI resolution.
