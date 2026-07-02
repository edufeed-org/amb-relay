# Relay Content Visualization — Design

**Date:** 2026-07-02
**Status:** Approved (design), pending implementation plan
**Feature flag:** `VIZ_ENABLED` (default `false`)

## Goal

Give a single web page that visualizes the content of the AMB relay for a
**stakeholder / demo** audience: a "wow" overview that shows the corpus has
*meaningful, connected* content. Two intertwined jobs:

1. **Corpus overview** — how much content, of what types, on what subjects, by
   whom.
2. **Relationships** — a force-directed graph connecting communities, authors,
   and the transferkiosk project hierarchy.

Non-goals: public browsing/search UI, per-resource detail pages, editing,
authentication, calendar map/timeline. YAGNI — this is a read-only demo view.

## Audience & scale assumptions

- Audience: stakeholders/funders/partners in a guided demo. Optimize for an
  impressive, legible overview over deep drill-down.
- Scale: **medium (~500–5000 addressable events)**. The graph therefore renders
  **entities as nodes, content as weight** — never one dot per resource.
  Individual resources appear only in the click-through drill-down panel.

## Architecture (Approach A — relay-embedded live page)

Add a small, self-contained module to the existing Go relay. No separate service,
no separate deploy. The relay already serves HTTP via khatru and already holds
Typesense connection details in-process.

```
Browser ──GET /viz──────────────► embedded static page (HTML/JS/CSS via //go:embed)
        ──GET /viz/stats────────► JSON: corpus counts + facets      ┐
        ──GET /viz/graph────────► JSON: aggregated nodes + edges    ├─ viz.go / viz_graph.go
        ──GET /viz/node/{t}/{id}► JSON: capped list of real events  ┘
                                        │
                                        ▼
                             Typesense REST API (facet_by / search)
                             reusing TSBackend.Host + TSBackend.ApiKey
```

### Integration points (verified)

- **Routing:** khatru exposes `relay.Router() *http.ServeMux`. Register the
  `/viz*` handlers on it; non-websocket/non-NIP-11/non-NIP-86 requests fall
  through to this mux. No server wrapping needed.
- **Typesense access:** the eventstore's `TSBackend` struct exposes `Host`,
  `ApiKey`, and `CollectionName` (all exported) and talks to Typesense over its
  plain REST API (no SDK). The viz module reuses those fields to issue
  `GET /collections/<name>/documents/search?q=*&facet_by=...` calls. No new
  dependency, no SDK.

### New files

- `viz.go` — HTTP handlers, in-memory cache, Typesense query helpers.
- `viz_graph.go` — graph aggregation (facets/docs → nodes + edges).
- `web/viz/index.html`, `web/viz/app.js`, `web/viz/style.css` — frontend.
- `web/viz/force-graph.min.js` — vendored graph lib (see Frontend).
- `viz_test.go`, `viz_graph_test.go` — tests.

## Endpoints

All endpoints are read-only, unauthenticated, and gated behind `VIZ_ENABLED`.
When the flag is off, no routes are registered.

### `GET /viz`

Serves the embedded `index.html` (and sibling assets under `/viz/`). Assets are
baked into the binary with `//go:embed web/viz`.

### `GET /viz/stats`

Per-collection document counts plus one `facet_by` search (`q=*`, `per_page=0`)
on the AMB collection for the subject breakdown. Response:

```json
{
  "totals": { "resources": 2314, "content_types": 7, "authors": 41,
              "communities": 6, "projects": 18 },
  "by_type": [ { "kind": 30142, "label": "AMB resource", "count": 1830 },
               { "kind": 30023, "label": "Long-form",    "count": 210 }, ... ],
  "subjects": [ { "id": "...s1017", "label": "Chemie", "count": 240 }, ... ]
}
```

- `totals.resources` = sum of content-collection doc counts;
  `content_types` = number of enabled collections with ≥1 doc;
  `authors`/`communities`/`projects` = distinct counts derived from the graph
  aggregation (`publisher` facet cardinality, community set size, 30143 count).
- `by_type` = per-kind doc counts (feeds the stat strip / a compact type row).
- `subjects` = `about` facet on the AMB (30142) collection — the one facet the
  "corpus overview" goal needs. Rendered as a compact subjects breakdown
  (see Open questions).

Scope note (YAGNI): resource-type / educational-level / publisher facet arrays
are **not** computed in the MVP. They're cheap to add later (one more
`facet_by`) if a richer overview view is built, but nothing renders them now.

### `GET /viz/graph`

Builds the aggregated entity graph. Response:

```json
{
  "nodes": [ { "id": "community:<pubkey>", "type": "community",
               "label": "Chemie-NW", "weight": 340 },
             { "id": "author:<pubkey>", "type": "author",
               "label": "e-teaching.org", "weight": 128 },
             { "id": "tk:30143:<pub>:<d>", "type": "project",
               "label": "OER-Transfer", "weight": 12 }, ... ],
  "edges": [ { "source": "author:<pubkey>", "target": "community:<pubkey>",
               "weight": 24, "kind": "author_community" },
             { "source": "tk:30144:...", "target": "tk:30143:...",
               "weight": 1, "kind": "part_of" }, ... ]
}
```

Node types & encoding:

| type          | source                                   | color  | size (weight)             |
|---------------|------------------------------------------|--------|---------------------------|
| `community`   | `community_shares` + `community` facet   | blue   | distinct content count    |
| `author`      | `publisher`/pubkey facet across content  | orange | resource count            |
| `project`     | 30143 docs                               | green  | # measures+pubs under it  |
| `measure`     | 30144 docs                               | teal   | 1                         |
| `publication` | 30145 docs                               | purple | 1                         |

Edge kinds:

- `author_community` — author has ≥1 piece of content in that community;
  weight = count.
- `part_of` — transferkiosk `partOf` (measure→project, publication→project or
  publication→measure per the `a`-tag marker).
- `author_project` — a project's author node → the project node.
- `community_project` — a project shared into a community → that community.

**Aggregation rules (medium scale):**

- Authors: keep top `VIZ_TOP_AUTHORS` (default 40) by resource count; fold the
  remainder into a single `author:others` node (weight = summed count, no
  edges). Prevents a hairball while preserving the "many contributors" signal.
- Communities: all kept (handful).
- Transferkiosk: all 30143/30144/30145 kept (medium scale → a few hundred docs
  max); the full project→measure→publication trees are the demo centerpiece.
- Content (30142/30023/30818/…): **never** emitted as nodes. Represented only as
  node weight and edge thickness.

### `GET /viz/node/{type}/{id}`

Live drill-down for the panel. Returns a capped (≤50) list of the real events
behind a node, via a targeted Typesense search:

- `author:<pubkey>` → content where `publisher`/pubkey matches.
- `community:<pubkey>` → content stamped/shared with that community
  (`community:<pubkey>` NIP-50-style filter).
- `project|measure|publication:<coord>` → the doc itself + its `partOf` children.

```json
{ "node": { "id": "author:<pubkey>", "label": "e-teaching.org",
            "type": "author", "total": 128 },
  "items": [ { "title": "Lerntheorien im Überblick", "kind": 30142,
               "community": "Chemie-NW", "naddr": "naddr1..." }, ... ],
  "truncated": true }
```

Only fields already public over Nostr are returned.

## Frontend (Layout A — Hero graph)

Single page, the force-directed graph as the hero. Structure:

- **Top:** stat strip — resources, content types, authors, communities,
  projects (from `/viz/stats` `totals`).
- **Center:** the force graph (fills the viewport).
- **Bottom-left:** legend (node-type colors + "thicker line = more shared
  content").
- **Top-right or floating:** **layer toggles** — checkboxes to show/hide
  communities, authors, transferkiosk trees independently (approved).
- **Right (on click):** drill-down panel sliding in with the node's real
  resources from `/viz/node/...`.

Interaction & encoding (approved in mockup):

- Node **color = type**, **size = content weight**, **edge thickness = shared
  count**.
- **Always-on labels** (approved). Layer toggles keep the visible node count low
  enough that permanent labels stay legible.
- Hover highlights a node's neighborhood; click opens the drill-down panel.

**Library:** vendor `force-graph` (vasturiano, MIT) — a single canvas-based file
committed under `web/viz/` and embedded. Canvas handles hundreds of nodes with
labels smoothly. No CDN dependency (works offline / in a locked-down demo
network), no build step. Plain JS orchestrates fetch → render → toggle → panel.

## Cross-cutting concerns

### Security / exposure

- Endpoints are read-only and unauthenticated but expose only (a) aggregated
  counts and (b) capped lists of events the relay already serves publicly over
  Nostr. No admin key, no BoltDB internals, no NIP-86 surface.
- `VIZ_ENABLED=false` by default. For the demo, route `/viz` through Traefik
  like the relay's other public paths.

### Caching / performance

- `/viz/stats` and `/viz/graph` compute once and cache in-memory with TTL
  `VIZ_CACHE_TTL` (default 60s); a page load never re-hits Typesense within the
  window.
- `/viz/node/...` is live per click (cheap, capped at 50).
- Transferkiosk pull is a few hundred docs — trivial.

### Configuration (new env vars)

| var              | default | meaning                                     |
|------------------|---------|---------------------------------------------|
| `VIZ_ENABLED`    | `false` | register `/viz*` routes + serve the page    |
| `VIZ_TOP_AUTHORS`| `40`    | top-N authors before folding into "others"  |
| `VIZ_CACHE_TTL`  | `60s`   | in-memory cache TTL for stats + graph        |

Documented in `.env.example` and `CLAUDE.md`.

### Testing

- `viz_graph_test.go` — table-driven: synthetic facet/doc inputs → expected
  nodes/edges, covering the top-N author fold, the "others" node, and `partOf`
  tree wiring (measure→project, publication→project/measure).
- `viz_test.go` — handler tests asserting JSON shape and the `VIZ_ENABLED` gate
  (routes absent when off).
- Frontend verified manually in a browser (golden path + a node with no
  relationships); no JS test harness added.

## Build sequence (for the plan)

1. `viz_graph.go` aggregator + tests (pure, no HTTP) — the core logic.
2. `viz.go` Typesense query helpers + cache + handlers + tests.
3. Wire routes behind `VIZ_ENABLED` in `main.go`; add env vars.
4. `web/viz` frontend (vendored `force-graph`, stat strip, graph, legend,
   toggles, drill-down panel).
5. Docs: `.env.example`, `CLAUDE.md`.

## Open questions

1. **How much "corpus overview" in Layout A?** You asked for both overview (1)
   and relationships (2), then chose the hero-graph layout, whose overview is
   the stat strip. The spec adds a **compact subjects breakdown** (a small
   horizontal bar strip below the legend or in a corner) so view 1 is more than
   raw totals, without competing with the hero graph. If you'd rather keep the
   page graph-only and drop the subjects element, we remove it and the
   `subjects` facet with it. **Default: keep the compact subjects strip.**

Deferred (not blocking): richer overview charts — treemap / type bars /
activity timeline (Layouts B/C) — remain a future second view; the endpoints are
shaped so they can be added without rework.
