package main

import (
	"log"
	"sync"
	"time"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/eventstore/boltdb"
)

// BoltWriteBuffer queues BoltDB writes and processes them in a background
// goroutine. Events are processed one-by-one (no batch API in BoltDB) and
// sequentially to correctly handle same-d-tag replacements.
type BoltWriteBuffer struct {
	boltDB *boltdb.BoltBackend
	ch     chan boltOp
	done   chan struct{}
	wg     sync.WaitGroup
}

type boltOp struct {
	event   nostr.Event
	replace bool // true = ReplaceEvent, false = SaveEvent
}

const boltBufSize = 10_000

// NewBoltWriteBuffer creates and starts a background buffer that processes
// BoltDB writes sequentially. Call Close() to drain and shut down.
func NewBoltWriteBuffer(boltDB *boltdb.BoltBackend) *BoltWriteBuffer {
	buf := &BoltWriteBuffer{
		boltDB: boltDB,
		ch:     make(chan boltOp, boltBufSize),
		done:   make(chan struct{}),
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

		backoff := time.Duration(1<<attempt) * time.Second
		if backoff > 16*time.Second {
			backoff = 16 * time.Second
		}
		log.Printf("bolt-buffer: write failed (attempt %d/%d): %v, retrying in %v", attempt+1, boltMaxRetries, err, backoff)
		time.Sleep(backoff)
	}
	log.Printf("bolt-buffer: dropping event %s after %d retries (client can re-send)", op.event.ID, boltMaxRetries)
}

// Close signals the buffer to drain and waits for the background goroutine to finish.
func (b *BoltWriteBuffer) Close() {
	close(b.done)
	b.wg.Wait()
}
