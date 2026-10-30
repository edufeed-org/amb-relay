package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"fiatjaf.com/nostr"
	"github.com/edufeed-org/amb-relay/vocabmap"
)

const (
	mappingRefreshTimeout  = 60 * time.Second
	mappingOnDemandTimeout = 3 * time.Second
	mappingRetryAfter      = time.Minute
	maxOnDemandSources     = 32
	mappingPageSize        = 300

	// maxOnDemandLoadsPerQuery bounds how many not-yet-loaded sources a
	// single Graph call will attempt; the rest load on a later query. Keeps
	// the hot path bounded even when a Trust lists many unknown sources.
	maxOnDemandLoadsPerQuery = 4

	// maxCachedGraphs and maxFailedSources bound the per-trust-set graph
	// cache and the negative cache: both are keyed by client-chosen
	// expand:<…> sources, so unbounded maps would let any client grow
	// memory at will.
	maxCachedGraphs  = 64
	maxFailedSources = 1024
)

// Trust is the set of mapping sources a query expands with: concordances
// (39741 addresses) and authors whose every mapping is trusted (hex pubkeys).
type Trust struct {
	Concordances []string
	Authors      []string
}

// sourceKeys are "c:<address>" and "p:<hex>", deduplicated and sorted.
func (t Trust) sourceKeys() []string {
	set := map[string]bool{}
	for _, c := range t.Concordances {
		set["c:"+c] = true
	}
	for _, a := range t.Authors {
		set["p:"+a] = true
	}
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

type mappingSource interface {
	Fetch(ctx context.Context, relays []string, filter nostr.Filter) ([]nostr.Event, error)
}

type sourceEntry struct {
	concepts []vocabmap.Concept
	mappings []vocabmap.Mapping
	lastUsed time.Time
}

// MappingRegistry keeps the concordances, mappings and scheme hierarchies
// that concept expansion trusts, as in-memory graphs. Defaults are loaded by
// Init/refresh only; per-query sources load on first use (bounded by
// mappingOnDemandTimeout) and are refreshed with the defaults. A failed load
// keeps the last good data. Modeled on CommunityRegistry.
type MappingRegistry struct {
	src      mappingSource
	relays   []string
	defaults Trust

	// onDemandTimeout bounds a whole Graph on-demand load loop (not each
	// source within it). A registry field, not the bare const, so tests can
	// shrink it instead of waiting out mappingOnDemandTimeout for real.
	onDemandTimeout time.Duration

	mu       sync.Mutex
	entries  map[string]*sourceEntry
	failedAt map[string]time.Time
	graphs   map[string]*vocabmap.Graph
	stopCh   chan struct{}
	stopOnce sync.Once
}

func NewMappingRegistry(src mappingSource, relays []string, defaults Trust) *MappingRegistry {
	return &MappingRegistry{
		src: src, relays: relays, defaults: defaults,
		onDemandTimeout: mappingOnDemandTimeout,
		entries:         map[string]*sourceEntry{},
		failedAt:        map[string]time.Time{},
		graphs:          map[string]*vocabmap.Graph{},
		stopCh:          make(chan struct{}),
	}
}

func (r *MappingRegistry) Defaults() Trust { return r.defaults }

func (r *MappingRegistry) isDefault(key string) bool {
	for _, k := range r.defaults.sourceKeys() {
		if k == key {
			return true
		}
	}
	return false
}

// Graph returns the expansion graph for trust. ok is false when none of its
// sources is loaded; the caller then runs the query unexpanded.
//
// On-demand loads for sources not yet cached share a single
// onDemandTimeout for the whole call (not one timeout per source — a
// Trust naming several unknown sources must not cost N * onDemandTimeout
// sequentially on the query path) and are capped at
// maxOnDemandLoadsPerQuery; any remainder is simply not attempted this
// call and loads on a later query instead.
func (r *MappingRegistry) Graph(ctx context.Context, trust Trust) (*vocabmap.Graph, bool) {
	keys := trust.sourceKeys()
	if len(keys) == 0 {
		return nil, false
	}
	toLoad := r.toLoad(keys)
	if len(toLoad) > maxOnDemandLoadsPerQuery {
		toLoad = toLoad[:maxOnDemandLoadsPerQuery]
	}
	if len(toLoad) > 0 {
		lctx, cancel := context.WithTimeout(ctx, r.onDemandTimeout)
		for _, k := range toLoad {
			if lctx.Err() != nil {
				// The shared deadline is already exhausted (an earlier
				// source in this same call used it all up): stop rather
				// than "attempt" and immediately fail the rest, which
				// would markFailed them for mappingRetryAfter even though
				// they were never really tried. Leave them unmarked so a
				// later query attempts them with a fresh deadline.
				break
			}
			e, err := r.load(lctx, k)
			if err != nil {
				log.Printf("expand: load %s: %v", k, err)
				r.markFailed(k)
				continue
			}
			r.store(k, e)
		}
		cancel()
	}
	return r.graphFor(keys)
}

// toLoad: keys not loaded yet, not defaults, not failed within mappingRetryAfter.
func (r *MappingRegistry) toLoad(keys []string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, k := range keys {
		if _, ok := r.entries[k]; ok || r.isDefault(k) {
			continue
		}
		if t, failed := r.failedAt[k]; failed && time.Since(t) < mappingRetryAfter {
			continue
		}
		out = append(out, k)
	}
	return out
}

// markFailed records a failed load. Entries older than mappingRetryAfter
// carry no information (toLoad ignores them) and are pruned; beyond
// maxFailedSources the oldest entry is dropped.
func (r *MappingRegistry) markFailed(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	for k, t := range r.failedAt {
		if now.Sub(t) >= mappingRetryAfter {
			delete(r.failedAt, k)
		}
	}
	for len(r.failedAt) >= maxFailedSources {
		var oldest string
		for k, t := range r.failedAt {
			if oldest == "" || t.Before(r.failedAt[oldest]) {
				oldest = k
			}
		}
		delete(r.failedAt, oldest)
	}
	r.failedAt[key] = now
}

func (r *MappingRegistry) store(key string, e *sourceEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if old, ok := r.entries[key]; ok {
		e.lastUsed = old.lastUsed
	} else {
		e.lastUsed = time.Now()
	}
	r.entries[key] = e
	delete(r.failedAt, key)
	r.graphs = map[string]*vocabmap.Graph{}
	for {
		var oldest string
		n := 0
		for k, v := range r.entries {
			if r.isDefault(k) {
				continue
			}
			n++
			if oldest == "" || v.lastUsed.Before(r.entries[oldest].lastUsed) {
				oldest = k
			}
		}
		if n <= maxOnDemandSources {
			return
		}
		delete(r.entries, oldest)
	}
}

// graphFor builds (or returns the cached) graph over the loaded subset of
// keys. The cache key is that loaded subset only, so trust sets differing
// just in unknown/unloadable sources share one graph.
func (r *MappingRegistry) graphFor(keys []string) (*vocabmap.Graph, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	var present []*sourceEntry
	var presentKeys []string
	for _, k := range keys {
		if e, ok := r.entries[k]; ok {
			e.lastUsed = now
			present = append(present, e)
			presentKeys = append(presentKeys, k)
		}
	}
	if len(present) == 0 {
		return nil, false
	}
	cacheKey := strings.Join(presentKeys, ",")
	if g, ok := r.graphs[cacheKey]; ok {
		return g, true
	}
	var concepts []vocabmap.Concept
	var mappings []vocabmap.Mapping
	seenC, seenM := map[string]bool{}, map[string]bool{}
	for _, e := range present {
		for _, c := range e.concepts {
			if !seenC[c.Address] {
				seenC[c.Address] = true
				concepts = append(concepts, c)
			}
		}
		for _, m := range e.mappings {
			if k := m.Author + "|" + m.D; !seenM[k] {
				seenM[k] = true
				mappings = append(mappings, m)
			}
		}
	}
	g := vocabmap.NewGraph(concepts, mappings)
	if len(r.graphs) >= maxCachedGraphs {
		r.graphs = map[string]*vocabmap.Graph{}
	}
	r.graphs[cacheKey] = g
	return g, true
}

func (r *MappingRegistry) load(ctx context.Context, key string) (*sourceEntry, error) {
	kind, value, _ := strings.Cut(key, ":")
	switch kind {
	case "c":
		return r.loadConcordance(ctx, value)
	case "p":
		return r.loadAuthor(ctx, value)
	}
	return nil, fmt.Errorf("unknown source %q", key)
}

func (r *MappingRegistry) loadConcordance(ctx context.Context, addr string) (*sourceEntry, error) {
	parts := strings.SplitN(addr, ":", 3)
	if len(parts) != 3 || parts[0] != "39741" {
		return nil, fmt.Errorf("not a concordance address: %s", addr)
	}
	owner, err := nostr.PubKeyFromHex(parts[1])
	if err != nil {
		return nil, err
	}
	evs, err := r.src.Fetch(ctx, r.relays, nostr.Filter{
		Kinds: []nostr.Kind{vocabmap.KindConcordance}, Authors: []nostr.PubKey{owner},
		Tags: nostr.TagMap{"d": []string{parts[2]}},
	})
	if err != nil {
		return nil, err
	}
	latest := vocabmap.LatestByAddress(evs)
	if len(latest) == 0 {
		return nil, errors.New("concordance not found")
	}
	conc, ok := vocabmap.ParseConcordance(latest[0])
	if !ok {
		return nil, errors.New("invalid concordance event")
	}
	authors := []nostr.PubKey{owner}
	for _, m := range conc.Maintainers {
		if pk, err := nostr.PubKeyFromHex(m); err == nil {
			authors = append(authors, pk)
		}
	}
	mevs, err := r.src.Fetch(ctx, r.relays, nostr.Filter{
		Kinds: []nostr.Kind{vocabmap.KindMapping}, Authors: authors,
		Tags: nostr.TagMap{"a": []string{addr}},
	})
	if err != nil {
		return nil, err
	}
	members := vocabmap.Members(conc, parseMappings(mevs))
	if len(members) == 0 {
		return nil, errors.New("concordance has no member mappings")
	}
	concepts, err := r.loadConcepts(ctx, conc.Schemes)
	if err != nil {
		return nil, err
	}
	return &sourceEntry{concepts: concepts, mappings: members}, nil
}

func (r *MappingRegistry) loadAuthor(ctx context.Context, hexKey string) (*sourceEntry, error) {
	pk, err := nostr.PubKeyFromHex(hexKey)
	if err != nil {
		return nil, err
	}
	evs, err := r.src.Fetch(ctx, r.relays, nostr.Filter{Kinds: []nostr.Kind{vocabmap.KindMapping}, Authors: []nostr.PubKey{pk}})
	if err != nil {
		return nil, err
	}
	mappings := parseMappings(evs)
	if len(mappings) == 0 {
		return nil, errors.New("author has no mappings")
	}
	var schemes []string
	seen := map[string]bool{}
	for _, m := range mappings {
		for _, s := range m.Schemes {
			if !seen[s] {
				seen[s] = true
				schemes = append(schemes, s)
			}
		}
	}
	concepts, err := r.loadConcepts(ctx, schemes)
	if err != nil {
		return nil, err
	}
	return &sourceEntry{concepts: concepts, mappings: mappings}, nil
}

func parseMappings(evs []nostr.Event) []vocabmap.Mapping {
	var out []vocabmap.Mapping
	for _, ev := range vocabmap.LatestByAddress(evs) {
		if m, ok := vocabmap.ParseMapping(ev); ok {
			out = append(out, m)
		}
	}
	return out
}

// loadConcepts fetches the concepts of schemes (for the in-scheme hierarchy).
// An empty answer for a non-empty scheme list is treated as a failed fetch.
func (r *MappingRegistry) loadConcepts(ctx context.Context, schemes []string) ([]vocabmap.Concept, error) {
	if len(schemes) == 0 {
		return nil, nil
	}
	evs, err := r.src.Fetch(ctx, r.relays, nostr.Filter{Kinds: []nostr.Kind{vocabmap.KindConcept}, Tags: nostr.TagMap{"a": schemes}})
	if err != nil {
		return nil, err
	}
	var out []vocabmap.Concept
	for _, ev := range vocabmap.LatestByAddress(evs) {
		if c, ok := vocabmap.ParseConcept(ev); ok {
			out = append(out, c)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("no concepts for schemes")
	}
	return out, nil
}

// refresh reloads the defaults and every cached source; failures keep the
// last good data.
func (r *MappingRegistry) refresh(ctx context.Context) {
	keys := r.defaults.sourceKeys()
	r.mu.Lock()
	for k := range r.entries {
		if !r.isDefault(k) {
			keys = append(keys, k)
		}
	}
	r.mu.Unlock()
	for _, k := range keys {
		e, err := r.load(ctx, k)
		if err != nil {
			log.Printf("expand: refresh %s: %v (keeping last good)", k, err)
			continue
		}
		r.store(k, e)
	}
}

func (r *MappingRegistry) Init() {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), mappingRefreshTimeout)
		defer cancel()
		r.refresh(ctx)
	}()
}

func (r *MappingRegistry) StartRefreshLoop(interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				ctx, cancel := context.WithTimeout(context.Background(), mappingRefreshTimeout)
				r.refresh(ctx)
				cancel()
			case <-r.stopCh:
				return
			}
		}
	}()
}

func (r *MappingRegistry) Stop() { r.stopOnce.Do(func() { close(r.stopCh) }) }

// pageFetcher fetches one page (one relay, one already-limited-and-cursored
// filter) of events. poolMappingSource.fetchPage is the production
// implementation; tests inject a fake one to exercise the paging/dedupe
// logic in pageRelay without a network.
type pageFetcher func(ctx context.Context, url string, filter nostr.Filter) ([]nostr.Event, error)

// poolMappingSource pages each relay separately (until-paging a merged
// multi-relay stream skips events only one relay holds) and dedupes by id.
type poolMappingSource struct {
	pool *nostr.Pool
}

func (s poolMappingSource) Fetch(ctx context.Context, relays []string, filter nostr.Filter) ([]nostr.Event, error) {
	byID := map[nostr.ID]nostr.Event{}
	for _, url := range relays {
		if err := pageRelay(ctx, url, filter, byID, s.fetchPage); err != nil {
			return nil, err
		}
	}
	out := make([]nostr.Event, 0, len(byID))
	for _, ev := range byID {
		out = append(out, ev)
	}
	return out, nil
}

// pageRelay pages one relay via fetch, merging every page into byID — the
// global, cross-relay dedupe set. The "did this page move the cursor
// forward" guard uses seen, a set scoped to this relay call alone: a
// second, mirrored relay returning its own first page full of events byID
// already holds (from an earlier relay) must not look like a relay stuck
// replaying the same page forever. seen only answers "is this new to THIS
// relay's pagination", never "is this new overall".
func pageRelay(ctx context.Context, url string, filter nostr.Filter, byID map[nostr.ID]nostr.Event, fetch pageFetcher) error {
	seen := map[nostr.ID]bool{}
	var until nostr.Timestamp
	for {
		f := filter
		f.Limit = mappingPageSize
		f.Until = until
		evs, err := fetch(ctx, url, f)
		if err != nil {
			return fmt.Errorf("%s: %w", url, err)
		}
		n := len(evs)
		var oldest nostr.Timestamp
		newToRelay := 0
		for _, ev := range evs {
			if oldest == 0 || ev.CreatedAt < oldest {
				oldest = ev.CreatedAt
			}
			if !seen[ev.ID] {
				seen[ev.ID] = true
				newToRelay++
			}
			byID[ev.ID] = ev
		}
		if n < mappingPageSize {
			return nil
		}
		if newToRelay == 0 {
			return fmt.Errorf("%s: more than %d events share created_at %d", url, mappingPageSize, oldest)
		}
		until = oldest
	}
}

// fetchPage is the production pageFetcher for one relay, one page. It uses
// the Relay-level API directly (EnsureRelay + Subscribe) instead of the
// Pool's FetchMany/FetchManyNotifyClosed, because only at that level is a
// real EOSE distinguishable from every other way a subscription can end —
// Pool.subManyEose folds all of those into either a silent channel close or
// a CLOSED message, discarding exactly the distinction this needs.
//
//   - pool.EnsureRelay(url) dials using pool.Context, not this call's ctx, so
//     it runs in a goroutine and the result is raced against ctx.Done().
//   - relay.Subscribe(ctx, filter, opts) with MaxWaitForEOSE disabled (set to
//     the sentinel documented on SubscriptionOptions, time.Duration(math.MaxInt64))
//     so the pool's fake-EOSE timer never fires — ctx is the only timeout.
//     Without this, a fake EOSE firing mid-page is indistinguishable from a
//     real one once it reaches sub.EndOfStoredEvents, which is the failure
//     mode this whole rewrite exists to close.
//   - the read loop selects on sub.Events, sub.EndOfStoredEvents,
//     sub.ClosedReason and ctx.Done():
//       - sub.EndOfStoredEvents fired ⇒ a genuine EOSE: dispatchEose() (in
//         package nostr's subscription.go) only sends on it after
//         storedwg.Wait(), i.e. once every event stored before the EOSE has
//         already been delivered through sub.Events and received below, so
//         the page is complete.
//       - sub.Events closing (more == false) before that ⇒ the subscription
//         ended some other way (connection dropped, Unsub elsewhere, relay
//         gone) — an error, not a short-but-complete page. The reason is
//         read from context.Cause(sub.Context), the cause PrepareSubscription
//         and the connection-death goroutine in relay.go actually cancel it
//         with.
//       - sub.ClosedReason ⇒ the relay sent CLOSED: an error.
//       - ctx.Done() ⇒ ctx.Err().
//   - EnsureRelay/Subscribe failing is now itself an error (previously, an
//     unreachable relay silently produced zero events — indistinguishable
//     from a short, complete, empty page).
//   - sub.Unsub() always runs on return (success or error) via defer, per
//     Subscribe's own doc comment ("remember to cancel subscriptions").
func (s poolMappingSource) fetchPage(ctx context.Context, url string, filter nostr.Filter) ([]nostr.Event, error) {
	type relayResult struct {
		relay *nostr.Relay
		err   error
	}
	relayCh := make(chan relayResult, 1)
	go func() {
		relay, err := s.pool.EnsureRelay(url)
		relayCh <- relayResult{relay, err}
	}()

	var relay *nostr.Relay
	select {
	case res := <-relayCh:
		if res.err != nil {
			return nil, fmt.Errorf("%s: connect: %w", url, res.err)
		}
		relay = res.relay
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	sub, err := relay.Subscribe(ctx, filter, nostr.SubscriptionOptions{
		MaxWaitForEOSE: time.Duration(math.MaxInt64),
	})
	if err != nil {
		return nil, fmt.Errorf("%s: subscribe: %w", url, err)
	}
	defer sub.Unsub()

	var out []nostr.Event
	for {
		select {
		case evt, ok := <-sub.Events:
			if !ok {
				return nil, fmt.Errorf("%s: subscription ended before EOSE: %w", url, context.Cause(sub.Context))
			}
			out = append(out, evt)
		case <-sub.EndOfStoredEvents:
			return out, nil
		case reason := <-sub.ClosedReason:
			return nil, fmt.Errorf("%s: CLOSED: %s", url, reason)
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}
