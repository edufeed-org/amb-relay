package main

import (
	"errors"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/eventstore"
)

// boltEventWriter is the subset of *boltdb.BoltBackend the buffer needs.
// Extracted so tests can inject a fake without touching real BoltDB storage;
// *boltdb.BoltBackend satisfies this implicitly.
type boltEventWriter interface {
	SaveEvent(nostr.Event) error
	ReplaceEvent(nostr.Event) ([]nostr.Event, error)
}

// BoltWriteBuffer queues BoltDB writes and processes them in a background
// goroutine. Events are processed one-by-one (no batch API in BoltDB) and
// sequentially to correctly handle same-d-tag replacements.
type BoltWriteBuffer struct {
	boltDB  boltEventWriter
	ch      chan boltOp
	done    chan struct{}
	closed  atomic.Bool
	wg      sync.WaitGroup
	sleepFn func(time.Duration)

	// drainDeadline bounds Close()'s drain loop; defaults to
	// boltDrainDeadline. Overridable (set directly on the struct before
	// calling Close) so tests can exercise deadline-abandonment without a
	// 45s wait.
	drainDeadline time.Duration
}

type boltOp struct {
	event   nostr.Event
	replace bool // true = ReplaceEvent, false = SaveEvent
}

const boltBufSize = 10_000

// boltDrainDeadline bounds Close()'s drain loop. process() can itself block
// up to ~31s per op on transient-error retries (boltMaxRetries backoff), so
// without a cap a deep backlog against a wedged BoltDB could stretch
// shutdown far past any reasonable stop_grace_period. On expiry the drain
// loop logs whatever depth remains abandoned in the channel — those events
// are not durably written, but the client got no OK for them (see Queue)
// and reindex/HYDRATE_ON_START is the recovery path regardless.
const boltDrainDeadline = 45 * time.Second

// NewBoltWriteBuffer creates and starts a background buffer that processes
// BoltDB writes sequentially. Call Close() to drain and shut down.
func NewBoltWriteBuffer(boltDB boltEventWriter) *BoltWriteBuffer {
	buf := &BoltWriteBuffer{
		boltDB:        boltDB,
		ch:            make(chan boltOp, boltBufSize),
		done:          make(chan struct{}),
		sleepFn:       time.Sleep,
		drainDeadline: boltDrainDeadline,
	}
	buf.wg.Add(1)
	go buf.run()
	return buf
}

// Queue sends an event to the buffer for async BoltDB persistence. Returns
// false if the buffer has been (or is being) closed and the event was
// logged-and-dropped instead of enqueued.
//
// relay.StoreEvent/ReplaceEvent (main.go) call Queue BEFORE khatru sends the
// NIP-01 OK reply (see khatru/adding.go's handleNormal: it turns a non-nil
// StoreEvent/ReplaceEvent error into OK=false), so a false return here is
// wired to a non-nil error there — the client gets OK=false and can retry,
// rather than a false-positive OK for an event that was never queued.
func (b *BoltWriteBuffer) Queue(event nostr.Event, replace bool) bool {
	if b.closed.Load() {
		log.Printf("bolt-buffer: dropping event %s, buffer closed for shutdown; client should retry", event.ID)
		return false
	}
	b.ch <- boltOp{event: event, replace: replace}
	return true
}

const boltMaxRetries = 5

func (b *BoltWriteBuffer) run() {
	defer b.wg.Done()

	for {
		select {
		case op, ok := <-b.ch:
			if !ok {
				return
			}
			b.process(op)

		case <-b.done:
			b.drain()
			return
		}
	}
}

// drain is the shutdown path. b.ch is NEVER closed — closing it here would
// race any producer still alive past Close() (e.g. a hijacked websocket
// goroutine that http.Server.Shutdown does not wait for), and a Queue()
// send landing on a closed channel panics. Instead b.ch is read with
// select/default in a loop: the common case (queue already drained or
// near-empty) returns as soon as the channel reports empty.
//
// Each op is processed via runBounded against b.drainDeadline, so even a
// BoltDB call that hangs forever — not just the ~31s retry-backoff worst
// case — can't stretch shutdown out unboundedly; on expiry the remaining
// queue depth is logged and the drain abandoned, leaving the timed-out
// op's goroutine to finish (or hang) on its own.
//
// Race safety: Close() sets b.closed BEFORE closing b.done (set-then-drain),
// so any Queue() call that observes closed==true never sends to b.ch at
// all. A call that read closed==false a moment earlier and is concurrently
// blocked on `b.ch <- op` still succeeds — the channel is never closed, so
// that send can never panic. It either lands here and gets drained (the
// select/default loop keeps consuming until the channel is genuinely
// empty), or, in the rare case it arrives after this loop has already
// concluded the channel is empty and returned, it sits unconsumed until
// process exit — harmless, since the caller already got a non-OK reply
// (or, for the stamp-fetch `store` callback in main.go, is not tied to any
// client reply at all).
func (b *BoltWriteBuffer) drain() {
	deadline := time.Now().Add(b.drainDeadline)
	for {
		select {
		case op := <-b.ch:
			if !runBounded(deadline, func() { b.process(op) }) {
				log.Printf("bolt-buffer: drain deadline (%s) exceeded while processing a write, abandoning (client got no OK for these; can re-send); %d further write(s) still queued", b.drainDeadline, len(b.ch))
				return
			}
		default:
			return
		}
	}
}

// permanentRejectionPrefixes are NIP-01 OK-message prefixes that can never
// succeed on retry. ReplaceEvent itself never surfaces "already exists" or
// "newer version exists" as an error (it silently no-ops, see boltdb's
// ReplaceEvent), so in practice only SaveEvent's ErrDupEvent and genuine
// validation-style errors hit this path — but we classify defensively.
var permanentRejectionPrefixes = []string{"duplicate:", "blocked:", "invalid:"}

func isPermanentRejection(err error) bool {
	msg := err.Error()
	for _, prefix := range permanentRejectionPrefixes {
		if strings.HasPrefix(msg, prefix) {
			return true
		}
	}
	return false
}

func (b *BoltWriteBuffer) process(op boltOp) {
	for attempt := range boltMaxRetries {
		var err error
		if op.replace {
			_, err = b.boltDB.ReplaceEvent(op.event)
		} else {
			err = b.boltDB.SaveEvent(op.event)
		}
		if err == nil {
			return
		}

		if errors.Is(err, eventstore.ErrDupEvent) {
			// Routine: khatru's pre-write duplicate check can miss under
			// Typesense projection lag, so the event is already in Bolt by
			// the time we get here. Success-equivalent — not worth a retry
			// or a log line.
			return
		}

		if isPermanentRejection(err) {
			// Can never succeed no matter how many times we retry it.
			log.Printf("bolt-buffer: dropping event %s, permanent rejection: %v", op.event.ID, err)
			return
		}

		backoff := time.Duration(1<<attempt) * time.Second
		if backoff > 16*time.Second {
			backoff = 16 * time.Second
		}
		log.Printf("bolt-buffer: write failed (attempt %d/%d): %v, retrying in %v", attempt+1, boltMaxRetries, err, backoff)
		b.sleepFn(backoff)
	}
	log.Printf("bolt-buffer: dropping event %s after %d retries (client can re-send)", op.event.ID, boltMaxRetries)
}

// Close signals the buffer to drain (bounded by boltDrainDeadline) and waits
// for the background goroutine to finish. Set-then-drain: closed is stored
// BEFORE done is closed, so the drain-loop race described on drain() cannot
// slip a send past a closed channel — there is no closed channel to slip
// past, by construction.
func (b *BoltWriteBuffer) Close() {
	b.closed.Store(true)
	close(b.done)
	b.wg.Wait()
}
