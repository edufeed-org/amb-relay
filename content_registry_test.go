package main

import (
	"errors"
	"iter"
	"testing"

	"fiatjaf.com/nostr"
)

func makeSimpleEvent(kind nostr.Kind, tags nostr.Tags) nostr.Event {
	return nostr.Event{Kind: kind, Tags: tags}
}

func TestRegistryValidateAndStoreDispatch(t *testing.T) {
	var storedAMB, storedLF int
	reg := newRegistry(
		contentType{
			kinds:    []nostr.Kind{30142},
			validate: func(nostr.Event) (bool, string) { return false, "" },
			store:    func(nostr.Event) { storedAMB++ },
		},
		contentType{
			kinds:    []nostr.Kind{30023},
			validate: func(nostr.Event) (bool, string) { return true, "lf-rejected" },
			store:    func(nostr.Event) { storedLF++ },
		},
	)

	if reject, _ := reg.validate(makeSimpleEvent(30142, nil)); reject {
		t.Fatal("30142 should pass validation")
	}
	if reject, msg := reg.validate(makeSimpleEvent(30023, nil)); !reject || msg != "lf-rejected" {
		t.Fatalf("30023 validate = %v %q, want true lf-rejected", reject, msg)
	}
	if reject, msg := reg.validate(makeSimpleEvent(31337, nil)); !reject || msg != "kind not accepted" {
		t.Fatalf("unregistered kind = %v %q, want true 'kind not accepted'", reject, msg)
	}

	reg.store(makeSimpleEvent(30142, nil))
	reg.store(makeSimpleEvent(30023, nil))
	reg.store(makeSimpleEvent(31337, nil)) // no-op
	if storedAMB != 1 || storedLF != 1 {
		t.Fatalf("store counts = %d %d, want 1 1", storedAMB, storedLF)
	}
}

func TestRegistryKindsUnion(t *testing.T) {
	reg := newRegistry(
		contentType{kinds: []nostr.Kind{30142}},
		contentType{kinds: []nostr.Kind{30023}},
	)
	got := reg.kinds()
	if len(got) != 2 || got[0] != 30142 || got[1] != 30023 {
		t.Fatalf("kinds() = %v, want [30142 30023]", got)
	}
}

func TestRegistryTargetsChunked(t *testing.T) {
	reg := newRegistry(
		contentType{kinds: []nostr.Kind{30142}, chunked: true},
		contentType{kinds: []nostr.Kind{31922, 31923, 31924, 31925}}, // calendar, not chunked
	)

	cases := []struct {
		name  string
		kinds []nostr.Kind
		want  bool
	}{
		{"kind-agnostic includes chunked", nil, true},
		{"chunked kind", []nostr.Kind{30142}, true},
		{"calendar only", []nostr.Kind{31923}, false},
		{"calendar mix", []nostr.Kind{31922, 31923, 31924}, false},
		{"chunked + calendar mix", []nostr.Kind{30142, 31923}, true},
		{"unregistered kind", []nostr.Kind{1}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := reg.targetsChunked(nostr.Filter{Kinds: c.kinds}); got != c.want {
				t.Errorf("targetsChunked(%v) = %v, want %v", c.kinds, got, c.want)
			}
		})
	}
}

func TestSearchHasFreeText(t *testing.T) {
	cases := []struct {
		name   string
		search string
		want   bool
	}{
		{"empty", "", false},
		{"free term", "mathematik", true},
		{"pure field filter", "community:abcdef", false},
		{"dotted field filter", "publisher.name:e-teaching.org", false},
		{"term plus field filter", "mathematik community:abcdef", true},
		{"sort directive only", "sort:datePublished", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := searchHasFreeText(c.search); got != c.want {
				t.Errorf("searchHasFreeText(%q) = %v, want %v", c.search, got, c.want)
			}
		})
	}
}

// With no chunked content types registered, even a kind-agnostic filter must
// not claim the chunk path.
func TestRegistryTargetsChunkedNoneRegistered(t *testing.T) {
	reg := newRegistry(
		contentType{kinds: []nostr.Kind{31922, 31923}},
	)
	if reg.targetsChunked(nostr.Filter{}) {
		t.Error("targetsChunked(kind-agnostic) = true with no chunked types, want false")
	}
}

func mkFetch(events ...nostr.Event) fetchFunc {
	return func(filter nostr.Filter, maxLimit int) iter.Seq[nostr.Event] {
		return func(yield func(nostr.Event) bool) {
			for _, e := range events {
				if !yield(e) {
					return
				}
			}
		}
	}
}

func collect(seq iter.Seq[nostr.Event]) []nostr.Event {
	var out []nostr.Event
	for e := range seq {
		out = append(out, e)
	}
	return out
}

func TestRegistryFetchRoutingAndDedup(t *testing.T) {
	amb := nostr.Event{ID: nostr.ID{1}, Kind: 30142}
	lf := nostr.Event{ID: nostr.ID{2}, Kind: 30023}
	reg := newRegistry(
		contentType{kinds: []nostr.Kind{30142}, fetch: mkFetch(amb)},
		contentType{kinds: []nostr.Kind{30023}, fetch: mkFetch(lf)},
	)

	// kind-scoped: only the AMB backend.
	got := collect(reg.fetch(nostr.Filter{Kinds: []nostr.Kind{30142}}, 10))
	if len(got) != 1 || got[0].ID != amb.ID {
		t.Fatalf("30142 filter = %v, want [amb]", got)
	}
	// no kinds: both, in registration order.
	got = collect(reg.fetch(nostr.Filter{}, 10))
	if len(got) != 2 || got[0].ID != amb.ID || got[1].ID != lf.ID {
		t.Fatalf("no-kind filter = %v, want [amb lf]", got)
	}
}

func TestRegistryFetchDedupAcrossBackends(t *testing.T) {
	dup := nostr.Event{ID: nostr.ID{7}, Kind: 30142}
	reg := newRegistry(
		contentType{kinds: []nostr.Kind{30142}, fetch: mkFetch(dup)},
		contentType{kinds: []nostr.Kind{30023}, fetch: mkFetch(dup)}, // same id leaks in
	)
	got := collect(reg.fetch(nostr.Filter{}, 10))
	if len(got) != 1 {
		t.Fatalf("expected de-dup to 1 event, got %d", len(got))
	}
}

func TestRegistryFetchEarlyExit(t *testing.T) {
	a := nostr.Event{ID: nostr.ID{1}, Kind: 30142}
	b := nostr.Event{ID: nostr.ID{2}, Kind: 30142}
	reg := newRegistry(contentType{kinds: []nostr.Kind{30142}, fetch: mkFetch(a, b)})
	var seen int
	for range reg.fetch(nostr.Filter{}, 10) {
		seen++
		break // caller early-exit
	}
	if seen != 1 {
		t.Fatalf("early exit yielded %d, want 1", seen)
	}
}

func TestRegistryCountFanOut(t *testing.T) {
	reg := newRegistry(
		contentType{kinds: []nostr.Kind{30142}, count: func(nostr.Filter) (uint32, error) { return 3, nil }},
		contentType{kinds: []nostr.Kind{30023}, count: func(nostr.Filter) (uint32, error) { return 5, nil }},
	)
	// no kinds: sum both.
	n, err := reg.count(nostr.Filter{})
	if err != nil || n != 8 {
		t.Fatalf("count(all) = %d %v, want 8 nil", n, err)
	}
	// kind-scoped: only AMB.
	n, err = reg.count(nostr.Filter{Kinds: []nostr.Kind{30142}})
	if err != nil || n != 3 {
		t.Fatalf("count(30142) = %d %v, want 3 nil", n, err)
	}
	// unowned kind: zero.
	n, err = reg.count(nostr.Filter{Kinds: []nostr.Kind{40000}})
	if err != nil || n != 0 {
		t.Fatalf("count(unowned) = %d %v, want 0 nil", n, err)
	}
}

func TestRegistryDeleteEverywhere(t *testing.T) {
	var hitAMB, hitLF int
	reg := newRegistry(
		contentType{kinds: []nostr.Kind{30142}, deleteID: func(nostr.ID) error { hitAMB++; return nil }},
		contentType{kinds: []nostr.Kind{30023}, deleteID: func(nostr.ID) error { hitLF++; return nil }},
	)
	if err := reg.deleteEverywhere(nostr.ID{9}, nil); err != nil {
		t.Fatalf("deleteEverywhere err = %v", err)
	}
	if hitAMB != 1 || hitLF != 1 {
		t.Fatalf("delete hits = %d %d, want 1 1 (id carries no kind, so all collections tried)", hitAMB, hitLF)
	}
}

func TestRegistryDeleteEverywhereReturnsFirstErrorLogsRest(t *testing.T) {
	errAMB := errors.New("amb boom")
	errLF := errors.New("lf boom")
	var logged []error
	reg := newRegistry(
		contentType{kinds: []nostr.Kind{30142}, deleteID: func(nostr.ID) error { return errAMB }},
		contentType{kinds: []nostr.Kind{30023}, deleteID: func(nostr.ID) error { return errLF }},
	)
	err := reg.deleteEverywhere(nostr.ID{9}, func(e error) { logged = append(logged, e) })
	if err != errAMB {
		t.Fatalf("first error = %v, want amb boom", err)
	}
	if len(logged) != 1 || logged[0] != errLF {
		t.Fatalf("logged = %v, want [lf boom]", logged)
	}
}

func TestRegistryKind0ReadWriteSplit(t *testing.T) {
	var fetched bool
	profileType := contentType{
		kinds:    []nostr.Kind{0},
		validate: func(nostr.Event) (bool, string) { return true, "kind not accepted" },
		store:    func(nostr.Event) {},
		fetch: func(_ nostr.Filter, _ int) iter.Seq[nostr.Event] {
			return func(yield func(nostr.Event) bool) { fetched = true }
		},
		count:    func(nostr.Filter) (uint32, error) { return 0, nil },
		deleteID: func(nostr.ID) error { return nil },
		chunked:  false,
	}
	reg := newRegistry(profileType)

	// Write path: client kind-0 submissions are rejected.
	reject, msg := reg.validate(nostr.Event{Kind: 0})
	if !reject || msg != "kind not accepted" {
		t.Fatalf("kind-0 write: reject=%v msg=%q, want reject + \"kind not accepted\"", reject, msg)
	}

	// Read path: a kind-0 REQ is routed to the profiles fetch.
	for range reg.fetch(nostr.Filter{Kinds: []nostr.Kind{0}}, 10) {
	}
	if !fetched {
		t.Fatal("kind-0 read was not routed to the profiles fetch func")
	}

	// kind-0 search must not be treated as chunked (no chunk-rerank).
	if reg.targetsChunked(nostr.Filter{Kinds: []nostr.Kind{0}}) {
		t.Fatal("kind-0 should not be chunked")
	}
}

// A search combining free text with a field filter must NOT be owned by the
// chunk-rerank path: rerank sends the raw string to the chunk index, which
// treats the filter tokens as text, and filter.Matches never re-applies
// NIP-50 field filters — so the filter was silently dropped and the query
// answered WIDER than asked (verified live: "LADEN publisher.name:zzz-bogus"
// returned free-text hits instead of zero). See issue #22.
func TestRerankOwnsSearch(t *testing.T) {
	reg := newRegistry(
		contentType{kinds: []nostr.Kind{30142}, chunked: true},
		contentType{kinds: []nostr.Kind{31922, 31923, 31924, 31925}}, // calendar, not chunked
	)

	cases := []struct {
		name   string
		kinds  []nostr.Kind
		search string
		want   bool
	}{
		{"plain free text", []nostr.Kind{30142}, "mathematik", true},
		{"empty search", []nostr.Kind{30142}, "", false},
		{"pure field filter", []nostr.Kind{30142}, "publisher.name:e-teaching.org", false},
		{"free text plus field filter", []nostr.Kind{30142}, "forschung publisher.name:e-teaching.org", false},
		{"free text plus community filter", []nostr.Kind{30142}, "mathematik community:abcdef", false},
		{"free text plus sort directive", []nostr.Kind{30142}, "mathematik sort:datePublished", true},
		{"non-chunked kinds", []nostr.Kind{31923}, "mathematik", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := nostr.Filter{Kinds: c.kinds, Search: c.search}
			if got := reg.rerankOwns(f); got != c.want {
				t.Errorf("rerankOwns(kinds=%v, search=%q) = %v, want %v", c.kinds, c.search, got, c.want)
			}
		})
	}
}
