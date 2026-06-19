# Community Sharing Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let relay clients query for content (learning resources, calendar events, long-form, wiki) that is *shared with a community* — both directly (`h`-tagged content) and indirectly (member-published reposts/labels), per the Communikey/Nostr-Community spec.

**Architecture:** "Shared with a community" has two mechanisms. (A) **Direct targeting:** a content event carries `["h", "<community-pubkey>"]`; we fold that into a queryable `community` field on every collection. (B) **Indirect / share events:** a community member publishes a kind 6/16 repost or kind 1985 label that carries `h` + a reference to content they didn't author. We accept those share events into a new id-keyed collection, resolve their referenced content (fetching it if absent), verify the sharer is a member of the target community's Activity list, and **denormalize** the community onto the referenced content's doc via a durable provenance index — so both mechanisms answer with the *same* single `#h` / NIP-50 query.

**Tech Stack:** Go, khatru relay framework, Typesense (search), BoltDB (raw events + indexes), `fiatjaf.com/nostr` (nostrlib fork). Cross-repo: the shared eventstore lives in `../nostrlib/eventstore/typesense30142`.

## Global Constraints

- The `community` field is a Typesense `string[]` (multi-targeting is allowed by spec) — verbatim field name `community` on every collection.
- A query maps the NIP-01 tag filter `#h` **and** the NIP-50 search token `community:<pubkey>` onto that one field. No new tag name is invented.
- Cross-repo changes to `../nostrlib/eventstore/typesense30142` are picked up locally via the parent `go.work`; for Docker/server they require pushing nostrlib + bumping the `replace` pseudo-version in `go.mod` (see CLAUDE.md "Updating the nostrlib dependency"). Each phase that touches nostrlib ends with that bump.
- Member-gated trust (Phase 4): an indirect share only stamps `community:X` when the share event's author is in community X's Activity-section kind-30000 list. Direct `h` on the content itself is always trusted (it's the author's own targeting).
- Community discovery is **auto-discovery from `h` tags**: any `h` value seen on an accepted content/share event becomes a community candidate (durable, dedup'd queue). Safe because writes are already allowlist-gated, so `h` values come only from trusted authors; an unresolvable 10222 simply never gates/stamps.
- Community data fetch is **hint-first, configured-fallback**: fetch a community's kind-10222 from its `r` relay hints (and the Activity `a` tag's relay hint for the kind-30000 list), falling back to a configured relay set (reuse `PROFILE_RELAYS`-style env) on miss.
- Everything ships behind existing/new feature gates; with gates off, behavior is byte-for-byte unchanged.

---

## Roadmap (four independently shippable phases)

| Phase | Deliverable | Gated by | Blocks |
|-------|-------------|----------|--------|
| **1** | `community` field: direct `h` targeting queryable across all collections (Model A) | always-on (additive field) | — |
| **2** | Share-event ingestion: accept/validate/index kinds 6/16/1985 in a new id-keyed collection | `COMMUNITY_SHARES_ENABLED` | needs P1's field for stamping in P4 |
| **3** | Community registry: auto-discover communities, fetch 10222 + resolve Activity kind-30000 membership | `COMMUNITY_SHARES_ENABLED` | P4 gating |
| **4** | Provenance index + member-gated denormalization (Model B): stamp `community[]` from member shares, un-stamp on delete, replay on reindex | `COMMUNITY_SHARES_ENABLED` | — |

**This document contains the full executable task breakdown for Phase 1.** Phases 2–4 are captured as design specs at the end (resolved architecture, file map, interfaces) — each becomes its own full plan via the writing-plans skill before execution, once Phase 1 is merged.

---

# Phase 1 — The `community` field (Model A)

**Outcome:** A 30142 / 30023 / 30818 / 31922–31925 event carrying `["h", "<pubkey>"]` becomes findable by `{"#h":["<pubkey>"]}` and by NIP-50 `search:"community:<pubkey>"`, on its collection. Reindex repopulates existing events (the projection now emits the field; no reindex code change).

**File map:**
- `../nostrlib/eventstore/typesense30142/nostr_amb.go` — add `handleHTags`, call it in `NostrToAMB`.
- `../nostrlib/eventstore/typesense30142/types.go` — add `Community []string` to `AMBMetadata`.
- `../nostrlib/eventstore/typesense30142/typesense.go` — add `community` field to `DefaultSchema()`.
- `../nostrlib/eventstore/typesense30142/query.go` — map `h` → `community` in `buildNostrFilterExpression`.
- `structured.go` (amb-relay) — add `Community []string` to `structuredEnvelope`, populate in `newStructuredEnvelope`, add field in `structuredEnvelopeFields`.
- Tests alongside each.

---

### Task 1: AMB projection folds `h` tags into `community`

**Files:**
- Modify: `../nostrlib/eventstore/typesense30142/types.go` (AMBMetadata struct, near line 247)
- Modify: `../nostrlib/eventstore/typesense30142/nostr_amb.go` (add `handleHTags` near `handleTTags` line 128; call it near line 583)
- Test: `../nostrlib/eventstore/typesense30142/nostr_amb_test.go`

**Interfaces:**
- Produces: `func handleHTags(tags nostr.Tags) []string`; `AMBMetadata.Community []string` with JSON tag `community,omitempty`.

- [ ] **Step 1: Write the failing test**

In `nostr_amb_test.go`:

```go
func TestNostrToAMB_Community(t *testing.T) {
	assert := assert.New(t)

	tags := nostr.Tags{
		{"d", "test-resource-id"},
		{"h", "660d8c78651f70487ec9b8ddc283e29cf2561693dda3ba246d3fd3c08dbb7083"},
		{"h", "abababababababababababababababababababababababababababababababab"},
	}
	event := createTestEvent(tags)

	amb, err := NostrToAMB(event)

	assert.NoError(err)
	assert.NotNil(amb)
	assert.Equal(2, len(amb.Community))
	assert.Contains(amb.Community, "660d8c78651f70487ec9b8ddc283e29cf2561693dda3ba246d3fd3c08dbb7083")
	assert.Contains(amb.Community, "abababababababababababababababababababababababababababababababab")
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd ../nostrlib/eventstore/typesense30142 && go test -run TestNostrToAMB_Community ./...`
Expected: FAIL — `amb.Community undefined` (compile error).

- [ ] **Step 3: Add the struct field**

In `types.go`, after the `NostrATags` line (247), inside `AMBMetadata`:

```go
	// Community targeting (Communikey/Nostr-Community 'h' tags). Each value is
	// a community pubkey the content is shared with. Queryable via #h and the
	// NIP-50 `community:<pubkey>` token.
	Community []string `json:"community,omitempty"`
```

- [ ] **Step 4: Add the extractor and call it**

In `nostr_amb.go`, after `handleTTags` (line ~137):

```go
// handleHTags extracts community pubkeys from Nostr-native 'h' tags
// (Communikey community targeting).
func handleHTags(tags nostr.Tags) []string {
	var communities []string
	for _, tag := range tags {
		if len(tag) >= 2 && tag[0] == "h" {
			communities = append(communities, tag[1])
		}
	}
	return communities
}
```

Then in `NostrToAMB`, right after `amb.Keywords = handleTTags(event.Tags)` (line ~583):

```go
	// Handle 'h' tags (community targeting)
	amb.Community = handleHTags(event.Tags)
```

- [ ] **Step 5: Run test to verify it passes**

Run: `cd ../nostrlib/eventstore/typesense30142 && go test -run TestNostrToAMB_Community ./...`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git -C ../nostrlib add eventstore/typesense30142/types.go eventstore/typesense30142/nostr_amb.go eventstore/typesense30142/nostr_amb_test.go
git -C ../nostrlib commit -m "feat(typesense30142): project 'h' community tags into AMB document"
```

---

### Task 2: `community` field in the AMB default schema

**Files:**
- Modify: `../nostrlib/eventstore/typesense30142/typesense.go` (`DefaultSchema`, near line 221)
- Test: `../nostrlib/eventstore/typesense30142/typesense_test.go` (create if absent)

**Interfaces:**
- Produces: `DefaultSchema()` includes a `community` `string[]` optional field.

- [ ] **Step 1: Write the failing test**

In `typesense_test.go` (create if it doesn't exist; package `typesense30142`):

```go
package typesense30142

import "testing"

func TestDefaultSchemaHasCommunityField(t *testing.T) {
	schema := DefaultSchema()
	for _, f := range schema.Fields {
		if f.Name == "community" {
			if f.Type != "string[]" {
				t.Fatalf("community field type = %q, want string[]", f.Type)
			}
			if !f.Optional {
				t.Fatalf("community field must be Optional")
			}
			return
		}
	}
	t.Fatal("community field missing from DefaultSchema")
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd ../nostrlib/eventstore/typesense30142 && go test -run TestDefaultSchemaHasCommunityField ./...`
Expected: FAIL — "community field missing from DefaultSchema".

- [ ] **Step 3: Add the schema field**

In `typesense.go`, in `DefaultSchema()`, right after the `nostr_a` field (line ~221):

```go
			// Community targeting ('h' tags). string[] so multi-community
			// targeting and the NIP-50 `community:<pubkey>` token both work.
			{Name: "community", Type: "string[]", Optional: true, Facet: true},
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd ../nostrlib/eventstore/typesense30142 && go test -run TestDefaultSchemaHasCommunityField ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git -C ../nostrlib add eventstore/typesense30142/typesense.go eventstore/typesense30142/typesense_test.go
git -C ../nostrlib commit -m "feat(typesense30142): add community field to AMB default schema"
```

---

### Task 3: Map `#h` and `community:` search onto the field

**Files:**
- Modify: `../nostrlib/eventstore/typesense30142/query.go` (`buildNostrFilterExpression`, switch near line 96)
- Test: `../nostrlib/eventstore/typesense30142/query_test.go`

**Interfaces:**
- Consumes: `buildNostrFilterExpression(filter nostr.Filter) string` (existing).
- Produces: `#h` tag filter emits `community:=\`<pubkey>\``. The NIP-50 `community:<pubkey>` token already maps via the generic `field:value` path (`BuildTypesenseQuery`), so no separate change is needed for search.

- [ ] **Step 1: Write the failing test**

In `query_test.go`:

```go
func TestBuildNostrFilterExpression_HTag(t *testing.T) {
	filter := nostr.Filter{
		Tags: nostr.TagMap{
			"h": {"660d8c78651f70487ec9b8ddc283e29cf2561693dda3ba246d3fd3c08dbb7083"},
		},
	}
	result := buildNostrFilterExpression(filter)
	expected := "community:=`660d8c78651f70487ec9b8ddc283e29cf2561693dda3ba246d3fd3c08dbb7083`"
	if result != expected {
		t.Errorf("Expected %q, got %q", expected, result)
	}
}

func TestBuildNostrFilterExpression_MultipleHTags(t *testing.T) {
	filter := nostr.Filter{
		Tags: nostr.TagMap{
			"h": {"aaaa", "bbbb"},
		},
	}
	result := buildNostrFilterExpression(filter)
	expected := "(community:=`aaaa` || community:=`bbbb`)"
	if result != expected {
		t.Errorf("Expected %q, got %q", expected, result)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd ../nostrlib/eventstore/typesense30142 && go test -run 'TestBuildNostrFilterExpression_HTag|TestBuildNostrFilterExpression_MultipleHTags' ./...`
Expected: FAIL — both return `""` because `h` currently hits the `default: continue` branch.

- [ ] **Step 3: Add the case**

In `query.go`, in the `switch` inside the tag loop (after `case tagName == "a":` line ~104):

```go
		case tagName == "h":
			tsField = "community"
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd ../nostrlib/eventstore/typesense30142 && go test -run 'TestBuildNostrFilterExpression_HTag|TestBuildNostrFilterExpression_MultipleHTags' ./...`
Expected: PASS.

- [ ] **Step 5: Run the full eventstore suite**

Run: `cd ../nostrlib/eventstore/typesense30142 && go test ./...`
Expected: PASS (integration tests that need a live Typesense are skipped/fail-fast as in the existing baseline — confirm no *new* failures vs. a clean checkout).

- [ ] **Step 6: Commit**

```bash
git -C ../nostrlib add eventstore/typesense30142/query.go eventstore/typesense30142/query_test.go
git -C ../nostrlib commit -m "feat(typesense30142): map #h tag filter onto community field"
```

---

### Task 4: Structured collections (calendar/longform/wiki) carry `community`

The structured collections store only the envelope + projected fields; arbitrary tags aren't kept. Folding `h` into the shared `structuredEnvelope` covers all three types in one place.

**Files:**
- Modify: `structured.go` (`structuredEnvelope` struct line 18; `newStructuredEnvelope` line 27; `structuredEnvelopeFields` line 43)
- Test: `structured_test.go`

**Interfaces:**
- Consumes: `newStructuredEnvelope(event *nostr.Event) (structuredEnvelope, error)` (existing).
- Produces: `structuredEnvelope.Community []string` (JSON `community,omitempty`); `structuredEnvelopeFields()` includes a `community` `string[]` field.

- [ ] **Step 1: Write the failing test**

In `structured_test.go`:

```go
func TestNewStructuredEnvelope_Community(t *testing.T) {
	evt := nostr.Event{
		Kind:    30023,
		Content: "body",
		Tags: nostr.Tags{
			{"d", "x"},
			{"h", "660d8c78651f70487ec9b8ddc283e29cf2561693dda3ba246d3fd3c08dbb7083"},
		},
	}
	env, err := newStructuredEnvelope(&evt)
	if err != nil {
		t.Fatalf("newStructuredEnvelope: %v", err)
	}
	if len(env.Community) != 1 || env.Community[0] != "660d8c78651f70487ec9b8ddc283e29cf2561693dda3ba246d3fd3c08dbb7083" {
		t.Errorf("Community = %v", env.Community)
	}
}

func TestStructuredEnvelopeFields_Community(t *testing.T) {
	found := false
	for _, f := range structuredEnvelopeFields() {
		if f.Name == "community" {
			found = true
			if f.Type != "string[]" || !f.Optional {
				t.Errorf("community field = %+v", f)
			}
		}
	}
	if !found {
		t.Fatal("community field missing from structuredEnvelopeFields")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -run 'TestNewStructuredEnvelope_Community|TestStructuredEnvelopeFields_Community' .`
Expected: FAIL — `env.Community undefined` (compile error).

- [ ] **Step 3: Add the field, populate it, declare the schema field**

In `structured.go`, add to the `structuredEnvelope` struct (after `EventRaw` line 23):

```go
	Community      []string `json:"community,omitempty"`
```

In `newStructuredEnvelope`, before the `return`, build the slice and set it:

```go
	var community []string
	for _, tag := range event.Tags {
		if len(tag) >= 2 && tag[0] == "h" {
			community = append(community, tag[1])
		}
	}
```

and add `Community: community,` to the returned `structuredEnvelope{...}` literal.

In `structuredEnvelopeFields`, add to the returned slice:

```go
		{Name: "community", Type: "string[]", Facet: true, Optional: true},
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test -run 'TestNewStructuredEnvelope_Community|TestStructuredEnvelopeFields_Community' .`
Expected: PASS.

- [ ] **Step 5: Run the relay unit suite**

Run: `go test .`
Expected: PASS (no new failures vs. baseline; integration tests needing Typesense behave as before).

- [ ] **Step 6: Commit**

```bash
git add structured.go structured_test.go
git commit -m "feat(structured): carry community 'h' tags on longform/wiki/calendar docs"
```

---

### Task 5: Bump the nostrlib dependency so Docker/server builds get the field

**Files:**
- Modify: `go.mod` (the `replace fiatjaf.com/nostr => git.edufeed.org/edufeed/nostrlib v0.0.0-...` line)

- [ ] **Step 1: Push nostrlib**

```bash
git -C ../nostrlib push
```

- [ ] **Step 2: Get the new pseudo-version**

Run: `GOWORK=off go list -m git.edufeed.org/edufeed/nostrlib@latest`
Expected: prints a new `v0.0.0-<timestamp>-<hash>` string.

- [ ] **Step 3: Update the replace directive**

Edit `go.mod`, replace the nostrlib pseudo-version with the value from Step 2, then:

Run: `GOWORK=off go mod tidy`

- [ ] **Step 4: Verify the standalone build**

Run: `GOWORK=off go build .`
Expected: builds with no error.

- [ ] **Step 5: Commit**

```bash
git add go.mod go.sum
git commit -m "chore: bump nostrlib for community field"
```

---

### Task 6: Manual end-to-end verification + docs

**Files:**
- Modify: `CLAUDE.md` (document the `community`/`#h` query in the canonical query-shapes section)

- [ ] **Step 1: Bring up the stack and reindex**

```bash
docker compose up -d typesense
go run .
```
In another shell, publish a 30142 with an `h` tag (replace `<sec>`/community pubkey):

```bash
nak event -k 30142 -d test-comm -t "h=660d8c78651f70487ec9b8ddc283e29cf2561693dda3ba246d3fd3c08dbb7083" -t "title=t" ws://localhost:3334 --sec <sec>
```

- [ ] **Step 2: Query by `#h` via a Go client (nak can't do bare `#h` reliably — use the documented TagMap client snippet from CLAUDE.md), and by NIP-50:**

```bash
nak req --search "community:660d8c78651f70487ec9b8ddc283e29cf2561693dda3ba246d3fd3c08dbb7083" -k 30142 ws://localhost:3334
```
Expected: returns the event published in Step 1.

- [ ] **Step 3: Confirm Typesense holds the field**

```bash
curl -H "X-TYPESENSE-API-KEY: $TS_APIKEY" \
  "$TS_HOST/collections/$TS_COLLECTION/documents/search?q=*&filter_by=community:=660d8c78651f70487ec9b8ddc283e29cf2561693dda3ba246d3fd3c08dbb7083&per_page=1"
```
Expected: `found >= 1`.

- [ ] **Step 4: Document the query shape**

In `CLAUDE.md`, add to the canonical query-shapes block:

```
- "Content shared with a community": `{"kinds":[30142,31923],"#h":["<community-pubkey>"],"limit":50}` → community field exact match. NIP-50 equivalent: `search:"community:<community-pubkey>"`.
```

- [ ] **Step 5: Commit**

```bash
git add CLAUDE.md
git commit -m "docs: community 'h' query shape"
```

---

## Phase 1 Self-Review

- **Spec coverage:** direct `h` targeting queryable on AMB (Tasks 1–3) and on all structured types (Task 4); NIP-50 token works via the existing `field:value` path (Task 3 note); existing events covered by running reindex (projection now emits the field). ✓
- **Type consistency:** `community` / `[]string` / `string[]` used identically in AMBMetadata, structuredEnvelope, both schemas, and the query mapping. ✓
- **Placeholder scan:** every code step shows real code and a runnable command. ✓

---

# Phase 2 — Share-event ingestion (design spec → own plan)

**Goal:** Accept, validate, store, and index kind 6 (repost), 16 (generic repost), 1985 (label) events that carry an `h` (community) tag, in a new id-keyed Typesense collection, so `{"kinds":[6,16,1985],"#h":["X"]}` lists what's been shared into community X and exposes the referenced content coords.

**Why a separate collection:** these are *regular* (non-addressable) events — no `d` tag, so they don't fit the `GenerateDocumentID(pubkey, d)` addressable pattern. Doc id = event id. BoltDB already stores raw events (like kind 5); the new collection is search/index only.

**File map:**
- `shares.go` (new, amb-relay) — `ShareDocument`, `nostrToShare`, `validateShare`, `storeShare`, `shareSchema`. Reuses `structured.go` upsert helpers.
- `main.go` — new `tsDB5` backend gated by `COMMUNITY_SHARES_ENABLED`; register a `contentType{kinds:[6,16,1985], chunked:false, ...}`.
- `content_registry.go` — no change (registry already kind-driven).
- `.env.example`, `CLAUDE.md`, `reindex.go` (append a structured reindex target).

**Document shape (`ShareDocument`):** id (=event id), `community []string` (from `h`), `refE []string` (e-tag ids), `refA []string` (a-tag coords), `refR []string` (r-tag urls), `refKind int` (from `k` on reposts), `labelNamespace`/`labelValue` (from `L`/`l`), plus `structuredEnvelope` (carries raw event for reconstruction; **note** envelope's `community` already folds `h` — reuse it rather than a second field).

**Validation (`validateShare`):** require `h` (≥1 community) AND at least one reference tag (`e`/`a`/`r`). Reposts (6/16) should carry `e` or `a`; labels (1985) need `e`/`a`/`r`. Reject otherwise.

**Interfaces produced (consumed by Phase 4):**
- `nostrToShare(*nostr.Event) (*ShareDocument, error)`
- A way to enumerate stored share events by community for reindex/replay (BoltDB query by kind + in-memory `h` filter, mirroring `backfillAuthors`).

**Open sub-decisions to resolve in this phase's plan:** bare-`e` handling (no kind known) — store the ref but mark `refKind=0`; Phase 4 decides whether to resolve it.

---

# Phase 3 — Community registry (design spec → own plan)

**Goal:** Maintain, per community pubkey, the set of member pubkeys allowed to publish into its **Activity** section — the gate Phase 4 gates stamping on.

**Architecture:** A `CommunityRegistry` modeled on **`AllowlistManager`** (`allowlist.go`) + **`ProfileManager`** (`profile_manager.go`):
- **Discovery (auto):** every accepted content/share event's `h` values are enqueued as community candidates into a durable BoltDB bucket (`community_queue`), mirroring `EnqueueProfileCandidate`. Dedup'd; hooked in `StoreEvent`/`ReplaceEvent` next to `profileMgr.Enqueue`.
- **Resolution:** for each candidate, fetch its kind-10222 (hint-first: 10222 `r` tags; fallback: configured `COMMUNITY_RELAYS`, default = `PROFILE_RELAYS`). Parse the **Activity** section: find the `content`=Activity block's `a` = `30000:<pk>:<d>` and its relay hint. Fetch that kind-30000 and extract `p` tags — this is almost exactly `AllowlistManager.fetchList` (lines 255–297); factor a shared helper if clean.
- **Storage:** in-memory `map[community]map[memberPubkey]bool` behind a `sync.RWMutex`, rebuilt on a `COMMUNITY_REFRESH_INTERVAL` loop (like `StartRefreshLoop`). The candidate queue is the durable part; resolved membership is cache.
- **Spec nuance:** the **Rooms** section carries no `a` (per the spec we fetched) — ignore it. Only the Activity (and any `a`-bearing) section feeds membership for *shares*. Reposts/labels belong to Activity.

**Interfaces produced (consumed by Phase 4):**
- `func (r *CommunityRegistry) IsMember(community, pubkey string) bool`
- `func (r *CommunityRegistry) Enqueue(community string)`
- `func (r *CommunityRegistry) Init() error`, `StartRefreshLoop(time.Duration)`, `Stop()`

**Env:** `COMMUNITY_RELAYS` (fallback relay set, default mirrors `PROFILE_RELAYS`), `COMMUNITY_REFRESH_INTERVAL` (default `6h`).

**Open sub-decisions:** unresolved-community race (10222 not yet fetched when a share arrives) → Phase 4 holds the share edge "pending" and re-evaluates on refresh, mirroring the profile queue's "leave queued" semantics.

---

# Phase 4 — Provenance index + member-gated denormalization (Model B) (design spec → own plan)

**Goal:** When a community **member** shares content into community X (a Phase-2 share event whose author `IsMember(X, author)`), stamp `community:X` onto the *referenced content's* doc — so the single `#h:X` query (Phase 1) returns indirectly-shared content too. Un-stamp correctly on share deletion / membership loss.

**Architecture:**
- **Provenance index (durable, BoltDB):** bucket keyed `(<contentCoordOrID>, <community>)` → set of justifying share-event ids, plus a `self` marker when the content's own `h` carries that community. This is the source of truth for *derived* membership; the Typesense `community[]` field is the union of `self` + members' shares, re-derived on every change.
- **Stamping on share ingest (Phase 2 hook):** after a share event passes validation, for each `community` it targets where `registry.IsMember(community, share.PubKey)`: resolve each reference (`a` coord directly; `e`+`k` if kind is one we serve; **skip bare `e`** and `r` URLs — not stampable). If the referenced content is stored locally, add the edge + re-derive its doc's `community[]`. If absent, **fetch it** (hint-first via the share's ref relay hint, fallback `COMMUNITY_RELAYS`) using the ProfileManager pool pattern, store it, then stamp. If fetch fails, leave the edge pending (retry on refresh loop).
- **Un-stamping:** on kind-5 deletion of a share event (or on membership loss during refresh), remove its edges and re-derive. A `(content, community)` edge survives as long as *any* justifying share-id or the `self` marker remains — so deleting one repost never drops a community still justified by another share or by direct `h`.
- **Reindex replay:** `reindex.go` rebuilds `community[]` from BOTH the content's own `h` tags (the `self` marker, already handled by Phase 1 projection) AND the provenance index edges. Add a post-projection pass that overlays index-derived communities onto each rebuilt doc.

**Interfaces consumed:** Phase 1 `community` field + projection; Phase 2 `nostrToShare` + share enumeration; Phase 3 `IsMember`.

**Open sub-decisions to resolve in this phase's plan:** exact BoltDB bucket layout + the re-derive/overlay mechanism against Typesense (partial update of one field vs. full doc re-upsert — the AMB collection's content-patch path in `reindex.go`/`content_ts.go` is the precedent to follow); whether fetched-and-stored external content should also be served as a first-class event (it will be, since it lands in BoltDB + its own collection) or kept index-only.

---

## Execution note

Phase 1 is fully executable from this document. Phases 2–4 each need their own writing-plans pass (the design is locked above; the task-level TDD breakdown is not yet written) before execution.
