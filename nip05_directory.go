package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"fiatjaf.com/nostr"
)

const (
	// nip05DirectoryTimeout bounds one directory fetch.
	nip05DirectoryTimeout = 15 * time.Second
	// nip05DirectoryMaxBytes caps a directory response; a members list of a
	// few thousand names is well under 1 MiB.
	nip05DirectoryMaxBytes = 8 << 20
)

// directoryFetcher returns the NIP-05 "names" map (name → pubkey hex) of one
// directory URL. Injected so tests never do real HTTP.
type directoryFetcher func(ctx context.Context, url string) (map[string]string, error)

// NIP05Directory seeds the profile index with the members of NIP-05
// directories (PROFILE_NIP05_DIRECTORIES), not only with content authors.
// Without it a deployment's members who have not published anything to this
// relay are never fetched and cannot be found by profile search, although
// their address (name@domain) vouches for them.
//
// A directory is the full .well-known/nostr.json without ?name= — NIP-05
// leaves that open, but self-hosted name services (edufeed's nip05-service)
// answer it with every name. A directory that cannot be fetched keeps the
// members of its last successful fetch, so an outage never shrinks the set.
type NIP05Directory struct {
	urls     []string
	fetch    directoryFetcher
	mu       sync.Mutex
	members  map[string]map[nostr.PubKey]bool // url → members of its last good fetch
	stopCh   chan struct{}
	stopOnce sync.Once
}

func NewNIP05Directory(urls []string, fetch directoryFetcher) *NIP05Directory {
	return &NIP05Directory{
		urls:    urls,
		fetch:   fetch,
		members: map[string]map[nostr.PubKey]bool{},
		stopCh:  make(chan struct{}),
	}
}

// Poll fetches every directory and returns the members that were not known
// before this poll, in first-seen order.
func (d *NIP05Directory) Poll(ctx context.Context) []nostr.PubKey {
	before := make(map[nostr.PubKey]bool)
	for _, pk := range d.Members() {
		before[pk] = true
	}
	for _, url := range d.urls {
		names, err := d.fetch(ctx, url)
		if err != nil {
			fmt.Printf("profile: nip05 directory %s: %v\n", url, err)
			continue
		}
		set := make(map[nostr.PubKey]bool, len(names))
		for _, hexpk := range names {
			if pk, err := nostr.PubKeyFromHex(strings.ToLower(hexpk)); err == nil {
				set[pk] = true
			}
		}
		d.mu.Lock()
		d.members[url] = set
		d.mu.Unlock()
	}
	var added []nostr.PubKey
	for _, pk := range d.Members() {
		if !before[pk] {
			added = append(added, pk)
		}
	}
	return added
}

// Members returns the union of all directories' members, each pubkey once.
func (d *NIP05Directory) Members() []nostr.PubKey {
	d.mu.Lock()
	defer d.mu.Unlock()
	seen := make(map[nostr.PubKey]bool)
	var out []nostr.PubKey
	for _, url := range d.urls {
		for pk := range d.members[url] {
			if !seen[pk] {
				seen[pk] = true
				out = append(out, pk)
			}
		}
	}
	return out
}

// Start polls once in the background and then every interval, enqueueing
// only members that are new since the previous poll. Keeping known members
// fresh is the profile refresh loop's job (main.go unions Members() into its
// backfill), so a short interval here costs one HTTP GET per directory.
func (d *NIP05Directory) Start(enqueue func(nostr.PubKey), interval time.Duration) {
	go func() {
		poll := func() {
			ctx, cancel := context.WithTimeout(context.Background(), nip05DirectoryTimeout*time.Duration(max(1, len(d.urls))))
			defer cancel()
			added := d.Poll(ctx)
			for _, pk := range added {
				enqueue(pk)
			}
			if len(added) > 0 {
				fmt.Printf("profile: nip05 directories added %d member(s)\n", len(added))
			}
		}
		poll()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				poll()
			case <-d.stopCh:
				return
			}
		}
	}()
}

// Stop is idempotent: a second call must not re-close the channel.
func (d *NIP05Directory) Stop() { d.stopOnce.Do(func() { close(d.stopCh) }) }

// fetchNIP05Directory GETs a nostr.json and returns its "names" map. A non-200
// answer is an error, never an empty directory.
func fetchNIP05Directory(ctx context.Context, url string) (map[string]string, error) {
	ctx, cancel := context.WithTimeout(ctx, nip05DirectoryTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	var body struct {
		Names map[string]string `json:"names"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, nip05DirectoryMaxBytes)).Decode(&body); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	return body.Names, nil
}

// parseNIP05DirectoryURLs reads PROFILE_NIP05_DIRECTORIES: comma-separated
// domains (edufeed.org → https://edufeed.org/.well-known/nostr.json) or full
// http(s) URLs, used as given.
func parseNIP05DirectoryURLs(raw string) []string {
	var out []string
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		switch {
		case entry == "":
		case strings.HasPrefix(entry, "https://"), strings.HasPrefix(entry, "http://"):
			out = append(out, entry)
		default:
			out = append(out, "https://"+entry+"/.well-known/nostr.json")
		}
	}
	return out
}

// unionPubkeys concatenates lists, keeping the first occurrence of each pubkey.
func unionPubkeys(lists ...[]nostr.PubKey) []nostr.PubKey {
	seen := make(map[nostr.PubKey]bool)
	var out []nostr.PubKey
	for _, list := range lists {
		for _, pk := range list {
			if !seen[pk] {
				seen[pk] = true
				out = append(out, pk)
			}
		}
	}
	return out
}
