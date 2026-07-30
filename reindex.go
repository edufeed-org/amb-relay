package main

import (
	"fmt"
	"iter"
	"log"
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
	liveEventIDs := make(map[string]struct{}, 1024)

	// Track the Typesense document id for each live event so we can
	// reproject content rows keyed by event hex id back to {pubkey}:{d-tag}.
	liveDocIDs := make(map[string]string)

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
		liveEventIDs[eventIDHex] = struct{}{}
		if docID, err := tsDocIDFromEvent(event); err == nil {
			liveDocIDs[eventIDHex] = docID
			if !dedup.Keep(docID, event) {
				r.superseded.Add(1)
				continue
			}
		} else {
			// No d-tag: nothing to dedupe on, and no document id to collide
			// with either, so it is projected exactly as before.
			log.Printf("reindex: skipping content projection for %s: %v", eventIDHex, err)
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
	type liveContent struct {
		id    string
		entry ContentEntry
	}
	var orphanCandidates []string
	var liveRows []liveContent

	_ = r.content.ForEach(func(eventID string, entry ContentEntry) error {
		if _, alive := liveEventIDs[eventID]; !alive {
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
	for _, row := range liveRows {
		docID, ok := liveDocIDs[row.id]
		if !ok {
			// Event has no d-tag (or iteration failed to record it). The
			// content row stays in BoltDB; a future reindex once the event
			// is re-projected can retry.
			log.Printf("reindex: content patch skipped for %s: no typesense doc id", row.id)
			r.errors.Add(1)
			continue
		}
		if err := PatchContent(r.tsDB.Host, r.tsDB.ApiKey, r.tsDB.CollectionName, docID, row.entry); err != nil {
			log.Printf("reindex: content patch failed for %s: %v", row.id, err)
			r.errors.Add(1)
			continue
		}
		r.contentPatched.Add(1)
	}

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
