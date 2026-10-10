package main

import (
	"context"
	"errors"
	"fmt"
	"log"
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
		entries:  map[string]*sourceEntry{},
		failedAt: map[string]time.Time{},
		graphs:   map[string]*vocabmap.Graph{},
		stopCh:   make(chan struct{}),
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
func (r *MappingRegistry) Graph(ctx context.Context, trust Trust) (*vocabmap.Graph, bool) {
	keys := trust.sourceKeys()
	if len(keys) == 0 {
		return nil, false
	}
	for _, k := range r.toLoad(keys) {
		lctx, cancel := context.WithTimeout(ctx, mappingOnDemandTimeout)
		e, err := r.load(lctx, k)
		cancel()
		if err != nil {
			log.Printf("expand: load %s: %v", k, err)
			r.markFailed(k)
			continue
		}
		r.store(k, e)
	}
	return r.graphFor(strings.Join(keys, ","), keys)
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

func (r *MappingRegistry) markFailed(key string) {
	r.mu.Lock()
	r.failedAt[key] = time.Now()
	r.mu.Unlock()
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

func (r *MappingRegistry) graphFor(cacheKey string, keys []string) (*vocabmap.Graph, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	var present []*sourceEntry
	for _, k := range keys {
		if e, ok := r.entries[k]; ok {
			e.lastUsed = now
			present = append(present, e)
		}
	}
	if len(present) == 0 {
		return nil, false
	}
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

// poolMappingSource pages each relay separately (until-paging a merged
// multi-relay stream skips events only one relay holds) and dedupes by id.
type poolMappingSource struct {
	pool *nostr.Pool
}

func (s poolMappingSource) Fetch(ctx context.Context, relays []string, filter nostr.Filter) ([]nostr.Event, error) {
	byID := map[nostr.ID]nostr.Event{}
	for _, url := range relays {
		var until nostr.Timestamp
		for {
			f := filter
			f.Limit = mappingPageSize
			f.Until = until
			n, unseen := 0, 0
			var oldest nostr.Timestamp
			for ev := range s.pool.FetchMany(ctx, []string{url}, f, nostr.SubscriptionOptions{}) {
				n++
				if oldest == 0 || ev.CreatedAt < oldest {
					oldest = ev.CreatedAt
				}
				if _, ok := byID[ev.ID]; !ok {
					byID[ev.ID] = ev.Event
					unseen++
				}
			}
			if err := ctx.Err(); err != nil {
				return nil, fmt.Errorf("%s: %w", url, err)
			}
			if n < mappingPageSize {
				break
			}
			if unseen == 0 {
				return nil, fmt.Errorf("%s: more than %d events share created_at %d", url, mappingPageSize, oldest)
			}
			until = oldest
		}
	}
	out := make([]nostr.Event, 0, len(byID))
	for _, ev := range byID {
		out = append(out, ev)
	}
	return out, nil
}
