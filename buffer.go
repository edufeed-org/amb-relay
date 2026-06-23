package main

import (
	"log"
	"sync"
	"time"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/eventstore/typesense30142"
)

// stampPatch is a community-field-only PATCH routed through the buffer so it
// serializes AFTER any pending upsert of the same doc — preserving the
// single-writer invariant and never being clobbered by a later flush.
type stampPatch struct {
	DocID       string
	Communities []string
}

// projectionTask is one unit of work:
//   - Stamp != nil → flush the pending batch then apply a community-field PATCH.
//   - Content != nil → flush the batch (including this event) then patch content.
//   - Neither set → event-only metadata projection (batched).
//
// The flush-before-patch property (for both Content and Stamp tasks) is what
// eliminates the dual-writer race against the live indexer / buffer flushes.
type projectionTask struct {
	Event   nostr.Event
	Content *ContentEntry
	Stamp   *stampPatch
}

// projectorWriter is the buffer's interface to Typesense. Production wires
// this to tsDB.BatchUpsertEvents and PatchContent. Tests inject a fake.
type projectorWriter interface {
	Upsert(events []nostr.Event) (indexed int, errs []error)
	Patch(event nostr.Event, content ContentEntry) error
	PatchCommunity(docID string, communities []string) error
}

// TSWriteBuffer queues projection tasks and serializes them through a
// single goroutine. Event-only tasks accumulate into batches sized by
// batchSize; content tasks force-flush the pending batch (including the
// task's event) and then patch.
//
// Single-writer property: this is the only goroutine that talks to
// Typesense for writes. setcontent / refetchcontent must NOT call the
// Typesense PATCH endpoint directly.
type TSWriteBuffer struct {
	writer        projectorWriter
	ch            chan projectionTask
	batchSize     int
	flushInterval time.Duration
	done          chan struct{}
	wg            sync.WaitGroup
}

// newProjectorBuffer is the test-friendly constructor (writer is injected).
func newProjectorBuffer(writer projectorWriter, batchSize int, flushInterval time.Duration) *TSWriteBuffer {
	if batchSize < 1 {
		batchSize = 1
	}
	b := &TSWriteBuffer{
		writer:        writer,
		ch:            make(chan projectionTask, batchSize*100),
		batchSize:     batchSize,
		flushInterval: flushInterval,
		done:          make(chan struct{}),
	}
	b.wg.Add(1)
	go b.run()
	return b
}

// Queue enqueues an event-only projection.
func (b *TSWriteBuffer) Queue(event nostr.Event) {
	b.ch <- projectionTask{Event: event}
}

// QueueContent enqueues a projection that includes a content patch. The
// buffer guarantees Event is upserted into Typesense before Patch runs.
func (b *TSWriteBuffer) QueueContent(event nostr.Event, content ContentEntry) {
	b.ch <- projectionTask{Event: event, Content: &content}
}

// QueueCommunityPatch enqueues a community-field-only PATCH. The buffer
// flushes the pending batch (so any in-flight upsert of this doc lands
// first) before applying the patch — the same flush-before-patch guarantee
// as QueueContent, keeping the buffer the single Typesense writer.
func (b *TSWriteBuffer) QueueCommunityPatch(docID string, communities []string) {
	b.ch <- projectionTask{Stamp: &stampPatch{DocID: docID, Communities: communities}}
}

// Close drains the queue and waits for the worker to finish.
func (b *TSWriteBuffer) Close() {
	close(b.done)
	b.wg.Wait()
}

func (b *TSWriteBuffer) run() {
	defer b.wg.Done()

	batch := make([]nostr.Event, 0, b.batchSize)
	ticker := time.NewTicker(b.flushInterval)
	defer ticker.Stop()

	// flushBatch upserts the in-memory batch. The retry budget lives inside
	// writer.Upsert (productionWriter computes embeddings once and retries
	// only the HTTP call). flushBatch therefore treats one Upsert call as
	// one terminal attempt: on failure it logs and clears the batch — the
	// events are durable in BoltDB and recoverable via reindex. This avoids
	// the embed-amplification cascade that took down the host on 2026-06-11.
	flushBatch := func() {
		if len(batch) == 0 {
			return
		}
		if b.upsertOnce(batch) {
			log.Printf("ts-buffer: dropping %d events after Upsert exhausted retries (data safe in BoltDB, reindex to recover)", len(batch))
		}
		batch = batch[:0]
	}

	drainFlush := func() {
		if len(batch) == 0 {
			return
		}
		if b.upsertOnce(batch) {
			log.Printf("ts-buffer: shutdown flush failed for %d events (data safe in BoltDB, reindex to recover)", len(batch))
		}
		batch = batch[:0]
	}

	// drain is the shutdown path: best-effort, no retries, no sleeps.
	// Closed b.ch must be passed to it; caller is responsible for that.
	drain := func() {
		for task := range b.ch {
			switch {
			case task.Stamp != nil:
				// Community-patch task: flush pending upserts first so any
				// in-flight upsert of this doc lands before the PATCH.
				drainFlush()
				if err := b.writer.PatchCommunity(task.Stamp.DocID, task.Stamp.Communities); err != nil {
					log.Printf("ts-buffer: shutdown community patch %s: %v", task.Stamp.DocID, err)
				}
			case task.Content != nil:
				batch = append(batch, task.Event)
				drainFlush()
				if err := b.writer.Patch(task.Event, *task.Content); err != nil {
					log.Printf("ts-buffer: shutdown patch %s: %v", task.Event.ID.Hex(), err)
				}
			default:
				batch = append(batch, task.Event)
				if len(batch) >= b.batchSize {
					drainFlush()
				}
			}
		}
		drainFlush()
	}

	for {
		// Prioritise the shutdown signal so that tasks queued concurrently
		// with Close() are drained by the best-effort drain path.
		select {
		case <-b.done:
			close(b.ch)
			drain()
			return
		default:
		}

		select {
		case task, ok := <-b.ch:
			if !ok {
				flushBatch()
				return
			}
			switch {
			case task.Stamp != nil:
				// Community-patch task: flush pending upserts first so any
				// in-flight upsert of this doc lands before the PATCH.
				flushBatch()
				if err := b.writer.PatchCommunity(task.Stamp.DocID, task.Stamp.Communities); err != nil {
					log.Printf("ts-buffer: community patch %s: %v", task.Stamp.DocID, err)
				}
			case task.Content != nil:
				// Content task: ensure event is upserted before patching.
				batch = append(batch, task.Event)
				flushBatch()
				if err := b.writer.Patch(task.Event, *task.Content); err != nil {
					log.Printf("ts-buffer: patch %s: %v", task.Event.ID.Hex(), err)
				}
			default:
				batch = append(batch, task.Event)
				if len(batch) >= b.batchSize {
					flushBatch()
				}
			}

		case <-ticker.C:
			flushBatch()

		case <-b.done:
			close(b.ch)
			drain()
			return
		}
	}
}

// upsertOnce returns true if the entire batch failed (retryable), false
// on success or partial-failure (some indexed, some conversion errors).
func (b *TSWriteBuffer) upsertOnce(batch []nostr.Event) (failed bool) {
	indexed, errs := b.writer.Upsert(batch)
	if len(errs) > 0 {
		for _, err := range errs {
			log.Printf("ts-buffer: batch error: %v", err)
		}
	}
	if indexed == 0 && len(errs) > 0 {
		log.Printf("ts-buffer: flush failed for %d events, will retry", len(batch))
		return true
	}
	log.Printf("ts-buffer: flushed %d/%d events", indexed, len(batch))
	return false
}

// productionWriter wires the buffer to the real Typesense backend.
//
// Upsert prepares the batch (NostrToAMB + embed) ONCE, then retries only the
// HTTP upsert against transient TS errors. This is the critical property the
// 2026-06-11 incident postmortem demanded: under TS WAL replay 503s, retries
// must not re-fire the embed service. The retry budget is generous (5 attempts,
// 1s→16s backoff ≈ 31s) so the buffer itself doesn't need a second retry
// layer — see flushBatch.
type productionWriter struct {
	tsDB                  *typesense30142.TSBackend
	host, apiKey, colName string
}

const (
	upsertHTTPMaxRetries = 5
	upsertHTTPMaxBackoff = 16 * time.Second
)

func (p *productionWriter) Upsert(events []nostr.Event) (int, []error) {
	docs, prepErrs := p.tsDB.PrepareBatch(events)
	if len(docs) == 0 {
		return 0, prepErrs
	}

	var indexed int
	var lastErrs []error
	for attempt := 0; attempt <= upsertHTTPMaxRetries; attempt++ {
		indexed, lastErrs = p.tsDB.BatchUpsertDocs(docs)
		if indexed > 0 || len(lastErrs) == 0 {
			break
		}
		if attempt == upsertHTTPMaxRetries {
			break
		}
		backoff := time.Duration(1<<attempt) * time.Second
		if backoff > upsertHTTPMaxBackoff {
			backoff = upsertHTTPMaxBackoff
		}
		time.Sleep(backoff)
	}

	if len(prepErrs) > 0 {
		lastErrs = append(prepErrs, lastErrs...)
	}
	return indexed, lastErrs
}

func (p *productionWriter) Patch(event nostr.Event, content ContentEntry) error {
	docID, err := tsDocIDFromEvent(event)
	if err != nil {
		return err
	}
	return PatchContent(p.host, p.apiKey, p.colName, docID, content)
}

func (p *productionWriter) PatchCommunity(docID string, communities []string) error {
	if communities == nil {
		communities = []string{}
	}
	return patchDoc(p.host, p.apiKey, p.colName, docID, map[string]any{"community": communities})
}

// NewTSWriteBuffer is the production constructor used by main.go.
func NewTSWriteBuffer(tsDB *typesense30142.TSBackend, batchSize int, flushInterval time.Duration) *TSWriteBuffer {
	w := &productionWriter{
		tsDB:    tsDB,
		host:    tsDB.Host,
		apiKey:  tsDB.ApiKey,
		colName: tsDB.CollectionName,
	}
	return newProjectorBuffer(w, batchSize, flushInterval)
}
