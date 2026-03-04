package main

import (
	"log"
	"sync"
	"time"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/eventstore/typesense30142"
)

// TSWriteBuffer queues events and batch-flushes them to Typesense.
// This decouples Typesense indexing from the event acceptance path,
// preventing timeouts during bulk imports.
type TSWriteBuffer struct {
	tsDB          *typesense30142.TSBackend
	ch            chan nostr.Event
	batchSize     int
	flushInterval time.Duration
	done          chan struct{}
	wg            sync.WaitGroup
}

// NewTSWriteBuffer creates and starts a background buffer that batch-flushes
// events to Typesense. Call Close() to drain and shut down.
func NewTSWriteBuffer(tsDB *typesense30142.TSBackend, batchSize int, flushInterval time.Duration) *TSWriteBuffer {
	buf := &TSWriteBuffer{
		tsDB:          tsDB,
		ch:            make(chan nostr.Event, batchSize*100),
		batchSize:     batchSize,
		flushInterval: flushInterval,
		done:          make(chan struct{}),
	}
	buf.wg.Add(1)
	go buf.run()
	return buf
}

// Queue sends an event to the buffer for async Typesense indexing.
func (b *TSWriteBuffer) Queue(event nostr.Event) {
	b.ch <- event
}

func (b *TSWriteBuffer) run() {
	defer b.wg.Done()

	batch := make([]nostr.Event, 0, b.batchSize)
	ticker := time.NewTicker(b.flushInterval)
	defer ticker.Stop()

	for {
		select {
		case event, ok := <-b.ch:
			if !ok {
				// Channel closed — flush remaining and exit
				if len(batch) > 0 {
					b.flush(batch)
				}
				return
			}
			batch = append(batch, event)
			if len(batch) >= b.batchSize {
				b.flush(batch)
				batch = batch[:0]
			}

		case <-ticker.C:
			if len(batch) > 0 {
				b.flush(batch)
				batch = batch[:0]
			}

		case <-b.done:
			// Drain remaining events from channel
			close(b.ch)
			for event := range b.ch {
				batch = append(batch, event)
				if len(batch) >= b.batchSize {
					b.flush(batch)
					batch = batch[:0]
				}
			}
			if len(batch) > 0 {
				b.flush(batch)
			}
			return
		}
	}
}

func (b *TSWriteBuffer) flush(batch []nostr.Event) {
	indexed, errs := b.tsDB.BatchUpsertEvents(batch)
	if len(errs) > 0 {
		for _, err := range errs {
			log.Printf("ts-buffer: batch error: %v", err)
		}
	}
	log.Printf("ts-buffer: flushed %d/%d events", indexed, len(batch))
}

// Close signals the buffer to drain and waits for the background goroutine to finish.
func (b *TSWriteBuffer) Close() {
	close(b.done)
	b.wg.Wait()
}
