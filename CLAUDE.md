# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

AMB Relay is a Nostr relay specializing in AMB (Learning Resource Metadata) events (kind 30142). Built on khatru relay framework with Typesense as the full-text search backend.

## Specifications

* nostr naddr of specification: `naddr1qvzqqqrcvypzp0wzr7fmrcktw4sgemxh5zsq5auh08vnvlwf0x9anusn7pkft0zgqy28wumn8ghj7un9d3shjtnyv9kh2uewd9hsqzm9v36kvet9vskkzmtzvjvrtf`
* The relay implements the `ext:` namespace defined in NIP-AMB for non-core metadata fields. See the [eventstore README](https://git.edufeed.org/edufeed/nostrlib/src/branch/master/eventstore/typesense30142/README.md#extension-namespace-ext) for query examples.
* **Community spec (Communikey):** `naddr1qvzqqqrcvgpzqesd33ux28msfplvnwxac2p7988j2ctf8hdrhgjx607nczxmkuyrqy88wumn8ghj7mn0wvhxcmmv9uq3vamnwvaz7tmjv4kxz7fwv35hgar09ec82c30qq8kummnw3ez6cm0d4kh2mnfw3usu25ny8` — kind-30818 wiki "Nostr protocol specification for Communities" (pubkey `660d8c78651f70487ec9b8ddc283e29cf2561693dda3ba246d3fd3c08dbb7083`, d-tag `nostr-community`). Defines: community pubkey = identifier; publications target a community via an `h` tag carrying the community pubkey; reposts (kind 6/16) bring external content into a community feed; canonical read query is `#h` = community pubkey. This is the spec the community-shares feature implements.

## Build & Run Commands

```bash
# Local development (starts both Typesense and relay)
docker compose up

# Local development with eventstore changes (run relay outside Docker)
docker compose up -d typesense
go run .

# Production deployment
docker compose up -d --build

# Run tests in eventstore
cd ../nostrlib/eventstore/typesense30142 && go test ./...
```

## Architecture

```
Nostr Client → Khatru Relay (:3334) ─┬→ Typesense (:8108)  [search index]
              NIP-86 HTTP API ────────┘  BoltDB             [raw events + bans]
                                  ▲
                                  │  NIP-86 setcontent (NIP-98 signed)
                                  │
                              amb-indexer ──► Typesense (amb_chunks_30142)
                                  │           [chunk embeddings]
                                  ▼
                                Tika   (PDF extraction)
                                Embed  (sentence-transformers; in-stack)
```

The relay itself does not fetch or embed resources — that is `amb-indexer`'s job (see [`../amb-indexer`](../amb-indexer)). The indexer subscribes to kind-30142 events, extracts text, chunks and embeds it, and calls the relay's NIP-86 `setcontent` to populate the `content` field on each event. Chunks go to a separate Typesense collection.

Embedding runs in-stack as the `embed` service (`./embed`) — a small FastAPI container loading `intfloat/multilingual-e5-base` (768-dim). The service prepends an `input_type` prefix (`query:` for searches, `passage:` for indexed text) as required by the e5-base instruction-tuned contract. Both the relay (semantic write-path) and the indexer (chunk pipeline) point `EMBED_ENDPOINT` at `http://embed:8100/embed`, so there's no external dependency on `embed.edufeed.org` for the docker stack. Pytest suite for the service lives in `embed/tests/`.

**Main Entry Point:** `main.go` - Sets up khatru relay with dual-write to Typesense (search) and BoltDB (raw persistence), NIP-42 auth, NIP-86 management API, and Negentropy protocol.

**Management:** `management.go` - BoltDB-backed store for ban lists (pubkeys, events) and Typesense schema configuration, sharing the same bbolt database as the event store.

**Reindexer:** `reindex.go` - Async reindex from BoltDB to Typesense with progress tracking, triggered via NIP-86 `reindex` method. Rebuilds **every** enabled content type's collection: the AMB (30142) collection via its special path (schema recreate → batch upsert → content-patch/orphan-GC), then each structured type (`longform_30023`, `wiki_30818`) via drop+reproject (`structuredReindexTarget` built in `main.go`; reprojection reuses `reprojectStructured`/`nostrToLongform`/`nostrToWiki`). With `LONGFORM_ENABLED`/`WIKI_ENABLED` off the structured targets are empty, so reindex is unchanged.

**Event Flow:**
- Banned pubkeys are rejected on submission (checked before validation)
- Accepts kind 30142 events (AMB educational metadata) and kind 5 deletion events (NIP-09)
- Kind 30142 events are saved to both BoltDB (raw) and Typesense (indexed); kind 5 events are stored in BoltDB only and served on REQ straight from BoltDB (filterable by `kinds`, `authors`, `ids`, `#e`, `#a`, `#k`) per NIP-09's "relays SHOULD continue to publish deletion requests"
- Deletion events remove the referenced event from both BoltDB and Typesense (author must match); each deleted id is recorded in a `deleted_events` BoltDB bucket and re-publication of the exact deleted event is rejected. Kind-5 write policy: must carry an `e` tag, or an `a` tag referencing a kind the relay serves
- Queries go through Typesense for full-text search capability
- Queries support NIP-01 filter fields, tag filters, and NIP-50 search — see [eventstore README](https://git.edufeed.org/edufeed/nostrlib/src/branch/master/eventstore/typesense30142/README.md) for full query documentation
- Tags using the `ext:<ns>:<facet>:<sub>` shape (NIP-AMB extension namespace) are folded into a separate `ext` object in Typesense and queryable via NIP-50 (`ext.<ns>.<facet>.id:val`) and tag filter (`#ext:<ns>:<facet>:id`).

**Long-form (NIP-23 kind-30023), gated behind `LONGFORM_ENABLED`:**
- When `LONGFORM_ENABLED=true`, the relay also accepts kind-30023 long-form events. Required tags: `d` + `title` (validated in OnEvent; rejected when missing).
- 30023 structured fields are projected (`nostrToLongform`, `longform.go`) into a SEPARATE Typesense collection (`longform_30023` / `TS_COLLECTION_LONGFORM`), keeping the AMB collection pristine. StoreEvent/ReplaceEvent route 30023 to the second backend (`tsDB2`); DeleteEvent targets both collections.
- The query path merges all registered collections by kind via the content-type registry (`content_registry.go`): a filter's `kinds` selects which backends to hit (`registry.selected`/`registry.fetch`), and the ID-only parent fetch from chunk re-ranking queries every registered type.
- amb-indexer chunks 30023 `event.content` DIRECTLY (no external fetch/Tika) and writes to the SAME shared chunk collection (`amb_chunks_30142`); chunk coords are kind-prefixed (`30023:<pubkey>:<d>`).
- NIP-50 search over `kinds:[30142,30023]` returns interleaved results ranked by chunk score; opted-in clients (adding `21142`) receive a snippet kind-21142 event after each result carrying the correct parent `k` tag (`parentKind` derives it from the coord prefix).

**Wiki (NIP-54 kind-30818), gated behind `WIKI_ENABLED`:**
- When `WIKI_ENABLED=true`, the relay also accepts kind-30818 wiki events. Required tag: `d` (validated by `validateWiki`); `title`/`summary` optional. Structurally a near-subset of long-form 30023.
- 30818 structured fields are projected (`nostrToWiki`, `wiki.go`) into a SEPARATE Typesense collection (`wiki_30818` / `TS_COLLECTION_WIKI`), registered as a third `contentType` in the registry with its own backend (`tsDB3`). Long-form and wiki share the synchronous import-upsert + envelope helpers in `structured.go` (`upsertStructuredDoc`, `structuredEnvelope`, `storeStructured`); the AMB collection stays special (write buffer, embedder, NIP-86 schema).
- amb-indexer chunks 30818 `event.content` via the SAME content-direct path as 30023 (no external fetch/Tika/setcontent) into the shared chunk collection; chunk coords are kind-prefixed (`30818:<pubkey>:<d>`). Wiki content is Djot, so its headings are not parsed as ATX — wiki chunks carry no heading locators.

**Transferkiosk (NIP-DIDACTIC kinds 30143 Projekt / 30144 Maßnahme), gated behind `TRANSFERKIOSK_ENABLED`:**
- When `TRANSFERKIOSK_ENABLED=true`, the relay accepts both kinds. Required tags: `d` + `name` (validated in `validateTransferkiosk`). The two kinds map to schema.org types: Projekt=ResearchProject, Maßnahme=Action. Both are projected (`nostrToTransferkiosk`, `transferkiosk.go`) into a SINGLE shared Typesense collection (`transferkiosk` / `TS_COLLECTION_TRANSFERKIOSK`), registered as a fourth `contentType` in the registry with its own backend (`tsDB6`). StoreEvent/ReplaceEvent route both kinds to the same backend; DeleteEvent targets the collection.
- The `searchText` field folds `description` + narrative tag values + every `prefLabel:de` concept label so topical search reaches term labels from taxonomies (about/audience/activity/actionField/actionScope/studyModel/objective/transferability). High-coverage facets (`about`, `audience`, `activity`, `status`, `funderName`, `funderProgram`, `hostName`, `hostBundesland`, `startDate`, `endDate`) enable precise filtering. Measures link to their parent project via the `partOf` facet (an `a` tag with coord `30143:<pub>:<d>` and marker `isPartOf` or `isOutputOf`).
- amb-indexer chunks both kinds via the content-direct path (no external fetch/Tika/setcontent) into the shared chunk collection; chunk coords are kind-prefixed (`30143:<pubkey>:<d>` for Projekt, `30144:...` for Maßnahme).
- Reindex appends a `structuredReindexTarget` (in `main.go`) covering both kinds: one BoltDB pass rebuilds the Typesense collection via drop+reproject (`nostrToTransferkiosk`).
- Publikationen were migrated to NKBIP-01 kind 30040 (see Publications; spec `docs/superpowers/specs/2026-07-16-transferkiosk-nkbip01-migration-design.md`); kind 30145 is retired.

**Publications (NKBIP-01 kinds 30040 index / 30041 section), gated behind `PUBLICATIONS_ENABLED`:**
- When `PUBLICATIONS_ENABLED=true`, the relay accepts kind-30040 (publication index) and kind-30041 (publication section/content) events per NKBIP-01 — the spec edufeed-app uses to publish scientific publications, alongside general curated publications. Required tags for both kinds: `d` + `title` (validated in `validatePublication`). NKBIP-01 additionally requires kind-30040's `content` field to be empty — a 30040 with non-empty content is rejected; the article's fulltext arrives later via kind-routed `setcontent`, never in the signed event.
- Both kinds project (`nostrToPublication`, `publications.go`) into a SINGLE shared Typesense collection (`publications` / `TS_COLLECTION_PUBLICATIONS`), registered as a `contentType` in the registry with its own backend (`tsDB7`). StoreEvent/ReplaceEvent route both kinds to the same backend; DeleteEvent targets the collection. Doc ids are kind-folded (`docIDFor`, `structured.go`): `{kind}:{pubkey}:{d}`, so the same (pubkey, d) under 30040 and 30041 never collide.
- `searchText` folds author/creator/editor names, creator affiliations, keywords (`t` tags), every `about:prefLabel:*` concept label, and the `published_by` venue, so topical search reaches names and taxonomy labels that have no dedicated facet. Facets: `type`, `doi`, `author`, `creatorName`, `keywords`, `published_on`, `published_by`, `about`, `inLanguage`, `license`, `sections`, `partOf`. **Query these via NIP-50 `field:value` syntax** (e.g. `search:"doi:10.1234/abcd.5678"`) — the shared query builder (`buildNostrFilterExpression`, nostrlib `typesense30142/query.go`) only maps a handful of tag names to Typesense fields (`t`→`keywords`, plus `r`/`p`/`a`/`h` and any `ns:facet` shape); a plain `#doi`/`#author`/`#published_on`/… tag filter falls into the builder's `default: continue` case and is silently skipped, so those facets are reachable only through `search`, never a bare tag filter. `#t` is the one exception — it maps to `keywords`.
- **The Reihe model:** a 30040's `a` tags fold into `sections` in tag order — normally kind-30041 section coords, but NKBIP-01 also lets a 30040 nest other 30040 indices (a "Reihe"/series over constituent publications, or a collected volume over its chapters). `sections` never assumes a fixed child kind. **Limitation: no reverse link** — a nested 30040/30041 carries no back-reference to its parent(s), so "which Reihe is this part of" needs a client-side search over candidate parents' `sections`, not a facet lookup.
- **Marker-aware `a`-tag routing:** the 4th `a`-tag element is ambiguous across specs — NKBIP-01 reserves it for an OPTIONAL EVENT ID (64-hex), while NIP-DIDACTIC (transferkiosk) uses it for word markers. `nostrToPublication` routes on it: `isPartOf`/`isOutputOf` → the `partOf` facet (e.g. a transferkiosk-born publication's parent-project/measure link, `30143:<pub>:<d>`/`30144:<pub>:<d>`); absent or 64-hex (`isHex64`) → `sections`, keeping the parts list purely the publication's own content; any other word marker (vocab concept refs, `documents`) is a typed link that stays in `eventRaw` only, folded into neither facet.
- **Transferkiosk-born extension tags:** publications converted from the retired kind-30145 Publikation carry extension tags NKBIP-01 explicitly allows alongside the standard fields: `additionalType` (preserves the original schema.org type, e.g. ScholarlyArticle/Book, when `type` is normalized to `academic`/`book`), `editor:*`, `publicationLocation`, `pageRange`, and the `publicationType:*` concept triple. Of these, `editor:name` folds into `searchText` and `a`+`isOutputOf` routes to the `partOf` facet (see above); `additionalType`, `publicationLocation`, `pageRange`, and `publicationType:*` have no dedicated facet and survive only in `eventRaw`.
- Kind-routed `setcontent`/`refetchcontent` (`main.go`): 30040 PATCHes the publications collection directly (`setPublicationContent`/`clearPublicationContent`, `publications.go`) instead of the AMB collection's buffered `tsBuf.QueueContent` path — safe here because structured docs upsert synchronously on write, so the doc already exists by patch time. `ContentStore` is shared with the AMB path (same BoltDB bucket, keyed by event id hex).
- Reindex appends a `structuredReindexTarget` (`main.go`) covering both kinds: drop+reproject via `nostrToPublication`, then an `after` hook replays every ContentStore row belonging to a live kind-30040 event onto the rebuilt docs — reprojection alone writes `content=""` for 30040 (the raw event's content is empty by the NKBIP-01 rule), so without the replay the fulltext patched in by amb-indexer would be lost on every reindex.
- Reindex orphan-GC (`reindex.go`, `classifyOrphans`) keeps ContentStore rows whose event exists under **any** kind, not just AMB (30142) — e.g. kind-30040 publication fulltext belongs to another collection, whose own reindex target (above) replays it. Only rows whose event is gone from BoltDB entirely are deleted.
- Community stamping (Phase 4) covers both kinds: `stampTargets[30040]` and `stampTargets[30041]` route to `tsDB7`, so publications shared into a community are reachable via NIP-50 `search:"community:X"` like every other content type.
- amb-indexer: kind-30041 sections take the content-direct path (chunk `event.content` directly, no fetch/Tika/setcontent — the section body lives in the signed event). Kind-30040 indices go through the normal fetch path but with an NKBIP-01-specific source URL: `encoding:contentUrl` (the uploaded article file) takes precedence over `source` (a landing-page fallback); DOI resolution is deliberately out of scope. A 30040 with neither tag (DOI-only bibliographic metadata, no fetchable article) is skipped quietly — the cursor still advances, but the event is not dead-lettered, since this is expected for metadata-only entries. Chunk coords are kind-prefixed (`30040:<pubkey>:<d>` / `30041:<pubkey>:<d>`).

**Calendar (NIP-52 kinds 31922/31923/31924/31925), gated behind `CALENDAR_ENABLED`:**
- When `CALENDAR_ENABLED=true`, the relay accepts NIP-52 date events (31922), time events (31923), calendars (31924), and RSVPs (31925). Validation (`validateCalendar`): date/time event kinds (31922/31923) require `d`+`title`+`start`; calendars require `d`+`title`; RSVPs require `d`+`a`+`status` ∈ {accepted,declined,tentative}.
- Calendar events are **dual-indexed**. Structured fields project (`nostrToCalendar`, `calendar.go`) into a SEPARATE Typesense collection (`calendar_31922` / `TS_COLLECTION_CALENDAR`) for full-text/kind/tag search; the nostrlib `khatru/calendar` BoltDB index (`CALENDAR_INDEX_PATH`, default `./data/calendar_index.db`) holds start/end/geohash for range and location queries. A `CalendarStore` wraps the shared BoltDB but is used ONLY for its index + range-query path (never `SaveEvent`/`Close`, since boltBuf persists and boltDB is closed elsewhere).
- The calendar `contentType`'s `fetch` routes per query (`calendarFetch`): a filter carrying range/geo params (`#start_after`, `#start_before`, `#end_after`, `#end_before`, `#g`) over the event kinds (31922/31923) hits the Bolt index; everything else — full-text `search`, plain kind listing, 31924/31925 — hits Typesense. A `search` term combined with a **time range** (and no geohash) is served by Typesense in one query: `start`/`end` are int64 facets, and `buildNostrFilterExpression` (nostrlib `typesense30142/query.go`) maps the four range params to inclusive numeric `filter_by` clauses (`start:>=`/`start:<=`/`end:>=`/`end:<=`, matching the Bolt index's inclusive bounds). Geohash still forces the Bolt path even with a `search` term, because Typesense carries geohash only as an exact facet, not a prefix.
- Reindex appends a `structuredReindexTarget` (in `main.go`) covering all four kinds: one BoltDB pass rebuilds both the Typesense collection and the Bolt index (`reproject` indexes event kinds then upserts to Typesense). The Bolt index is not cleared first, so entries for events no longer in BoltDB persist as harmless orphans.

**Profiles (kind-0 author index), gated behind `PROFILES_ENABLED`:**
- When `PROFILES_ENABLED=true`, the relay indexes the kind-0 profiles of authors who publish content here, so clients can resolve org/person names to pubkeys (e.g. for calendar `authors` filters). Served via NIP-50 `search` over `kinds:[0]`.
- Client kind-0 writes are **rejected** (`validate` returns "kind not accepted"); the index is populated only by the internal `ProfileManager` (`profile_manager.go`), which fetches kind-0 from `PROFILE_RELAYS` for every content author (enqueued on write into a durable BoltDB `profile_queue` bucket, plus a startup backfill scan and a `PROFILE_REFRESH_INTERVAL` refresh). Authors whose kind-0 is not found on `PROFILE_RELAYS` are retried against `PROFILE_FALLBACK_RELAYS` within the same drain, so a profile that lives only on a non-standard relay (e.g. relay.damus.io) is still indexed.
- kind-0 is registered as a read-only `contentType` (`fetch: profilesDB.QueryEvents`) in a SEPARATE Typesense collection (`profiles_0` / `TS_COLLECTION_PROFILES`); `profilesDB.RawEventStore` is nil, so events are reconstructed from the stored `eventRaw`. Profiles are NOT part of the BoltDB→Typesense reindex.
- Each fetched profile's claimed `nip05` is verified at index time (`verifyNIP05`, `profile_manager.go`): the relay resolves `https://<domain>/.well-known/nostr.json` and compares the mapped pubkey. The result is stored as `nip05_verified` (bool) in the profiles collection — **ranking and filtering only, never inclusion**: unverified profiles stay searchable, verified ones rank first at equal text relevance (`SearchSortBy` on the profiles backend), and clients can filter with NIP-50 `search:"<term> nip05_verified:true"`. The flag is relay-side metadata, not part of the signed kind-0 — clients wanting a displayed checkmark still verify client-side. Verification re-runs on every refresh, so a revoked `.well-known` entry clears the flag within `PROFILE_REFRESH_INTERVAL`.
- Content writes kick a debounced drain (10s), so a new author is searchable seconds after their first accepted event instead of at the next refresh; a drain that leaves candidates unresolved (kind-0 not yet propagated to `PROFILE_RELAYS`) retries exactly once after 15m. All drains serialize through one worker goroutine.

**Community shares (NIP-18 kind-16 reposts + legacy kind-30222 targeted publications), gated behind `COMMUNITY_SHARES_ENABLED`:**
- When `COMMUNITY_SHARES_ENABLED=true`, the relay accepts community-share *events*: NIP-18 generic reposts (kind 16, the current edufeed-app mechanism) and legacy Communikey "targeted publications" (kind 30222). Kind 6 (kind-1 reposts) and kind 1985 (labels) are NOT used. Validation (`validateShare`, `shares.go`): both kinds require ≥1 target community AND ≥1 content reference (`e` or `a`); kind 30222 is addressable, so it additionally requires a `d` tag.
- Community targeting is **kind-aware** (`shareCommunities`): the `h` tag targets a community for both kinds; the `p` tag targets a community only for kind 30222 (on kind 16 a `p` is the reposted content's author, never a community). Targets fold into the shared `community` envelope field, so `#h` and NIP-50 `community:<pubkey>` hit shares exactly like every other collection.
- Share fields project (`nostrToShare`, `shares.go`) into a SEPARATE id-keyed Typesense collection (`community_shares` / `TS_COLLECTION_SHARES`) registered as its own `contentType` with backend `tsDB5`; the doc records `refE`/`refA`/`refKind` (the shared content's references) plus the structured envelope. Doc id is `event.ID.Hex()` for kind 16, `GenerateDocumentID(pubkey, d)` for addressable kind 30222. Reindex rebuilds it via a `structuredReindexTarget` (drop+reproject through `nostrToShare`).
- This phase indexes the share *pointers* only. Stamping the referenced content's own doc with the community (so a NIP-50 `search:"community:X"` query over kind 30142/30023/… returns the shared content itself) is Phase 4, below.

**Community registry (kind-10222 + kind-30000), active with `COMMUNITY_SHARES_ENABLED`:** the relay maintains a `CommunityRegistry` (`community_registry.go`, modeled on `AllowlistManager`) that resolves each discovered community's kind-10222 definition (fetched from `COMMUNITY_RELAYS`, refreshed every `COMMUNITY_REFRESH_INTERVAL`) plus any kind-30000 member lists its sections reference. `IsMember(community, pubkey, kind)` is **open by default** — a community is restricted for a kind only when a section covering it names a resolvable member list (open wins on conflict; an unreachable list is owner-only). Communities are auto-discovered by a BoltDB backfill scan of `h`/`p` targets (shares + content); the resolved cache is never pruned on refresh (keep-last-known). This is the membership gate for Phase 4 (member-gated stamping); on its own it changes no query behavior.

**Member-gated stamping (Phase 4), active with `COMMUNITY_SHARES_ENABLED`:** when a community member shares content (kind-16/30222), the relay stamps the *referenced content's own doc* with the community so a NIP-50 `search:"community:X"` query over content kinds (30142/30023/30818/31922/31923) returns the shared content itself. A `CommunityStamper` (`community_stamp.go`) runs one idempotent `reconcile(coord)` per affected content: it re-derives the content's community set from the `community_shares` collection (`refA` facet), gates each via `CommunityRegistry.IsMember(community, shareAuthor, contentKind)`, unions with the content's own `h` tags, and PATCHes the `community` field (`patchDoc`). Referenced content not on the relay is fetched hint-first from `COMMUNITY_RELAYS`, validated, and stored first-class before stamping. Triggers: share write, share deletion (un-stamp — re-derive without the deleted share), content write (re-apply after projection), reindex completion (replay), and a `COMMUNITY_REFRESH_INTERVAL` sweep (retry absent fetches). There is no durable provenance index or pending queue — the shares collection is the source of truth. Stamping is eventually consistent: a PATCH that misses a not-yet-flushed doc is corrected by the next sweep.

> **Query with NIP-50 `community:X`, not a `#h:X` tag filter.** Stamping denormalizes the community into the relay-side `community` index field; it does **not** (and cannot) add an `h` tag to the content event, since that would invalidate the author's signature. The relay maps a `#h:X` REQ to `community:=X` and *does* return stamped content — but whether a client keeps it is client-dependent: an SDK that re-validates REQ results against the raw event's tags (e.g. the `fiatjaf.com/nostr` higher-level subscription path, which logs "filter does not match" and drops it) discards stamped content, because the signed event carries no `h:X` tag; a thin client like `nak` that prints raw relay responses without re-filtering *does* surface it (confirmed on dev 2026-06-23). Because `#h:X` is unreliable across clients, query stamped content via the NIP-50 `search:"community:X"` filter — it carries no tag to re-validate, so it surfaces stamped content on every client. Strict-revalidating clients see only Model-A content (the event's own `h` tag) under `#h:X`; Model-B (stamped) content is reliably reachable only via `community:X`.

**Canonical query shapes (the LLM/MCP contract):** the foundation for a future calendar MCP server that translates natural language into these filters.
- "Events in the next week" (time-based): `{"kinds":[31923],"#start_after":["<now>"],"#start_before":["<now+7d>"],"limit":100}` → Bolt time index. Date-based (31922) is identical; `start` "YYYY-MM-DD" is stored as the Unix start-of-day.
- "Events near a location": `{"kinds":[31922,31923],"#g":["u33d"],"limit":100}` → Bolt geohash prefix (prefix length = radius).
- "Events about a topic": `{"kinds":[31922,31923,31924],"search":"mathematik","limit":50}` → Typesense full-text.
- "Projects/measures about a topic": `{"kinds":[30143,30144],"search":"aktivierung von studierenden im seminar","limit":50}` → transferkiosk collection, BM25⊕vector over name/searchText/content (searchText carries concept labels + narratives).
- "Measures of a project": `{"kinds":[30144],"#partOf":["30143:<pub>:<d>"],"limit":50}` → partOf facet filter.
- "Scientific publications about a topic": `{"kinds":[30040],"search":"künstliche intelligenz hochschullehre","limit":50}` → publications collection, BM25⊕vector over title/summary/searchText/content (content = extracted article PDF text). Academic only: `search:"<topic> type:academic"`. By DOI: `search:"doi:10.1234/abcd.5678"`. Parts of a publication/Reihe: resolve the 30040 doc's `sections` coords, then fetch each addressable event (no reverse facet).
- "Publications of a project": `{"kinds":[30040],"search":"partOf:30143:<pub>:<d>","limit":50}` → `partOf` facet via NIP-50 field syntax (a plain `#partOf` tag filter is skipped by the query builder).
- "Content shared with a community": `{"kinds":[30142,31923],"search":"community:<community-pubkey>","limit":50}` → `community` field exact match. With `COMMUNITY_SHARES_ENABLED` the field is the full set `own-h-tags ∪ member-gated-shared-communities`: a content doc matches a community either because the content itself carries that `h` tag (Model A) OR because a community member shared it via a kind-16/30222 event and Phase-4 stamping denormalized the community onto the doc (Model B, member-gated via `IsMember`). Multi-community content matches any of them. **Use the NIP-50 `search:"community:X"` form, not a `#h:X` tag filter:** the relay resolves both server-side against the `community` field, but Model-B (stamped) content carries no `h` tag on its signed event, so SDK clients that re-validate REQ results against raw tags drop it under `#h:X` (thin clients like `nak` keep it — behavior is client-dependent, so `#h:X` is unreliable). `community:X` surfaces stamped content on every client.
- "What's been shared into a community" (share events): `{"kinds":[16,30222],"#h":["<community-pubkey>"],"limit":50}` → community_shares collection; the community field folds `h` (both kinds) plus `p` (kind 30222 only). NIP-50 equivalent: `search:"community:<community-pubkey>"`. NOTE: this returns the *share events* (pointers). To get the referenced content itself, query the content kinds with `search:"community:<community-pubkey>"` — Phase-4 stamping (above) makes member-shared content match that NIP-50 query.
- Combined intent **topic + time** ("next week, about math") is served server-side in one REQ: `{"kinds":[31923],"#start_after":["<now>"],"#start_before":["<now+7d>"],"search":"mathematik"}` routes to Typesense, which applies the time window as a numeric `filter_by` on `start`/`end` alongside the full-text ranking. Adding **geohash** to that mix still requires client-side composition (geohash-prefix is Bolt-only): issue the Bolt range/geo REQ, then post-filter by topic against the returned events (the full event travels in `eventRaw`).

**Typesense Schema Management:**
- Custom NIP-86 methods (`getcollectionschema`, `updatecollectionschema`, `resetcollectionschema`, `reindex`, `getreindexstatus`) via khatru's `Generic` handler
- Schema config persisted in BoltDB; on startup, custom schema (if stored) overrides the hardcoded default
- Reindex drops and rebuilds every enabled content type's collection from BoltDB events (AMB keeps its content-patch/orphan-GC step; structured types are drop+reproject)

## Ingest pipeline (amb-indexer)

`amb-indexer` runs as a separate service. It is wired into this repo's `docker-compose.yml` and shares the same Typesense instance. Its Typesense collection (`amb_chunks_30142` by default) is disjoint from the relay's event collection.

- Source: [`../amb-indexer`](../amb-indexer) — see its `CLAUDE.md` and `README.md` for the pipeline detail.
- Compose services added alongside the relay: `tika` (Apache Tika for PDF extraction), `embed` (sentence-transformers FastAPI service; built from `./embed`), and `amb-indexer` itself (writes to a named volume `indexer_data`). The embed model cache is persisted via the `embed_model_cache` named volume.
- Auth: the indexer's pubkey (derived from `INDEXER_NSEC`) must be in the relay's `ADMIN_PUBKEYS` so its NIP-86 `setcontent` calls are accepted. Set `ADMIN_PUBKEYS` in this repo's `.env`.
- Configuration for the indexer lives in `../amb-indexer/.env.indexer` (template: `.env.indexer.example`).

**Fulltext content methods (admin-only, NIP-86):**
- `setcontent [event_id, text, fetched_at, status, (source_url)]` — amb-indexer writes extracted fulltext back; updates ContentStore + Typesense `content`/`content_fetched_at`/`content_status` fields.
- `refetchcontent [event_id]` — clears stored content and flags the event in the `needs_refetch` BoltDB bucket so the indexer will re-ingest it.
- `listrefetch []` — returns `{event_ids: [...]}` of events currently flagged for re-ingestion. Survives indexer downtime.
- `acknowledgerefetch [event_ids]` — indexer calls this after re-enqueueing, removing each id from the bucket. Returns `{acknowledged: N}`.

The indexer polls `listrefetch` every `REFETCH_POLL_INTERVAL` seconds, re-fetches each event, and acks the ids it accepted.

## Key Dependencies

- **nostrlib** (`fiatjaf.com/nostr`): Fork of nostr libraries including khatru relay framework and eventstore — hosted at [git.edufeed.org/edufeed/nostrlib](https://git.edufeed.org/edufeed/nostrlib)
- **eventstore/typesense30142**: Typesense wrapper for kind 30142 events (part of nostrlib)
- **eventstore/boltdb**: BoltDB wrapper for raw event persistence (part of nostrlib)

## Local Development with eventstore

The eventstore (`typesense30142`) lives in the nostrlib fork at `../nostrlib/eventstore/typesense30142`.

**How dependencies resolve:**

- **`go.mod`** has `replace fiatjaf.com/nostr => git.edufeed.org/edufeed/nostrlib v0.0.0-...` — this is what Docker and server deployments use (downloads from Gitea).
- **Parent `go.work`** at `edufeed/go.work` overrides the replace and uses `./nostrlib` directly — this is what local `go run .` uses.

```
edufeed/
├── go.work          # workspace: ./amb-relay, ./communikey-relay, ./nostrlib
├── amb-relay/
│   └── go.mod       # replace → git.edufeed.org/edufeed/nostrlib (for Docker/server)
└── nostrlib/
    └── eventstore/
        └── typesense30142/
```

Local changes to the eventstore are immediately available on relay restart (`go run .`). No need to push or update versions during development.

### Updating the nostrlib dependency

After pushing nostrlib changes to git.edufeed.org, update go.mod so Docker builds pick up the new code:

```bash
# Get the new pseudo-version
GOWORK=off go list -m git.edufeed.org/edufeed/nostrlib@latest

# Update the replace directive in go.mod with the new version, then:
GOWORK=off go mod tidy
```

To verify the standalone build works (without local nostrlib):

```bash
GOWORK=off go build .
```

## Environment Variables

Required in `.env` (copy from `.env.example`):
- `NAME`, `PUBKEY`, `DESCRIPTION`, `ICON`: Relay metadata
- `TS_APIKEY`: Typesense API key (default: `xyz` for local dev)
- `TS_HOST`: Typesense URL (`http://localhost:8108` for local dev)
- `TS_COLLECTION`: Collection name for events
- `DB_PATH`: BoltDB file path (default: `./data/relay.db`)
- `ADMIN_PUBKEYS`: Comma-separated hex pubkeys for NIP-86 management API access (in addition to `PUBKEY`)
- `LONGFORM_ENABLED`: Set to `true` to accept kind-30023 (NIP-23 long-form) events and enable the second Typesense collection (default `false`).
- `TS_COLLECTION_LONGFORM`: Collection name for long-form structured fields (default `longform_30023`).
- `WIKI_ENABLED`: Set to `true` to accept kind-30818 (NIP-54 wiki) events and enable a third Typesense collection (default `false`).
- `TS_COLLECTION_WIKI`: Collection name for wiki structured fields (default `wiki_30818`).
- `TRANSFERKIOSK_ENABLED`: Set to `true` to accept kinds 30143/30144 (NIP-DIDACTIC transferkiosk events) and enable a fourth Typesense collection (default `false`). Kind 30145 (Publikation) is retired; see `PUBLICATIONS_ENABLED`.
- `TS_COLLECTION_TRANSFERKIOSK`: Collection name for transferkiosk structured fields (default `transferkiosk`).
- `PUBLICATIONS_ENABLED`: Set to `true` to accept kinds 30040/30041 (NKBIP-01 curated publications) and enable their Typesense collection (default `false`).
- `TS_COLLECTION_PUBLICATIONS`: Collection name for publications (default `publications`).
- `PROFILES_ENABLED`: Set to `true` to index kind-0 profiles of content authors and serve them via NIP-50 `search` over `kinds:[0]` (default `false`).
- `TS_COLLECTION_PROFILES`: Collection name for kind-0 profiles (default `profiles_0`).
- `PROFILE_RELAYS`: Comma-separated source relays the fetcher pulls kind-0 from (default `wss://relay.edufeed.org`).
- `PROFILE_FALLBACK_RELAYS`: Comma-separated fallback relays queried only for authors whose kind-0 was not found on `PROFILE_RELAYS` (default `wss://purplepag.es,wss://relay.damus.io,wss://relay.nostr.band`). Set to empty to disable. Lets profiles that live only on a non-standard relay still be indexed.
- `PROFILE_REFRESH_INTERVAL`: How often to re-fetch known authors' kind-0, as a Go duration (default `6h`).
- `COMMUNITY_SHARES_ENABLED`: Set to `true` to accept community-share events (NIP-18 kind-16 reposts + legacy kind-30222 targeted publications) and enable their Typesense collection (default `false`).
- `TS_COLLECTION_SHARES`: Collection name for community share events (default `community_shares`).
- `COMMUNITY_RELAYS`: Comma-separated relays the community registry fetches kind-10222/kind-30000 from (default `wss://relay.edufeed.org`). Active only when `COMMUNITY_SHARES_ENABLED=true`.
- `COMMUNITY_REFRESH_INTERVAL`: How often to re-resolve known communities, as a Go duration (default `6h`). Active only when `COMMUNITY_SHARES_ENABLED=true`.
- `QUERY_FETCH_TIMEOUT_MS`: Milliseconds a single REQ's search fetch is allowed to run before the relay stops waiting and lets khatru terminate the subscription with EOSE (default `20000`). Hardens against a degraded/lagging Typesense: each backend call already has its own client-side timeout, but a kind-less filter fans out across every registered content type SERIALLY (`registry.fetch`), so on one shared Typesense instance a genuinely slow (not fast-erroring) backend can otherwise stack up to `N × (per-call timeout)` before EOSE ships — several minutes at today's registration count, indistinguishable from a client hang. `boundedSeq` (`query_budget.go`) wraps the composed `relay.QueryStored` result so the REQ handshake always terminates within this budget, regardless of how many content types are registered or how unresponsive any one backend is; a fast clean error (e.g. Typesense's 503 "Not Ready or Lagging") already terminates well under the budget on its own.

Optional chunk re-ranking (chunk-rerank + snippet layer lives in nostrlib `khatru/semantic`): when `CHUNK_RERANK_ENABLED=true` and `INDEXER_API_TOKEN` is set, NIP-50 searches are re-ranked by the best matching fulltext passage from amb-indexer's `POST /search_chunks` (`INDEXER_BASE_URL`, default `http://amb-indexer:8080`). Each `/search_chunks` call is bounded by `CHUNK_RERANK_TIMEOUT_MS` (default `5000`); the budget must clear the indexer's query-embed step (1-2s on a CPU embed service) — too tight a budget cancels legitimate warm searches mid-embed, surfacing as indexer 500s and silent fallback. Falls back to plain Typesense search on any indexer error or empty chunk result, so recall never degrades. Negentropy syncs carry no search field and bypass it naturally. Searches that additionally opt in with `kinds:[30142,21142]` receive an
ephemeral kind-21142 snippet event after each result, carrying the best
matching passage (`content`), `e`/`a`/`k` references to the parent, a
`score` tag, and `page`/`heading`/`source_url` locators when known
(`chunk_snippet.go`). Snippets are relay-signed with `RELAY_SECKEY` (a
dedicated key; per-boot generated when unset), never stored, and never
emitted on fallback paths. Clients that only ask for kind 30142 see exactly
the pre-snippet behavior.

## Testing

### nak CLI

```bash
# Full-text search
nak req --search "mathematik" -k 30142 ws://localhost:3334

# Field-specific search
nak req --search "publisher.name:e-teaching.org" -k 30142 ws://localhost:3334

# Time range filter
nak req --since 1700000000 --until 1800000000 -k 30142 ws://localhost:3334

# Delete an event (NIP-09) — by addressable event reference
nak event -k 5 -t "a=30142:<author-pubkey>:<d-tag>" --sec <your-secret> ws://localhost:3334
```

**Limitation:** `nak` does not support colon-delimited tag names (`#about:id`, `#learningResourceType:id`). For these filters, use a Go client with `nostr.TagMap`:

```go
relay.Subscribe(ctx, nostr.Filters{{
    Kinds: []nostr.Kind{30142},
    Tags: nostr.TagMap{
        "about:id": {"https://w3id.org/kim/schulfaecher/s1017"},
    },
    Limit: 10,
}})
```

### Direct Typesense debugging

```bash
curl -H "X-TYPESENSE-API-KEY: $TS_APIKEY" \
  "$TS_HOST/collections/$TS_COLLECTION/documents/search?q=*&per_page=1"
```

For the full list of supported filters, see the [eventstore README](https://git.edufeed.org/edufeed/nostrlib/src/branch/master/eventstore/typesense30142/README.md).

## MCPs 

* use nostr mcp for resolving nostr addresses or other related nostr questions.
* use other mcps if appropriate

## Skills

* consult skills if needed

