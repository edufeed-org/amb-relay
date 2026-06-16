# Multi-Content Search Platform — Design

**Date:** 2026-06-16
**Status:** Draft for discussion — no code yet
**Scope:** platform-wide (amb-relay, calendar-relay, future content relays, amb-indexer, nostrlib)
**Depends on:** chunk re-ranking (`chunk_rerank.go`), snippet events (`chunk_snippet.go`), the chunk pipeline in `../amb-indexer`

## Problem

amb-relay has grown a valuable search stack: a Typesense structured index, a
separate chunk/embedding index (`amb_chunks_30142`), chunk-level re-ranking of
NIP-50 search, and ephemeral kind-21142 snippet events. All of it is hardwired
to kind 30142 (AMB educational metadata).

We want the same capabilities — full-text + semantic search, passage snippets,
chunked indexing — for **other content types**: NIP-52 calendar events
(31922–31925), NIP-23 long-form articles (30023), NIP-54 wikis (30818), NIP-B0
web bookmarks (39701), and more. The question is not just "is it possible"
(it is) but **how to integrate it sustainably**: one relay or many, rework or
rename, one repo or several, and how the whole thing stays maintainable once
five content types live in the codebase.

## Findings: the landscape we already have

This is the decisive context. The workspace **already runs dedicated relays per
content type**, each its own git repo, Docker deployment, NIP-11 identity, and
operator pubkey:

| Repo | Kinds | Storage / index | LOC |
|---|---|---|---|
| `amb-relay` | 30142 (+5) | Typesense (structured) + BoltDB + chunk index | ~big |
| `calendar-relay` | 31922–31925 (NIP-52) | BoltDB + bleve calendar index | ~590 |
| `communikey-relay` | communikey defs | BoltDB | ~105 |
| `standard-86-relay` | any (generic) | BoltDB + policies | ~600 |

Two structural facts shape everything below:

1. **nostrlib already hosts relay "flavors" as subpackages.** calendar-relay's
   content logic lives in `fiatjaf.com/nostr/khatru/calendar`
   (`NewCalendarRelayWithConfig`, validation, bleve index). The relay repo's
   `main.go` is just *wiring*. Siblings exist: `khatru/blossom`,
   `khatru/grasp`, `khatru/landing`, `khatru/policies`. The eventstore layer
   likewise hosts `eventstore/typesense30142`, `eventstore/boltdb`,
   `eventstore/bleve`. nostrlib also already vendors the NIP packages we'd
   need: `nip52`, `nip23`, `nip54`, `nipb0`.

2. **The relay repos duplicate boilerplate heavily.** The `adminSet` type
   (`isAdmin`/`isAllowed`/`grant`/`revoke`/`isStatic`) is copy-pasted verbatim
   between `amb-relay/main.go:853-949` and `calendar-relay/main.go:298-395`.
   The ban/admin `ManagementStore`, the entire NIP-86 wiring (`BanPubKey`,
   `ListBannedPubKeys`, `GrantAdmin`, …), `getVersion`, `getEnvOrDefault` —
   all duplicated across amb-relay, calendar-relay, and standard-86-relay.
   This is an existing maintainability debt that any expansion will multiply.

3. **Dependency model is polyrepo + pinned shared lib.** Each relay pins a
   nostrlib pseudo-version via `replace`; `bump-nostrlib.sh` rolls every
   consumer forward after a nostrlib push; `go.work` overrides to local
   `./nostrlib` for development. Some relays intentionally pin *older*
   nostrlib because their code lags API changes — independent versioning is a
   feature here, not an accident.

## Goal

Add chunked/semantic/snippet search for new content types **without**:
- multiplying operational toil per content type (N deployments, domains, TLS
  certs, NIP-11 docs, monitoring targets, negentropy syncs),
- forcing clients to connect to N relays to search "all edufeed content,"
- multiplying the copy-pasted boilerplate by five.

And **with** one coherent cross-content semantic search surface, because the
embedding/chunk index is the asset that makes "find anything about
photosynthesis across articles, resources, and wikis" possible.

## Architectural decisions

### Decision 1 — One consolidated relay around a projector registry.

> **This supersedes an earlier draft that recommended a dedicated relay per
> content type.** That recommendation over-weighted the *existing* pattern and
> under-weighted operational scaling: N relays = N deployments, domains, certs,
> NIP-11 docs, monitoring targets, and — worst — a client must connect to and
> merge N relays to search everything. `relaykit` extraction fixes code
> duplication but not this operational multiplication.

A Nostr relay is **kind-agnostic at the protocol level**: one khatru relay can
accept 30142 + 30023 + 30818 + 31922–31925 + 39701 at once, and NIP-01 filters
carry `kinds` so clients ask for exactly what they want. The dedicated-relay
split in the workspace today is an *artifact of storage divergence* (calendar
uses a bleve index, AMB uses Typesense). Once every content type runs through
the Typesense projector model (Decision 4.2) + the shared chunk index
(Decision 2), that divergence disappears and the reason for separate relays
goes with it.

So: **one relay process** (evolve amb-relay into it — see *Branding*), built
around a **kind→projector registry**. The internal architecture of Decisions
2–4 is unchanged; it simply mounts in one process instead of N:

```
one relay (edufeed-relay)
  ├─ kind→projector registry
  │     30142  → AMB projector      → collection amb_30142
  │     30023  → longform projector → collection longform_30023
  │     30818  → wiki projector     → collection wiki_30818
  │     31922… → calendar projector → collection calendar_3192x
  │     39701  → bookmark projector → collection bookmark_39701
  │     (unregistered kinds → catch-all collection)
  ├─ shared chunk index (semantic, cross-content)
  └─ IS the search surface — cross-content search is just a multi-kind query
```

**Adding content type N+1 = register one projector** (schema + `Project()` +
validator + query-map) in **one repo**. No new deploy, domain, cert, or
monitor. That is the scaling property; it is strictly better than
thin-relay-per-type.

**The one real tradeoff — shared blast radius** (one relay down = all content
down; no per-content NIP-11 identity) — is recoverable **without code
changes**: the same binary, driven by a config selecting which kinds it
accepts, deploys as one instance for everything *or* splits into kind-subset
instances later if isolation is ever needed (e.g. a privacy-sensitive calendar
deployment). Consolidate the codebase; keep deployment topology a config knob.
That is more flexible than N repos, not less.

**The principled line for staying separate** is not "one kind per relay" but:
*same operator + cross-search wanted → consolidate; genuinely separate
community/operator with no cross-search need → its own relay.* `communikey-relay`
may stay separate on that test; the edufeed content kinds (AMB, calendar,
longform, wiki, bookmarks) consolidate.

### Decision 2 — Two indexes with opposite topologies.

This is the technical core (carried over from prior discussion, now lifted to
platform scope):

- **Structured index → per-kind Typesense collections.** Schemas diverge hard
  (calendar needs start/end/geohash; long-form needs title/summary/
  published_at; AMB needs its ~40-field vocabulary). One collection = one
  schema, so each kind gets its own (`amb_30142`, `calendar_31923`,
  `longform_30023`, …). Routing is free: the relay reads `filter.Kinds` and
  picks the collection. Multi-kind → Typesense `multi_search` + merge.

- **Chunk/semantic index → ONE shared collection (`chunks`).** The chunk
  document is uniform regardless of parent kind: `{chunk_text, embedding,
  event_coord, parent_kind, page, heading, source_url}`. Keeping it single is
  what makes cross-content semantic ranking work in a *single* vector query.
  Splitting per kind would force merging un-normalized similarity scores
  across collections (Typesense does not normalize relevance across
  collections). Add a `kind` field so a search can optionally scope to content
  types; ranking stays unified.

### Decision 3 — Cross-content search = shared chunk index + a search surface.

With dedicated relays, "search everything" needs an aggregation point. Rather
than fan out across N relays and merge at the client, **one indexer writes all
content types' chunks into the shared `chunks` collection**, and a single
**search surface** queries it:

```
                       one consolidated relay (edufeed-relay)
   accepts 30142 / 30023 / 30818 / 31922… / 39701  via kind→projector registry
        │                  │                      │                    │
   amb_30142        longform_30023          wiki_30818          calendar_3192x   (per-kind structured collections)
        └──────────────────┴──────────────────────┴────────────────────┘
                                        │
   indexer subscribes to all kinds → fetch/extract OR read event.content → chunk → embed
                                        │
                          SHARED chunk index  (Typesense `chunks`, keyed by event_coord + kind)
                                        │
            the SAME relay is the search surface — cross-content semantic search
            + kind-21142 snippets, served by mounting the chunk-rerank layer on
            its QueryStored. No separate aggregation hop.
```

Per-kind structured collections serve structured filtering on their own kind;
the shared chunk index serves cross-content semantic search. A query that mixes
a search term *and* kind-specific structured filters intersects the two (the
current `chunkRerankQuery` already does the lightweight version at
`chunk_rerank.go:118` via `filter.Matches`). Because storage and search live in
one relay, cross-content search is just a multi-kind REQ — not a fan-out.

### Decision 4 — The projector registry is the core abstraction; back it with shared packages.

Inside the consolidated relay, the **kind→projector registry** is what makes
adding a content type a one-file change. Back it with packages that also serve
the genuinely-separate relays (communikey, standard-86) and kill today's
duplication. Following the `khatru/calendar` precedent:

1. **`khatru/relaykit`** — the boilerplate every relay copies today: the
   `adminSet` (static + dynamic admins), the bolt-backed ban/allowlist
   `ManagementStore`, the NIP-86 management wiring, `getVersion`,
   env helpers. The consolidated relay *and* any still-separate relay collapse
   to `relaykit.Mount(relay, cfg)` + their content-specific bits. **Do this
   first** — pure debt reduction, independent of the search work, and it
   de-risks everything after by shrinking every relay to near-pure wiring.

2. **`eventstore/typesense`** — generalize `typesense30142` into a Nostr→
   Typesense projection core with a pluggable per-kind **projector**. This *is*
   the registry from Decision 1:

   ```go
   type Projector interface {
       Kinds()  []nostr.Kind
       Schema() CollectionSchema          // structured fields for this kind
       Project(event nostr.Event) (map[string]any, error)
       QueryMap() map[string]string       // tag/filter → typesense field
   }
   ```

   `typesense30142`'s existing 900-line mapper (`nostr_amb.go`) becomes the
   **first registered projector**, unchanged in behaviour. New kinds register a
   small projector (long-form is ~6 fields vs AMB's ~40). The generic `ext{}`
   nested-object pattern (`types.go:243`, auto-indexed via
   `enable_nested_fields`, `typesense.go:217`) is the template for folding
   kind-specific structured data, and doubles as a **catch-all collection** for
   long-tail kinds nobody wants to hand-craft a schema for.

3. **`khatru/semantic`** — the chunk-rerank query wrapper
   (`chunk_rerank.go`), snippet builder (`chunk_snippet.go`), `ChunkSearcher`
   HTTP client, and embedding client, parameterized by the coordinate
   prefix/parent-kind (today hardcoded `"30142"` at `chunk_snippet.go:28` and
   the `"30142:"` prefix in the indexer's `BuildChunkDocs`). The consolidated
   relay mounts it on `QueryStored` once and every registered kind gets
   cross-content semantic search + snippets.

The consolidated relay keeps a `replace`-pinned nostrlib like every sibling, so
extraction doesn't change its deploy model — only where the code lives.

## Per-content-type integration recipe

There are three content **shapes**, not five content types. The shape
determines the indexer path:

| Shape | Examples | Text source | Indexer work |
|---|---|---|---|
| Metadata → external resource | AMB 30142, web bookmark 39701 | fetch URL → Tika/HTML → chunk | **already built** |
| Content-bearing event | long-form 30023, wiki 30818 | `event.content` (markdown) directly | **simpler** — no fetch, no SSRF, no Tika |
| Structured-short event | calendar 31922/31923 | title+description (tiny) | skip chunking; embed synthesized text, rely on structured fields |

Concretely, adding a content type =
1. **register a projector** in the consolidated relay (schema + `Project()` +
   validator + query-map) — one file, no new repo/deploy/domain;
2. register its kinds in the indexer's `NostrSource.Kinds` (already a
   configurable slice — `nostr_source.go:21`; only pinned to `{30142}` at
   `amb-indexer/main.go:165`) and pick the text-source path by shape;
3. the shared chunk index + `khatru/semantic` give it cross-content search and
   snippets for free.

Long-form and wikis are the cheapest first win: their text is *in the event*,
so they ride the chunk index with no fetch pipeline at all.

## Branding / naming

Consolidation re-opens the rename that an earlier draft argued against.

- **Evolve amb-relay into `edufeed-relay`** (or similar). It already has the
  most machinery — Typesense, chunk index, semantic layer, ACL — so it is the
  natural host for the consolidated relay. **AMB becomes projector #1 among
  several**, not the relay's identity.
- The **projector registry** is the headline abstraction; the relay's brand is
  "edufeed content + search," not any single standard.
- Keep the AMB *standard* (kind 30142, the NIP-AMB naddr) exactly as-is in
  NIP-11 — the relay still speaks AMB, it just also speaks long-form, wiki,
  calendar, bookmarks. Renaming the repo ≠ dropping AMB support.

## Repo structure & long-term maintenance

The relay topology question ("one relay or many") is settled by Decision 1:
**one consolidated relay** for the edufeed content kinds. What remains is the
*code* layout:

**Recommended — one `edufeed-relay` repo + shared nostrlib packages.**
- The consolidated relay is a single repo (evolved amb-relay), a single deploy,
  a single binary. Adding a content type touches only this repo (a projector).
- Shared machinery (`relaykit`, `eventstore/typesense`, `khatru/semantic`)
  lives in nostrlib, consumed by the consolidated relay *and* by any
  genuinely-separate relay (communikey, standard-86) that stays on its own.
- Deployment topology stays a **config knob**, not a code decision: run one
  instance for all kinds, or split into kind-subset instances later for
  isolation — same binary either way.
- This keeps the `bump-nostrlib.sh` + `go.work` model intact; the consolidated
  relay pins nostrlib like every sibling.

**Not recommended — a relay repo per content type.** That is the thing that
doesn't scale (Decision 1): linear growth in deploys/domains/certs/monitoring
and a client fan-out to search everything. Code extraction alone doesn't fix
the operational multiplication.

The one genuinely shared *state* — the `chunks` collection and the indexer that
fills it — lives in **one repo** (extend `amb-indexer`, or rename to a neutral
`content-indexer`), since it's a single deployed service writing a single index
across all content types.

## Phasing

1. **Extract `khatru/relaykit`** and collapse amb-relay + calendar-relay +
   standard-86-relay onto it. Pure debt paydown; no behaviour change; de-risks
   the rest. *(No new content types yet.)*
2. **Generalize the indexer**: configurable coordinate prefix + `event.content`
   text-source + multi-kind subscription, writing the shared `chunks`
   collection. Highest leverage — proves cross-content search end-to-end.
3. **Generalize `eventstore/typesense` with the projector registry** and migrate
   amb-relay's structured collection onto it (AMB as projector #1, byte-for-byte).
   This is the consolidation enabler — the relay now accepts kinds by
   registration. Rename amb-relay → `edufeed-relay` here or in step 6.
4. **Register long-form + wiki projectors** (content-bearing shape) — accepted
   by the now-consolidated relay, searchable via the shared chunk index with
   minimal structured schemas. First real new types; no new repos.
5. **Extract `khatru/semantic`** and mount it once on the consolidated relay's
   `QueryStored` so every registered kind gets cross-content search + snippets.
6. **Register calendar + bookmark projectors** (structured-short and
   metadata→resource shapes); migrate calendar-relay's events in.

Steps 1–2 are independently valuable and low-risk; the consolidation proper
begins at step 3, and everything downstream depends on the shared `chunks`
index (step 2) and the registry (step 3) existing.

## Risks

- **Shared chunk index = shared blast radius.** A bad indexer deploy degrades
  search for *all* content types at once. The existing fail-open design
  (fallback to plain Typesense on any chunk error — `chunk_rerank.go:67-73`)
  contains this, but cross-content makes the index a tier-1 dependency.
- **nostrlib churn.** Pushing more shared code into nostrlib means more
  consumers move in lockstep; `bump-nostrlib.sh` already manages this, but the
  blast radius of a breaking nostrlib change grows. Keep `relaykit`/`semantic`
  APIs narrow and stable.
- **Mixed semantic + structured queries** (e.g. "calendar events about climate
  in Berlin next week") intersect two indexes; the pre-filter-before-vector
  path is the fiddliest bit and should be prototyped in step 2, not deferred.
- **AMB migration.** Folding AMB onto the generic `typesense` projector
  (step 3) touches a production collection; keep AMB byte-for-byte as
  projector #1 and reindex behind the existing `reindex` NIP-86 path.
- **Consolidation migration.** Moving calendar-relay's existing events
  (bleve-indexed) into the consolidated relay (step 6) needs a one-time
  import + reindex into `calendar_3192x`; calendar-relay can run read-only in
  parallel during the bake, mirroring the prod→full-stack migration playbook.

## Open questions

- One indexer for all kinds vs. sharded indexers writing the shared `chunks`
  index? (Throughput/isolation vs. operational simplicity. Lean single until
  throughput forces sharding.)
- Should the consolidated relay ever be deployment-split by kind-subset, and if
  so what triggers it (privacy isolation for calendar/RSVP? blast-radius)? Keep
  it one instance until a concrete need appears — the config knob makes this a
  late, reversible call.
- Does `communikey-relay` join the consolidation or stay separate? Apply the
  Decision 1 test: same operator + cross-search wanted?
- Embedding model: the shared `chunks` index pins one model
  (`paraphrase-multilingual-MiniLM-L12-v2`, 384-dim). Cross-content is fine on
  one model; revisit only if a content type needs a domain-specific embedder
  (which would force a separate chunk collection — the one justified split).
