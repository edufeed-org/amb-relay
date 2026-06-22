package main

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"time"

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

type communityMembership struct {
	Owner   string
	Members map[nostr.Kind]map[string]bool // present key = restricted; absent = open
}

func (m communityMembership) allows(pubkey string, kind nostr.Kind) bool {
	if pubkey == m.Owner {
		return true
	}
	set, restricted := m.Members[kind]
	if !restricted {
		return true
	}
	return set[pubkey]
}

func listKey(lc *listCoord) string { return lc.Pubkey + ":" + lc.DTag }

// buildMembership assembles per-kind membership. Open wins: a kind with any
// open covering section stays open. Otherwise the kind is restricted to the
// union of its sections' resolved lists; a list absent from `lists` (fetch
// failed) contributes nothing, making an all-unreachable kind owner-only.
func buildMembership(owner string, sections []communitySection, lists map[string]map[string]bool) communityMembership {
	openKinds := make(map[nostr.Kind]bool)
	restricted := make(map[nostr.Kind]map[string]bool)
	for _, sec := range sections {
		for _, k := range sec.Kinds {
			if sec.List == nil {
				openKinds[k] = true
				continue
			}
			if restricted[k] == nil {
				restricted[k] = make(map[string]bool)
			}
			for pk := range lists[listKey(sec.List)] {
				restricted[k][pk] = true
			}
		}
	}
	members := make(map[nostr.Kind]map[string]bool)
	for k, set := range restricted {
		if openKinds[k] {
			continue
		}
		members[k] = set
	}
	return communityMembership{Owner: owner, Members: members}
}

// Task 3: registry refresh + IsMember (open-default, keep-last-known)

const communityRefreshTimeout = 30 * time.Second

type communitySource interface {
	Resolve(ctx context.Context, relays []string, community string) (communityMembership, bool)
}

// backfillCommunities enumerates distinct community targets across stored
// events of the given kinds (shares + content), first-seen order.
func backfillCommunities(q eventQuerier, kinds []nostr.Kind, maxLimit int) []string {
	seen := make(map[string]bool)
	var out []string
	for ev := range q.QueryEvents(nostr.Filter{Kinds: kinds}, maxLimit) {
		ev := ev
		for _, c := range shareCommunities(&ev) {
			if !seen[c] {
				seen[c] = true
				out = append(out, c)
			}
		}
	}
	return out
}

// CommunityRegistry resolves each discovered community's kind-10222 (+ optional
// kind-30000 lists) into an in-memory membership cache and answers IsMember.
// Modeled on AllowlistManager: RWMutex map + pool fetch + refresh loop. The
// cache is never pruned on refresh, so a fetch failure keeps last-known.
type CommunityRegistry struct {
	src      communitySource
	backfill func() []string
	relays   []string

	mu       sync.RWMutex
	resolved map[string]communityMembership
	stopCh   chan struct{}
}

func NewCommunityRegistry(src communitySource, backfill func() []string, relays []string) *CommunityRegistry {
	return &CommunityRegistry{
		src:      src,
		backfill: backfill,
		relays:   relays,
		resolved: make(map[string]communityMembership),
		stopCh:   make(chan struct{}),
	}
}

// IsMember reports whether pubkey may publish kind into community. An unresolved
// community is open by default.
func (r *CommunityRegistry) IsMember(community, pubkey string, kind nostr.Kind) bool {
	r.mu.RLock()
	m, ok := r.resolved[community]
	r.mu.RUnlock()
	if !ok {
		return true
	}
	return m.allows(pubkey, kind)
}

// refresh re-derives the known-community set from the backfill scan and
// re-resolves each. Entries are updated in place and never deleted: a failed or
// not-yet-resolved community simply retains its prior value (or stays absent ⇒
// open), satisfying keep-last-known.
func (r *CommunityRegistry) refresh(ctx context.Context) {
	for _, c := range r.backfill() {
		if _, err := nostr.PubKeyFromHex(c); err != nil {
			continue
		}
		m, ok := r.src.Resolve(ctx, r.relays, c)
		if !ok {
			continue
		}
		r.mu.Lock()
		r.resolved[c] = m
		r.mu.Unlock()
	}
}

func (r *CommunityRegistry) Init() error {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), communityRefreshTimeout)
		defer cancel()
		r.refresh(ctx)
	}()
	return nil
}

func (r *CommunityRegistry) StartRefreshLoop(interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				ctx, cancel := context.WithTimeout(context.Background(), communityRefreshTimeout)
				r.refresh(ctx)
				cancel()
			case <-r.stopCh:
				return
			}
		}
	}()
}

func (r *CommunityRegistry) Stop() { close(r.stopCh) }

// membersFromTags collects the p-tag pubkeys from a kind-30000 (or contact)
// list event into a set.
func membersFromTags(tags nostr.Tags) map[string]bool {
	set := make(map[string]bool)
	for _, tag := range tags {
		if len(tag) >= 2 && tag[0] == "p" {
			set[tag[1]] = true
		}
	}
	return set
}

type poolCommunitySource struct {
	pool *nostr.Pool
}

func (s poolCommunitySource) Resolve(ctx context.Context, relays []string, community string) (communityMembership, bool) {
	pk, err := nostr.PubKeyFromHex(community)
	if err != nil {
		return communityMembership{}, false
	}
	def := s.pool.QuerySingle(ctx, relays, nostr.Filter{
		Kinds:   []nostr.Kind{10222},
		Authors: []nostr.PubKey{pk},
		Limit:   1,
	}, nostr.SubscriptionOptions{})
	if def == nil {
		return communityMembership{}, false
	}
	sections := parseCommunitySections(&def.Event)
	lists := make(map[string]map[string]bool)
	for _, sec := range sections {
		if sec.List == nil {
			continue
		}
		key := listKey(sec.List)
		if _, done := lists[key]; done {
			continue
		}
		if set, ok := s.fetchList(ctx, relays, sec.List); ok {
			lists[key] = set
		}
	}
	return buildMembership(community, sections, lists), true
}

// fetchList resolves one kind-30000 member list, hint-first. Returns (set,true)
// only on success; (nil,false) on failure so buildMembership makes the kind
// owner-only.
func (s poolCommunitySource) fetchList(ctx context.Context, fallback []string, lc *listCoord) (map[string]bool, bool) {
	pk, err := nostr.PubKeyFromHex(lc.Pubkey)
	if err != nil {
		return nil, false
	}
	relays := fallback
	if lc.Relay != "" {
		relays = append([]string{lc.Relay}, fallback...)
	}
	res := s.pool.QuerySingle(ctx, relays, nostr.Filter{
		Kinds:   []nostr.Kind{30000},
		Authors: []nostr.PubKey{pk},
		Tags:    nostr.TagMap{"d": []string{lc.DTag}},
		Limit:   1,
	}, nostr.SubscriptionOptions{})
	if res == nil {
		return nil, false
	}
	return membersFromTags(res.Tags), true
}
