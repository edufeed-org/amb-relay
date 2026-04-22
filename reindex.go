package main

import (
	"fmt"
	"log"
	"sync"
	"sync/atomic"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/eventstore/boltdb"
	"fiatjaf.com/nostr/eventstore/typesense30142"
)

const (
	reindexMaxEvents  = 10_000_000
	reindexBatchSize  = 100
)

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

	mu              sync.Mutex
	running         atomic.Bool
	total           atomic.Int64
	indexed         atomic.Int64
	errors          atomic.Int64
	contentPatched  atomic.Int64
	contentOrphaned atomic.Int64
	lastErr         atomic.Value // stores string
}

func NewReindexer(tsDB *typesense30142.TSBackend, boltDB *boltdb.BoltBackend, mgmt *ManagementStore, content *ContentStore) *Reindexer {
	return &Reindexer{
		tsDB:    tsDB,
		boltDB:  boltDB,
		mgmt:    mgmt,
		content: content,
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

	// Iterate all events from BoltDB
	for event := range r.boltDB.QueryEvents(nostr.Filter{Kinds: []nostr.Kind{30142}}, reindexMaxEvents) {
		r.total.Add(1)
		liveEventIDs[event.ID.Hex()] = struct{}{}
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
		if err := PatchContent(r.tsDB.Host, r.tsDB.ApiKey, r.tsDB.CollectionName, row.id, row.entry); err != nil {
			log.Printf("reindex: content patch failed for %s: %v", row.id, err)
			r.errors.Add(1)
			continue
		}
		r.contentPatched.Add(1)
	}

	log.Printf("reindex: completed. total=%d indexed=%d errors=%d content_patched=%d content_orphaned=%d",
		r.total.Load(), r.indexed.Load(), r.errors.Load(),
		r.contentPatched.Load(), r.contentOrphaned.Load())
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
