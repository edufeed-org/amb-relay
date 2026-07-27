package main

import (
	"errors"
	"log"
	"strings"
	"sync"
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
	wg      sync.WaitGroup
	sleepFn func(time.Duration)
}

type boltOp struct {
	event   nostr.Event
	replace bool // true = ReplaceEvent, false = SaveEvent
}

const boltBufSize = 10_000

// NewBoltWriteBuffer creates and starts a background buffer that processes
// BoltDB writes sequentially. Call Close() to drain and shut down.
func NewBoltWriteBuffer(boltDB boltEventWriter) *BoltWriteBuffer {
	buf := &BoltWriteBuffer{
		boltDB:  boltDB,
		ch:      make(chan boltOp, boltBufSize),
		done:    make(chan struct{}),
		sleepFn: time.Sleep,
	}
	buf.wg.Add(1)
	go buf.run()
	return buf
}

// Queue sends an event to the buffer for async BoltDB persistence.
func (b *BoltWriteBuffer) Queue(event nostr.Event, replace bool) {
	b.ch <- boltOp{event: event, replace: replace}
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
			close(b.ch)
			for op := range b.ch {
				b.process(op)
			}
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

// Close signals the buffer to drain and waits for the background goroutine to finish.
func (b *BoltWriteBuffer) Close() {
	close(b.done)
	b.wg.Wait()
}
