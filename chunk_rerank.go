package main

import (
	"context"
	"iter"
	"log"
	"slices"
	"sort"

	"fiatjaf.com/nostr"
)

// ChunkHit is one passage-level match from the indexer's chunk collection:
// which event it belongs to, how well it matched, and the passage itself
// with its source locators (used for kind-21142 snippet events).
type ChunkHit struct {
	EventID    string
	EventCoord string // "30142:<pubkey>:<d-tag>", verbatim from the indexer
	Score      float64
	Snippet    string
	Page       int // 0 = unknown
	Heading    string
	SourceURL  string
}

// ChunkSearcher queries the amb-indexer chunk index. Production wires this
// to POST /search_chunks; tests inject a fake.
type ChunkSearcher interface {
	SearchChunks(ctx context.Context, q string, k int) ([]ChunkHit, error)
}

// fetchFunc abstracts tsDB.QueryEvents so the re-rank logic is testable
// without Typesense.
type fetchFunc func(filter nostr.Filter, maxLimit int) iter.Seq[nostr.Event]

const (
	// chunkSearchKMultiplier oversamples chunks relative to the requested
	// event limit, since one event typically owns several matching chunks.
	chunkSearchKMultiplier = 4
	chunkSearchKMax        = 200
)

// chunkRerankQuery serves NIP-50 search queries ranked by chunk-level
// relevance: it asks the indexer's chunk index for the best passages, then
// returns the parent events in best-chunk-score order. When the client opted
// in by including kind 21142 in the filter, each parent is followed by an
// ephemeral snippet event carrying its best matching passage (signed with
// sk). Any condition that prevents re-ranking (no search term, no searcher,
// indexer error, zero chunk hits) falls back to the plain event-level search
// — which never emits snippets — so recall is never worse than today.
func chunkRerankQuery(ctx context.Context, filter nostr.Filter, searcher ChunkSearcher, fetch fetchFunc, maxLimit int, sk nostr.SecretKey) iter.Seq[nostr.Event] {
	if filter.Search == "" || searcher == nil {
		return fetch(filter, maxLimit)
	}

	limit := filter.Limit
	if limit <= 0 || limit > maxLimit {
		limit = maxLimit
	}

	k := limit * chunkSearchKMultiplier
	if k > chunkSearchKMax {
		k = chunkSearchKMax
	}

	hits, err := searcher.SearchChunks(ctx, filter.Search, k)
	if err != nil {
		log.Printf("chunk-rerank: search_chunks failed, falling back to plain search: %v", err)
		return fetch(filter, maxLimit)
	}
	if len(hits) == 0 {
		return fetch(filter, maxLimit)
	}

	// Rank parent events by their best chunk hit.
	best := make(map[string]ChunkHit, len(hits))
	for _, h := range hits {
		if b, ok := best[h.EventID]; !ok || h.Score > b.Score {
			best[h.EventID] = h
		}
	}
	ranked := make([]string, 0, len(best))
	for id := range best {
		ranked = append(ranked, id)
	}
	sort.Slice(ranked, func(i, j int) bool { return best[ranked[i]].Score > best[ranked[j]].Score })

	parentIDs := make([]nostr.ID, 0, len(ranked))
	bestByID := make(map[nostr.ID]ChunkHit, len(ranked))
	for _, hex := range ranked {
		id, err := nostr.IDFromHex(hex)
		if err != nil {
			continue
		}
		parentIDs = append(parentIDs, id)
		bestByID[id] = best[hex]
	}
	if len(parentIDs) == 0 {
		return fetch(filter, maxLimit)
	}

	byID := make(map[nostr.ID]nostr.Event, len(parentIDs))
	for e := range fetch(nostr.Filter{IDs: parentIDs}, len(parentIDs)) {
		byID[e.ID] = e
	}

	wantSnippets := slices.Contains(filter.Kinds, kindSearchSnippet)

	return func(yield func(nostr.Event) bool) {
		n := 0
		for _, id := range parentIDs {
			e, ok := byID[id]
			if !ok {
				continue
			}
			// filter.Matches ignores the Search field, so this applies only
			// the remaining constraints (kinds, authors, tags, since/until).
			if !filter.Matches(e) {
				continue
			}
			if !yield(e) {
				return
			}
			if wantSnippets {
				if snip, ok := buildSnippetEvent(sk, bestByID[id]); ok {
					if !yield(snip) {
						return
					}
				}
			}
			n++
			if n >= limit {
				return
			}
		}
	}
}
