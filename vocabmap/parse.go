// Package vocabmap reads NIP-VOCAB concepts (39738), standalone mappings
// (39740) and concordances (39741), and expands a concept identifier through
// them (graph.go). It does no I/O; amb-relay's MappingRegistry feeds it.
package vocabmap

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"fiatjaf.com/nostr"
	"golang.org/x/text/unicode/norm"
)

const (
	KindScheme      nostr.Kind = 39737
	KindConcept     nostr.Kind = 39738
	KindMapping     nostr.Kind = 39740
	KindConcordance nostr.Kind = 39741

	skosNS = "http://www.w3.org/2004/02/skos/core#"
)

var relations = map[string]bool{
	"exactMatch": true, "closeMatch": true, "broadMatch": true, "narrowMatch": true, "relatedMatch": true,
}

// Concept is a 39738's identifier (its `i` URI, else nostr:<address>), its
// address, the scheme addresses it belongs to and its hierarchy links
// (concept addresses).
type Concept struct {
	ID       string
	Address  string
	Schemes  []string
	Broader  []string
	Narrower []string
}

// Mapping is a valid 39740. Schemes are the 39737 addresses among its a tags.
type Mapping struct {
	D          string
	Relation   string
	From       string
	To         []string
	FromScheme string
	ToScheme   string
	PartOf     string
	Author     string
	Schemes    []string
}

// Concordance is a 39741. Schemes are the 39737 addresses among its a tags.
type Concordance struct {
	Address     string
	Author      string
	FromScheme  string
	ToScheme    string
	Maintainers []string
	Schemes     []string
}

func first(ev nostr.Event, name string) (string, bool) {
	for _, t := range ev.Tags {
		if len(t) >= 2 && t[0] == name {
			return t[1], true
		}
	}
	return "", false
}

// single returns the value of a tag that occurs exactly once.
func single(ev nostr.Event, name string) (string, bool) {
	value, n := "", 0
	for _, t := range ev.Tags {
		if len(t) >= 2 && t[0] == name {
			value = t[1]
			n++
		}
	}
	return value, n == 1
}

// Address is kind:pubkey:d of an addressable event.
func Address(ev nostr.Event) string {
	d, _ := first(ev, "d")
	return fmt.Sprintf("%d:%s:%s", ev.Kind, ev.PubKey.Hex(), d)
}

func uniqueNFC(values []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range values {
		if v == "" {
			continue
		}
		v = norm.NFC.String(v)
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

// canonicalSet: NFC, deduplicated, code-point sorted (byte order of UTF-8 is
// code-point order), joined with "|".
func canonicalSet(values []string) string {
	set := uniqueNFC(values)
	sort.Strings(set)
	return strings.Join(set, "|")
}

// MappingIdentifier is the NIP-VOCAB mapping d: the TS4NFDI sameness
// identifier over subject {from}, predicate skos:<relation>, objects to.
func MappingIdentifier(from string, to []string, relation string) string {
	input := canonicalSet([]string{from}) + " " + skosNS + relation + " " + canonicalSet(to)
	sum := sha256.Sum256([]byte(input))
	return "mapping:" + hex.EncodeToString(sum[:])
}

func ParseConcept(ev nostr.Event) (Concept, bool) {
	if ev.Kind != KindConcept {
		return Concept{}, false
	}
	if d, ok := first(ev, "d"); !ok || d == "" {
		return Concept{}, false
	}
	c := Concept{Address: Address(ev)}
	if i, ok := first(ev, "i"); ok && i != "" {
		c.ID = norm.NFC.String(i)
	} else {
		c.ID = "nostr:" + c.Address
	}
	for _, t := range ev.Tags {
		if len(t) < 2 || t[0] != "a" {
			continue
		}
		marker := ""
		if len(t) >= 4 {
			marker = t[3]
		}
		switch {
		case strings.HasPrefix(t[1], "39737:"):
			c.Schemes = append(c.Schemes, t[1])
		case strings.HasPrefix(t[1], "39738:") && marker == "broader":
			c.Broader = append(c.Broader, t[1])
		case strings.HasPrefix(t[1], "39738:") && marker == "narrower":
			c.Narrower = append(c.Narrower, t[1])
		}
	}
	return c, true
}

// ParseMapping validates a 39740 like nostr-vocab-core parseMapping: single
// d/relation/from/fromScheme/toScheme, known relation, ≥1 to, and d equal to
// the recomputed sameness identifier.
func ParseMapping(ev nostr.Event) (Mapping, bool) {
	if ev.Kind != KindMapping {
		return Mapping{}, false
	}
	d, ok1 := single(ev, "d")
	rel, ok2 := single(ev, "relation")
	from, ok3 := single(ev, "from")
	fromScheme, ok4 := single(ev, "fromScheme")
	toScheme, ok5 := single(ev, "toScheme")
	if !(ok1 && ok2 && ok3 && ok4 && ok5) || d == "" || from == "" || fromScheme == "" || toScheme == "" || !relations[rel] {
		return Mapping{}, false
	}
	var rawTo []string
	for _, t := range ev.Tags {
		if len(t) >= 2 && t[0] == "to" {
			rawTo = append(rawTo, t[1])
		}
	}
	to := uniqueNFC(rawTo)
	if len(to) == 0 || MappingIdentifier(from, to, rel) != d {
		return Mapping{}, false
	}
	m := Mapping{D: d, Relation: rel, From: norm.NFC.String(from), To: to,
		FromScheme: fromScheme, ToScheme: toScheme, Author: ev.PubKey.Hex()}
	for _, t := range ev.Tags {
		if len(t) < 2 || t[0] != "a" {
			continue
		}
		if len(t) >= 4 && t[3] == "partOf" {
			if m.PartOf == "" {
				m.PartOf = t[1]
			}
		} else if strings.HasPrefix(t[1], "39737:") {
			m.Schemes = append(m.Schemes, t[1])
		}
	}
	return m, true
}

func ParseConcordance(ev nostr.Event) (Concordance, bool) {
	if ev.Kind != KindConcordance {
		return Concordance{}, false
	}
	d, _ := first(ev, "d")
	fromScheme, _ := first(ev, "fromScheme")
	toScheme, _ := first(ev, "toScheme")
	if d == "" || fromScheme == "" || toScheme == "" {
		return Concordance{}, false
	}
	c := Concordance{Address: Address(ev), Author: ev.PubKey.Hex(), FromScheme: fromScheme, ToScheme: toScheme}
	for _, t := range ev.Tags {
		switch {
		case len(t) >= 4 && t[0] == "p" && t[3] == "maintainer":
			c.Maintainers = append(c.Maintainers, t[1])
		case len(t) >= 2 && t[0] == "a" && strings.HasPrefix(t[1], "39737:"):
			c.Schemes = append(c.Schemes, t[1])
		}
	}
	return c, true
}

// Members are the mappings that belong to c: partOf c, authored by its owner
// or a maintainer, with matching schemes; first occurrence per d wins.
// Callers pass only the latest event per address.
func Members(c Concordance, ms []Mapping) []Mapping {
	allowed := map[string]bool{c.Author: true}
	for _, m := range c.Maintainers {
		allowed[m] = true
	}
	seen := map[string]bool{}
	var out []Mapping
	for _, m := range ms {
		if m.PartOf != c.Address || !allowed[m.Author] || m.FromScheme != c.FromScheme || m.ToScheme != c.ToScheme || seen[m.D] {
			continue
		}
		seen[m.D] = true
		out = append(out, m)
	}
	return out
}

// LatestByAddress keeps the newest event per address (ties: larger id),
// sorted by address.
func LatestByAddress(evs []nostr.Event) []nostr.Event {
	best := map[string]nostr.Event{}
	for _, ev := range evs {
		a := Address(ev)
		prev, ok := best[a]
		if !ok || ev.CreatedAt > prev.CreatedAt || (ev.CreatedAt == prev.CreatedAt && ev.ID.Hex() > prev.ID.Hex()) {
			best[a] = ev
		}
	}
	keys := make([]string, 0, len(best))
	for a := range best {
		keys = append(keys, a)
	}
	sort.Strings(keys)
	out := make([]nostr.Event, 0, len(keys))
	for _, a := range keys {
		out = append(out, best[a])
	}
	return out
}
