// hydrate-bolt scans the relay's Typesense collection, decodes each
// document's eventRaw payload, and writes any missing events into the
// relay's BoltDB raw event store.
//
// Usage:
//
//	export TS_HOST=http://localhost:8108
//	export TS_APIKEY=...
//	export TS_COLLECTION=amb
//	export DB_PATH=./data/relay.db
//	go run ./cmd/hydrate-bolt
//
// IMPORTANT: BoltDB requires an exclusive lock — stop the relay container
// first, then run this script, then restart the relay.
//
// Background: the relay's eventstore (typesense30142/query.go) treats
// Typesense as a search index and reads payloads from the raw event store
// by ID. If a doc is in Typesense but its event isn't in the raw store
// (skew from a write path that bypassed dual-write, or a one-time bulk
// import into TS only), the relay's REQ path silently drops it: "Search
// succeeded, found N events" logs but nothing is sent on the websocket.
// This script reconciles that skew.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/eventstore/boltdb"
)

func main() {
	pageSize := flag.Int("page-size", 250, "Typesense per_page")
	dryRun := flag.Bool("dry-run", false, "scan and parse but do not write to BoltDB")
	flag.Parse()

	tsHost := mustEnv("TS_HOST")
	tsKey := mustEnv("TS_APIKEY")
	tsColl := mustEnv("TS_COLLECTION")
	dbPath := os.Getenv("DB_PATH")
	if dbPath == "" {
		dbPath = "./data/relay.db"
	}

	var sink EventSink
	if *dryRun {
		sink = countingSink{}
		log.Printf("dry-run: not writing to BoltDB")
	} else {
		if dir := filepath.Dir(dbPath); dir != "." {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				log.Fatalf("mkdir %s: %v", dir, err)
			}
		}
		bb := &boltdb.BoltBackend{Path: dbPath}
		if err := bb.Init(); err != nil {
			log.Fatalf("bolt init %s: %v (is the relay running and holding the lock?)", dbPath, err)
		}
		defer bb.Close()
		sink = bb
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	total := Stats{}
	page := 1
	for {
		raws, err := fetchPage(ctx, tsHost, tsKey, tsColl, page, *pageSize)
		if err != nil {
			log.Fatalf("fetch page %d: %v", page, err)
		}
		if len(raws) == 0 {
			break
		}
		s := HydrateBatch(raws, sink)
		total.Scanned += s.Scanned
		total.Saved += s.Saved
		total.AlreadyPresent += s.AlreadyPresent
		total.ParseErrors += s.ParseErrors
		total.SaveErrors += s.SaveErrors
		log.Printf("page %d: scanned=%d saved=%d already=%d parseErr=%d saveErr=%d (running: scanned=%d saved=%d)",
			page, s.Scanned, s.Saved, s.AlreadyPresent, s.ParseErrors, s.SaveErrors, total.Scanned, total.Saved)
		if len(raws) < *pageSize {
			break
		}
		page++
	}

	log.Printf("DONE scanned=%d saved=%d already=%d parseErr=%d saveErr=%d",
		total.Scanned, total.Saved, total.AlreadyPresent, total.ParseErrors, total.SaveErrors)
}

func mustEnv(name string) string {
	v := os.Getenv(name)
	if v == "" {
		log.Fatalf("%s is required", name)
	}
	return v
}

// fetchPage hits Typesense /documents/search with q=* and pulls only the
// eventRaw field. Per_page is the page-size flag; page is 1-indexed.
func fetchPage(ctx context.Context, host, apiKey, collection string, page, perPage int) ([]string, error) {
	q := url.Values{}
	q.Set("q", "*")
	q.Set("query_by", "name")
	q.Set("include_fields", "eventRaw")
	q.Set("per_page", fmt.Sprintf("%d", perPage))
	q.Set("page", fmt.Sprintf("%d", page))

	endpoint := fmt.Sprintf("%s/collections/%s/documents/search?%s", host, collection, q.Encode())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-TYPESENSE-API-KEY", apiKey)

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, body)
	}

	var parsed struct {
		Hits []struct {
			Document struct {
				EventRaw string `json:"eventRaw"`
			} `json:"document"`
		} `json:"hits"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	out := make([]string, 0, len(parsed.Hits))
	for _, h := range parsed.Hits {
		if h.Document.EventRaw != "" {
			out = append(out, h.Document.EventRaw)
		}
	}
	return out, nil
}

// countingSink is a no-op EventSink for --dry-run. It always reports the
// event as "saved" since we have no parse/persistence error to report — the
// rest of the stats (scanned, parse errors) still work because parse errors
// are caught in HydrateOne before SaveEvent is called.
type countingSink struct{}

func (countingSink) SaveEvent(_ nostr.Event) error { return nil }
