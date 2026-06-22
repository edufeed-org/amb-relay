# Community Sharing — Phase 4 (Member-Gated Stamping) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** When a community member shares content (kind-16/30222 share event), stamp the *referenced content's own doc* with `community:X`, so a `#h:X` / `community:X` query over content kinds (30142/30023/30818/31922/31923) returns the shared content itself — not just the share pointer. Fetch the referenced content if it isn't on the relay yet, so coverage is complete.

**Architecture:** A single idempotent `reconcile(content)` operation is the whole engine. It re-derives a content's community set from the `community_shares` Typesense collection (whose `refA`/`refE` are filterable facets) gated through `CommunityRegistry.IsMember`, unions it with the content's own `h` tags, and PATCHes just the `community` field via the existing `patchDoc` helper. Absent referenced content is fetched (hint-first) from `COMMUNITY_RELAYS`, validated, stored first-class, then stamped. Every trigger (share write, share delete, content write, reindex completion, periodic sweep) reduces to calling `reconcile`. The sweep is the retry-for-absent and reindex-replay mechanism, so **no durable pending queue and no provenance index are needed** — the shares collection is the work list and the source of truth.

**Tech Stack:** Go, `fiatjaf.com/nostr` (`nostr.Pool`, `eventstore/typesense30142`, `eventstore/boltdb`). Build/test: `GOWORK=off go test ./...` and `GOWORK=off go build .` (Bash sandbox disabled — the go-build cache and worktree git writes live outside the sandbox-writable set).

## Why this (simpler) shape — design rationale

Per the project's standing "simplify before executing" pass, this revision deliberately drops machinery the original design spec (`docs/superpowers/plans/2026-06-19-community-sharing.md` lines 519-538) carried:

- **No durable BoltDB provenance index** (`(content,community)→{share-ids}`). `community_shares` already stores `refA`/`refE` as Typesense facets, so "which shares reference content C" is a filter query. Re-deriving on demand is correct (no cache to invalidate) and is the same source the reindex replay must read anyway. This deletes an entire stateful subsystem (KISS).
- **No durable pending-fetch queue.** A share whose referenced content is absent locally *is* the pending-work record. The periodic sweep re-scans shares and retries fetches. This mirrors Phase 3's `CommunityRegistry`, which re-derives the community set from a backfill scan rather than persisting a queue (consistency with the established pattern; the user explicitly chose "fetch absent content" — this delivers that outcome without a new subsystem).
- **One operation, five triggers.** `reconcile(coord, kind, pubkey, d)` is idempotent and total: it computes the full desired `community` field and PATCHes it. Share-write, share-delete, content-write, reindex-done, and sweep all call it. No per-trigger special-case logic (DRY).
- **Reuse, not reinvention:** `patchDoc` (`content_ts.go:59`) for the single-field PATCH; `GenerateDocumentID` (`typesense30142/replace.go:24`) for doc ids; `shareCommunities` (`shares.go:41`) for kind-aware community targets; `CommunityRegistry.IsMember` (Phase 3) for the gate; `nostr.Pool` for fetch (the relay's established pattern — not `sdk.System`, which the repo uses nowhere). The stamper takes its collaborators as injected funcs, so the engine is unit-testable with fakes (mirrors Phase 3's `communitySource` interface) and `main.go` wires the real Typesense/BoltDB/Pool impls.
- **Deliberately NOT built (YAGNI):** membership-loss un-stamping (user chose share-delete-only un-stamp; 18/20 live communities are open, so transitions are rare and the sweep already re-evaluates `IsMember` each tick anyway); bare `e`+`r` reference resolution (edufeed-app always emits an `a` tag for addressable content shares, and every stampable kind is addressable — `a`-only resolution covers the real data; `e`-only shares are skipped, matching the original spec).

## Global Constraints

- **Gating:** the stamper is wired only when `COMMUNITY_SHARES_ENABLED == "true"` (the existing `sharesEnabled` flag, `main.go:67`). Off ⇒ zero new behavior, no sweep loop, no triggers.
- **Stampable kinds only:** stamp the content collections — AMB 30142 (`tsDB`), long-form 30023 (`tsDB2`), wiki 30818 (`tsDB3`), calendar 31922/31923/31924/31925 (`tsDB4`) — and only those that are enabled. NEVER stamp the profiles (`profiles_0`) or shares (`community_shares`) collections.
- **Doc id:** `typesense30142.GenerateDocumentID(pubkeyHex, dTag)` for every stampable kind (all addressable). The `a`-coord `"<kind>:<pubkeyHex>:<d>"` yields these parts directly.
- **`community` field is the full set:** PATCH always writes `own-h-tags ∪ member-gated-shared-communities`. The field is replaced, not appended — a stale stamp is removed by recomputing the whole set. An empty result writes `[]` (clears the field).
- **Member-gating:** a shared community is stamped only when `registry.IsMember(community, share.author, referencedContentKind)` is true. Open-by-default means most pass.
- **Eventual consistency is acceptable:** stamping is denormalized search convenience, not correctness-critical. A PATCH that 404s (e.g. AMB 30142 doc still buffered in the write buffer) is logged and left; the next sweep re-PATCHes. Never block or error a write path on a stamp failure.
- **Fetched content is first-class:** absent referenced content is fetched, validated via the registry, and stored through the same buffer+project path as any event (so it appears in all queries, then gets stamped). Reject (validation-failed) fetched events are dropped.
- **Local-only:** all work in this worktree on a Phase-4 branch off the current dev tip. No deploy/push/migration without explicit user consent.

## Key reusable code (read before implementing)

- `shareCommunities(*nostr.Event) []string` — `shares.go:41`. Kind-aware target extractor (`h` both kinds + `p` for 30222 only). Use to get a share's targeted communities.
- `patchDoc(host, apiKey, collection, docID string, body map[string]any) error` — `content_ts.go:59`. Generic Typesense single-doc PATCH. Use with `body{"community": []string{...}}`.
- `tsDocIDFromEvent(event nostr.Event) (string, error)` — `content_ts.go:27`. `{pubkey}:{d-tag}` for an addressable event; use when resolving an `e`-ref's stored event (not needed for the `a`-only path, but available).
- `typesense30142.GenerateDocumentID(pubkeyHex, dTag string) string` — `typesense30142/replace.go:24`.
- `CommunityRegistry.IsMember(community, pubkey string, kind nostr.Kind) bool` — `community_registry.go` (Phase 3). The gate.
- `poolCommunitySource` / `nostr.Pool.QuerySingle(ctx, urls, filter, opts) *nostr.RelayEvent` — `community_registry.go` / nostrlib. Returns nil on miss; `RelayEvent` embeds `nostr.Event` (`&res.Event`).
- TSBackend fields `Host`, `ApiKey`, `CollectionName` — `main.go:177-185`. Build the per-kind collection map from these.
- `registry.validate(event) (reject bool, msg string)` — `content_registry.go:64`. Validate fetched content before storing.
- Write closures pattern: `boltBuf.Queue(event, false)` + `reg.store(event)` — `main.go:694-701` (`relay.StoreEvent`). The stamper's injected `store` closure does exactly this.
- Manager lifecycle shapes (`Init`/`StartRefreshLoop`/`Stop`, 30s ctx timeout per tick): `community_registry.go` (Phase 3) and `profile_manager.go`. Copy for the sweep loop.

---

## Task 1: Pure ref + stamp derivation

**Files:** Create `community_stamp.go`, `community_stamp_test.go`.

**Interfaces produced:**
- `type shareRef struct { Coord string; Kind nostr.Kind; Pubkey, DTag, Author string; Communities []string; RelayHint string }`
- `func shareToRef(event nostr.Event) (shareRef, bool)` — from the first parseable `a` tag (`"<kind>:<pubkeyHex>:<d>"`) + `shareCommunities(event)`; `Author = event.PubKey.Hex()`; `RelayHint = aTag[2]` if present. Returns `false` (skip) when there is no parseable `a` tag or no targeted community.
- `func contentOwnCommunities(event nostr.Event) []string` — the content event's own `h` tag values (Model A), de-duplicated, first-seen order.
- `func deriveStamps(refs []shareRef, isMember func(community, pubkey string, kind nostr.Kind) bool) map[string]map[string]bool` — `coord → set-of-communities`, including a community only when `isMember(community, ref.Author, ref.Kind)`.

**Interfaces consumed:** `shareCommunities` (shares.go:41).

- [ ] **Step 1: Write failing tests**

```go
package main

import (
	"testing"

	"fiatjaf.com/nostr"
)

func TestShareToRefAtag(t *testing.T) {
	ev := nostr.Event{
		Kind:   16,
		PubKey: mustPK(t, "0000000000000000000000000000000000000000000000000000000000000002"),
		Tags: nostr.Tags{
			{"e", "deadbeef"},
			{"a", "30023:776c7bfe528c041cd1114efb6d48100b2e49d4faf27e301fb3f83c64a28694f4:my-d", "wss://hint.example"},
			{"k", "30023"},
			{"h", "C1"},
			{"h", "C2"},
		},
	}
	ref, ok := shareToRef(ev)
	if !ok {
		t.Fatal("expected a parseable share ref")
	}
	if ref.Coord != "30023:776c7bfe528c041cd1114efb6d48100b2e49d4faf27e301fb3f83c64a28694f4:my-d" {
		t.Fatalf("coord = %q", ref.Coord)
	}
	if ref.Kind != 30023 || ref.DTag != "my-d" || ref.RelayHint != "wss://hint.example" {
		t.Fatalf("ref = %+v", ref)
	}
	if ref.Pubkey != "776c7bfe528c041cd1114efb6d48100b2e49d4faf27e301fb3f83c64a28694f4" {
		t.Fatalf("pubkey = %q", ref.Pubkey)
	}
	if len(ref.Communities) != 2 || ref.Communities[0] != "C1" || ref.Communities[1] != "C2" {
		t.Fatalf("communities = %v", ref.Communities)
	}
}

func TestShareToRefSkips(t *testing.T) {
	// No `a` tag → skip.
	noA := nostr.Event{Kind: 16, Tags: nostr.Tags{{"e", "x"}, {"h", "C1"}}}
	if _, ok := shareToRef(noA); ok {
		t.Fatal("share without a-tag must be skipped")
	}
	// `a` tag but no targeted community → skip.
	noComm := nostr.Event{Kind: 16, Tags: nostr.Tags{{"a", "30023:abcd:d"}}}
	if _, ok := shareToRef(noComm); ok {
		t.Fatal("share with no community target must be skipped")
	}
	// Malformed `a` value → skip.
	badA := nostr.Event{Kind: 16, Tags: nostr.Tags{{"a", "garbage"}, {"h", "C1"}}}
	if _, ok := shareToRef(badA); ok {
		t.Fatal("malformed a-coord must be skipped")
	}
}

func TestContentOwnCommunities(t *testing.T) {
	ev := nostr.Event{Kind: 30023, Tags: nostr.Tags{{"h", "C1"}, {"h", "C2"}, {"h", "C1"}, {"t", "x"}}}
	got := contentOwnCommunities(ev)
	if len(got) != 2 || got[0] != "C1" || got[1] != "C2" {
		t.Fatalf("own communities = %v", got)
	}
}

func TestDeriveStampsMemberGated(t *testing.T) {
	refs := []shareRef{
		{Coord: "30023:p:d", Kind: 30023, Author: "alice", Communities: []string{"C1", "C2"}},
		{Coord: "30023:p:d", Kind: 30023, Author: "mallory", Communities: []string{"C3"}},
	}
	// alice is a member of C1 only; mallory of nothing.
	isMember := func(community, pubkey string, _ nostr.Kind) bool {
		return pubkey == "alice" && community == "C1"
	}
	stamps := deriveStamps(refs, isMember)
	set := stamps["30023:p:d"]
	if !set["C1"] || set["C2"] || set["C3"] {
		t.Fatalf("member-gated stamps = %v", set)
	}
}
```

Add this test helper at the bottom of `community_stamp_test.go`:

```go
func mustPK(t *testing.T, hex string) nostr.PubKey {
	t.Helper()
	pk, err := nostr.PubKeyFromHex(hex)
	if err != nil {
		t.Fatalf("bad pubkey hex %q: %v", hex, err)
	}
	return pk
}
```

- [ ] **Step 2: Run, verify FAIL** — `GOWORK=off go test . -run 'TestShareToRef|TestContentOwnCommunities|TestDeriveStamps' -v` → undefined.

- [ ] **Step 3: Implement** in `community_stamp.go`

```go
package main

import (
	"strconv"
	"strings"

	"fiatjaf.com/nostr"
)

// shareRef is one share event resolved to the addressable content it targets.
type shareRef struct {
	Coord       string // "<kind>:<pubkeyHex>:<d>"
	Kind        nostr.Kind
	Pubkey      string
	DTag        string
	Author      string // share event author (the member doing the sharing)
	Communities []string
	RelayHint   string // optional relay hint from the a-tag's 3rd element
}

// shareToRef resolves a share event to the addressable content it references via
// its first parseable `a` tag, plus the communities it targets. Returns false
// (skip) when there is no parseable `a` coord or no targeted community. e-only
// shares are intentionally skipped: every stampable kind is addressable and
// edufeed-app always emits an `a` tag for them.
func shareToRef(event nostr.Event) (shareRef, bool) {
	communities := shareCommunities(&event)
	if len(communities) == 0 {
		return shareRef{}, false
	}
	for _, tag := range event.Tags {
		if len(tag) < 2 || tag[0] != "a" {
			continue
		}
		parts := strings.SplitN(tag[1], ":", 3)
		if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
			continue
		}
		n, err := strconv.Atoi(parts[0])
		if err != nil {
			continue
		}
		ref := shareRef{
			Coord:       tag[1],
			Kind:        nostr.Kind(n),
			Pubkey:      parts[1],
			DTag:        parts[2],
			Author:      event.PubKey.Hex(),
			Communities: communities,
		}
		if len(tag) >= 3 {
			ref.RelayHint = tag[2]
		}
		return ref, true
	}
	return shareRef{}, false
}

// contentOwnCommunities returns a content event's own `h` community targets
// (Model A), de-duplicated in first-seen order.
func contentOwnCommunities(event nostr.Event) []string {
	seen := make(map[string]bool)
	var out []string
	for _, tag := range event.Tags {
		if len(tag) >= 2 && tag[0] == "h" && !seen[tag[1]] {
			seen[tag[1]] = true
			out = append(out, tag[1])
		}
	}
	return out
}

// deriveStamps maps each referenced content coord to the set of communities it
// is shared into, gated by membership: a community is included only when the
// share's author may publish the referenced content's kind into it.
func deriveStamps(refs []shareRef, isMember func(community, pubkey string, kind nostr.Kind) bool) map[string]map[string]bool {
	stamps := make(map[string]map[string]bool)
	for _, ref := range refs {
		for _, c := range ref.Communities {
			if !isMember(c, ref.Author, ref.Kind) {
				continue
			}
			if stamps[ref.Coord] == nil {
				stamps[ref.Coord] = make(map[string]bool)
			}
			stamps[ref.Coord][c] = true
		}
	}
	return stamps
}
```

- [ ] **Step 4: Run, verify PASS** — same `-run`.
- [ ] **Step 5: Commit** — `git add community_stamp.go community_stamp_test.go && git commit -m "feat(community): pure share-ref + member-gated stamp derivation"`

---

## Task 2: CommunityStamper.reconcile (local path) with injected deps

**Files:** Modify `community_stamp.go`, `community_stamp_test.go`.

**Interfaces produced:**
- `type CommunityStamper struct {…}` with injected function-typed collaborators (so the engine is unit-testable; `main.go` wires real Typesense/BoltDB/Pool impls in Task 4):
  - `isMember func(community, pubkey string, kind nostr.Kind) bool`
  - `sharesFor func(coord string) []nostr.Event` — shares whose `refA` facet contains `coord`
  - `allShares func() []nostr.Event` — every share (for sweep/replay)
  - `lookup func(coord string) (nostr.Event, bool)` — local content by coord
  - `fetch func(ref shareRef, hints []string) (nostr.Event, bool)` — remote fetch (Task 3 wires it; nil-safe here)
  - `validate func(nostr.Event) (reject bool, msg string)`
  - `store func(nostr.Event)` — first-class store
  - `patch func(kind nostr.Kind, docID string, communities []string) error`
- `func (s *CommunityStamper) reconcile(coord string, kind nostr.Kind, pubkey, dTag string)` — recompute and PATCH the content's full `community` field; fetch+store if absent and any community is desired.
- `func unionSorted(a []string, b map[string]bool) []string` — `own ∪ desired`, de-duplicated, deterministic (sorted) order.

- [ ] **Step 1: Write failing tests** (append to `community_stamp_test.go`)

```go
import "sort" // add to the import block if not present

type patchCall struct {
	kind        nostr.Kind
	docID       string
	communities []string
}

func newTestStamper() (*CommunityStamper, *[]patchCall) {
	var patches []patchCall
	s := &CommunityStamper{
		isMember: func(_, _ string, _ nostr.Kind) bool { return true },
		patch: func(kind nostr.Kind, docID string, communities []string) error {
			patches = append(patches, patchCall{kind, docID, communities})
			return nil
		},
	}
	return s, &patches
}

func mkShare(author, coord, community string) nostr.Event {
	parts := mustPK(nil, "")
	_ = parts
	return nostr.Event{
		Kind:   16,
		PubKey: pkOrZero(author),
		Tags:   nostr.Tags{{"a", coord}, {"k", "30023"}, {"h", community}},
	}
}

func TestReconcileLocalStampsOwnPlusShared(t *testing.T) {
	const owner = "776c7bfe528c041cd1114efb6d48100b2e49d4faf27e301fb3f83c64a28694f4"
	coord := "30023:" + owner + ":d1"
	s, patches := newTestStamper()
	s.sharesFor = func(c string) []nostr.Event {
		if c != coord {
			return nil
		}
		return []nostr.Event{mkShare(owner, coord, "C1")}
	}
	s.lookup = func(c string) (nostr.Event, bool) {
		// local content carries its own h tag C0
		return nostr.Event{Kind: 30023, PubKey: pkOrZero(owner), Tags: nostr.Tags{{"d", "d1"}, {"h", "C0"}}}, true
	}
	s.reconcile(coord, 30023, owner, "d1")

	if len(*patches) != 1 {
		t.Fatalf("want 1 patch, got %d", len(*patches))
	}
	got := (*patches)[0]
	want := []string{"C0", "C1"}
	sort.Strings(got.communities)
	if got.kind != 30023 || len(got.communities) != 2 || got.communities[0] != want[0] || got.communities[1] != want[1] {
		t.Fatalf("patch = %+v", got)
	}
}

func TestReconcileNonMemberNotStamped(t *testing.T) {
	const owner = "776c7bfe528c041cd1114efb6d48100b2e49d4faf27e301fb3f83c64a28694f4"
	coord := "30023:" + owner + ":d1"
	s, patches := newTestStamper()
	s.isMember = func(_, _ string, _ nostr.Kind) bool { return false }
	s.sharesFor = func(string) []nostr.Event { return []nostr.Event{mkShare(owner, coord, "C1")} }
	s.lookup = func(string) (nostr.Event, bool) {
		return nostr.Event{Kind: 30023, PubKey: pkOrZero(owner), Tags: nostr.Tags{{"d", "d1"}}}, true
	}
	s.reconcile(coord, 30023, owner, "d1")
	if len(*patches) != 1 || len((*patches)[0].communities) != 0 {
		t.Fatalf("non-member share must yield empty community set, got %+v", *patches)
	}
}

func TestReconcileAbsentNoDesiredSkips(t *testing.T) {
	s, patches := newTestStamper()
	s.sharesFor = func(string) []nostr.Event { return nil } // no shares → no desired
	s.lookup = func(string) (nostr.Event, bool) { return nostr.Event{}, false }
	s.reconcile("30023:p:d", 30023, "p", "d")
	if len(*patches) != 0 {
		t.Fatalf("absent content with no desired communities must not patch, got %+v", *patches)
	}
}
```

Add these test helpers to `community_stamp_test.go`:

```go
// pkOrZero parses a hex pubkey, falling back to the zero key for fixtures that
// don't care about the author identity.
func pkOrZero(hexKey string) nostr.PubKey {
	if pk, err := nostr.PubKeyFromHex(hexKey); err == nil {
		return pk
	}
	pk, _ := nostr.PubKeyFromHex("0000000000000000000000000000000000000000000000000000000000000002")
	return pk
}
```

> NOTE: delete the stray `mustPK(nil, "")` lines if your `mustPK` requires a non-nil `*testing.T` — the `mkShare` helper above should simply build the event. Corrected `mkShare`:
> ```go
> func mkShare(author, coord, community string) nostr.Event {
> 	return nostr.Event{
> 		Kind:   16,
> 		PubKey: pkOrZero(author),
> 		Tags:   nostr.Tags{{"a", coord}, {"k", "30023"}, {"h", community}},
> 	}
> }
> ```

- [ ] **Step 2: Run, verify FAIL** — `GOWORK=off go test . -run TestReconcile -v`.

- [ ] **Step 3: Implement** (append to `community_stamp.go`; add `sort` to imports)

```go
type CommunityStamper struct {
	isMember  func(community, pubkey string, kind nostr.Kind) bool
	sharesFor func(coord string) []nostr.Event
	allShares func() []nostr.Event
	lookup    func(coord string) (nostr.Event, bool)
	fetch     func(ref shareRef, hints []string) (nostr.Event, bool)
	validate  func(nostr.Event) (reject bool, msg string)
	store     func(nostr.Event)
	patch     func(kind nostr.Kind, docID string, communities []string) error

	stopCh chan struct{}
}

// reconcile recomputes the full `community` field for one addressable content
// coord and PATCHes it. The desired set is the member-gated union of every
// share that references this coord; the written value is own-h-tags ∪ desired.
// Absent content with at least one desired community is fetched, validated, and
// stored first-class before stamping. A fetch miss is left for the next sweep.
func (s *CommunityStamper) reconcile(coord string, kind nostr.Kind, pubkey, dTag string) {
	var refs []shareRef
	for _, ev := range s.sharesFor(coord) {
		if ref, ok := shareToRef(ev); ok {
			refs = append(refs, ref)
		}
	}
	desired := deriveStamps(refs, s.isMember)[coord] // may be nil

	ev, local := s.lookup(coord)
	if !local {
		if len(desired) == 0 {
			return // nothing to stamp and nothing to fetch
		}
		if s.fetch == nil {
			return
		}
		fetched, ok := s.fetch(shareRef{Coord: coord, Kind: kind, Pubkey: pubkey, DTag: dTag}, hintsFromRefs(refs))
		if !ok {
			return // retried by the next sweep
		}
		if s.validate != nil {
			if reject, _ := s.validate(fetched); reject {
				return
			}
		}
		if s.store != nil {
			s.store(fetched)
		}
		ev = fetched
	}

	final := unionSorted(contentOwnCommunities(ev), desired)
	docID := typesense30142GenerateDocumentID(pubkey, dTag)
	if err := s.patch(kind, docID, final); err != nil {
		fmt.Printf("community stamp: patch %s (%d): %v\n", coord, kind, err)
	}
}

// unionSorted returns the sorted, de-duplicated union of a slice and a set.
func unionSorted(a []string, b map[string]bool) []string {
	set := make(map[string]bool, len(a)+len(b))
	for _, v := range a {
		set[v] = true
	}
	for v := range b {
		set[v] = true
	}
	out := make([]string, 0, len(set))
	for v := range set {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

func hintsFromRefs(refs []shareRef) []string {
	var hints []string
	for _, r := range refs {
		if r.RelayHint != "" {
			hints = append(hints, r.RelayHint)
		}
	}
	return hints
}
```

Add the imports `fmt` and `sort` to `community_stamp.go`. For the doc id, add a tiny wrapper so the file has no direct typesense import churn (or import `typesense30142` and call `typesense30142.GenerateDocumentID` directly — preferred):

```go
// at the top, in the import block:
//   "fiatjaf.com/nostr/eventstore/typesense30142"
// and replace typesense30142GenerateDocumentID(pubkey, dTag) with:
//   typesense30142.GenerateDocumentID(pubkey, dTag)
```

> Implementer: use `typesense30142.GenerateDocumentID(pubkey, dTag)` directly and import the package; the `typesense30142GenerateDocumentID` name above is a placeholder to avoid a forward reference in this prose.

- [ ] **Step 4: Run, verify PASS** — `GOWORK=off go test . -run 'TestReconcile|TestShareToRef|TestContentOwnCommunities|TestDeriveStamps' -v`.
- [ ] **Step 5: Commit** — `git commit -am "feat(community): stamper reconcile (local path, member-gated, full-set PATCH)"`

---

## Task 3: Fetch-absent path, reconcileAll, lifecycle (Init/sweep/Stop)

**Files:** Modify `community_stamp.go`, `community_stamp_test.go`.

**Interfaces produced:**
- `func (s *CommunityStamper) reconcileAll()` — derive distinct referenced coords from `allShares()` and `reconcile` each. Used by the sweep and the reindex replay.
- `func (s *CommunityStamper) reconcileShare(event nostr.Event)` — convenience: `shareToRef` then `reconcile`. Used by the share-write and share-delete triggers.
- `func (s *CommunityStamper) Init()` — one background `reconcileAll` (never blocks startup).
- `func (s *CommunityStamper) StartSweepLoop(interval time.Duration)` — periodic `reconcileAll`.
- `func (s *CommunityStamper) Stop()`.
- `const communityStampTimeout = 60 * time.Second` (sweep budget; larger than the registry's 30s because a sweep may fetch absent content).

- [ ] **Step 1: Write failing tests** (append)

```go
import "time" // add if not present

func TestReconcileAllDistinctCoords(t *testing.T) {
	const owner = "776c7bfe528c041cd1114efb6d48100b2e49d4faf27e301fb3f83c64a28694f4"
	c1 := "30023:" + owner + ":d1"
	c2 := "30023:" + owner + ":d2"
	s, patches := newTestStamper()
	s.allShares = func() []nostr.Event {
		return []nostr.Event{mkShare(owner, c1, "C1"), mkShare(owner, c1, "C1"), mkShare(owner, c2, "C2")}
	}
	s.sharesFor = func(coord string) []nostr.Event {
		var out []nostr.Event
		for _, e := range s.allShares() {
			if ref, ok := shareToRef(e); ok && ref.Coord == coord {
				out = append(out, e)
			}
		}
		return out
	}
	s.lookup = func(string) (nostr.Event, bool) {
		return nostr.Event{Kind: 30023, PubKey: pkOrZero(owner), Tags: nostr.Tags{{"d", "x"}}}, true
	}
	s.reconcileAll()
	if len(*patches) != 2 {
		t.Fatalf("want 2 distinct-coord patches, got %d (%+v)", len(*patches), *patches)
	}
}

func TestReconcileFetchesAbsentThenStampsAndStores(t *testing.T) {
	const owner = "776c7bfe528c041cd1114efb6d48100b2e49d4faf27e301fb3f83c64a28694f4"
	coord := "30023:" + owner + ":d9"
	s, patches := newTestStamper()
	s.sharesFor = func(string) []nostr.Event { return []nostr.Event{mkShare(owner, coord, "C1")} }
	s.lookup = func(string) (nostr.Event, bool) { return nostr.Event{}, false } // absent
	var fetched, stored bool
	s.fetch = func(ref shareRef, _ []string) (nostr.Event, bool) {
		fetched = true
		return nostr.Event{Kind: 30023, PubKey: pkOrZero(owner), Tags: nostr.Tags{{"d", "d9"}}}, true
	}
	s.validate = func(nostr.Event) (bool, string) { return false, "" }
	s.store = func(nostr.Event) { stored = true }
	s.reconcile(coord, 30023, owner, "d9")
	if !fetched || !stored || len(*patches) != 1 {
		t.Fatalf("absent path: fetched=%v stored=%v patches=%d", fetched, stored, len(*patches))
	}
}

func TestReconcileFetchRejectedNotStored(t *testing.T) {
	const owner = "776c7bfe528c041cd1114efb6d48100b2e49d4faf27e301fb3f83c64a28694f4"
	coord := "30023:" + owner + ":dx"
	s, patches := newTestStamper()
	s.sharesFor = func(string) []nostr.Event { return []nostr.Event{mkShare(owner, coord, "C1")} }
	s.lookup = func(string) (nostr.Event, bool) { return nostr.Event{}, false }
	s.fetch = func(shareRef, []string) (nostr.Event, bool) {
		return nostr.Event{Kind: 30023, PubKey: pkOrZero(owner), Tags: nostr.Tags{{"d", "dx"}}}, true
	}
	s.validate = func(nostr.Event) (bool, string) { return true, "rejected" }
	var stored bool
	s.store = func(nostr.Event) { stored = true }
	s.reconcile(coord, 30023, owner, "dx")
	if stored || len(*patches) != 0 {
		t.Fatalf("rejected fetch must not store or patch: stored=%v patches=%d", stored, len(*patches))
	}
}
```

- [ ] **Step 2: Run, verify FAIL** — `GOWORK=off go test . -run 'TestReconcileAll|TestReconcileFetch' -v`.

- [ ] **Step 3: Implement** (append; add `context` and `time` imports)

```go
const communityStampTimeout = 60 * time.Second

// reconcileShare resolves a share to its referenced content and reconciles it.
func (s *CommunityStamper) reconcileShare(event nostr.Event) {
	if ref, ok := shareToRef(event); ok {
		s.reconcile(ref.Coord, ref.Kind, ref.Pubkey, ref.DTag)
	}
}

// reconcileAll reconciles every distinct content coord referenced by any share.
// This is both the periodic sweep (retrying absent fetches and re-evaluating
// IsMember) and the post-reindex replay (re-applying stamps the projection
// wiped). The shares collection is the work list — no durable queue.
func (s *CommunityStamper) reconcileAll() {
	seen := make(map[string]bool)
	for _, ev := range s.allShares() {
		ref, ok := shareToRef(ev)
		if ok && !seen[ref.Coord] {
			seen[ref.Coord] = true
			s.reconcile(ref.Coord, ref.Kind, ref.Pubkey, ref.DTag)
		}
	}
}

func (s *CommunityStamper) Init() {
	go s.reconcileAll()
}

func (s *CommunityStamper) StartSweepLoop(interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				s.reconcileAll()
			case <-s.stopCh:
				return
			}
		}
	}()
}

func (s *CommunityStamper) Stop() { close(s.stopCh) }
```

> `communityStampTimeout` and `context` are used by the real `fetch` impl wired in Task 4 (the pool query is bounded by a 60s ctx per sweep). Keep the import; if vet flags `context` as unused at this task boundary, add the `fetch`-impl helper from Task 4 in the same commit or leave a `var _ = context.Background` — the implementer should prefer wiring Task 4's pool fetch helper here if it keeps the build clean. Reviewer: this cross-task import is expected.

- [ ] **Step 4: Run, verify PASS** — full `community_stamp` tests.
- [ ] **Step 5: Commit** — `git commit -am "feat(community): stamper fetch-absent path + reconcileAll + sweep lifecycle"`

---

## Task 4: main.go wiring (gated) + triggers + env + docs

**Files:** Modify `main.go`, `.env.example`, `CLAUDE.md`. No new unit test (process wiring; covered by build + Task 6 live E2E).

**Interfaces consumed:** `CommunityRegistry` (`communityReg`, Phase 3, `main.go` ~588), `tsDB`/`tsDB2`/`tsDB3`/`tsDB4`/`tsDB5` backends, `boltBuf`, `boltDB`, `reg`, `profileMgr`, `nostr.Pool`.

- [ ] **Step 1: Build the stamper after the CommunityRegistry block** (after the Phase-3 `communityReg` wiring, before `relay.QueryStored` at `main.go:673`). The stamper needs `communityReg` (for `IsMember`), so it must be constructed after it.

```go
	var stamper *CommunityStamper
	if sharesEnabled && communityReg != nil && tsDB5 != nil {
		// Per-kind stamp target: which Typesense backend holds each content kind.
		stampTargets := map[nostr.Kind]*typesense30142.TSBackend{30142: &tsDB}
		if longformEnabled && tsDB2 != nil {
			stampTargets[30023] = tsDB2
		}
		if wikiEnabled && tsDB3 != nil {
			stampTargets[30818] = tsDB3
		}
		if calendarEnabled && tsDB4 != nil {
			for _, k := range []nostr.Kind{31922, 31923, 31924, 31925} {
				stampTargets[k] = tsDB4
			}
		}

		stampPool := nostr.NewPool()
		// Reuse the Phase-3 community relays for fetch fallback.
		stampRelays := communityRelays // in scope from the Phase-3 block

		stamper = &CommunityStamper{
			isMember: communityReg.IsMember,
			sharesFor: func(coord string) []nostr.Event {
				var out []nostr.Event
				// refA is a filterable facet on the shares collection; QueryEvents
				// honors tag filters via #a-style TagMap.
				for ev := range tsDB5.QueryEvents(nostr.Filter{
					Kinds: []nostr.Kind{16, 30222},
					Tags:  nostr.TagMap{"a": []string{coord}},
				}, 10_000) {
					out = append(out, ev)
				}
				return out
			},
			allShares: func() []nostr.Event {
				var out []nostr.Event
				for ev := range boltDB.QueryEvents(nostr.Filter{Kinds: []nostr.Kind{16, 30222}}, 1_000_000) {
					out = append(out, ev)
				}
				return out
			},
			lookup: func(coord string) (nostr.Event, bool) {
				parts := strings.SplitN(coord, ":", 3)
				if len(parts) != 3 {
					return nostr.Event{}, false
				}
				kn, err := strconv.Atoi(parts[0])
				if err != nil {
					return nostr.Event{}, false
				}
				pk, err := nostr.PubKeyFromHex(parts[1])
				if err != nil {
					return nostr.Event{}, false
				}
				for ev := range boltDB.QueryEvents(nostr.Filter{
					Kinds:   []nostr.Kind{nostr.Kind(kn)},
					Authors: []nostr.PubKey{pk},
					Tags:    nostr.TagMap{"d": []string{parts[2]}},
					Limit:   1,
				}, 1) {
					return ev, true
				}
				return nostr.Event{}, false
			},
			fetch: func(ref shareRef, hints []string) (nostr.Event, bool) {
				pk, err := nostr.PubKeyFromHex(ref.Pubkey)
				if err != nil {
					return nostr.Event{}, false
				}
				relays := stampRelays
				if len(hints) > 0 {
					relays = append(append([]string{}, hints...), stampRelays...)
				}
				ctx, cancel := context.WithTimeout(context.Background(), communityStampTimeout)
				defer cancel()
				res := stampPool.QuerySingle(ctx, relays, nostr.Filter{
					Kinds:   []nostr.Kind{ref.Kind},
					Authors: []nostr.PubKey{pk},
					Tags:    nostr.TagMap{"d": []string{ref.DTag}},
					Limit:   1,
				}, nostr.SubscriptionOptions{})
				if res == nil {
					return nostr.Event{}, false
				}
				return res.Event, true
			},
			validate: reg.validate,
			store: func(e nostr.Event) {
				boltBuf.Queue(e, false)
				reg.store(e)
				if profileMgr != nil {
					profileMgr.Enqueue(e.PubKey)
				}
			},
			patch: func(kind nostr.Kind, docID string, communities []string) error {
				be, ok := stampTargets[kind]
				if !ok {
					return nil // not a stampable kind
				}
				if communities == nil {
					communities = []string{}
				}
				return patchDoc(be.Host, be.ApiKey, be.CollectionName, docID, map[string]any{"community": communities})
			},
			stopCh: make(chan struct{}),
		}
		stamper.Init()
		stamper.StartSweepLoop(communityRefresh) // reuse Phase-3 interval
		defer stamper.Stop()
		fmt.Printf("Community stamper: member-gated denormalization active (sweep every %s)\n", communityRefresh)
	}
```

> NOTE: `communityRelays` and `communityRefresh` are locals from the Phase-3 `communityReg` block. If they are scoped inside that `if sharesEnabled` block, hoist their declarations so this block can read them (or re-read the env the same way). The implementer must confirm scope and adjust — both blocks are under the same `sharesEnabled` guard, so hoisting to the outer block is the clean fix.

- [ ] **Step 2: Add the triggers.** In `relay.StoreEvent` and `relay.ReplaceEvent` (`main.go:694-709`), after `reg.store(event)`, reconcile:

```go
		if stamper != nil {
			switch event.Kind {
			case 16, 30222:
				go stamper.reconcileShare(event)
			default:
				if _, ok := stamper.patchTarget(event.Kind); ok {
					if coord, k, pk, d, ok := contentCoord(event); ok {
						go stamper.reconcile(coord, k, pk, d)
					}
				}
			}
		}
```

Add two small helpers (in `community_stamp.go`):

```go
// patchTarget reports whether a kind is stampable (has a content collection).
func (s *CommunityStamper) patchTarget(kind nostr.Kind) (struct{}, bool) {
	// The patch closure already no-ops unstampable kinds; this guard just
	// avoids spawning a goroutine for kind-0/share kinds. Implemented in
	// main.go's closure set via stampTargets; expose a membership check.
	_, ok := s.stampKinds[kind]
	return struct{}{}, ok
}

// contentCoord derives the addressable coord of a content event.
func contentCoord(event nostr.Event) (coord string, kind nostr.Kind, pubkey, dTag string, ok bool) {
	for _, tag := range event.Tags {
		if len(tag) >= 2 && tag[0] == "d" {
			pk := event.PubKey.Hex()
			return fmt.Sprintf("%d:%s:%s", event.Kind, pk, tag[1]), event.Kind, pk, tag[1], true
		}
	}
	return "", 0, "", "", false
}
```

> To support `patchTarget`, add a `stampKinds map[nostr.Kind]bool` field to `CommunityStamper` and populate it in the main.go wiring from the `stampTargets` keys (`stampKinds: kindsOf(stampTargets)` with a trivial helper, or set the field after construction). Keep it minimal — the goal is only to skip goroutines for non-content kinds. Implementer may instead inline the kind check in main.go against `stampTargets` and drop `patchTarget` entirely; either is acceptable as long as content-write reconciliation fires only for stampable kinds.

- [ ] **Step 3: Add the un-stamp trigger.** In `relay.DeleteEvent` (`main.go:710`), BEFORE `boltDB.DeleteEvent(id)`, capture whether the deleted event is a share and remember its ref; AFTER `reg.deleteEverywhere`, reconcile the affected coord (now the shares collection no longer contains the deleted share, so the recomputed set drops it):

```go
	relay.DeleteEvent = func(ctx context.Context, id nostr.ID) error {
		var deletedShareRef *shareRef
		if stamper != nil {
			for ev := range boltDB.QueryEvents(nostr.Filter{IDs: []nostr.ID{id}}, 1) {
				if ev.Kind == 16 || ev.Kind == 30222 {
					if ref, ok := shareToRef(ev); ok {
						r := ref
						deletedShareRef = &r
					}
				}
			}
		}
		boltDB.DeleteEvent(id)
		_ = contentStore.Delete(id.Hex())
		err := reg.deleteEverywhere(id, func(err error) {
			fmt.Printf("delete %s (secondary collection): %v\n", id.Hex(), err)
		})
		if deletedShareRef != nil {
			r := *deletedShareRef
			go stamper.reconcile(r.Coord, r.Kind, r.Pubkey, r.DTag)
		}
		return err
	}
```

> Confirm `nostr.Filter{IDs: []nostr.ID{id}}` is the correct id-filter shape for this nostrlib (grep an existing id query; if BoltDB exposes a direct `Get`-by-id, prefer it). The share write buffer (`tsBuf`/`boltBuf`) may delay the deleted share leaving the Typesense collection; `reconcile` PATCHes from the shares-collection state at call time, and the next sweep corrects any lag — acceptable per the eventual-consistency constraint.

- [ ] **Step 4: `.env.example`** — Phase 4 introduces no new env vars (it reuses `COMMUNITY_RELAYS` and `COMMUNITY_REFRESH_INTERVAL` from Phase 3). Add a comment under the Phase-3 community block noting the stamper reuses them:

```
# Phase 4 (member-gated stamping) reuses COMMUNITY_RELAYS (fetch source for
# absent shared content) and COMMUNITY_REFRESH_INTERVAL (stamp sweep cadence).
```

- [ ] **Step 5: `CLAUDE.md`** — extend the "Community shares" section:

> **Member-gated stamping (Phase 4), active with `COMMUNITY_SHARES_ENABLED`:** when a community member shares content (kind-16/30222), the relay stamps the *referenced content's own doc* with the community so a `#h:X` / `community:X` query over content kinds (30142/30023/30818/31922/31923) returns the shared content itself. A `CommunityStamper` (`community_stamp.go`) runs one idempotent `reconcile(coord)` per affected content: it re-derives the content's community set from the `community_shares` collection (`refA` facet), gates each via `CommunityRegistry.IsMember(community, shareAuthor, contentKind)`, unions with the content's own `h` tags, and PATCHes the `community` field (`patchDoc`). Referenced content not on the relay is fetched hint-first from `COMMUNITY_RELAYS`, validated, and stored first-class before stamping. Triggers: share write, share deletion (un-stamp — re-derive without the deleted share), content write (re-apply after projection), reindex completion (replay), and a `COMMUNITY_REFRESH_INTERVAL` sweep (retry absent fetches). There is no durable provenance index or pending queue — the shares collection is the source of truth. Stamping is eventually consistent: a PATCH that misses a not-yet-flushed doc is corrected by the next sweep.

- [ ] **Step 6: Build** — `GOWORK=off go build .` → PASS. `GOWORK=off go test ./...` → PASS (no behavior change with gate off).
- [ ] **Step 7: Commit** — `git commit -am "feat(community): wire CommunityStamper triggers under COMMUNITY_SHARES_ENABLED; docs"`

---

## Task 5: Reindex replay hook

**Files:** Modify `reindex.go`, `main.go`. Add one unit test for the hook invocation.

**Interfaces produced:** `Reindexer` gains an optional `afterRun func()` callback invoked once at the end of `run()`; `main.go` sets it to `stamper.reconcileAll` so a reindex (which rebuilds collections from raw events and thus wipes stamps) re-applies them.

- [ ] **Step 1: Write failing test** (`reindex_test.go`, create or append)

```go
package main

import "testing"

func TestReindexerAfterRunCallbackFires(t *testing.T) {
	called := false
	r := &Reindexer{afterRun: func() { called = true }}
	r.runAfter()
	if !called {
		t.Fatal("afterRun must be invoked by runAfter")
	}
}
```

- [ ] **Step 2: Run, verify FAIL** — `GOWORK=off go test . -run TestReindexerAfterRun -v`.

- [ ] **Step 3: Implement.** Add the field + a tiny indirection so it is testable without a full reindex:

In `reindex.go`, add to the `Reindexer` struct:
```go
	afterRun func() // optional; invoked once after a reindex completes (stamp replay)
```
At the very end of `run()` (after the completion log line, ~`reindex.go:211`):
```go
	r.runAfter()
```
And add:
```go
// runAfter invokes the post-reindex callback if set. Separated for testability.
func (r *Reindexer) runAfter() {
	if r.afterRun != nil {
		r.afterRun()
	}
}
```

- [ ] **Step 4: Wire it in main.go.** Where the `Reindexer` is constructed (`grep -n NewReindexer main.go`), after construction set:
```go
	if stamper != nil {
		reindexer.afterRun = func() { stamper.reconcileAll() }
	}
```
> If `NewReindexer` is called before the stamper exists in `main.go`'s ordering, move the stamper construction earlier or set `afterRun` after both exist. Keep `NewReindexer`'s signature unchanged (the field is set post-construction) to avoid churn in its other callers/tests.

- [ ] **Step 5: Run, verify PASS** — `GOWORK=off go test . -run TestReindexerAfterRun -v`; then `GOWORK=off go build . && GOWORK=off go test ./...`.
- [ ] **Step 6: Commit** — `git commit -am "feat(community): replay stamps after reindex via Reindexer.afterRun"`

---

## Task 6: Live verification + final docs sweep

**Files:** No production code. A throwaway test, run then deleted (NOT committed).

- [ ] **Step 1: Build** — `GOWORK=off go build -o /tmp/amb-relay-p4 .` (sandbox off).

- [ ] **Step 2: Live E2E against dev** (sandbox off; DEV ONLY — never prod/oersi). Run the relay locally with a dev `.env` (`COMMUNITY_SHARES_ENABLED=true`, dev Typesense, `COMMUNITY_RELAYS=wss://relay.edufeed.org`), reusing the Phase-2/3 E2E setup. Then a throwaway focused test that:
  1. Picks a REAL community + a REAL kind-16 share from `wss://relay.edufeed.org` whose referenced `a`-coord content IS present on dev.amb-relay (locality survey showed ~75% are).
  2. Asserts that after the stamper's sweep, a query `{"kinds":[30142|30023|…],"#h":["<community>"]}` against the dev relay returns the referenced content doc (stamped), not only the share pointer.
  3. Picks a share whose referenced content is ABSENT and asserts that after a sweep the content has been fetched, stored, and stamped (or, if the source relay no longer serves it, that it is left pending without error — log the outcome).
  Record results in the progress ledger.

- [ ] **Step 3: Delete the throwaway test.** `rm` it; confirm `git status` shows only intended changes.

- [ ] **Step 4: Final docs check** — verify CLAUDE.md "Community shares" + "Canonical query shapes" reflect that content-level `#h` now returns shared content (update the "shared with a community" canonical shape note: it now includes member-shared content, not only directly-`h`-tagged content).

- [ ] **Step 5: Commit** any doc refinements — `git commit -am "docs(community): note Phase-4 content-level stamping in canonical query shapes"`

---

## Final whole-branch review

Dispatch the final code-reviewer (superpowers:requesting-code-review) over the Phase-4 branch diff (`scripts/review-package <merge-base> HEAD`). Hand it these global constraints: the gating rule; stampable-kinds-only (never profiles/shares); `community` field is the full recomputed set (own-h ∪ member-gated-shared); member-gating via `IsMember`; un-stamp on share-delete only (no membership-loss sweep — verify it is genuinely absent, by design); fetched content is validated + first-class; eventual consistency (no write-path errors on stamp failure); no durable provenance index / no pending queue (re-derive from shares); `a`-only ref resolution (e-only skipped). Then stop and report — **do not merge or deploy without explicit user consent.**

## Verification summary

- `GOWORK=off go test ./...` green: shareToRef (a-tag / skips), contentOwnCommunities, deriveStamps (member-gated), reconcile (local own∪shared, non-member empty, absent-no-desired skip), fetch-absent (fetch+validate+store+stamp, rejected-not-stored), reconcileAll distinct coords, reindex afterRun callback.
- `GOWORK=off go build .` green (standalone).
- Gate off ⇒ no stamper log line, no sweep loop, no triggers, zero behavior change.
- Live (dev only): a real member share stamps its referenced (local) content so content-level `#h` returns it; an absent referenced content is fetched+stored+stamped (or left pending without error).

## Out of scope (later)

Membership-loss un-stamping (re-evaluating transitions beyond the sweep's per-tick `IsMember` recheck); bare `e`+`r` reference resolution; serving fetched external content under a separate provenance marker; any cross-relay backfill of historical shares beyond what `allShares()` already scans from BoltDB.
