package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"fiatjaf.com/nostr"
)

func pkHex(t *testing.T) (nostr.PubKey, string) {
	t.Helper()
	pk := nostr.Generate().Public()
	return pk, pk.Hex()
}

// scriptedFetch answers each directory URL from a mutable table; a URL mapped
// to nil fails, as an unreachable directory would.
type scriptedFetch struct {
	mu    sync.Mutex
	names map[string]map[string]string
}

func (s *scriptedFetch) set(url string, names map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.names[url] = names
}

func (s *scriptedFetch) fetch(_ context.Context, url string) (map[string]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	names := s.names[url]
	if names == nil {
		return nil, errors.New("unreachable")
	}
	return names, nil
}

func sortedHex(pks []nostr.PubKey) []string {
	out := make([]string, 0, len(pks))
	for _, pk := range pks {
		out = append(out, pk.Hex())
	}
	slices.Sort(out)
	return out
}

func TestDirectoryPollReturnsOnlyNewMembers(t *testing.T) {
	a, aHex := pkHex(t)
	b, bHex := pkHex(t)
	src := &scriptedFetch{names: map[string]map[string]string{}}
	src.set("https://x/.well-known/nostr.json", map[string]string{"anna": aHex})
	d := NewNIP05Directory([]string{"https://x/.well-known/nostr.json"}, src.fetch)

	if got := sortedHex(d.Poll(context.Background())); !slices.Equal(got, []string{aHex}) {
		t.Fatalf("first poll added %v, want [anna]", got)
	}
	src.set("https://x/.well-known/nostr.json", map[string]string{"anna": aHex, "ben": bHex})
	if got := d.Poll(context.Background()); len(got) != 1 || got[0] != b {
		t.Fatalf("second poll added %v, want only ben", sortedHex(got))
	}
	if got := sortedHex(d.Members()); !slices.Equal(got, sortedHex([]nostr.PubKey{a, b})) {
		t.Fatalf("members %v, want anna+ben", got)
	}
}

func TestDirectoryFailedFetchKeepsPreviousMembers(t *testing.T) {
	a, aHex := pkHex(t)
	src := &scriptedFetch{names: map[string]map[string]string{}}
	src.set("u", map[string]string{"anna": aHex})
	d := NewNIP05Directory([]string{"u"}, src.fetch)
	d.Poll(context.Background())

	src.set("u", nil) // directory down
	if got := d.Poll(context.Background()); len(got) != 0 {
		t.Fatalf("failed poll added %v", sortedHex(got))
	}
	if got := d.Members(); len(got) != 1 || got[0] != a {
		t.Fatalf("failed poll dropped members: %v", sortedHex(got))
	}
}

func TestDirectoryUnionsDirectoriesAndDropsInvalidHex(t *testing.T) {
	a, aHex := pkHex(t)
	b, bHex := pkHex(t)
	src := &scriptedFetch{names: map[string]map[string]string{}}
	src.set("u1", map[string]string{"anna": aHex, "bad": "not-hex", "upper": strings.ToUpper(bHex)})
	src.set("u2", map[string]string{"ben": bHex, "anna2": aHex})
	d := NewNIP05Directory([]string{"u1", "u2"}, src.fetch)

	got := sortedHex(d.Poll(context.Background()))
	if !slices.Equal(got, sortedHex([]nostr.PubKey{a, b})) {
		t.Fatalf("added %v, want anna+ben once each", got)
	}
}

func TestDirectoryStartEnqueuesNewMembersOnEachTick(t *testing.T) {
	a, aHex := pkHex(t)
	b, bHex := pkHex(t)
	src := &scriptedFetch{names: map[string]map[string]string{}}
	src.set("u", map[string]string{"anna": aHex})
	d := NewNIP05Directory([]string{"u"}, src.fetch)

	var mu sync.Mutex
	var enqueued []nostr.PubKey
	d.Start(func(pk nostr.PubKey) {
		mu.Lock()
		defer mu.Unlock()
		enqueued = append(enqueued, pk)
	}, 20*time.Millisecond)
	defer d.Stop()

	count := func() int { mu.Lock(); defer mu.Unlock(); return len(enqueued) }
	waitForMsg(t, time.Second, func() bool { return count() == 1 }, "startup poll never enqueued anna")
	src.set("u", map[string]string{"anna": aHex, "ben": bHex})
	waitForMsg(t, time.Second, func() bool { return count() == 2 }, "tick never enqueued ben")
	time.Sleep(60 * time.Millisecond) // more ticks, no new members
	mu.Lock()
	defer mu.Unlock()
	if len(enqueued) != 2 || enqueued[0] != a || enqueued[1] != b {
		t.Fatalf("enqueued %v, want anna then ben exactly once", sortedHex(enqueued))
	}
}

func TestDirectoryStopIsIdempotent(t *testing.T) {
	d := NewNIP05Directory(nil, (&scriptedFetch{names: map[string]map[string]string{}}).fetch)
	d.Start(func(nostr.PubKey) {}, time.Hour)
	d.Stop()
	d.Stop()
}

func TestFetchNIP05DirectoryParsesNames(t *testing.T) {
	_, aHex := pkHex(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/nostr.json" || r.URL.RawQuery != "" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`{"names":{"anna":"` + aHex + `"},"relays":{}}`))
	}))
	defer srv.Close()

	names, err := fetchNIP05Directory(context.Background(), srv.URL+"/.well-known/nostr.json")
	if err != nil || names["anna"] != aHex {
		t.Fatalf("names %v err %v", names, err)
	}
}

func TestFetchNIP05DirectoryRejectsNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusBadGateway)
	}))
	defer srv.Close()
	if _, err := fetchNIP05Directory(context.Background(), srv.URL); err == nil {
		t.Fatal("502 must be an error, or a down directory would read as empty")
	}
}

func TestParseNIP05DirectoryURLs(t *testing.T) {
	got := parseNIP05DirectoryURLs(" edufeed.org , https://x.example/.well-known/nostr.json,,http://local:8080/n.json ")
	want := []string{
		"https://edufeed.org/.well-known/nostr.json",
		"https://x.example/.well-known/nostr.json",
		"http://local:8080/n.json",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestUnionPubkeys(t *testing.T) {
	a, _ := pkHex(t)
	b, _ := pkHex(t)
	c, _ := pkHex(t)
	got := unionPubkeys([]nostr.PubKey{a, b}, []nostr.PubKey{b, c})
	if len(got) != 3 || got[0] != a || got[1] != b || got[2] != c {
		t.Fatalf("got %v", sortedHex(got))
	}
}
