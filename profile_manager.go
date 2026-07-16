package main

import (
	"context"
	"fmt"
	"iter"
	"sync"
	"time"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/nip05"
	"fiatjaf.com/nostr/sdk"
)

const (
	// profileDrainTimeout bounds an entire drain pass. It must be large enough to
	// walk the WHOLE queue in one pass: the queue is processed in pubkey-sorted
	// order, so a budget that truncates mid-queue permanently starves the tail
	// (unresolved authors at the front stay queued and are re-fetched every pass,
	// so the window never advances past them). With thousands of queued authors in
	// batches of batchSize, each batch costing up to two profileFetchTimeout-bounded
	// fetches (primary then fallback), a generous budget keeps one pass covering the
	// full backlog.
	profileDrainTimeout = 20 * time.Minute
	// profileFetchTimeout bounds a single relay-set fetch. A replaceable fetch
	// waits for EOSE from every relay (or the deadline), so without this one
	// slow/unresponsive relay — common with public profile aggregators — would
	// starve the rest of the drain by consuming the whole drain budget.
	profileFetchTimeout = 15 * time.Second
	// nip05VerifyTimeout bounds one .well-known/nostr.json lookup so a single
	// dead domain cannot stall the drain.
	nip05VerifyTimeout = 5 * time.Second
	// nip05VerifyConcurrency bounds parallel lookups within one fetched batch:
	// lookups are independent HTTP fetches to third-party domains, and a batch
	// of slow domains must not be paid for serially inside the drain budget.
	nip05VerifyConcurrency = 8
)

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

// nip05Verifier reports whether a profile's claimed nip05 identifier resolves
// back to the given pubkey. Injected so tests never do real HTTP.
type nip05Verifier func(ctx context.Context, identifier string, pk nostr.PubKey) bool

// verifyNIP05 resolves the identifier's .well-known/nostr.json and checks the
// mapped pubkey. Any failure — invalid identifier, unreachable domain, name
// missing from the response, pubkey mismatch — counts as unverified; transient
// network failures self-heal on the next refresh drain, which re-verifies.
func verifyNIP05(ctx context.Context, identifier string, pk nostr.PubKey) bool {
	if identifier == "" || !nip05.IsValidIdentifier(identifier) {
		return false
	}
	vctx, cancel := context.WithTimeout(ctx, nip05VerifyTimeout)
	defer cancel()
	ptr, err := nip05.QueryIdentifier(vctx, identifier)
	if err != nil {
		return false
	}
	return ptr.PublicKey == pk
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
	queue          profileQueue
	src            profileSource
	store          func(nostr.Event, bool)
	backfill       func() []nostr.PubKey
	verify         nip05Verifier
	relays         []string
	fallbackRelays []string
	batchSize      int
	stopCh         chan struct{}
	stopOnce       sync.Once
}

func NewProfileManager(q profileQueue, src profileSource, store func(nostr.Event, bool), backfill func() []nostr.PubKey, verify nip05Verifier, relays, fallbackRelays []string, batchSize int) *ProfileManager {
	if batchSize <= 0 {
		batchSize = 50
	}
	return &ProfileManager{
		queue:          q,
		src:            src,
		store:          store,
		backfill:       backfill,
		verify:         verify,
		relays:         relays,
		fallbackRelays: fallbackRelays,
		batchSize:      batchSize,
		stopCh:         make(chan struct{}),
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
// stays queued for the next drain. Invalid hex is dropped. Pubkeys still
// unresolved after PROFILE_RELAYS are retried against PROFILE_FALLBACK_RELAYS,
// so a profile that lives only on a non-standard relay (e.g. relay.damus.io)
// is still indexed.
func (p *ProfileManager) drainOnce(ctx context.Context) {
	queued, err := p.queue.ListProfileQueue()
	if err != nil {
		fmt.Printf("profile: list queue: %v\n", err)
		return
	}
	if len(queued) == 0 {
		return
	}
	var viaPrimary, viaFallback int
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
		remaining := p.fetchStore(ctx, p.relays, authors)
		viaPrimary += len(authors) - len(remaining)
		if len(remaining) > 0 && len(p.fallbackRelays) > 0 {
			stillMissing := p.fetchStore(ctx, p.fallbackRelays, remaining)
			viaFallback += len(remaining) - len(stillMissing)
		}
		if ctx.Err() != nil {
			break // drain budget exhausted; the rest stays queued for next pass
		}
	}
	fmt.Printf("profile: drain resolved %d of %d queued (%d primary, %d fallback)\n",
		viaPrimary+viaFallback, len(queued), viaPrimary, viaFallback)
}

// fetchStore fetches kind-0 for authors from relays; each successfully parsed
// profile has its claimed nip05 verified (bounded parallelism) and is then
// stored and dequeued. It returns the authors that yielded no stored profile,
// so the caller can escalate them to fallback relays. The fetch is bounded by
// profileFetchTimeout so one slow relay cannot starve the drain; each
// verification is separately bounded by nip05VerifyTimeout.
func (p *ProfileManager) fetchStore(ctx context.Context, relays []string, authors []nostr.PubKey) []nostr.PubKey {
	fctx, cancel := context.WithTimeout(ctx, profileFetchTimeout)
	events := p.src.Fetch(fctx, relays, authors)
	cancel()

	type fetched struct {
		ev       nostr.Event
		nip05    string
		verified bool
	}
	var batch []*fetched
	for _, ev := range events {
		meta, err := sdk.ParseMetadata(ev)
		if err != nil {
			continue // leave queued; don't store a malformed profile
		}
		batch = append(batch, &fetched{ev: ev, nip05: meta.NIP05})
	}

	sem := make(chan struct{}, nip05VerifyConcurrency)
	var wg sync.WaitGroup
	for _, f := range batch {
		if f.nip05 == "" {
			continue // nothing claimed, nothing to verify
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(f *fetched) {
			defer wg.Done()
			defer func() { <-sem }()
			f.verified = p.verify(ctx, f.nip05, f.ev.PubKey)
		}(f)
	}
	wg.Wait()

	stored := make(map[nostr.PubKey]bool, len(batch))
	for _, f := range batch {
		p.store(f.ev, f.verified)
		_ = p.queue.RemoveProfileCandidate(f.ev.PubKey.Hex())
		stored[f.ev.PubKey] = true
	}
	var remaining []nostr.PubKey
	for _, pk := range authors {
		if !stored[pk] {
			remaining = append(remaining, pk)
		}
	}
	return remaining
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

// Stop is idempotent: a second call must not re-close the channel.
func (p *ProfileManager) Stop() { p.stopOnce.Do(func() { close(p.stopCh) }) }

// enqueueShareCommunities enqueues the kind-0 fetch for every community a share
// (kind 16/30222) targets, so a brand-new community's name resolves on the next
// drain instead of waiting for the periodic backfill. nil-safe: a no-op when
// profiles are disabled.
func enqueueShareCommunities(pm *ProfileManager, event nostr.Event) {
	if pm == nil {
		return
	}
	for _, hexpk := range shareCommunities(&event) {
		if pk, err := nostr.PubKeyFromHex(hexpk); err == nil {
			pm.Enqueue(pk)
		}
	}
}
