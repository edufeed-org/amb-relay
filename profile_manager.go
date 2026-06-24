package main

import (
	"context"
	"fmt"
	"iter"
	"time"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/sdk"
)

// profileDrainTimeout bounds a single drain's network fetch, matching the
// AllowlistManager refresh timeout.
const profileDrainTimeout = 30 * time.Second

// profileQueue is the durable candidate queue ProfileManager drains.
// *ManagementStore satisfies it.
type profileQueue interface {
	EnqueueProfileCandidate(pubkey string) error
	RemoveProfileCandidate(pubkey string) error
	ListProfileQueue() ([]string, error)
}

// profileSource fetches the latest kind-0 event for each requested pubkey.
type profileSource interface {
	Fetch(ctx context.Context, relays []string, pubkeys []nostr.PubKey) []nostr.Event
}

// eventQuerier is the read surface ProfileManager needs for startup backfill.
// *boltdb.BoltBackend satisfies it.
type eventQuerier interface {
	QueryEvents(filter nostr.Filter, maxLimit int) iter.Seq[nostr.Event]
}

// poolSource fetches kind-0 events from remote relays via a nostr.Pool.
// FetchManyReplaceable dedups to the latest kind-0 per pubkey (d is empty).
type poolSource struct {
	pool *nostr.Pool
}

func (s poolSource) Fetch(ctx context.Context, relays []string, pubkeys []nostr.PubKey) []nostr.Event {
	if len(pubkeys) == 0 {
		return nil
	}
	m := s.pool.FetchManyReplaceable(ctx, relays, nostr.Filter{
		Kinds:   []nostr.Kind{0},
		Authors: pubkeys,
	}, nostr.SubscriptionOptions{})
	var out []nostr.Event
	m.Range(func(_ nostr.ReplaceableKey, ev nostr.Event) bool {
		out = append(out, ev)
		return true
	})
	return out
}

// backfillAuthors enumerates the distinct authors of stored content events,
// preserving first-seen order.
func backfillAuthors(q eventQuerier, kinds []nostr.Kind, maxLimit int) []nostr.PubKey {
	seen := make(map[nostr.PubKey]bool)
	var out []nostr.PubKey
	for ev := range q.QueryEvents(nostr.Filter{Kinds: kinds}, maxLimit) {
		if !seen[ev.PubKey] {
			seen[ev.PubKey] = true
			out = append(out, ev.PubKey)
		}
	}
	return out
}

// backfillProfileCandidates returns the de-duplicated union of content authors
// and discovered community pubkeys, so the profile index covers communities
// (named in share h/p tags, never as event authors) on the same Init+refresh
// cadence as authors. Reuses backfillAuthors + backfillCommunities; adds no
// fetch path.
func backfillProfileCandidates(q eventQuerier, contentKinds, communityKinds []nostr.Kind, maxLimit int) []nostr.PubKey {
	out := backfillAuthors(q, contentKinds, maxLimit)
	seen := make(map[nostr.PubKey]bool, len(out))
	for _, pk := range out {
		seen[pk] = true
	}
	for _, hexpk := range backfillCommunities(q, communityKinds, maxLimit) {
		pk, err := nostr.PubKeyFromHex(hexpk)
		if err != nil || seen[pk] {
			continue
		}
		seen[pk] = true
		out = append(out, pk)
	}
	return out
}

// ProfileManager owns the kind-0 fetch loop: enqueue candidates, drain them by
// fetching kind-0 from PROFILE_RELAYS, and periodically re-enqueue known
// authors so renamed profiles stay fresh. Modeled on AllowlistManager.
type ProfileManager struct {
	queue     profileQueue
	src       profileSource
	store     func(nostr.Event)
	backfill  func() []nostr.PubKey
	relays    []string
	batchSize int
	stopCh    chan struct{}
}

func NewProfileManager(q profileQueue, src profileSource, store func(nostr.Event), backfill func() []nostr.PubKey, relays []string, batchSize int) *ProfileManager {
	if batchSize <= 0 {
		batchSize = 50
	}
	return &ProfileManager{
		queue:     q,
		src:       src,
		store:     store,
		backfill:  backfill,
		relays:    relays,
		batchSize: batchSize,
		stopCh:    make(chan struct{}),
	}
}

// Enqueue queues an author pubkey for kind-0 fetch. Fire-and-forget: a queue
// error is logged but never propagated to the content write path.
func (p *ProfileManager) Enqueue(pk nostr.PubKey) {
	if err := p.queue.EnqueueProfileCandidate(pk.Hex()); err != nil {
		fmt.Printf("profile: enqueue %s: %v\n", pk.Hex(), err)
	}
}

// drainOnce fetches kind-0 for every queued pubkey in batches. A pubkey whose
// kind-0 is fetched+parsed is stored and removed; one with no returned event
// stays queued for the next drain. Invalid hex is dropped.
func (p *ProfileManager) drainOnce(ctx context.Context) {
	queued, err := p.queue.ListProfileQueue()
	if err != nil {
		fmt.Printf("profile: list queue: %v\n", err)
		return
	}
	for start := 0; start < len(queued); start += p.batchSize {
		end := start + p.batchSize
		if end > len(queued) {
			end = len(queued)
		}
		var authors []nostr.PubKey
		for _, hexpk := range queued[start:end] {
			pk, err := nostr.PubKeyFromHex(hexpk)
			if err != nil {
				_ = p.queue.RemoveProfileCandidate(hexpk) // drop garbage
				continue
			}
			authors = append(authors, pk)
		}
		if len(authors) == 0 {
			continue
		}
		for _, ev := range p.src.Fetch(ctx, p.relays, authors) {
			if _, err := sdk.ParseMetadata(ev); err != nil {
				continue // leave queued; don't store a malformed profile
			}
			p.store(ev)
			_ = p.queue.RemoveProfileCandidate(ev.PubKey.Hex())
		}
	}
}

// Init seeds the queue from a backfill scan, then drains in the background so
// relay startup is never blocked on the network fetch.
func (p *ProfileManager) Init() error {
	for _, pk := range p.backfill() {
		p.Enqueue(pk)
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), profileDrainTimeout)
		defer cancel()
		p.drainOnce(ctx)
	}()
	return nil
}

// StartRefreshLoop periodically re-enqueues known content authors and drains,
// catching renamed/updated profiles.
func (p *ProfileManager) StartRefreshLoop(interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				for _, pk := range p.backfill() {
					p.Enqueue(pk)
				}
				ctx, cancel := context.WithTimeout(context.Background(), profileDrainTimeout)
				p.drainOnce(ctx)
				cancel()
			case <-p.stopCh:
				return
			}
		}
	}()
}

func (p *ProfileManager) Stop() { close(p.stopCh) }
