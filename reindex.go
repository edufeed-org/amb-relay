package main

import (
	"cmp"
	"fmt"
	"iter"
	"log"
	"slices"
	"sync"
	"sync/atomic"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/eventstore/boltdb"
	"fiatjaf.com/nostr/eventstore/typesense30142"
)

const (
	reindexMaxEvents = 10_000_000
	reindexBatchSize = 100
)

// structuredReindexTarget describes one structured (long-form/wiki) collection
// to rebuild during a reindex: drop+recreate it, then reproject every BoltDB
// event of its kinds. recreate/reproject are closures so reindex.go stays free
// of per-kind schema/projection detail (they are built in main.go beside the
// backends).
type structuredReindexTarget struct {
	label     string
	kinds     []nostr.Kind
	recreate  func() error
	reproject func(nostr.Event) error
	// after, when set, runs once after the reprojection pass — e.g. the
	// publications target replays indexer-written content from the
	// ContentStore onto the rebuilt docs. Returns (patched, errs) to fold
	// into the reindex counters.
	after func() (patched, errs int64)
}

type ReindexStatus struct {
	Running bool  `json:"running"`
	Total   int64 `json:"total"`
	Indexed int64 `json:"indexed"`
	// Superseded counts events skipped because a newer version of the same
	// addressable document was projected instead. Reported so total > indexed
	// reads as resolved versions rather than as unexplained shrinkage — a
	// reindex that silently drops events and one that correctly supersedes
	// them otherwise look identical from the outside.
	Superseded      int64  `json:"superseded"`
	Errors          int64  `json:"errors"`
	ContentPatched  int64  `json:"content_patched"`
	ContentOrphaned int64  `json:"content_orphaned"`
	Error           string `json:"error,omitempty"`
}

type Reindexer struct {
	tsDB    *typesense30142.TSBackend
	boltDB  *boltdb.BoltBackend
	mgmt    *ManagementStore
	content *ContentStore

	structured []structuredReindexTarget

	mu              sync.Mutex
	running         atomic.Bool
	total           atomic.Int64
	indexed         atomic.Int64
	superseded      atomic.Int64
	errors          atomic.Int64
	contentPatched  atomic.Int64
	contentOrphaned atomic.Int64
	lastErr         atomic.Value // stores string
	afterRun        func()       // optional; invoked once after a reindex completes (stamp replay)
}

func NewReindexer(tsDB *typesense30142.TSBackend, boltDB *boltdb.BoltBackend, mgmt *ManagementStore, content *ContentStore, structured []structuredReindexTarget) *Reindexer {
	return &Reindexer{
		tsDB:       tsDB,
		boltDB:     boltDB,
		mgmt:       mgmt,
		content:    content,
		structured: structured,
	}
}

// Start begins a reindex in the background. Returns an error if one is already running.
func (r *Reindexer) Start() error {
	if !r.running.CompareAndSwap(false, true) {
		return fmt.Errorf("reindex already in progress")
	}

	// Reset counters
	r.total.Store(0)
	r.indexed.Store(0)
	r.superseded.Store(0)
	r.errors.Store(0)
	r.contentPatched.Store(0)
	r.contentOrphaned.Store(0)
	r.lastErr.Store("")

	go r.run()
	return nil
}

func (r *Reindexer) run() {
	defer r.running.Store(false)

	// Load schema (custom or default)
	schema, err := r.mgmt.LoadSchema()
	if err != nil {
		r.lastErr.Store(fmt.Sprintf("failed to load schema: %v", err))
		return
	}
	// Ensure the content fields are present even on custom schemas.
	if schema == nil {
		def := typesense30142.DefaultSchema()
		schema = &def
	}
	ensureContentFields(schema)

	// Recreate the collection with the (possibly updated) schema
	if err := r.tsDB.RecreateCollection(schema); err != nil {
		r.lastErr.Store(fmt.Sprintf("failed to recreate collection: %v", err))
		return
	}

	log.Println("reindex: collection recreated, starting event re-indexing with batching")

	// Collect events in batches for efficient bulk upsert
	var batch []nostr.Event

	// Every event the walk saw, with everything the content replay below needs
	// to place and order its row. ONE map on purpose: liveness, timestamp and
	// document id are recorded in a single assignment, so an event cannot be
	// marked live without also carrying the created_at that orders it. Kept as
	// three maps, dropping just the timestamp write left the whole suite green
	// and silently restored amb-relay#11.
	walked := make(map[string]walkedEvent, 1024)

	// BoltDB can hold more than one version of the same addressable event, and
	// the Typesense document id collapses them onto one document. The import
	// upsert is unconditional, so without this the version written LAST wins
	// whatever its created_at — and the walk is newest-first, which makes that
	// always the oldest. See nostrlib#4.
	dedup := typesense30142.NewAddressDedup()

	// Iterate all events from BoltDB
	for event := range r.boltDB.QueryEvents(nostr.Filter{Kinds: []nostr.Kind{30142}}, reindexMaxEvents) {
		r.total.Add(1)
		eventIDHex := event.ID.Hex()
		// Every walked version stays in the live set: it exists in BoltDB, so
		// its content row is not an orphan even when the version itself is
		// superseded. Content replay keys on the document id either way.
		w, err := recordWalked(event)
		if err != nil {
			log.Printf("reindex: skipping content projection for %s: %v", eventIDHex, err)
		}
		walked[eventIDHex] = w
		// Resolved against the projection's OWN key, not the content-replay
		// key above: tsDocIDFromEvent rejects an empty d value, but NostrToAMB
		// still assigns such an event a real (and colliding) document id, so
		// deduping on the content key would leave exactly this PR's bug in
		// place for d="" resources.
		if key, addressable := ambDedupKey(event); addressable {
			if !dedup.Keep(key, event) {
				r.superseded.Add(1)
				continue
			}
		}
		batch = append(batch, event)

		// Process batch when full
		if len(batch) >= reindexBatchSize {
			indexed, errs := r.tsDB.BatchUpsertEvents(batch)
			r.indexed.Add(int64(indexed))
			r.errors.Add(int64(len(errs)))
			for _, err := range errs {
				log.Printf("reindex: batch error: %v", err)
			}
			batch = batch[:0] // Reset batch, reuse backing array
		}
	}

	// Process remaining events in final batch
	if len(batch) > 0 {
		indexed, errs := r.tsDB.BatchUpsertEvents(batch)
		r.indexed.Add(int64(indexed))
		r.errors.Add(int64(len(errs)))
		for _, err := range errs {
			log.Printf("reindex: batch error: %v", err)
		}
	}

	// Content replay + orphan GC.
	// Classify rows while holding only a read txn (ContentStore.ForEach uses DB.View).
	// Then perform Delete/PatchContent OUTSIDE the ForEach callback to avoid
	// nesting a write txn (Delete) or long HTTP calls inside the read txn.
	var orphanCandidates []string
	var liveRows []liveContent

	_ = r.content.ForEach(func(eventID string, entry ContentEntry) error {
		if _, alive := walked[eventID]; !alive {
			orphanCandidates = append(orphanCandidates, eventID)
		} else {
			liveRows = append(liveRows, liveContent{id: eventID, entry: entry})
		}
		return nil
	})

	// A row absent from the AMB live set may belong to another kind's
	// collection (kind-routed setcontent) — only rows whose event is gone
	// from BoltDB entirely are deleted.
	orphans := classifyOrphans(orphanCandidates, func(idHex string) (nostr.Kind, bool) {
		id, err := nostr.IDFromHex(idHex)
		if err != nil {
			return 0, false
		}
		for e := range r.boltDB.QueryEvents(nostr.Filter{IDs: []nostr.ID{id}, Limit: 1}, 1) {
			return e.Kind, true
		}
		return 0, false
	})

	// Orphan GC — outside the read txn.
	for _, id := range orphans {
		if err := r.content.Delete(id); err != nil {
			log.Printf("reindex: orphan delete failed for %s: %v", id, err)
		} else {
			r.contentOrphaned.Add(1)
		}
	}

	// Live content replay — outside the read txn.
	patched, errs := replayContent(liveRows, walked,
		func(docID string, entry ContentEntry) error {
			return PatchContent(r.tsDB.Host, r.tsDB.ApiKey, r.tsDB.CollectionName, docID, entry)
		})
	r.contentPatched.Add(patched)
	r.errors.Add(errs)

	// Rebuild every enabled structured (long-form/wiki) collection from BoltDB
	// truth. Empty when LONGFORM/WIKI are disabled, so reindex is byte-for-byte
	// unchanged in that case.
	for _, t := range r.structured {
		r.reindexStructured(t)
	}

	log.Printf("reindex: completed. total=%d indexed=%d superseded=%d errors=%d content_patched=%d content_orphaned=%d",
		r.total.Load(), r.indexed.Load(), r.superseded.Load(), r.errors.Load(),
		r.contentPatched.Load(), r.contentOrphaned.Load())
	r.runAfter()
}

// runAfter invokes the post-reindex callback if set. Separated for testability.
func (r *Reindexer) runAfter() {
	if r.afterRun != nil {
		r.afterRun()
	}
}

// ambDedupKey returns the Typesense document id NostrToAMB will store an AMB
// event under, and whether it assigns one at all.
//
// It deliberately mirrors NostrToAMB rather than reusing tsDocIDFromEvent:
// NostrToAMB keys off the d tag being PRESENT and takes its value verbatim, so
// an event carrying {"d",""} gets the real, collidable id `{pubkey}:`, while
// tsDocIDFromEvent treats an empty value as "no d-tag" because content replay
// has no use for it. Deduping on the content key would therefore skip
// resolution for d="" events and leave them on the unconditional
// last-write-wins path this fix exists to close.
//
// Only an event with no d tag at all gets no id — Typesense assigns its own,
// so there is nothing for it to collide with.
func ambDedupKey(event nostr.Event) (string, bool) {
	for _, tag := range event.Tags {
		if len(tag) >= 2 && tag[0] == "d" {
			return typesense30142.GenerateDocumentID(event.PubKey.Hex(), tag[1]), true
		}
	}
	return "", false
}

// structuredDedupKey returns the Typesense document id an event will be
// projected onto, and whether versions of it need resolving at all.
//
// Only addressable kinds need it: their document id is derived from
// (kind, pubkey, d), so BoltDB holding two versions of one address means two
// events landing on one document. Non-addressable kinds in these collections
// (kind 16 shares) key on the event hex id, which is unique per event, so every
// one of them is its own document and must be projected.
//
// An addressable event with no d tag is address d="" per NIP-01, so those
// legitimately share a document id and resolve against each other.
func structuredDedupKey(event nostr.Event) (string, bool) {
	if !event.Kind.IsAddressable() {
		return "", false
	}
	return docIDFor(event.Kind, event.PubKey.Hex(), event.Tags.GetD()), true
}

// reindexStructuredEvents reprojects each event, returning (total, indexed,
// superseded, errs). Pure over its inputs so it is unit-testable without live
// Typesense or BoltDB. Continues past a failing event, mirroring the AMB batch
// path.
//
// Versions of the same addressable document are resolved before projection, for
// the same reason the AMB pass does it: the upsert is unconditional, so without
// this the version reprojected LAST wins whatever its created_at. See
// nostrlib#4.
func reindexStructuredEvents(label string, events iter.Seq[nostr.Event], reproject func(nostr.Event) error) (total, indexed, superseded, errs int64) {
	dedup := typesense30142.NewAddressDedup()
	for event := range events {
		total++
		if docID, addressable := structuredDedupKey(event); addressable && !dedup.Keep(docID, event) {
			superseded++
			continue
		}
		if err := reproject(event); err != nil {
			log.Printf("reindex: %s reproject %s failed: %v", label, event.ID.Hex(), err)
			errs++
			continue
		}
		indexed++
	}
	return
}

// reindexStructured drops+recreates a structured collection then reprojects all
// its BoltDB events into the shared reindex counters.
func (r *Reindexer) reindexStructured(t structuredReindexTarget) {
	if err := t.recreate(); err != nil {
		log.Printf("reindex: recreate %s collection failed: %v", t.label, err)
		r.errors.Add(1)
		return
	}
	total, indexed, superseded, errs := reindexStructuredEvents(
		t.label,
		r.boltDB.QueryEvents(nostr.Filter{Kinds: t.kinds}, reindexMaxEvents),
		t.reproject,
	)
	r.total.Add(total)
	r.indexed.Add(indexed)
	r.superseded.Add(superseded)
	r.errors.Add(errs)

	if t.after != nil {
		patched, errs := t.after()
		r.contentPatched.Add(patched)
		r.errors.Add(errs)
	}
}

// liveContent is a stored content row whose event is still in BoltDB.
type liveContent struct {
	id    string
	entry ContentEntry
}

// walkedEvent is what the BoltDB walk records about one event for the content
// replay: when it was created, and which Typesense document its content row
// belongs on ("" when the event has no usable d-tag).
type walkedEvent struct {
	createdAt nostr.Timestamp
	docID     string
}

// recordWalked builds the content-replay bookkeeping for one walked event.
//
// Extracted from the walk so that "record this event as live" and "record the
// created_at that orders its content row" cannot come apart: dropping the
// timestamp here is a test failure, whereas as a separate assignment in run()
// it was a silent no-op that left the whole suite green.
//
// The error is returned rather than swallowed so the caller can log it, but the
// walkedEvent is still valid and MUST still be stored — the event is live in
// BoltDB either way, and a content row for it is not an orphan. Its docID is
// empty, which replayContent reads as "cannot place this row"; tsDocIDFromEvent
// returns a non-empty id only on success, so "" is unambiguous.
func recordWalked(event nostr.Event) (walkedEvent, error) {
	docID, err := tsDocIDFromEvent(event)
	if err != nil {
		return walkedEvent{createdAt: event.CreatedAt}, err
	}
	return walkedEvent{createdAt: event.CreatedAt, docID: docID}, nil
}

// orderContentReplay sorts content rows into the order they must be patched in,
// so that where several rows land on the same Typesense document the version
// that WINS is patched LAST.
//
// The winner is not simply "the newest". The metadata on that document is
// chosen by AddressDedup.Keep, which defers to nostr.IsOlder: newest
// created_at, and on a created_at TIE the LOWEST event id. The replay has to
// agree with that comparator exactly, or the document ends up with one
// version's metadata and another's fulltext — which is amb-relay#11 itself.
// Hence ascending created_at, then DESCENDING id so the lowest id is last.
//
// Event ids are fixed-length lowercase hex, so comparing the strings is the
// same ordering as bytes.Compare over the decoded ids, which is what IsOlder
// uses.
//
// This is a total order over distinct ids, so sort stability is not
// load-bearing — two rows only compare equal if they are the same event.
//
// Rows whose event was not walked (absent from walked) sort as timestamp 0,
// i.e. first, so a known-newer row still overwrites them.
func orderContentReplay(rows []liveContent, walked map[string]walkedEvent) {
	slices.SortFunc(rows, func(a, b liveContent) int {
		if c := cmp.Compare(walked[a.id].createdAt, walked[b.id].createdAt); c != 0 {
			return c
		}
		return cmp.Compare(b.id, a.id)
	})
}

// replayContent patches every live content row onto its Typesense document, in
// the order orderContentReplay establishes.
//
// That ordering is the fix for amb-relay#11. PatchContent is an unconditional
// PATCH and ContentStore.ForEach walks BoltDB key order — lexicographic by
// event id hex, unrelated to time. So where an address has several stored
// versions and more than one carries a content row, every row was patched onto
// the one shared document in arbitrary order: the document could end up with
// the newest version's metadata (which AddressDedup now guarantees) and an
// older version's fulltext, non-deterministically, on every reindex.
//
// Sorting means the dedup winner's content is the last write and wins.
// Deliberately NOT "patch only the dedup winner": a superseded version may hold
// the only fetched content for that resource, and dropping it would trade a
// wrong answer for a missing one.
//
// Ordering also matters for licensing, not just freshness. When a resource's
// license is not on the allowlist the indexer stores an EMPTY-text row for it
// (amb-indexer worker.go, "Text stripped when license not permissive"). If a
// newer version denies the license but an older permissive version's row is
// patched after it, the document keeps text it is not licensed to expose.
// Winner-last makes that clearing reliable rather than order-dependent.
//
// Separated from run() for testability — the order in which patch is called is
// the whole behaviour, and it is not observable through run().
func replayContent(
	rows []liveContent,
	walked map[string]walkedEvent,
	patch func(docID string, entry ContentEntry) error,
) (patched, errs int64) {
	orderContentReplay(rows, walked)

	for _, row := range rows {
		docID := walked[row.id].docID
		if docID == "" {
			// Event has no d-tag (or iteration failed to record it). The
			// content row stays in BoltDB; a future reindex once the event
			// is re-projected can retry.
			log.Printf("reindex: content patch skipped for %s: no typesense doc id", row.id)
			errs++
			continue
		}
		if err := patch(docID, row.entry); err != nil {
			log.Printf("reindex: content patch failed for %s: %v", row.id, err)
			errs++
			continue
		}
		patched++
	}
	return patched, errs
}

// classifyOrphans returns the content-row ids whose event no longer exists in
// BoltDB at all. Rows whose event exists under ANY kind are kept: non-AMB
// rows (e.g. kind-30040 publication fulltext) belong to another collection,
// whose own reindex target replays them.
func classifyOrphans(candidates []string, kindOf func(string) (nostr.Kind, bool)) (orphans []string) {
	for _, id := range candidates {
		if _, ok := kindOf(id); !ok {
			orphans = append(orphans, id)
		}
	}
	return
}

// GetStatus returns the current reindex status.
func (r *Reindexer) GetStatus() ReindexStatus {
	status := ReindexStatus{
		Running:         r.running.Load(),
		Total:           r.total.Load(),
		Indexed:         r.indexed.Load(),
		Superseded:      r.superseded.Load(),
		Errors:          r.errors.Load(),
		ContentPatched:  r.contentPatched.Load(),
		ContentOrphaned: r.contentOrphaned.Load(),
	}
	if v := r.lastErr.Load(); v != nil {
		if s, ok := v.(string); ok {
			status.Error = s
		}
	}
	return status
}
