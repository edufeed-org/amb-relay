package main

import (
	"strconv"
	"strings"

	"fiatjaf.com/nostr"
)

type listCoord struct {
	Pubkey string
	DTag   string
	Relay  string
}

type communitySection struct {
	Kinds []nostr.Kind
	List  *listCoord
}

// parseListCoord parses an `a` value of the form "30000:<pubkey>:<d>".
// Returns nil for any other kind (e.g. 30168 forms) or a malformed value.
func parseListCoord(aTag []string) *listCoord {
	if len(aTag) < 2 {
		return nil
	}
	parts := strings.SplitN(aTag[1], ":", 3)
	if len(parts) != 3 || parts[0] != "30000" {
		return nil
	}
	lc := &listCoord{Pubkey: parts[1], DTag: parts[2]}
	if len(aTag) >= 3 {
		lc.Relay = aTag[2]
	}
	return lc
}

// parseCommunitySections splits a kind-10222 flat tag list into content-type
// sections (delimited by ["content", …] markers), collecting each section's k
// kinds and first 30000 list ref. Tags before the first marker belong to no
// section.
func parseCommunitySections(event *nostr.Event) []communitySection {
	var secs []communitySection
	cur := -1
	for _, tag := range event.Tags {
		if len(tag) < 1 {
			continue
		}
		switch tag[0] {
		case "content":
			secs = append(secs, communitySection{})
			cur = len(secs) - 1
		case "k":
			if cur < 0 || len(tag) < 2 {
				continue
			}
			if n, err := strconv.Atoi(tag[1]); err == nil {
				secs[cur].Kinds = append(secs[cur].Kinds, nostr.Kind(n))
			}
		case "a":
			if cur < 0 || secs[cur].List != nil {
				continue
			}
			if lc := parseListCoord(tag); lc != nil {
				secs[cur].List = lc
			}
		}
	}
	return secs
}
