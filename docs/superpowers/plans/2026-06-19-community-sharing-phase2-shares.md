# Community Sharing — Phase 2: Share-event ingestion Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Accept, validate, store, and index community **share events** so a `{"kinds":[16,30222],"#h":["X"]}` REQ lists what has been shared into community X and exposes the referenced content coordinates — using the share-event shapes that actually exist in the wild.

**Architecture:** A share event is a *pointer*: a community member republishes or targets someone else's content at a community. Two shapes coexist on the producing app (`../edufeed-app`) and the communikey/outbox relays:
- **Primary — NIP-18 generic repost (kind 16):** carries one `h` per target community plus `e`/`a`/`k`/`p` references to the shared content (`createCommunityReposts` in edufeed-app). Regular (non-addressable) event → doc id = event id.
- **Legacy — Communikey "targeted publication" (kind 30222):** addressable (`d` tag); targets a community via a `p` tag (majority) or an `h` tag, plus `a`/`e`/`k` references. Read-only/back-compat; still present on relay.edufeed.org.

Both shapes land in **one new id-keyed Typesense collection** (`community_shares`), registered as a non-chunked `contentType` keyed on kinds `[16, 30222]`. The `community` field is folded **kind-aware**: `h` for both kinds, plus `p` for kind 30222 only (on kind 16 `p` is the *original author*, never a community). Folding `h`/`p` into the shared `community` field means the Phase-1 `#h` and NIP-50 `community:<pubkey>` query path works against shares with zero query-layer change. BoltDB already persists raw events (regular and addressable); this collection is search/index only.

**Tech Stack:** Go, khatru relay framework, Typesense (search), BoltDB (raw events), `fiatjaf.com/nostr` (nostrlib fork). This phase is **amb-relay-local** — no nostrlib change is required (the `h`→`community` query mapping already shipped in Phase 1's nostrlib bump).

## Global Constraints

- **Kinds are exactly `16` (primary) and `30222` (legacy).** NOT kind 6 (kind-1 reposts only; zero community shares found), NOT kind 1985 (labels — not used by the producing app). This supersedes the `[6,16,1985]` list in the original Phase-2 design spec (`2026-06-19-community-sharing.md`), which was written before the producing app was inspected.
- **Community targeting is kind-aware:** kind 16 → `h` tags only; kind 30222 → `h` **or** `p` tags. Reuse the existing `community` Typesense field (the verbatim name `community`, `string[]`) so the Phase-1 `#h` / `community:<pubkey>` query path is reused unchanged. No new tag name and no new query mapping are invented.
- Doc id: kind 16 (regular) → `event.ID.Hex()`; kind 30222 (addressable) → `typesense30142.GenerateDocumentID(pubkey, dTag)` (so a re-published 30222 with the same `(pubkey,d)` upserts in place). `eventID` is stored on every doc so `DeleteEvent` (which deletes by `filter_by=eventID:=<hex>`) works for both.
- Everything ships behind a new `COMMUNITY_SHARES_ENABLED` gate. With the gate off, the registry omits the type and behavior is byte-for-byte unchanged (kinds 16/30222 are rejected with "kind not accepted" exactly as today).
- This collection is **not chunked** (`chunked: false`): share events carry no fulltext to rank, and chunk-rerank must never own their queries.
- Phase 2 delivers **ingestion + query only**. It does NOT yet stamp `community:X` onto the *referenced* content's doc — that member-gated denormalization is Phase 4. A `#h:X` query after Phase 2 returns directly-`h`-tagged content (Phase 1) **plus** the share events themselves (this phase), not yet the indirectly-shared content.

---

## File map

- `shares.go` (new) — `ShareDocument`, `shareCommunities`, `nostrToShare`, `validateShare`, `storeShare`, `sharesSchema`. Reuses `structured.go` helpers (`newStructuredEnvelope`, `structuredEnvelopeFields`, `storeStructured`, `reprojectStructured`).
- `shares_test.go` (new) — unit tests for the projection, community fold, and validation (no live Typesense needed).
- `main.go` — `sharesEnabled` flag; `tsDB5` backend (gated); one `contentType{kinds:[16,30222], chunked:false, …}`; one `structuredReindexTarget`.
- `.env.example`, `CLAUDE.md` — document the gate, collection env, and query shape.

---

### Task 1: `shares.go` — projection (`ShareDocument` + `nostrToShare` + `shareCommunities`)

**Files:**
- Create: `shares.go`
- Test: `shares_test.go`

**Interfaces:**
- Consumes: `newStructuredEnvelope(*nostr.Event) (structuredEnvelope, error)` and `structuredEnvelopeFields() []typesense30142.Field` (from `structured.go`); `typesense30142.GenerateDocumentID(pubkey, dTag string) string`.
- Produces: `func nostrToShare(*nostr.Event) (*ShareDocument, error)`; `func shareCommunities(*nostr.Event) []string`; the `ShareDocument` type and the `kindGenericRepost`/`kindTargetedPublication` constants (consumed by Tasks 2–3 and Phase 4's `nostrToShare` reference).

- [ ] **Step 1: Write the failing test**

In `shares_test.go`:

```go
package main

import (
	"testing"

	"fiatjaf.com/nostr"
)

func TestShareCommunities(t *testing.T) {
	repost := nostr.Event{
		Kind: 16,
		Tags: nostr.Tags{
			{"e", "abc"},
			{"k", "30142"},
			{"p", "AUTHOR_pubkey_not_a_community"},
			{"h", "comm1"},
			{"h", "comm2"},
		},
	}
	got := shareCommunities(&repost)
	if len(got) != 2 || got[0] != "comm1" || got[1] != "comm2" {
		t.Fatalf("kind-16 communities = %v, want [comm1 comm2] (p must NOT count)", got)
	}

	targeted := nostr.Event{
		Kind: 30222,
		Tags: nostr.Tags{
			{"d", "x"},
			{"a", "30142:auth:slug"},
			{"p", "commA"},
			{"h", "commB"},
			{"p", "commA"}, // duplicate must be deduped
		},
	}
	got = shareCommunities(&targeted)
	if len(got) != 2 || got[0] != "commA" || got[1] != "commB" {
		t.Fatalf("kind-30222 communities = %v, want [commA commB] (p counts, deduped)", got)
	}
}

func TestNostrToShare_Repost(t *testing.T) {
	evt := nostr.Event{
		ID:   nostr.ID{0xab},
		Kind: 16,
		Tags: nostr.Tags{
			{"e", "EVENT_ID_REF"},
			{"a", "30142:auth:slug"},
			{"k", "30142"},
			{"p", "author"},
			{"h", "comm1"},
		},
	}
	doc, err := nostrToShare(&evt)
	if err != nil {
		t.Fatalf("nostrToShare: %v", err)
	}
	if doc.ID != evt.ID.Hex() {
		t.Errorf("kind-16 doc id = %q, want event id %q", doc.ID, evt.ID.Hex())
	}
	if len(doc.RefE) != 1 || doc.RefE[0] != "EVENT_ID_REF" {
		t.Errorf("RefE = %v", doc.RefE)
	}
	if len(doc.RefA) != 1 || doc.RefA[0] != "30142:auth:slug" {
		t.Errorf("RefA = %v", doc.RefA)
	}
	if doc.RefKind != 30142 {
		t.Errorf("RefKind = %d, want 30142", doc.RefKind)
	}
	if len(doc.Community) != 1 || doc.Community[0] != "comm1" {
		t.Errorf("Community = %v, want [comm1]", doc.Community)
	}
	if doc.EventKind != 16 {
		t.Errorf("EventKind = %d, want 16", doc.EventKind)
	}
}

func TestNostrToShare_TargetedPublicationDocID(t *testing.T) {
	evt := nostr.Event{
		ID:   nostr.ID{0xcd},
		Kind: 30222,
		Tags: nostr.Tags{
			{"d", "slug-1"},
			{"a", "30142:auth:slug"},
			{"p", "commA"},
		},
	}
	doc, err := nostrToShare(&evt)
	if err != nil {
		t.Fatalf("nostrToShare: %v", err)
	}
	want := nostrToShareDocID(t, evt.PubKey.Hex(), "slug-1")
	if doc.ID != want {
		t.Errorf("kind-30222 doc id = %q, want addressable id %q", doc.ID, want)
	}
	if len(doc.Community) != 1 || doc.Community[0] != "commA" {
		t.Errorf("Community = %v, want [commA] (from p)", doc.Community)
	}
}

func TestNostrToShare_TargetedPublicationMissingD(t *testing.T) {
	evt := nostr.Event{Kind: 30222, Tags: nostr.Tags{{"a", "30142:auth:slug"}, {"p", "commA"}}}
	if _, err := nostrToShare(&evt); err == nil {
		t.Fatal("expected error for kind-30222 missing 'd' tag")
	}
}
```

Add this test helper at the bottom of `shares_test.go` (keeps the GenerateDocumentID call out of the assertions):

```go
func nostrToShareDocID(t *testing.T, pubkey, d string) string {
	t.Helper()
	return typesense30142.GenerateDocumentID(pubkey, d)
}
```

and add the import:

```go
	"fiatjaf.com/nostr/eventstore/typesense30142"
```

- [ ] **Step 2: Run test to verify it fails**

Run: `GOWORK=off go test -run 'TestShareCommunities|TestNostrToShare' .`
Expected: FAIL — `undefined: shareCommunities`, `undefined: nostrToShare`, `undefined: ShareDocument` (compile error).

- [ ] **Step 3: Write `shares.go` (projection half)**

Create `shares.go`:

```go
package main

import (
	"fmt"
	"strconv"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/eventstore/typesense30142"
)

// Community-share event kinds. kindGenericRepost is the NIP-18 generic repost —
// the current edufeed-app community-share mechanism (one `h` per target
// community + e/a/k/p references). kindTargetedPublication is the legacy
// Communikey "targeted publication": addressable, targets a community via `p`
// (majority) or `h`. Kind 6 (kind-1 reposts) and 1985 (labels) are NOT used.
const (
	kindGenericRepost       nostr.Kind = 16
	kindTargetedPublication nostr.Kind = 30222
)

// ShareDocument is the Typesense document for a community share event. It records
// which communities the share targets (Community, folded kind-aware from h/p) and
// which content it references (RefE/RefA/RefKind), so a
// {"kinds":[16,30222],"#h":["X"]} REQ lists what was shared into community X. It
// embeds structuredEnvelope for raw-event reconstruction; Community is set
// explicitly rather than via the envelope's h-only fold, because kind 30222 also
// targets via `p`.
type ShareDocument struct {
	ID      string   `json:"id"`
	RefE    []string `json:"refE,omitempty"`
	RefA    []string `json:"refA,omitempty"`
	RefKind int      `json:"refKind,omitempty"`
	structuredEnvelope
}

// shareCommunities folds the target community pubkeys from a share event,
// kind-aware: `h` for both kinds, plus `p` for kind 30222 (legacy targeted
// publications target via `p`). On kind 16 a `p` tag is the original author of
// the reposted content, never a community, so it is ignored. Order-preserving
// and deduped.
func shareCommunities(event *nostr.Event) []string {
	seen := make(map[string]bool)
	var out []string
	add := func(v string) {
		if v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	for _, tag := range event.Tags {
		if len(tag) < 2 {
			continue
		}
		switch {
		case tag[0] == "h":
			add(tag[1])
		case tag[0] == "p" && event.Kind == kindTargetedPublication:
			add(tag[1])
		}
	}
	return out
}

// nostrToShare projects a kind-16 or kind-30222 event into a ShareDocument.
func nostrToShare(event *nostr.Event) (*ShareDocument, error) {
	env, err := newStructuredEnvelope(event)
	if err != nil {
		return nil, err
	}
	env.Community = shareCommunities(event)

	var id string
	if event.Kind == kindTargetedPublication {
		dTag := event.Tags.GetD()
		if dTag == "" {
			return nil, fmt.Errorf("targeted publication %s missing required 'd' tag", event.ID.Hex())
		}
		id = typesense30142.GenerateDocumentID(event.PubKey.Hex(), dTag)
	} else {
		id = event.ID.Hex()
	}

	doc := &ShareDocument{ID: id, structuredEnvelope: env}
	for _, tag := range event.Tags {
		if len(tag) < 2 {
			continue
		}
		switch tag[0] {
		case "e":
			doc.RefE = append(doc.RefE, tag[1])
		case "a":
			doc.RefA = append(doc.RefA, tag[1])
		case "k":
			if n, err := strconv.Atoi(tag[1]); err == nil {
				doc.RefKind = n
			}
		}
	}
	return doc, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `GOWORK=off go test -run 'TestShareCommunities|TestNostrToShare' .`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add shares.go shares_test.go
git commit -m "feat(shares): project kind-16/30222 community share events"
```

---

### Task 2: `validateShare`

**Files:**
- Modify: `shares.go` (add `validateShare`)
- Test: `shares_test.go`

**Interfaces:**
- Produces: `func validateShare(event nostr.Event) (reject bool, msg string)` — the registry `validate` for the shares content type (same signature as `validateLongform`).

- [ ] **Step 1: Write the failing test**

In `shares_test.go`:

```go
func TestValidateShare(t *testing.T) {
	cases := []struct {
		name   string
		event  nostr.Event
		reject bool
	}{
		{"repost with h + e", nostr.Event{Kind: 16, Tags: nostr.Tags{{"h", "c"}, {"e", "id"}}}, false},
		{"repost with h + a", nostr.Event{Kind: 16, Tags: nostr.Tags{{"h", "c"}, {"a", "30142:x:y"}}}, false},
		{"repost no community", nostr.Event{Kind: 16, Tags: nostr.Tags{{"e", "id"}, {"p", "author"}}}, true},
		{"repost no reference", nostr.Event{Kind: 16, Tags: nostr.Tags{{"h", "c"}}}, true},
		{"targeted via p with a + d", nostr.Event{Kind: 30222, Tags: nostr.Tags{{"d", "s"}, {"p", "c"}, {"a", "30142:x:y"}}}, false},
		{"targeted via h with e + d", nostr.Event{Kind: 30222, Tags: nostr.Tags{{"d", "s"}, {"h", "c"}, {"e", "id"}}}, false},
		{"targeted missing d", nostr.Event{Kind: 30222, Tags: nostr.Tags{{"p", "c"}, {"a", "30142:x:y"}}}, true},
		{"targeted no community", nostr.Event{Kind: 30222, Tags: nostr.Tags{{"d", "s"}, {"a", "30142:x:y"}}}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			reject, msg := validateShare(c.event)
			if reject != c.reject {
				t.Errorf("validateShare = %v (%q), want reject=%v", reject, msg, c.reject)
			}
		})
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `GOWORK=off go test -run TestValidateShare .`
Expected: FAIL — `undefined: validateShare` (compile error).

- [ ] **Step 3: Add `validateShare` to `shares.go`**

```go
// validateShare accepts a community share event. Both kinds require at least one
// target community (kind-aware: h, or p for 30222) AND at least one content
// reference (e or a). Kind 30222 is addressable, so it additionally requires a
// `d` tag. A kind-16 repost without an `h` tag is a plain repost, not a community
// share, and is rejected.
func validateShare(event nostr.Event) (reject bool, msg string) {
	if len(shareCommunities(&event)) == 0 {
		return true, "share event missing community target (h tag, or p tag for kind 30222)"
	}
	hasRef, hasD := false, false
	for _, tag := range event.Tags {
		if len(tag) < 2 {
			continue
		}
		switch tag[0] {
		case "e", "a":
			hasRef = true
		case "d":
			hasD = true
		}
	}
	if !hasRef {
		return true, "share event missing content reference (e or a tag)"
	}
	if event.Kind == kindTargetedPublication && !hasD {
		return true, "targeted publication missing required 'd' tag"
	}
	return false, ""
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `GOWORK=off go test -run TestValidateShare .`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add shares.go shares_test.go
git commit -m "feat(shares): validate community share events (kind-aware)"
```

---

### Task 3: `sharesSchema` + `storeShare`

**Files:**
- Modify: `shares.go` (add `sharesSchema`, `storeShare`)
- Test: `shares_test.go`

**Interfaces:**
- Consumes: `storeStructured` and `structuredEnvelopeFields` (from `structured.go`).
- Produces: `func sharesSchema(name string) typesense30142.CollectionSchema`; `func storeShare(enabled bool, ts *typesense30142.TSBackend, event nostr.Event)`.

- [ ] **Step 1: Write the failing test**

In `shares_test.go`:

```go
func TestSharesSchema(t *testing.T) {
	schema := sharesSchema("community_shares")
	if schema.Name != "community_shares" {
		t.Fatalf("schema name = %q", schema.Name)
	}
	want := map[string]string{
		"id":        "string",
		"refE":      "string[]",
		"refA":      "string[]",
		"refKind":   "int32",
		"community": "string[]", // from structuredEnvelopeFields — drives #h / community:
		"eventID":   "string",   // delete-by-eventID needs this present
	}
	got := make(map[string]string)
	for _, f := range schema.Fields {
		got[f.Name] = f.Type
	}
	for name, typ := range want {
		if got[name] != typ {
			t.Errorf("field %q type = %q, want %q", name, got[name], typ)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `GOWORK=off go test -run TestSharesSchema .`
Expected: FAIL — `undefined: sharesSchema` (compile error).

- [ ] **Step 3: Add `sharesSchema` + `storeShare` to `shares.go`**

```go
// sharesSchema returns the Typesense collection schema for community share
// events. Reuses the envelope fields (eventID/eventKind/…/community) so the
// shared query path reconstructs events and #h / community:<pubkey> queries hit
// the same `community` field as every other collection.
func sharesSchema(name string) typesense30142.CollectionSchema {
	return typesense30142.CollectionSchema{
		Name:                name,
		DefaultSortingField: "eventCreatedAt",
		Fields: append([]typesense30142.Field{
			{Name: "id", Type: "string"},
			{Name: "refE", Type: "string[]", Optional: true, Facet: true},
			{Name: "refA", Type: "string[]", Optional: true, Facet: true},
			{Name: "refKind", Type: "int32", Optional: true, Facet: true},
		}, structuredEnvelopeFields()...),
	}
}

// storeShare projects and upserts a share event to the community_shares Typesense
// collection via the shared structured-collection helper (fire-and-forget; the
// event is already durable in BoltDB).
func storeShare(enabled bool, ts *typesense30142.TSBackend, event nostr.Event) {
	storeStructured(enabled, ts, event, "share", nostrToShare)
}
```

- [ ] **Step 4: Run test to verify it passes, then the whole unit suite**

Run: `GOWORK=off go test -run TestSharesSchema .`
Expected: PASS.

Run: `GOWORK=off go test .`
Expected: PASS (no new failures vs. baseline).

- [ ] **Step 5: Commit**

```bash
git add shares.go shares_test.go
git commit -m "feat(shares): community_shares schema + store helper"
```

---

### Task 4: Wire the shares backend into `main.go`

**Files:**
- Modify: `main.go` (flag near line 66; `tsDB5` backend near the profiles backend ~line 323; `contentType` registration near line 423; `structuredReindexTarget` near line 498)

**Interfaces:**
- Consumes: `validateShare`, `storeShare`, `nostrToShare`, `sharesSchema` (Tasks 1–3); the existing `contentType` struct, `newRegistry`, and `structuredReindexTarget` patterns.

- [ ] **Step 1: Add the gate flag**

In `main.go`, after line 66 (`profilesEnabled := …`):

```go
	sharesEnabled := os.Getenv("COMMUNITY_SHARES_ENABLED") == "true"
```

- [ ] **Step 2: Add the `tsDB5` backend**

In `main.go`, after the profiles backend block (after line ~323), add:

```go
	// Community shares (kind 16 repost + legacy kind 30222) Typesense backend —
	// gated behind COMMUNITY_SHARES_ENABLED. Disjoint id-keyed collection; nil
	// when the flag is off so the registry omits it. SearchFields is set to a
	// real indexed string field (eventID) because shares carry no fulltext —
	// they are queried by #h / community:<pubkey> / kind, never by free text.
	var tsDB5 *typesense30142.TSBackend
	if sharesEnabled {
		shColl := os.Getenv("TS_COLLECTION_SHARES")
		if shColl == "" {
			shColl = "community_shares"
		}
		shSchema := sharesSchema(shColl)
		tsDB5 = &typesense30142.TSBackend{
			ApiKey:          os.Getenv("TS_APIKEY"),
			Host:            os.Getenv("TS_HOST"),
			CollectionName:  shColl,
			RawEventStore:   &boltDB,
			Schema:          &shSchema,
			SearchFields:    "eventID",
			StopwordsSet:    stopwordsSet,
			StopwordsList:   stopwordsList,
			StopwordsLocale: "de",
		}
		if err := tsDB5.Init(); err != nil {
			panic(fmt.Sprintf("shares TSBackend init: %v", err))
		}
		fmt.Printf("Community shares (kinds 16, 30222) enabled — collection %s\n", shColl)
	}
```

- [ ] **Step 3: Register the content type**

In `main.go`, in the `contentTypes` assembly, after the profiles block (after line ~423), add:

```go
	if sharesEnabled && tsDB5 != nil {
		contentTypes = append(contentTypes, contentType{
			kinds:    []nostr.Kind{16, 30222},
			validate: validateShare,
			store:    func(e nostr.Event) { storeShare(true, tsDB5, e) },
			fetch:    tsDB5.QueryEvents,
			count:    tsDB5.CountEvents,
			deleteID: tsDB5.DeleteEvent,
			chunked:  false,
		})
	}
```

- [ ] **Step 4: Add the reindex target**

In `main.go`, after the calendar reindex-target block (after line ~498), add:

```go
	if sharesEnabled && tsDB5 != nil {
		structuredTargets = append(structuredTargets, structuredReindexTarget{
			label:     "shares",
			kinds:     []nostr.Kind{16, 30222},
			recreate:  func() error { return tsDB5.RecreateCollection(tsDB5.Schema) },
			reproject: func(e nostr.Event) error { return reprojectStructured(tsDB5, e, nostrToShare) },
		})
	}
```

- [ ] **Step 5: Build**

Run: `GOWORK=off go build .`
Expected: builds with no error.

- [ ] **Step 6: Run the whole unit suite**

Run: `GOWORK=off go test ./...`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add main.go
git commit -m "feat(shares): wire community_shares backend, registry, reindex behind COMMUNITY_SHARES_ENABLED"
```

---

### Task 5: Manual end-to-end verification + docs

**Files:**
- Modify: `.env.example` (document the new vars)
- Modify: `CLAUDE.md` (a "Community shares" section + canonical query shape)

- [ ] **Step 1: Bring up the stack with the gate on**

```bash
docker compose up -d typesense
COMMUNITY_SHARES_ENABLED=true go run .
```

- [ ] **Step 2: Publish a kind-16 community repost and a kind-30222 targeted publication**

In another shell (replace `<sec>` / community pubkey `<C>`):

```bash
# kind 16 generic repost targeting community <C>
nak event -k 16 \
  -t "e=0000000000000000000000000000000000000000000000000000000000000001" \
  -t "a=30142:author:slug" -t "k=30142" -t "p=author" -t "h=<C>" \
  ws://localhost:3334 --sec <sec>

# kind 30222 targeted publication targeting community <C> via p
nak event -k 30222 -d tp-1 \
  -t "a=30142:author:slug" -t "k=30142" -t "p=<C>" \
  ws://localhost:3334 --sec <sec>
```

Expected: both accepted (no `kind not accepted` / validation rejection).

- [ ] **Step 3: List shares into the community by NIP-50 `community:` and by kind**

```bash
nak req --search "community:<C>" -k 16 -k 30222 ws://localhost:3334
```

Expected: returns both events published in Step 2. (`#h` via a TagMap Go client returns the same set; `nak` can't emit bare `#h` reliably — see CLAUDE.md.)

- [ ] **Step 4: Confirm Typesense holds the docs with the community field**

```bash
curl -H "X-TYPESENSE-API-KEY: $TS_APIKEY" \
  "$TS_HOST/collections/community_shares/documents/search?q=*&filter_by=community:=<C>&per_page=10"
```

Expected: `found == 2`; each hit has `refA`/`refE`/`refKind` populated.

- [ ] **Step 5: Confirm the gate-off default is unchanged**

Restart without the flag (`go run .`), republish a kind-16 with `h`:

```bash
nak event -k 16 -t "e=...0001" -t "h=<C>" ws://localhost:3334 --sec <sec>
```

Expected: rejected with `kind not accepted` (the registry omits shares when the gate is off).

- [ ] **Step 6: Document the vars in `.env.example`**

Add:

```
# Community shares (NIP-18 kind-16 reposts + legacy kind-30222 targeted publications)
COMMUNITY_SHARES_ENABLED=false
TS_COLLECTION_SHARES=community_shares
```

- [ ] **Step 7: Document in `CLAUDE.md`**

Add a "Community shares" subsection near the other gated content types, and add this canonical query shape to the query-shapes block:

```
- "What's been shared into a community" (share events): `{"kinds":[16,30222],"#h":["<community-pubkey>"],"limit":50}` → community_shares collection; community field folds `h` (both kinds) plus `p` (kind 30222 only). NIP-50 equivalent: `search:"community:<community-pubkey>"`. NOTE: this returns the *share events*; the referenced content's own doc is only stamped with the community in Phase 4.
```

- [ ] **Step 8: Commit**

```bash
git add .env.example CLAUDE.md
git commit -m "docs(shares): COMMUNITY_SHARES_ENABLED gate, vars, query shape"
```

---

## Self-Review

- **Spec coverage:** kind 16 (primary, `h`) and kind 30222 (legacy, `p`-or-`h`) accepted/validated/indexed (Tasks 1–4); kind 6 and 1985 deliberately excluded per Global Constraints; one disjoint id-keyed collection with kind-aware doc id (Task 1); `#h` / `community:` reuse Phase-1 mapping via the shared `community` field (Tasks 1, 3); reindex replay (Task 4); gate-off no-op verified (Task 5 Step 5). ✓
- **Type consistency:** `ShareDocument`, `nostrToShare`, `shareCommunities`, `validateShare`, `sharesSchema`, `storeShare` names used identically across `shares.go`, tests, and `main.go` wiring; `validateShare` matches the `contentType.validate` signature `(reject bool, msg string)`; `reprojectStructured(tsDB5, e, nostrToShare)` matches the generic helper. ✓
- **Placeholder scan:** every code step shows real code and a runnable command. ✓
- **Reconciliation note:** the original design spec (`2026-06-19-community-sharing.md`, Phase 2 section) listed kinds `[6,16,1985]` and a single `community` fold from `h` only. This plan supersedes both: kinds are `[16,30222]`, and the fold is kind-aware (`p` counts for 30222). The reason is documented in `project_community_sharing.md` (CORRECTED share-event model, 2026-06-19) — verified against `../edufeed-app` source and live relays.

---

## Out of scope (later phases)

- **Phase 3 — community registry:** auto-discover communities from `h`/`p`, fetch kind-10222, resolve Activity kind-30000 membership (`IsMember`). Required to *gate* stamping.
- **Phase 4 — member-gated denormalization:** stamp `community:X` onto the *referenced* content's doc when the sharer `IsMember(X, author)`; un-stamp on delete/membership loss; reindex overlay. After Phase 4, the single `#h:X` query returns directly-`h`-tagged content **and** indirectly-shared content; until then, a `#h:X` query returns directly-tagged content plus the share-event pointers from this phase.
