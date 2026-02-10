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
	Running bool   `json:"running"`
	Total   int64  `json:"total"`
	Indexed int64  `json:"indexed"`
	Errors  int64  `json:"errors"`
	Error   string `json:"error,omitempty"`
}

type Reindexer struct {
	tsDB   *typesense30142.TSBackend
	boltDB *boltdb.BoltBackend
	mgmt   *ManagementStore

	mu      sync.Mutex
	running atomic.Bool
	total   atomic.Int64
	indexed atomic.Int64
	errors  atomic.Int64
	lastErr atomic.Value // stores string
}

func NewReindexer(tsDB *typesense30142.TSBackend, boltDB *boltdb.BoltBackend, mgmt *ManagementStore) *Reindexer {
	return &Reindexer{
		tsDB:   tsDB,
		boltDB: boltDB,
		mgmt:   mgmt,
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

	// Recreate the collection with the (possibly updated) schema
	if err := r.tsDB.RecreateCollection(schema); err != nil {
		r.lastErr.Store(fmt.Sprintf("failed to recreate collection: %v", err))
		return
	}

	log.Println("reindex: collection recreated, starting event re-indexing with batching")

	// Collect events in batches for efficient bulk upsert
	var batch []nostr.Event

	// Iterate all events from BoltDB
	for event := range r.boltDB.QueryEvents(nostr.Filter{Kinds: []nostr.Kind{30142}}, reindexMaxEvents) {
		r.total.Add(1)
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

	log.Printf("reindex: completed. total=%d indexed=%d errors=%d",
		r.total.Load(), r.indexed.Load(), r.errors.Load())
}

// GetStatus returns the current reindex status.
func (r *Reindexer) GetStatus() ReindexStatus {
	status := ReindexStatus{
		Running: r.running.Load(),
		Total:   r.total.Load(),
		Indexed: r.indexed.Load(),
		Errors:  r.errors.Load(),
	}
	if v := r.lastErr.Load(); v != nil {
		if s, ok := v.(string); ok {
			status.Error = s
		}
	}
	return status
}
