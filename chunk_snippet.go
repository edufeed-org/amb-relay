package main

import (
	"fmt"
	"log"
	"strconv"

	"fiatjaf.com/nostr"
)

// kindSearchSnippet is the ephemeral (never stored) event kind carrying the
// best matching fulltext passage for a chunk-reranked search result. See
// docs/superpowers/specs/2026-06-11-snippet-events-design.md.
const kindSearchSnippet = nostr.Kind(21142)

// buildSnippetEvent turns a chunk hit into a signed kind-21142 event
// annotating its parent 30142. Returns ok=false (and the parent is simply
// delivered without a snippet) when the hit has no snippet text or signing
// fails — snippets are best-effort decoration and must never break search.
func buildSnippetEvent(sk nostr.SecretKey, hit ChunkHit) (nostr.Event, bool) {
	if hit.Snippet == "" {
		return nostr.Event{}, false
	}

	tags := nostr.Tags{
		{"e", hit.EventID},
		{"a", hit.EventCoord},
		{"k", "30142"},
		{"score", fmt.Sprintf("%.4f", hit.Score)},
	}
	if hit.Page > 0 {
		tags = append(tags, nostr.Tag{"page", strconv.Itoa(hit.Page)})
	}
	if hit.Heading != "" {
		tags = append(tags, nostr.Tag{"heading", hit.Heading})
	}
	if hit.SourceURL != "" {
		tags = append(tags, nostr.Tag{"source_url", hit.SourceURL})
	}

	evt := nostr.Event{
		PubKey:    nostr.GetPublicKey(sk),
		CreatedAt: nostr.Now(),
		Kind:      kindSearchSnippet,
		Tags:      tags,
		Content:   hit.Snippet,
	}
	if err := evt.Sign(sk); err != nil {
		log.Printf("snippet: signing failed for parent %s: %v", hit.EventID, err)
		return nostr.Event{}, false
	}
	return evt, true
}
