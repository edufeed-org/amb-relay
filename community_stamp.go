package main

import (
	"strconv"
	"strings"

	"fiatjaf.com/nostr"
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
