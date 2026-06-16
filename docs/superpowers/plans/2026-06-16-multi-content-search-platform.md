# Multi-Content Search Platform Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Evolve amb-relay from a single-kind (30142) relay into a consolidated multi-content-type search platform (long-form, wiki, calendar, web bookmarks) sharing one chunk/semantic index — without abstracting ahead of evidence and without regressing the live AMB relay.

**Architecture:** Modular monolith. One relay process accepts many kinds; each content type is one cohesive registration (Kinds + Validate + Project + Schema + QueryMap + ChunkSource). A per-kind structured Typesense collection feeds exact-field search; one *shared* chunk collection feeds cross-content semantic ranking. Production migration is strangler-fig: flag-gated, parallel-run, golden-tested against AMB's current byte output.

**Tech Stack:** Go, khatru relay framework (`fiatjaf.com/nostr`), Typesense, BoltDB (bbolt), amb-indexer (chunk pipeline), in-stack embed service (paraphrase-multilingual-MiniLM-L12-v2, 384-dim).

**Sequencing principle:** *Plan-the-near, sketch-the-far.* Phase 1 is detailed in bite-sized TDD tasks. Phases 2–6 are milestone sketches — they will be re-planned once Phase 1 + long-form expose the real registry interface. The projector-registry abstraction is deliberately **deferred until long-form lands as a second concrete content type** (rule-of-three), so the interface is discovered, not guessed.

---

## Why this ordering (read before starting)

Three facts drive the phasing:

1. **`relaykit` extraction is already earned.** The ban/admin `ManagementStore` + `adminSet` core is duplicated *verbatim* across `amb-relay/management.go` and `calendar-relay/main.go` (and divergently in `standard-86-relay`). Rule-of-three is met *today*, independent of content-type work — so Phase 1 deduplicates this now.
2. **The projector registry is NOT yet earned.** Extracting a kind→projector abstraction from AMB alone would invent an interface from one example. Phase 2 stands up long-form as a *second* concrete content type with deliberate, tolerated duplication; Phase 3 extracts the registry from the two real examples.
3. **AMB is live (100k+ events).** Every generalization step runs behind an env flag, parallel-runs, and is locked by golden tests so AMB output is provably unchanged.

The cosmetic `amb-relay → nope-relay` rename is decoupled and deferred (Phase 6, optional): highest client/DNS/Traefik/Ansible risk, lowest functional value.

---

## File Structure

### Phase 1 — `khatru/relaykit` (new shared package)

- Create: `nostrlib/khatru/relaykit/store.go` — `Store` wrapping `*bbolt.DB`: ban buckets (pubkeys, events), admin bucket, `adminEntry`, `reasonEntry`, and the Ban/Allow/List/Is* methods currently duplicated.
- Create: `nostrlib/khatru/relaykit/admins.go` — `AdminSet` (was `adminSet`): `isAdmin`/`isAllowed`/`isStatic`/`grant`/`revoke`, seeded from static pubkeys + dynamic admin entries.
- Create: `nostrlib/khatru/relaykit/store_test.go`, `nostrlib/khatru/relaykit/admins_test.go`.
- Modify: `amb-relay/management.go` — delete the moved ban/admin core; keep AMB-specific store methods (schema, semantic config, access-control, allowlist, refetch). Embed or hold a `*relaykit.Store`.
- Modify: `amb-relay/main.go:853-949` — replace the local `adminSet` type with `relaykit.AdminSet`.
- Modify: `calendar-relay/main.go:298-395` — replace its `adminSet` + `ManagementStore{}` with `relaykit`.

### Phases 2–6 (sketch — files finalized at re-plan time)

- `amb-relay/projector/` — one file per content type once the registry exists (`amb.go`, `longform.go`, `wiki.go`, `calendar.go`, `bookmark.go`), each a cohesive registration.
- `nostrlib/eventstore/typesense30142/` → generalized to a projector-driven schema/projection (AMB becomes projector #1). Promote to a neutrally-named package only once stable.
- `amb-indexer/` — shared chunks collection + per-event coordinate prefix derived from kind (today hardcoded `30142:` at `worker.go:208`; `Kinds` is already a config slice at `nostr_source.go:21`).
- `nostrlib/khatru/semantic/` — extract chunk-rerank + snippet (`chunk_rerank.go`, `chunk_snippet.go`) once a second content type consumes it.

---

## Phase 1: Extract `khatru/relaykit` (ban + admin core)

**Rationale:** Pure deduplication of verbatim-copied code. No behavior change, no AMB risk. Establishes the shared-package workflow (`go.work` local dev, `bump-nostrlib.sh` for Docker) before any content-type work.

**Files:**
- Create: `nostrlib/khatru/relaykit/store.go`
- Create: `nostrlib/khatru/relaykit/store_test.go`
- Create: `nostrlib/khatru/relaykit/admins.go`
- Create: `nostrlib/khatru/relaykit/admins_test.go`
- Modify: `amb-relay/management.go`
- Modify: `amb-relay/main.go:853-949`
- Modify: `calendar-relay/main.go:298-395`

### Task 1: relaykit.Store — ban pubkey/event round-trip

**Files:**
- Create: `nostrlib/khatru/relaykit/store.go`
- Test: `nostrlib/khatru/relaykit/store_test.go`

- [ ] **Step 1: Write the failing test**

```go
// nostrlib/khatru/relaykit/store_test.go
package relaykit

import (
	"path/filepath"
	"testing"

	"fiatjaf.com/nostr"
	"go.etcd.io/bbolt"
)

func openTestDB(t *testing.T) *bbolt.DB {
	t.Helper()
	db, err := bbolt.Open(filepath.Join(t.TempDir(), "test.db"), 0600, nil)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestBanPubKeyRoundTrip(t *testing.T) {
	s, err := NewStore(openTestDB(t))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	pk := nostr.PubKey{1, 2, 3}

	if s.IsPubKeyBanned(pk) {
		t.Fatal("pubkey banned before any ban")
	}
	if err := s.BanPubKey(pk, "spam"); err != nil {
		t.Fatalf("BanPubKey: %v", err)
	}
	if !s.IsPubKeyBanned(pk) {
		t.Fatal("pubkey not banned after BanPubKey")
	}

	list, err := s.ListBannedPubKeys()
	if err != nil {
		t.Fatalf("ListBannedPubKeys: %v", err)
	}
	if len(list) != 1 || list[0].Reason != "spam" {
		t.Fatalf("unexpected ban list: %+v", list)
	}

	if err := s.AllowPubKey(pk); err != nil {
		t.Fatalf("AllowPubKey: %v", err)
	}
	if s.IsPubKeyBanned(pk) {
		t.Fatal("pubkey still banned after AllowPubKey")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd nostrlib && go test ./khatru/relaykit/ -run TestBanPubKeyRoundTrip -v`
Expected: FAIL — `undefined: NewStore`.

- [ ] **Step 3: Write minimal implementation**

Port the ban-pubkey core from `amb-relay/management.go:71-113` verbatim, parameterized on `*bbolt.DB`. Buckets created in `NewStore`. **CRITICAL — bbolt data compatibility:** the production store keys ban buckets by the **hex string** (`pubkey.Hex()`), NOT raw bytes, and skips entries that fail hex parse on read. Preserve this exactly, plus the `reasonEntry` JSON shape, so existing `relay.db` data reads back transparently in Task 5.

```go
// nostrlib/khatru/relaykit/store.go
package relaykit

import (
	"encoding/json"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/nip86"
	"go.etcd.io/bbolt"
)

var (
	bucketBannedPubKeys = []byte("banned_pubkeys")
	bucketBannedEvents  = []byte("banned_events")
	bucketAdmins        = []byte("admins")
)

type reasonEntry struct {
	Reason string `json:"reason"`
}

type Store struct{ DB *bbolt.DB }

func NewStore(db *bbolt.DB) (*Store, error) {
	err := db.Update(func(tx *bbolt.Tx) error {
		for _, b := range [][]byte{bucketBannedPubKeys, bucketBannedEvents, bucketAdmins} {
			if _, err := tx.CreateBucketIfNotExists(b); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &Store{DB: db}, nil
}

func (s *Store) BanPubKey(pk nostr.PubKey, reason string) error {
	val, _ := json.Marshal(reasonEntry{Reason: reason})
	return s.DB.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketBannedPubKeys).Put([]byte(pk.Hex()), val)
	})
}

func (s *Store) AllowPubKey(pk nostr.PubKey) error {
	return s.DB.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketBannedPubKeys).Delete([]byte(pk.Hex()))
	})
}

func (s *Store) IsPubKeyBanned(pk nostr.PubKey) bool {
	banned := false
	s.DB.View(func(tx *bbolt.Tx) error {
		banned = tx.Bucket(bucketBannedPubKeys).Get([]byte(pk.Hex())) != nil
		return nil
	})
	return banned
}

func (s *Store) ListBannedPubKeys() ([]nip86.PubKeyReason, error) {
	var out []nip86.PubKeyReason
	err := s.DB.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketBannedPubKeys).ForEach(func(k, v []byte) error {
			pk, err := nostr.PubKeyFromHex(string(k))
			if err != nil {
				return nil // skip invalid entries
			}
			var re reasonEntry
			json.Unmarshal(v, &re)
			out = append(out, nip86.PubKeyReason{PubKey: pk, Reason: re.Reason})
			return nil
		})
	})
	return out, err
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd nostrlib && go test ./khatru/relaykit/ -run TestBanPubKeyRoundTrip -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git -C nostrlib add khatru/relaykit/store.go khatru/relaykit/store_test.go
git -C nostrlib commit -m "feat(relaykit): ban pubkey store extracted from relays"
```

### Task 2: relaykit.Store — ban event round-trip

**Files:**
- Modify: `nostrlib/khatru/relaykit/store.go`
- Test: `nostrlib/khatru/relaykit/store_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestBanEventRoundTrip(t *testing.T) {
	s, err := NewStore(openTestDB(t))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	id := nostr.ID{9, 9, 9}

	if s.IsEventBanned(id) {
		t.Fatal("event banned before any ban")
	}
	if err := s.BanEvent(id, "illegal"); err != nil {
		t.Fatalf("BanEvent: %v", err)
	}
	if !s.IsEventBanned(id) {
		t.Fatal("event not banned after BanEvent")
	}
	list, err := s.ListBannedEvents()
	if err != nil {
		t.Fatalf("ListBannedEvents: %v", err)
	}
	if len(list) != 1 || list[0].Reason != "illegal" {
		t.Fatalf("unexpected ban list: %+v", list)
	}
	if err := s.AllowEvent(id); err != nil {
		t.Fatalf("AllowEvent: %v", err)
	}
	if s.IsEventBanned(id) {
		t.Fatal("event still banned after AllowEvent")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd nostrlib && go test ./khatru/relaykit/ -run TestBanEventRoundTrip -v`
Expected: FAIL — `s.BanEvent undefined`.

- [ ] **Step 3: Write minimal implementation**

Port `BanEvent`/`AllowEvent`/`IsEventBanned`/`ListBannedEvents` from `amb-relay/management.go:115-160`, keyed on the **hex string** `id.Hex()` in `bucketBannedEvents` (same compat reason as pubkeys), JSON `reasonEntry` values, skipping hex-parse failures on read, returning `nip86.IDReason`.

```go
func (s *Store) BanEvent(id nostr.ID, reason string) error {
	val, _ := json.Marshal(reasonEntry{Reason: reason})
	return s.DB.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketBannedEvents).Put([]byte(id.Hex()), val)
	})
}

func (s *Store) AllowEvent(id nostr.ID) error {
	return s.DB.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketBannedEvents).Delete([]byte(id.Hex()))
	})
}

func (s *Store) IsEventBanned(id nostr.ID) bool {
	banned := false
	s.DB.View(func(tx *bbolt.Tx) error {
		banned = tx.Bucket(bucketBannedEvents).Get([]byte(id.Hex())) != nil
		return nil
	})
	return banned
}

func (s *Store) ListBannedEvents() ([]nip86.IDReason, error) {
	var out []nip86.IDReason
	err := s.DB.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketBannedEvents).ForEach(func(k, v []byte) error {
			id, err := nostr.IDFromHex(string(k))
			if err != nil {
				return nil // skip invalid entries
			}
			var re reasonEntry
			json.Unmarshal(v, &re)
			out = append(out, nip86.IDReason{ID: id, Reason: re.Reason})
			return nil
		})
	})
	return out, err
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd nostrlib && go test ./khatru/relaykit/ -run TestBanEventRoundTrip -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git -C nostrlib add khatru/relaykit/store.go khatru/relaykit/store_test.go
git -C nostrlib commit -m "feat(relaykit): ban event store"
```

### Task 3: relaykit.Store — admin add/remove/list

**Files:**
- Modify: `nostrlib/khatru/relaykit/store.go`
- Test: `nostrlib/khatru/relaykit/store_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestAdminAddListRemove(t *testing.T) {
	s, err := NewStore(openTestDB(t))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	pk := "abc123"

	if err := s.AddAdmin(pk, []string{"banpubkey", "allowpubkey"}); err != nil {
		t.Fatalf("AddAdmin: %v", err)
	}
	admins, err := s.ListAdmins()
	if err != nil {
		t.Fatalf("ListAdmins: %v", err)
	}
	e, ok := admins[pk]
	if !ok || len(e.Methods) != 2 {
		t.Fatalf("admin not stored correctly: %+v", admins)
	}

	if err := s.RemoveAdmin(pk, []string{"banpubkey"}); err != nil {
		t.Fatalf("RemoveAdmin: %v", err)
	}
	admins, _ = s.ListAdmins()
	if len(admins[pk].Methods) != 1 || admins[pk].Methods[0] != "allowpubkey" {
		t.Fatalf("method not removed: %+v", admins[pk])
	}

	if err := s.RemoveAdmin(pk, []string{"allowpubkey"}); err != nil {
		t.Fatalf("RemoveAdmin: %v", err)
	}
	admins, _ = s.ListAdmins()
	if _, ok := admins[pk]; ok {
		t.Fatal("admin with no methods should be deleted")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd nostrlib && go test ./khatru/relaykit/ -run TestAdminAddListRemove -v`
Expected: FAIL — `s.AddAdmin undefined`.

- [ ] **Step 3: Write minimal implementation**

Port `AddAdmin`/`RemoveAdmin`/`ListAdmins` and the `adminEntry` type from `amb-relay/management.go:367-472`. Preserve the `FullAccess bool, Methods []string` shape and the "delete entry when methods empty and not full-access" semantics. Keep the JSON encoding identical to the source.

```go
type adminEntry struct {
	FullAccess bool     `json:"full_access"`
	Methods    []string `json:"methods"`
}

// AddAdmin / RemoveAdmin / ListAdmins ported verbatim from
// amb-relay/management.go:367-472, operating on bucketAdmins.
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd nostrlib && go test ./khatru/relaykit/ -run TestAdminAddListRemove -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git -C nostrlib add khatru/relaykit/store.go khatru/relaykit/store_test.go
git -C nostrlib commit -m "feat(relaykit): admin entry store"
```

### Task 4: relaykit.AdminSet — static + dynamic authorization

**Files:**
- Create: `nostrlib/khatru/relaykit/admins.go`
- Test: `nostrlib/khatru/relaykit/admins_test.go`

- [ ] **Step 1: Write the failing test**

```go
// nostrlib/khatru/relaykit/admins_test.go
package relaykit

import (
	"testing"

	"fiatjaf.com/nostr"
)

func TestAdminSetAuthorization(t *testing.T) {
	static := nostr.PubKey{1}
	dynamic := nostr.PubKey{2}
	stranger := nostr.PubKey{3}

	as := NewAdminSet([]nostr.PubKey{static})

	// Static admins are full-access and undeletable.
	if !as.isAdmin(static) || !as.isStatic(static) {
		t.Fatal("static pubkey should be admin and static")
	}
	if !as.isAllowed(static, "anymethod") {
		t.Fatal("static admin should be allowed any method")
	}

	// Dynamic admin scoped to one method.
	as.grant(dynamic, []string{"banpubkey"})
	if !as.isAllowed(dynamic, "banpubkey") {
		t.Fatal("granted method should be allowed")
	}
	if as.isAllowed(dynamic, "reindex") {
		t.Fatal("ungranted method must be denied")
	}
	if as.isStatic(dynamic) {
		t.Fatal("dynamic admin must not be static")
	}

	// Revoke removes the scope.
	as.revoke(dynamic, []string{"banpubkey"})
	if as.isAllowed(dynamic, "banpubkey") {
		t.Fatal("revoked method must be denied")
	}

	if as.isAllowed(stranger, "banpubkey") {
		t.Fatal("unknown pubkey must be denied")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd nostrlib && go test ./khatru/relaykit/ -run TestAdminSetAuthorization -v`
Expected: FAIL — `undefined: NewAdminSet`.

- [ ] **Step 3: Write minimal implementation**

Port the `adminSet` type and `isAdmin`/`isAllowed`/`isStatic`/`grant`/`revoke` from `amb-relay/main.go:853-949`, exported as `AdminSet` with a `NewAdminSet(static []nostr.PubKey)` constructor. Preserve the mutex and the static-vs-dynamic distinction exactly.

- [ ] **Step 4: Run test to verify it passes**

Run: `cd nostrlib && go test ./khatru/relaykit/ -run TestAdminSetAuthorization -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git -C nostrlib add khatru/relaykit/admins.go khatru/relaykit/admins_test.go
git -C nostrlib commit -m "feat(relaykit): AdminSet static+dynamic authorization"
```

### Task 5: Rewire amb-relay onto relaykit

**Files:**
- Modify: `amb-relay/management.go`
- Modify: `amb-relay/main.go:853-949`

- [ ] **Step 1: Confirm baseline green**

Run: `cd amb-relay && go test ./... 2>&1 | tail -20`
Expected: PASS (record current pass set as the regression baseline).

- [ ] **Step 2: Replace local ban/admin core with relaykit**

In `amb-relay/management.go`: delete `BanPubKey`/`AllowPubKey`/`ListBannedPubKeys`/`IsPubKeyBanned`/`BanEvent`/`AllowEvent`/`IsEventBanned`/`ListBannedEvents`/`AddAdmin`/`RemoveAdmin`/`ListAdmins` and the `adminEntry`/`reasonEntry` types. Add a `*relaykit.Store` field to `ManagementStore` (constructed via `relaykit.NewStore(m.DB)` — same bbolt handle, so bucket data is shared and unchanged). Forward the deleted methods to the embedded store, OR update call sites in `main.go` to call `mgmt.Store.BanPubKey(...)` directly. Keep all AMB-specific methods (schema, semantic config, access control, allowlist, refetch) untouched.

In `amb-relay/main.go:853-949`: delete the local `adminSet` type; replace with `relaykit.AdminSet` via `relaykit.NewAdminSet(staticPubkeys)`.

- [ ] **Step 3: Build + run full suite**

Run: `cd amb-relay && go build . && go test ./... 2>&1 | tail -20`
Expected: PASS, identical set to Step 1 baseline.

- [ ] **Step 4: Manual smoke (bbolt compat)**

Run against an existing dev `relay.db` copy: start `go run .`, exercise NIP-86 `banpubkey` + `listbannedpubkeys`, confirm a pre-existing banned pubkey still reads back. (Same buckets, same JSON — must be transparent.)

- [ ] **Step 5: Commit**

```bash
git -C amb-relay add management.go main.go
git -C amb-relay commit -m "refactor(amb-relay): use khatru/relaykit for ban+admin core"
```

### Task 6: Rewire calendar-relay onto relaykit

**Files:**
- Modify: `calendar-relay/main.go:298-395`

- [ ] **Step 1: Replace adminSet + ManagementStore with relaykit**

In `calendar-relay/main.go`: delete the `adminSet` type (298-395) and the local `ManagementStore{}` ban/admin usage; wire `relaykit.NewStore` + `relaykit.NewAdminSet`. The NIP-86 handler closures (`BanPubKey`, `AddAdmin`, etc. at 182-202) now forward to the relaykit store.

- [ ] **Step 2: Build**

Run: `cd calendar-relay && go build .`
Expected: success.

- [ ] **Step 3: Commit**

```bash
git -C calendar-relay add main.go
git -C calendar-relay commit -m "refactor(calendar-relay): use khatru/relaykit for ban+admin core"
```

### Task 7: Pin relaykit for Docker builds

**Files:**
- Modify: `amb-relay/go.mod`, `calendar-relay/go.mod` (via bump script)

- [ ] **Step 1: Push nostrlib, bump consumers**

```bash
git -C nostrlib push
./bump-nostrlib.sh   # or per CLAUDE.md: GOWORK=off go list -m git.edufeed.org/edufeed/nostrlib@latest then update replace + GOWORK=off go mod tidy
```

- [ ] **Step 2: Verify standalone (Docker-path) builds**

Run: `cd amb-relay && GOWORK=off go build . && cd ../calendar-relay && GOWORK=off go build .`
Expected: both succeed against the pinned pseudo-version.

- [ ] **Step 3: Commit**

```bash
git -C amb-relay add go.mod go.sum && git -C amb-relay commit -m "chore: bump nostrlib for relaykit"
git -C calendar-relay add go.mod go.sum && git -C calendar-relay commit -m "chore: bump nostrlib for relaykit"
```

**Phase 1 exit criteria:** No `adminSet`/ban-core duplication remains across amb-relay and calendar-relay; both build standalone; AMB test suite unchanged; existing banned-pubkey data reads back transparently.

---

## Phase 2: Long-form (kind 30023) as a second content type — *deliberate duplication*

**Goal:** Accept, store, index, and search kind 30023 in amb-relay **alongside** 30142, with the chunk pipeline indexing `event.content` directly (content-bearing shape — no external fetch). Do this by *copying and adapting* the 30142 path, NOT by abstracting. The duplication is the point: it surfaces what actually varies before Phase 3 extracts the registry.

**Deliverables:**
- amb-relay accepts 30023 (relax `main.go:323` single-kind gate; add 30023 to retention `main.go:62`).
- A second Typesense collection (or schema variant) for long-form structured fields (`title`, `summary`, `published_at`, `t` tags).
- amb-indexer indexes 30023 by chunking `event.content` directly: set its `Kinds` slice (`nostr_source.go:21`) to `{30142, 30023}`; derive the chunk coordinate prefix from the event kind instead of the hardcoded `30142:` (`worker.go:208`).
- Chunk hits flow into the SAME shared chunk collection; semantic re-rank + snippet (kind 21142) work cross-content. Generalize the hardcoded `"k","30142"` snippet tag (`chunk_snippet.go:28`) to the parent's actual kind.

**Test strategy:** Golden/characterization test capturing AMB's current projected Typesense document + snippet output BEFORE any change, asserted byte-stable after. New TDD tests for the 30023 projection + a cross-content search test (one query returns both a 30142 and a 30023 result ranked by chunk score).

**Flag-gate:** `LONGFORM_ENABLED` (default false) so production stays 30142-only until validated.

**Exit criteria:** 30023 events ingest end-to-end; a semantic query returns interleaved 30142 + 30023 results; AMB golden output unchanged.

**Risks:** The shared chunk collection now mixes kinds — confirm `search_chunks` returns enough per-kind coordinates to resolve parents. Snippet `a`-tag coordinate must use the right kind prefix per hit.

---

## Phase 3: Extract the projector registry — *from two real examples*

**Goal:** Now that 30142 and 30023 both exist with tolerated duplication, extract the cohesive registration the duplication revealed.

**Deliverables:**
- A `Projector` interface bundling what varies: `Kinds() []nostr.Kind`, `Validate(event)`, `Project(event) (tsDoc, error)`, `Schema() CollectionSchema`, `QueryMap(filter) tsQuery`, `ChunkSource(event) (text, coord, locators)`.
- `amb-relay/projector/amb.go` and `projector/longform.go` implementing it; a registry (`map[nostr.Kind]Projector`) replacing the `if event.Kind == 30142` branches in `main.go` (291, 323, 326-331, 453).
- AMB projector is #1 and golden-test-locked.

**Test strategy:** Registry dispatch tests; both projectors re-pass their Phase 2 golden tests through the new interface (proves the abstraction is behavior-preserving). Keep the registry IN amb-relay — do NOT promote to nostrlib yet.

**Exit criteria:** No per-kind `if` branches remain in the event path; adding a content type = registering one projector; AMB + long-form golden output unchanged.

---

## Phase 4: Register wiki (30818) + structured-short content

**Goal:** Add NIP-54 wiki (content-bearing, like long-form) and exercise the *structured-short* shape (events that skip chunking and rank on structured fields only) to prove the registry handles all three content shapes.

**Deliverables:** `projector/wiki.go`; a structured-short projector whose `ChunkSource` returns empty (registry skips chunking, search uses structured fields). Confirms the interface from Phase 3 didn't bake in chunk-always assumptions.

**Exit criteria:** Wiki search works; at least one structured-short type ranks without chunks; registry interface unchanged from Phase 3 (or changed once, deliberately, with both prior projectors migrated).

---

## Phase 5: Extract `khatru/semantic`

**Goal:** With ≥2 content types consuming chunk-rerank + snippets, promote `chunk_rerank.go` + `chunk_snippet.go` to `nostrlib/khatru/semantic` (rule-of-three on the *semantic* layer now met).

**Deliverables:** `nostrlib/khatru/semantic/` package; amb-relay consumes it via `go.work` then pinned. Snippet kind/tag generalization carried over.

**Exit criteria:** amb-relay builds standalone on the extracted package; snippet + rerank tests pass from the new location.

---

## Phase 6 (optional, deferred): Register calendar + bookmarks; consider rebrand

**Goal:** Fold calendar (31922-31925, metadata-shape) and web bookmarks (39701, metadata→external-fetch shape) into the consolidated relay; retire `calendar-relay` if consolidation proves out. Only here, if desired, evaluate the cosmetic `amb-relay → nope-relay` rename — last, isolated, with explicit client/DNS/Traefik/Ansible/NIP-11 migration steps and a parallel-name bake.

**Exit criteria:** All target kinds served by one process; rename (if done) is a no-op for functionality and reversible.

---

## Cross-cutting risks

- **Live AMB relay (100k+ events).** Mitigation: every generalization flag-gated + golden-tested; reindex via existing NIP-86 path; parallel-run on the dev mirror (per migration memory) before prod cutover.
- **Shared chunk collection cross-talk.** Mitigation: per-kind coordinate prefixes; cross-content search test asserts correct parent resolution.
- **Cross-repo bump churn.** Mitigation: keep new abstractions in amb-relay until stable; graduate to nostrlib only at phase exit (Phases 1 and 5).
- **Premature abstraction.** Mitigation: the registry waits for two concrete examples (Phase 3 after Phase 2's deliberate duplication).

---

## Self-review notes

- **Spec coverage:** Each content type from the design doc (long-form, wiki, calendar, bookmark) maps to a phase (2, 4, 6, 6). The two-index topology, shared chunk index, and three content shapes are exercised by Phases 2 (content-bearing), 4 (structured-short + metadata), 6 (metadata→fetch).
- **Placeholder scan:** Phase 1 steps carry real code or exact source line references (`management.go:71-145`, `main.go:853-949`). Phases 2-6 are intentionally milestone-level per "plan-the-near, sketch-the-far" — they will be re-planned with full TDD steps once Phase 1 + long-form land.
- **Type consistency:** `relaykit.Store`, `relaykit.AdminSet`, `relaykit.NewStore`, `relaykit.NewAdminSet`, `adminEntry`/`reasonEntry` used consistently across Tasks 1-6. `Projector` interface method set is consistent between Phases 3 and 4.
