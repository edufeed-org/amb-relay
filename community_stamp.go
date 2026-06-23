package main

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/eventstore/typesense30142"
)

// shareRef is one share event resolved to the addressable content it targets.
type shareRef struct {
	Coord       string // "<kind>:<pubkeyHex>:<d>"
	Kind        nostr.Kind
	Pubkey      string
	DTag        string
	Author      string // share event author (the member doing the sharing)
	Communities []string
	RelayHint   string // optional relay hint from the a-tag's 3rd element
}

// shareToRef resolves a share event to the addressable content it references via
// its first parseable `a` tag, plus the communities it targets. Returns false
// (skip) when there is no parseable `a` coord or no targeted community. e-only
// shares are intentionally skipped: every stampable kind is addressable and
// edufeed-app always emits an `a` tag for them.
func shareToRef(event nostr.Event) (shareRef, bool) {
	communities := shareCommunities(&event)
	if len(communities) == 0 {
		return shareRef{}, false
	}
	for _, tag := range event.Tags {
		if len(tag) < 2 || tag[0] != "a" {
			continue
		}
		parts := strings.SplitN(tag[1], ":", 3)
		if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
			continue
		}
		n, err := strconv.Atoi(parts[0])
		if err != nil {
			continue
		}
		ref := shareRef{
			Coord:       tag[1],
			Kind:        nostr.Kind(n),
			Pubkey:      parts[1],
			DTag:        parts[2],
			Author:      event.PubKey.Hex(),
			Communities: communities,
		}
		if len(tag) >= 3 {
			ref.RelayHint = tag[2]
		}
		return ref, true
	}
	return shareRef{}, false
}

// contentOwnCommunities returns a content event's own `h` community targets
// (Model A), de-duplicated in first-seen order.
func contentOwnCommunities(event nostr.Event) []string {
	seen := make(map[string]bool)
	var out []string
	for _, tag := range event.Tags {
		if len(tag) >= 2 && tag[0] == "h" && !seen[tag[1]] {
			seen[tag[1]] = true
			out = append(out, tag[1])
		}
	}
	return out
}

// deriveStamps maps each referenced content coord to the set of communities it
// is shared into, gated by membership: a community is included only when the
// share's author may publish the referenced content's kind into it.
func deriveStamps(refs []shareRef, isMember func(community, pubkey string, kind nostr.Kind) bool) map[string]map[string]bool {
	stamps := make(map[string]map[string]bool)
	for _, ref := range refs {
		for _, c := range ref.Communities {
			if !isMember(c, ref.Author, ref.Kind) {
				continue
			}
			if stamps[ref.Coord] == nil {
				stamps[ref.Coord] = make(map[string]bool)
			}
			stamps[ref.Coord][c] = true
		}
	}
	return stamps
}

// CommunityStamper recomputes and patches the `community` field on content docs
// whenever a community share event references them.
type CommunityStamper struct {
	isMember  func(community, pubkey string, kind nostr.Kind) bool
	sharesFor func(coord string) []nostr.Event
	allShares func() []nostr.Event
	lookup    func(coord string) (nostr.Event, bool)
	fetch     func(ref shareRef, hints []string) (nostr.Event, bool)
	validate  func(nostr.Event) (reject bool, msg string)
	store     func(nostr.Event)
	patch     func(kind nostr.Kind, docID string, communities []string) error

	// stampKinds is the set of content kinds whose docs can be stamped.
	// Populated in main.go from stampTargets keys. Used to skip goroutine
	// spawning for non-content kinds (kind-0, share kinds, etc.).
	stampKinds map[nostr.Kind]bool

	stopCh chan struct{}
}

// isStampKind reports whether a kind is in the set of stampable content kinds.
func (s *CommunityStamper) isStampKind(kind nostr.Kind) bool {
	return s.stampKinds[kind]
}

// contentCoord derives the addressable coord of a content event.
func contentCoord(event nostr.Event) (coord string, kind nostr.Kind, pubkey, dTag string, ok bool) {
	for _, tag := range event.Tags {
		if len(tag) >= 2 && tag[0] == "d" {
			pk := event.PubKey.Hex()
			return fmt.Sprintf("%d:%s:%s", event.Kind, pk, tag[1]), event.Kind, pk, tag[1], true
		}
	}
	return "", 0, "", "", false
}

// reconcile recomputes the full `community` field for one addressable content
// coord and PATCHes it. The desired set is the member-gated union of every
// share that references this coord; the written value is own-h-tags ∪ desired.
// Absent content with at least one desired community is fetched, validated, and
// stored first-class before stamping. A fetch miss is left for the next sweep.
func (s *CommunityStamper) reconcile(coord string, kind nostr.Kind, pubkey, dTag string) {
	if !s.isStampKind(kind) {
		return
	}
	var refs []shareRef
	for _, ev := range s.sharesFor(coord) {
		if ref, ok := shareToRef(ev); ok {
			refs = append(refs, ref)
		}
	}
	desired := deriveStamps(refs, s.isMember)[coord] // may be nil

	ev, local := s.lookup(coord)
	if !local {
		if len(desired) == 0 {
			return // nothing to stamp and nothing to fetch
		}
		if s.fetch == nil {
			return
		}
		fetched, ok := s.fetch(shareRef{Coord: coord, Kind: kind, Pubkey: pubkey, DTag: dTag}, hintsFromRefs(refs))
		if !ok {
			return // retried by the next sweep
		}
		if s.validate != nil {
			if reject, _ := s.validate(fetched); reject {
				return
			}
		}
		if s.store != nil {
			s.store(fetched)
		}
		ev = fetched
	}

	final := unionSorted(contentOwnCommunities(ev), desired)
	docID := typesense30142.GenerateDocumentID(pubkey, dTag)
	if err := s.patch(kind, docID, final); err != nil {
		fmt.Printf("community stamp: patch %s (%d): %v\n", coord, kind, err)
	}
}

// unionSorted returns the sorted, de-duplicated union of a slice and a set.
func unionSorted(a []string, b map[string]bool) []string {
	set := make(map[string]bool, len(a)+len(b))
	for _, v := range a {
		set[v] = true
	}
	for v := range b {
		set[v] = true
	}
	out := make([]string, 0, len(set))
	for v := range set {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

func hintsFromRefs(refs []shareRef) []string {
	var hints []string
	for _, r := range refs {
		if r.RelayHint != "" {
			hints = append(hints, r.RelayHint)
		}
	}
	return hints
}

const communityStampTimeout = 60 * time.Second

// reconcileShare resolves a share to its referenced content and reconciles it.
func (s *CommunityStamper) reconcileShare(event nostr.Event) {
	if ref, ok := shareToRef(event); ok {
		s.reconcile(ref.Coord, ref.Kind, ref.Pubkey, ref.DTag)
	}
}

// reconcileAll reconciles every distinct content coord referenced by any share.
// This is both the periodic sweep (retrying absent fetches and re-evaluating
// IsMember) and the post-reindex replay (re-applying stamps the projection
// wiped). The shares collection is the work list — no durable queue.
func (s *CommunityStamper) reconcileAll() {
	seen := make(map[string]bool)
	for _, ev := range s.allShares() {
		ref, ok := shareToRef(ev)
		if ok && !seen[ref.Coord] {
			seen[ref.Coord] = true
			s.reconcile(ref.Coord, ref.Kind, ref.Pubkey, ref.DTag)
		}
	}
}

func (s *CommunityStamper) Init() {
	go s.reconcileAll()
}

func (s *CommunityStamper) StartSweepLoop(interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				s.reconcileAll()
			case <-s.stopCh:
				return
			}
		}
	}()
}

func (s *CommunityStamper) Stop() { close(s.stopCh) }
