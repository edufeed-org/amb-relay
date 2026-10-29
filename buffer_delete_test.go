package main

import (
	"sync"
	"testing"
	"time"

	"fiatjaf.com/nostr"
)

// blockSet is a concurrency-safe stand-in for ManagementStore.isBlocked.
type blockSet struct {
	mu  sync.Mutex
	ids map[nostr.ID]bool
}

func (b *blockSet) add(id nostr.ID) { b.mu.Lock(); b.ids[id] = true; b.mu.Unlock() }
func (b *blockSet) has(ev nostr.Event) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.ids[ev.ID]
}

// Production incident 2026-10-29: banevent deleted the Typesense doc
// directly while the event's projection was still queued in the buffer; the
// later flush re-created the doc, so the banned event stayed searchable. The
// flush must re-check the gate and drop events blocked since they were queued.
func TestTSWriteBuffer_FlushDropsEventsBlockedAfterQueueing(t *testing.T) {
	w := &fakeWriter{}
	blocked := &blockSet{ids: map[nostr.ID]bool{}}
	buf := newProjectorBufferWithSkip(w, 10, time.Hour, blocked.has)

	sk := nostr.Generate()
	keep := unsignedEvent(sk, "keep")
	banned := unsignedEvent(sk, "banned")
	content := unsignedEvent(sk, "content")
	buf.Queue(keep)
	buf.Queue(banned)
	blocked.add(banned.ID) // ban lands while both are still queued
	blocked.add(content.ID)
	buf.QueueContent(content, ContentEntry{Text: "x"}) // forces a flush of the batch
	buf.Close()

	w.mu.Lock()
	defer w.mu.Unlock()
	for _, b := range w.upsertBatches {
		for _, id := range b {
			if id == banned.ID || id == content.ID {
				t.Fatalf("blocked event %s was upserted after being blocked", id.Hex()[:8])
			}
		}
	}
	if len(w.patches) != 0 {
		t.Fatalf("content patch for a blocked event must be dropped, got %v", w.patches)
	}
	var sawKeep bool
	for _, b := range w.upsertBatches {
		for _, id := range b {
			sawKeep = sawKeep || id == keep.ID
		}
	}
	if !sawKeep {
		t.Fatal("unblocked event must still be upserted")
	}
}

// QueueDelete serializes a delete after every projection queued before it,
// so an in-flight upsert can never land after the delete.
func TestTSWriteBuffer_QueueDeleteRunsAfterPendingUpserts(t *testing.T) {
	w := &fakeWriter{}
	buf := newProjectorBuffer(w, 10, time.Hour)

	sk := nostr.Generate()
	ev := unsignedEvent(sk, "doc")
	buf.Queue(ev)
	buf.QueueDelete(ev.ID)
	waitFor(t, 2*time.Second, func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		return len(w.deletes) == 1
	})
	buf.Close()

	w.mu.Lock()
	defer w.mu.Unlock()
	want := []string{"upsert:doc", "delete:" + ev.ID.Hex()[:8]}
	if len(w.ops) != 2 || w.ops[0] != want[0] || w.ops[1] != want[1] {
		t.Fatalf("ops = %v, want %v", w.ops, want)
	}
}

// The Bolt buffer applies the same gate at write time: an event banned or
// tombstoned between OnEvent and the buffered write must not reach Bolt.
func TestBoltWriteBuffer_SkipsEventsBlockedAfterQueueing(t *testing.T) {
	w := &fakeBoltWriter{}
	blocked := &blockSet{ids: map[nostr.ID]bool{}}
	sk := nostr.Generate()
	banned := unsignedEvent(sk, "banned")
	keep := unsignedEvent(sk, "keep")
	blocked.add(banned.ID)

	buf := NewBoltWriteBufferWithSkip(w, blocked.has)
	buf.Queue(banned, true)
	buf.Queue(keep, false)
	buf.Close()

	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.replaceCalls) != 0 {
		t.Fatalf("blocked event reached Bolt: %v", w.replaceCalls)
	}
	if len(w.saveCalls) != 1 || w.saveCalls[0].ID != keep.ID {
		t.Fatalf("saveCalls = %v, want only the unblocked event", w.saveCalls)
	}
}
