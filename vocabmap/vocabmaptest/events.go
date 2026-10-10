// Package vocabmaptest builds NIP-VOCAB events for tests.
package vocabmaptest

import (
	"fmt"

	"fiatjaf.com/nostr"
	"github.com/edufeed-org/amb-relay/vocabmap"
)

var (
	Owner      = nostr.GetPublicKey([32]byte{31: 1})
	Other      = nostr.GetPublicKey([32]byte{31: 2})
	Maintainer = nostr.GetPublicKey([32]byte{31: 3})
)

func SchemeAddr(pk nostr.PubKey, d string) string      { return fmt.Sprintf("39737:%s:%s", pk.Hex(), d) }
func ConcordanceAddr(pk nostr.PubKey, d string) string { return fmt.Sprintf("39741:%s:%s", pk.Hex(), d) }
func ID(pk nostr.PubKey, d string) string              { return fmt.Sprintf("nostr:39738:%s:%s", pk.Hex(), d) }

// Concept builds a 39738 in scheme; uri becomes its `i` tag when non-empty;
// broader are d-tags of concepts by the same author.
func Concept(pk nostr.PubKey, scheme, d, uri string, broader ...string) nostr.Event {
	tags := nostr.Tags{{"d", d}, {"a", SchemeAddr(pk, scheme), "wss://relay.example", "inScheme"}}
	for _, b := range broader {
		tags = append(tags, nostr.Tag{"a", fmt.Sprintf("39738:%s:%s", pk.Hex(), b), "wss://relay.example", "broader"})
	}
	if uri != "" {
		tags = append(tags, nostr.Tag{"i", uri})
	}
	return nostr.Event{Kind: vocabmap.KindConcept, PubKey: pk, CreatedAt: 1, Tags: tags}
}

func Concordance(pk nostr.PubKey, d, fromScheme, toScheme string, schemeAddrs []string, maintainers ...nostr.PubKey) nostr.Event {
	tags := nostr.Tags{{"d", d}, {"title", d, "de"}, {"fromScheme", fromScheme}, {"toScheme", toScheme}}
	for _, a := range schemeAddrs {
		tags = append(tags, nostr.Tag{"a", a})
	}
	for _, m := range maintainers {
		tags = append(tags, nostr.Tag{"p", m.Hex(), "", "maintainer"})
	}
	return nostr.Event{Kind: vocabmap.KindConcordance, PubKey: pk, CreatedAt: 1, Tags: tags}
}

// Mapping builds a valid 1:1 39740 (d = sameness identifier).
func Mapping(pk nostr.PubKey, from, relation, to, fromScheme, toScheme, partOf string, schemeAddrs ...string) nostr.Event {
	tags := nostr.Tags{
		{"d", vocabmap.MappingIdentifier(from, []string{to}, relation)},
		{"relation", relation}, {"from", from}, {"to", to},
		{"fromScheme", fromScheme}, {"toScheme", toScheme},
	}
	for _, a := range schemeAddrs {
		tags = append(tags, nostr.Tag{"a", a, "wss://relay.example"})
	}
	if partOf != "" {
		tags = append(tags, nostr.Tag{"a", partOf, "", "partOf"})
	}
	return nostr.Event{Kind: vocabmap.KindMapping, PubKey: pk, CreatedAt: 1, Tags: tags}
}
