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
// first, then run this script, then restart the relay. For an in-process
// version that runs at relay startup against the relay's own opened
// BoltDB, set HYDRATE_ON_START=true on the relay — see internal/hydrate
// for the shared library.
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
	"flag"
	"log"
	"os"
	"path/filepath"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/eventstore/boltdb"
	"github.com/edufeed-org/amb-relay/internal/hydrate"
)

func main() {
	pageSize := flag.Int("page-size", 250, "Typesense per_page")
	dryRun := flag.Bool("dry-run", false, "scan and parse but do not write to BoltDB")
	flag.Parse()

	cfg := hydrate.Config{
		TSHost:       mustEnv("TS_HOST"),
		TSAPIKey:     mustEnv("TS_APIKEY"),
		TSCollection: mustEnv("TS_COLLECTION"),
		PageSize:     *pageSize,
	}
	dbPath := os.Getenv("DB_PATH")
	if dbPath == "" {
		dbPath = "./data/relay.db"
	}

	var sink hydrate.EventSink
	var store hydrate.EventStore // non-nil only when not in dry-run; gates the verify pass
	if *dryRun {
		sink = countingSink{}
		log.Printf("dry-run: not writing to BoltDB; verify pass will be skipped")
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
		store = bb
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	total, verifyTotal, err := hydrate.Run(ctx, cfg, sink, store, nil)
	if err != nil {
		log.Fatalf("hydrate: %v", err)
	}

	log.Printf("DONE scanned=%d saved=%d already=%d parseErr=%d saveErr=%d mismatches=%d resaved=%d",
		total.Scanned, total.Saved, total.AlreadyPresent, total.ParseErrors, total.SaveErrors,
		verifyTotal.Mismatches, verifyTotal.Resaved)
}

func mustEnv(name string) string {
	v := os.Getenv(name)
	if v == "" {
		log.Fatalf("%s is required", name)
	}
	return v
}

// countingSink is a no-op EventSink for --dry-run. It always reports the
// event as "saved" since we have no parse/persistence error to report — the
// rest of the stats (scanned, parse errors) still work because parse errors
// are caught in HydrateOne before SaveEvent is called.
type countingSink struct{}

func (countingSink) SaveEvent(_ nostr.Event) error { return nil }
