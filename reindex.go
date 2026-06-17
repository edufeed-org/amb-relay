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
}

type ReindexStatus struct {
	Running         bool   `json:"running"`
	Total           int64  `json:"total"`
	Indexed         int64  `json:"indexed"`
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
	errors          atomic.Int64
	contentPatched  atomic.Int64
	contentOrphaned atomic.Int64
	lastErr         atomic.Value // stores string
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

	// Iterate all events from BoltDB
	for event := range r.boltDB.QueryEvents(nostr.Filter{Kinds: []nostr.Kind{30142}}, reindexMaxEvents) {
		r.total.Add(1)
		eventIDHex := event.ID.Hex()
		liveEventIDs[eventIDHex] = struct{}{}
		if docID, err := tsDocIDFromEvent(event); err == nil {
			liveDocIDs[eventIDHex] = docID
		} else {
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
	var orphans []string
	var liveRows []liveContent

	_ = r.content.ForEach(func(eventID string, entry ContentEntry) error {
		if _, alive := liveEventIDs[eventID]; !alive {
			orphans = append(orphans, eventID)
		} else {
			liveRows = append(liveRows, liveContent{id: eventID, entry: entry})
		}
		return nil
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

	log.Printf("reindex: completed. total=%d indexed=%d errors=%d content_patched=%d content_orphaned=%d",
		r.total.Load(), r.indexed.Load(), r.errors.Load(),
		r.contentPatched.Load(), r.contentOrphaned.Load())
}

// reindexStructuredEvents reprojects each event, returning (total, indexed,
// errs). Pure over its inputs so it is unit-testable without live Typesense or
// BoltDB. Continues past a failing event, mirroring the AMB batch path.
func reindexStructuredEvents(label string, events iter.Seq[nostr.Event], reproject func(nostr.Event) error) (total, indexed, errs int64) {
	for event := range events {
		total++
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
	total, indexed, errs := reindexStructuredEvents(
		t.label,
		r.boltDB.QueryEvents(nostr.Filter{Kinds: t.kinds}, reindexMaxEvents),
		t.reproject,
	)
	r.total.Add(total)
	r.indexed.Add(indexed)
	r.errors.Add(errs)
}

// GetStatus returns the current reindex status.
func (r *Reindexer) GetStatus() ReindexStatus {
	status := ReindexStatus{
		Running:         r.running.Load(),
		Total:           r.total.Load(),
		Indexed:         r.indexed.Load(),
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
