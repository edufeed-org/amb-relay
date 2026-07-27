package main

import (
	"context"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
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
	closed        atomic.Bool
	wg            sync.WaitGroup

	// drainDeadline bounds Close()'s drain loop; defaults to
	// tsDrainDeadline. Overridable (set directly on the struct before
	// calling Close — see the set-then-drain ordering argument on Close)
	// so tests can exercise deadline-abandonment without a 90s wait.
	drainDeadline time.Duration
}

// tsDrainDeadline bounds Close()'s drain loop. A single flush can itself
// retry for up to ~31s (upsertHTTPMaxRetries/upsertHTTPMaxBackoff), and a
// backlog of stamp/content tasks can each force their own flush — the
// review's nested-retry worst case put this at ~12.7min/flush against a
// hanging Typesense. drain() runs each drained task (or the final flush) in
// a goroutine raced against this deadline (see runBounded), so even a
// writer call that never returns at all can't hold shutdown open past it;
// on expiry the remaining queue depth is logged and the drain abandoned
// (data safe in BoltDB, reindex is the recovery path).
const tsDrainDeadline = 90 * time.Second

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
		drainDeadline: tsDrainDeadline,
	}
	b.wg.Add(1)
	go b.run()
	return b
}

// Queue enqueues an event-only projection. A no-op (logged) once the buffer
// is closing/closed — see Close and run's drain for why: the event is
// already durable in BoltDB by the time anything reaches this buffer, so
// dropping the Typesense projection here loses search-index freshness, not
// data; reindex recovers it.
func (b *TSWriteBuffer) Queue(event nostr.Event) {
	if b.closed.Load() {
		log.Printf("ts-buffer: dropping event %s projection, buffer closed for shutdown (data safe in BoltDB, reindex recovers)", event.ID.Hex())
		return
	}
	b.ch <- projectionTask{Event: event}
}

// QueueContent enqueues a projection that includes a content patch. The
// buffer guarantees Event is upserted into Typesense before Patch runs.
func (b *TSWriteBuffer) QueueContent(event nostr.Event, content ContentEntry) {
	if b.closed.Load() {
		log.Printf("ts-buffer: dropping content patch for %s, buffer closed for shutdown", event.ID.Hex())
		return
	}
	b.ch <- projectionTask{Event: event, Content: &content}
}

// QueueCommunityPatch enqueues a community-field-only PATCH. The buffer
// flushes the pending batch (so any in-flight upsert of this doc lands
// first) before applying the patch — the same flush-before-patch guarantee
// as QueueContent, keeping the buffer the single Typesense writer.
func (b *TSWriteBuffer) QueueCommunityPatch(docID string, communities []string) {
	if b.closed.Load() {
		log.Printf("ts-buffer: dropping community patch for %s, buffer closed for shutdown", docID)
		return
	}
	b.ch <- projectionTask{Stamp: &stampPatch{DocID: docID, Communities: communities}}
}

// Close signals shutdown and waits for the drain (bounded by
// tsDrainDeadline, see run) to finish.
//
// Set-then-drain: b.closed is stored BEFORE b.done is closed, so any
// Queue*/QueueContent/QueueCommunityPatch call that observes closed==true
// after this point never touches b.ch — it logs and returns instead of
// sending. b.ch itself is NEVER closed anywhere (see run's drain): closing
// it would panic any producer still alive past this call — a hijacked
// websocket goroutine http.Server.Shutdown does not wait for, an in-flight
// CommunityStamper reconcile (up to 60s), or a late NIP-86 setcontent — if
// it sends after close(). Without a channel close to race, a caller that
// read closed==false a moment before this Store and is concurrently
// blocked inside `b.ch <- task` still succeeds (the channel is open), and
// that task is picked up by the drain loop's select/default read exactly
// like any other queued task — it is never a send onto a closed channel,
// so it can never panic.
func (b *TSWriteBuffer) Close() {
	b.closed.Store(true)
	close(b.done)
	b.wg.Wait()
}

func (b *TSWriteBuffer) run() {
	defer b.wg.Done()

	batch := make([]nostr.Event, 0, b.batchSize)
	// pendingDocIDs tracks the Typesense doc id of every event currently
	// sitting in `batch`, unflushed. A community-patch (Stamp) task only
	// needs to force a flush when ITS target doc is in this set — i.e. its
	// own upsert hasn't landed yet, so an unordered PATCH could be clobbered
	// by the later flush (which re-derives `community` from the event's own
	// h-tags only). When the target doc isn't pending, its upsert already
	// landed via an earlier flush (batchSize/interval cadence), so the patch
	// can apply immediately with no flush at all — this is what let stamp
	// tasks collapse batching to 1-event upserts under stamper load
	// (root-cause 2026-07-28: 67 imports : 79 PATCHes in 24s).
	pendingDocIDs := make(map[string]struct{}, b.batchSize)
	ticker := time.NewTicker(b.flushInterval)
	defer ticker.Stop()

	addToBatch := func(event nostr.Event) {
		batch = append(batch, event)
		if docID, err := tsDocIDFromEvent(event); err == nil {
			pendingDocIDs[docID] = struct{}{}
		}
	}

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
		pendingDocIDs = make(map[string]struct{}, b.batchSize)
	}

	// process applies one task's effect. Shared by the live loop and the
	// shutdown drain loop below, so both get the identical
	// flush-before-patch ordering guarantee.
	process := func(task projectionTask) {
		switch {
		case task.Stamp != nil:
			// Community-patch task: only force a flush when this doc's
			// own upsert is still pending in the batch (see
			// pendingDocIDs comment above) — riding the normal
			// batchSize/interval cadence otherwise. The interval ticker
			// still bounds worst-case latency for whatever partial
			// batch is left once the queue goes idle.
			if _, pending := pendingDocIDs[task.Stamp.DocID]; pending {
				flushBatch()
			}
			if err := b.writer.PatchCommunity(task.Stamp.DocID, task.Stamp.Communities); err != nil {
				log.Printf("ts-buffer: community patch %s: %v", task.Stamp.DocID, err)
			}
		case task.Content != nil:
			// Content task: ensure event is upserted before patching.
			// Always forced — the event was just appended by THIS task,
			// so it is always the pending doc; there is no cadence to
			// ride without breaking the upsert-before-patch guarantee.
			addToBatch(task.Event)
			flushBatch()
			if err := b.writer.Patch(task.Event, *task.Content); err != nil {
				log.Printf("ts-buffer: patch %s: %v", task.Event.ID.Hex(), err)
			}
		default:
			addToBatch(task.Event)
			if len(batch) >= b.batchSize {
				flushBatch()
			}
		}
	}

	// drain is the shutdown path. b.ch is NEVER closed (see Close — closing
	// it here would panic any producer still alive past shutdown). Instead
	// it is read with select/default in a loop: the common case — queue
	// already empty/near-empty — falls straight to the default branch below
	// and returns immediately after a final flush of whatever partial batch
	// remains.
	//
	// Each processed task (and the final flush) runs through runBounded
	// against b.drainDeadline, so even a writer call that hangs forever —
	// not just the ~31s upsert-retry worst case — can't hold Close() open
	// indefinitely; a backlog of stamp/content tasks that would otherwise
	// each force their own flush (the review's nested-retry worst case,
	// ~12.7min/flush against a hanging Typesense) is bounded to this one
	// deadline total. On expiry the remaining queue depth is logged and the
	// drain abandoned (data safe in BoltDB, reindex is the recovery path);
	// the goroutine running the timed-out task/flush is left to finish (or
	// hang) on its own — harmless, since nothing else touches its captured
	// state once drain returns and the process is exiting anyway.
	drain := func() {
		deadline := time.Now().Add(b.drainDeadline)
		for {
			select {
			case task := <-b.ch:
				if !runBounded(deadline, func() { process(task) }) {
					log.Printf("ts-buffer: drain deadline (%s) exceeded while processing a task, abandoning (data safe in BoltDB, reindex to recover); %d further task(s) still queued", b.drainDeadline, len(b.ch))
					return
				}
			default:
				if !runBounded(deadline, flushBatch) {
					log.Printf("ts-buffer: drain deadline (%s) exceeded during final flush, abandoning (data safe in BoltDB, reindex to recover)", b.drainDeadline)
				}
				return
			}
		}
	}

	for {
		// Prioritise the shutdown signal so that tasks queued concurrently
		// with Close() are drained by the best-effort drain path.
		select {
		case <-b.done:
			drain()
			return
		default:
		}

		select {
		case task := <-b.ch:
			process(task)

		case <-ticker.C:
			flushBatch()

		case <-b.done:
			drain()
			return
		}
	}
}

// runBounded runs fn in a goroutine and reports whether it completed before
// deadline. If the deadline elapses first, fn's goroutine is abandoned
// (leaked) rather than waited on — there is no way to preempt a synchronous
// HTTP/BoltDB call mid-flight, and the caller (drain, in both buffers) is
// about to return and let the process exit anyway, so a leaked goroutine
// blocked in, say, a permanently-hanging Typesense call is harmless.
func runBounded(deadline time.Time, fn func()) bool {
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return false
	}
	done := make(chan struct{})
	go func() {
		fn()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(remaining):
		return false
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

	// maxEmbedBatchSize bounds each call to the embed service. The service
	// accepts a list of texts and returns one vector per text; batching
	// amortizes HTTP + model-forward overhead across the whole flush batch
	// instead of paying it once per event (see prepareBatch).
	maxEmbedBatchSize = 32

	// embedTimeout bounds a single (batched) embed HTTP call.
	embedTimeout = 30 * time.Second
)

func (p *productionWriter) Upsert(events []nostr.Event) (int, []error) {
	docs, prepErrs := prepareBatch(p.tsDB, events)
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

// prepareBatch converts events to AMBMetadata docs and computes embeddings
// with BOUNDED-BATCH calls to the embed service, instead of nostrlib
// PrepareBatch's one-embed-call-per-event loop (typesense30142/replace.go).
// That per-event loop was the root cause of the 2026-07-28 minutes-deep
// TSWriteBuffer backlog: a flush batch of N events issued N blocking 30s-
// timeout HTTP calls to the embed service serially, on the buffer's single
// worker goroutine. Batching collapses that to ceil(N/maxEmbedBatchSize)
// calls. Kept relay-side (not in nostrlib) because the seam already exists
// here: NostrToAMB and the embedder fields are exported, and amb-relay
// already carries its own BuildEmbedText (embedding.go) mirroring nostrlib's
// private buildEmbedText.
func prepareBatch(tsDB *typesense30142.TSBackend, events []nostr.Event) (docs []*typesense30142.AMBMetadata, errs []error) {
	if len(events) == 0 {
		return nil, nil
	}
	docs = make([]*typesense30142.AMBMetadata, 0, len(events))
	for _, event := range events {
		ambData, err := typesense30142.NostrToAMB(&event)
		if err != nil {
			errs = append(errs, fmt.Errorf("event %s: convert error: %w", event.ID.Hex(), err))
			continue
		}
		docs = append(docs, ambData)
	}

	if tsDB.Embedder == nil || len(tsDB.EmbedFields) == 0 || len(docs) == 0 {
		return docs, errs
	}

	// Collect embed texts alongside the doc index they belong to — docs with
	// an empty embed text (e.g. no configured fields populated) are skipped,
	// exactly like the per-event path did.
	type pendingEmbed struct {
		docIdx int
		text   string
	}
	pending := make([]pendingEmbed, 0, len(docs))
	for i, doc := range docs {
		text := BuildEmbedText(doc, tsDB.EmbedFields)
		if text != "" {
			pending = append(pending, pendingEmbed{docIdx: i, text: text})
		}
	}

	for start := 0; start < len(pending); start += maxEmbedBatchSize {
		end := start + maxEmbedBatchSize
		if end > len(pending) {
			end = len(pending)
		}
		chunk := pending[start:end]

		texts := make([]string, len(chunk))
		for i, p := range chunk {
			texts[i] = p.text
		}

		ctx, cancel := context.WithTimeout(context.Background(), embedTimeout)
		embeddings, err := tsDB.Embedder.Embed(ctx, texts, typesense30142.EmbedPassage)
		cancel()
		if err != nil {
			// Graceful degradation, same contract as the per-event path: the
			// docs in this chunk upsert without a vector rather than failing
			// the whole batch.
			log.Printf("Warning: batch embedding failed for %d events: %v", len(chunk), err)
			continue
		}
		for i, p := range chunk {
			if i < len(embeddings) {
				docs[p.docIdx].Embedding = embeddings[i]
			}
		}
	}

	return docs, errs
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
