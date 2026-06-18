package main

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/eventstore/boltdb"
	"fiatjaf.com/nostr/eventstore/typesense30142"
	"fiatjaf.com/nostr/khatru"
	"fiatjaf.com/nostr/khatru/calendar"
	"fiatjaf.com/nostr/khatru/landing"
	"fiatjaf.com/nostr/khatru/relaykit"
	"fiatjaf.com/nostr/khatru/semantic"
	"fiatjaf.com/nostr/nip11"
	"fiatjaf.com/nostr/nip86"
	"github.com/edufeed-org/amb-relay/internal/hydrate"
	"github.com/joho/godotenv"
)

func main() {
	// Load .env file (optional — Docker passes env vars directly)
	if err := godotenv.Load(); err != nil {
		if !os.IsNotExist(err) {
			fmt.Printf("Error loading .env file: %v\n", err)
		}
	}

	relay := khatru.NewRelay()
	if serviceURL := os.Getenv("SERVICE_URL"); serviceURL != "" {
		relay.ServiceURL = serviceURL
	}
	relay.Info.Name = os.Getenv("NAME")
	relay.Info.Description = os.Getenv("DESCRIPTION")
	relay.Info.Icon = os.Getenv("ICON")

	// NIP-11: Software identification
	relay.Info.Software = "https://git.edufeed.org/edufeed/amb-relay"
	relay.Info.Version = getVersion()

	// NIP-11: Supported NIPs (khatru defaults: 1, 11, 42, 70, 86)
	relay.Info.AddSupportedNIPs([]int{9, 45, 50}) // deletion, count, search
	// AMB-NIP reference (custom NIP for kind 30142 educational metadata)
	relay.Info.SupportedNIPs = append(relay.Info.SupportedNIPs,
		"naddr1qvzqqqrcvypzp0wzr7fmrcktw4sgemxh5zsq5auh08vnvlwf0x9anusn7pkft0zgqy28wumn8ghj7un9d3shjtnyv9kh2uewd9hsqzm9v36kvet9vskkzmtzvjvrtf")

	// NIP-11: Limitations
	relay.Info.Limitation = &nip11.RelayLimitationDocument{
		MaxLimit:         250,
		RestrictedWrites: true,
		AuthRequired:     false,
	}

	longformEnabled := os.Getenv("LONGFORM_ENABLED") == "true"
	wikiEnabled := os.Getenv("WIKI_ENABLED") == "true"
	calendarEnabled := os.Getenv("CALENDAR_ENABLED") == "true"

	// NIP-11: Retention
	retentionKinds := [][]int{{5}, {30142}}
	if longformEnabled {
		retentionKinds = append(retentionKinds, []int{30023})
	}
	if wikiEnabled {
		retentionKinds = append(retentionKinds, []int{30818})
	}
	if calendarEnabled {
		retentionKinds = append(retentionKinds, []int{31922}, []int{31923}, []int{31924}, []int{31925})
	}
	relay.Info.Retention = []*nip11.RelayRetentionDocument{
		{Kinds: retentionKinds},
	}

	// Parse relay operator pubkey
	var operatorPK nostr.PubKey
	if pkHex := os.Getenv("PUBKEY"); pkHex != "" {
		pk, err := nostr.PubKeyFromHex(pkHex)
		if err != nil {
			fmt.Printf("Error parsing PUBKEY: %v\n", err)
		} else {
			operatorPK = pk
			relay.Info.PubKey = &pk
		}
	}

	// Build admin pubkey set
	var staticAdmins []nostr.PubKey
	if operatorPK != (nostr.PubKey{}) {
		staticAdmins = append(staticAdmins, operatorPK)
	}
	if adminList := os.Getenv("ADMIN_PUBKEYS"); adminList != "" {
		for _, hex := range strings.Split(adminList, ",") {
			hex = strings.TrimSpace(hex)
			if pk, err := nostr.PubKeyFromHex(hex); err == nil {
				staticAdmins = append(staticAdmins, pk)
			} else {
				fmt.Printf("Error parsing admin pubkey %q: %v\n", hex, err)
			}
		}
	}
	admins := relaykit.NewAdminSet(staticAdmins)

	// BoltDB backend (raw event persistence) — initialized first so we can load schema
	dbPath := os.Getenv("DB_PATH")
	if dbPath == "" {
		dbPath = "./data/relay.db"
	}
	if dir := filepath.Dir(dbPath); dir != "." {
		if err := os.MkdirAll(dir, 0755); err != nil {
			panic(fmt.Sprintf("failed to create data directory %s: %v", dir, err))
		}
	}
	boltDB := boltdb.BoltBackend{Path: dbPath}
	if err := boltDB.Init(); err != nil {
		panic(err)
	}
	defer boltDB.Close()

	// Management store (bans + schema) — shares the same bbolt database
	mgmt := ManagementStore{}
	if err := mgmt.Init(boltDB.DB); err != nil {
		panic(err)
	}

	// ContentStore for fetched resource fulltext (bucket registered by mgmt.Init)
	contentStore := &ContentStore{DB: boltDB.DB}

	// Load persisted dynamic admins from BoltDB
	if persistedAdmins, err := mgmt.ListAdmins(); err != nil {
		fmt.Printf("Warning: failed to load persisted admins: %v\n", err)
	} else {
		for hex, entry := range persistedAdmins {
			if pk, err := nostr.PubKeyFromHex(hex); err == nil {
				if entry.FullAccess {
					admins.Grant(pk, nil)
				} else {
					admins.Grant(pk, entry.Methods)
				}
			}
		}
		if len(persistedAdmins) > 0 {
			fmt.Printf("Loaded %d dynamic admin(s) from BoltDB\n", len(persistedAdmins))
		}
	}

	// Allowlist-based access control
	acl := NewAllowlistManager(&mgmt)
	if err := acl.Init(); err != nil {
		panic(err)
	}
	acl.StartRefreshLoop(5 * time.Minute)
	defer acl.Stop()

	// Typesense backend (search index)
	tsDB := typesense30142.TSBackend{
		ApiKey:         os.Getenv("TS_APIKEY"),
		Host:           os.Getenv("TS_HOST"),
		CollectionName: os.Getenv("TS_COLLECTION"),
		RawEventStore:  &boltDB,
	}

	// Load custom schema from BoltDB if one was stored; otherwise start from
	// the default. Either way, ensure the content fields are present — they
	// are required by the fulltext plumbing.
	customSchema, err := mgmt.LoadSchema()
	if err != nil {
		fmt.Printf("Warning: failed to load custom schema: %v\n", err)
	}
	var effectiveSchema typesense30142.CollectionSchema
	if customSchema != nil {
		effectiveSchema = *customSchema
		fmt.Println("Using custom Typesense schema from BoltDB")
	} else {
		effectiveSchema = typesense30142.DefaultSchema()
	}
	ensureContentFields(&effectiveSchema)
	tsDB.Schema = &effectiveSchema

	if err := tsDB.Init(); err != nil {
		panic(err)
	}

	// Long-form (kind 30023) Typesense backend — gated behind LONGFORM_ENABLED.
	// Disjoint collection from the AMB events; nil when the flag is off so the
	// query/store closures degrade to the exact pre-flag behavior.
	var tsDB2 *typesense30142.TSBackend
	if longformEnabled {
		lfColl := os.Getenv("TS_COLLECTION_LONGFORM")
		if lfColl == "" {
			lfColl = "longform_30023"
		}
		lfSchema := longformSchema(lfColl)
		tsDB2 = &typesense30142.TSBackend{
			ApiKey:         os.Getenv("TS_APIKEY"),
			Host:           os.Getenv("TS_HOST"),
			CollectionName: lfColl,
			RawEventStore:  &boltDB,
			Schema:         &lfSchema,
			SearchFields:   "title,summary,content",
		}
		if err := tsDB2.Init(); err != nil {
			panic(fmt.Sprintf("longform TSBackend init: %v", err))
		}
		fmt.Printf("Long-form (kind 30023) enabled — collection %s\n", lfColl)
	}

	// Wiki (kind 30818) Typesense backend — gated behind WIKI_ENABLED. Disjoint
	// collection; nil when the flag is off so the registry simply omits it.
	var tsDB3 *typesense30142.TSBackend
	if wikiEnabled {
		wikiColl := os.Getenv("TS_COLLECTION_WIKI")
		if wikiColl == "" {
			wikiColl = "wiki_30818"
		}
		wSchema := wikiSchema(wikiColl)
		tsDB3 = &typesense30142.TSBackend{
			ApiKey:         os.Getenv("TS_APIKEY"),
			Host:           os.Getenv("TS_HOST"),
			CollectionName: wikiColl,
			RawEventStore:  &boltDB,
			Schema:         &wSchema,
			SearchFields:   "title,summary,content",
		}
		if err := tsDB3.Init(); err != nil {
			panic(fmt.Sprintf("wiki TSBackend init: %v", err))
		}
		fmt.Printf("Wiki (kind 30818) enabled — collection %s\n", wikiColl)
	}

	// Calendar (NIP-52 kinds 31922-31925) backends — gated behind CALENDAR_ENABLED.
	// Dual-indexed: a Typesense collection (full-text/kind/tag) plus the nostrlib
	// calendar BoltDB index (start/end/geohash range queries). Both nil/unused
	// when the flag is off, so the registry simply omits calendar.
	var tsDB4 *typesense30142.TSBackend
	var calStore *calendar.CalendarStore
	if calendarEnabled {
		calColl := os.Getenv("TS_COLLECTION_CALENDAR")
		if calColl == "" {
			calColl = "calendar_31922"
		}
		calSchema := calendarSchema(calColl)
		tsDB4 = &typesense30142.TSBackend{
			ApiKey:         os.Getenv("TS_APIKEY"),
			Host:           os.Getenv("TS_HOST"),
			CollectionName: calColl,
			RawEventStore:  &boltDB,
			Schema:         &calSchema,
			SearchFields:   "title,summary,content,location",
		}
		if err := tsDB4.Init(); err != nil {
			panic(fmt.Sprintf("calendar TSBackend init: %v", err))
		}

		calIndexPath := os.Getenv("CALENDAR_INDEX_PATH")
		if calIndexPath == "" {
			calIndexPath = "./data/calendar_index.db"
		}
		calStore, err = calendar.NewCalendarStore(&boltDB, calIndexPath)
		if err != nil {
			panic(fmt.Sprintf("calendar index init: %v", err))
		}
		// Close ONLY the index DB on shutdown. calStore.Close() would also close
		// the shared boltDB (double close); boltDB has its own defer in main.
		defer calStore.GetIndex().Close()
		fmt.Printf("Calendar (NIP-52) enabled — collection %s, index %s\n", calColl, calIndexPath)
	}

	// Optional: reconcile BoltDB ↔ Typesense at startup. Both stores are
	// open and the listener hasn't started yet, so this runs single-threaded
	// against the relay's own opened BoltDB — no lock juggling, unlike the
	// standalone cmd/hydrate-bolt which requires the container stopped.
	// Fail-open: a TS network blip at boot shouldn't keep the relay down.
	if os.Getenv("HYDRATE_ON_START") == "true" {
		hydrateCfg := hydrate.Config{
			TSHost:       os.Getenv("TS_HOST"),
			TSAPIKey:     os.Getenv("TS_APIKEY"),
			TSCollection: os.Getenv("TS_COLLECTION"),
			PageSize:     250,
		}
		hctx, hcancel := context.WithTimeout(context.Background(), 30*time.Minute)
		stats, verify, err := hydrate.Run(hctx, hydrateCfg, &boltDB, &boltDB, nil)
		hcancel()
		if err != nil {
			fmt.Printf("Warning: HYDRATE_ON_START failed: %v (continuing startup)\n", err)
		} else {
			fmt.Printf("HYDRATE_ON_START done: scanned=%d saved=%d already=%d parseErr=%d saveErr=%d mismatches=%d resaved=%d\n",
				stats.Scanned, stats.Saved, stats.AlreadyPresent, stats.ParseErrors, stats.SaveErrors,
				verify.Mismatches, verify.Resaved)
		}
	}

	// Write buffers: queue events for async persistence
	// tsBuf deferred first so boltBuf drains before tsBuf on shutdown (LIFO)
	tsBuf := NewTSWriteBuffer(&tsDB, 100, 500*time.Millisecond)
	defer tsBuf.Close()
	boltBuf := NewBoltWriteBuffer(&boltDB)
	defer boltBuf.Close()

	// Content-type registry: one registration per kind the relay serves. AMB
	// is always present; long-form is appended only when enabled, so every
	// event-path dispatch below is kind-agnostic and flag-free. Adding a
	// content type = appending one contentType here.
	contentTypes := []contentType{
		{
			kinds:    []nostr.Kind{30142},
			validate: validateAMB,
			store:    func(e nostr.Event) { tsBuf.Queue(e) },
			fetch:    tsDB.QueryEvents,
			count:    tsDB.CountEvents,
			deleteID: tsDB.DeleteEvent,
			chunked:  true,
		},
	}
	if longformEnabled && tsDB2 != nil {
		contentTypes = append(contentTypes, contentType{
			kinds:    []nostr.Kind{30023},
			validate: validateLongform,
			store:    func(e nostr.Event) { storeLongform(true, tsDB2, e) },
			fetch:    tsDB2.QueryEvents,
			count:    tsDB2.CountEvents,
			deleteID: tsDB2.DeleteEvent,
			chunked:  true,
		})
	}
	if wikiEnabled && tsDB3 != nil {
		contentTypes = append(contentTypes, contentType{
			kinds:    []nostr.Kind{30818},
			validate: validateWiki,
			store:    func(e nostr.Event) { storeWiki(true, tsDB3, e) },
			fetch:    tsDB3.QueryEvents,
			count:    tsDB3.CountEvents,
			deleteID: tsDB3.DeleteEvent,
			chunked:  true,
		})
	}
	if calendarEnabled && tsDB4 != nil {
		contentTypes = append(contentTypes, contentType{
			kinds:    []nostr.Kind{31922, 31923, 31924, 31925},
			validate: validateCalendar,
			store: func(e nostr.Event) {
				if calendar.IsCalendarEventKind(e.Kind) {
					if err := calStore.GetIndex().IndexEvent(e); err != nil {
						fmt.Printf("calendar index %s: %v\n", e.ID.Hex(), err)
					}
				}
				storeCalendar(true, tsDB4, e)
			},
			fetch: calendarFetch(calStore.QueryEvents, tsDB4.QueryEvents),
			count: tsDB4.CountEvents,
			deleteID: func(id nostr.ID) error {
				_ = calStore.GetIndex().RemoveEvent(id) // best-effort index cleanup
				return tsDB4.DeleteEvent(id)
			},
		})
	}
	reg := newRegistry(contentTypes...)

	// Initialize embedding client if configured
	var embedder *EmbeddingClient
	if endpoint := os.Getenv("EMBED_ENDPOINT"); endpoint != "" {
		token := os.Getenv("EMBED_TOKEN")
		embedder = NewEmbeddingClient(endpoint, token)
		fmt.Printf("Embedding service configured: %s\n", endpoint)
	}

	// Load semantic config and configure TSBackend
	semanticCfg, err := mgmt.LoadSemanticConfig()
	if err != nil {
		fmt.Printf("Warning: failed to load semantic config: %v\n", err)
		semanticCfg = DefaultSemanticConfig()
	}

	// Override enabled state from env var if set
	if os.Getenv("SEMANTIC_SEARCH_ENABLED") == "true" && embedder != nil {
		if !semanticCfg.Enabled {
			semanticCfg.Enabled = true
			mgmt.SaveSemanticConfig(semanticCfg) // Persist so NIP-86 reflects it
		}
	}

	if semanticCfg.Enabled && embedder != nil {
		tsDB.Embedder = embedder
		tsDB.EmbedFields = semanticCfg.EmbedFields
		fmt.Printf("Semantic search enabled with fields: %v\n", semanticCfg.EmbedFields)
	} else {
		fmt.Println("Semantic search disabled")
	}

	// Structured-collection reindex targets: each enabled structured type is
	// dropped+reprojected from BoltDB during a reindex (the AMB collection has
	// its own special path). Empty when both flags are off.
	var structuredTargets []structuredReindexTarget
	if longformEnabled && tsDB2 != nil {
		structuredTargets = append(structuredTargets, structuredReindexTarget{
			label:     "longform",
			kinds:     []nostr.Kind{30023},
			recreate:  func() error { return tsDB2.RecreateCollection(tsDB2.Schema) },
			reproject: func(e nostr.Event) error { return reprojectStructured(tsDB2, e, nostrToLongform) },
		})
	}
	if wikiEnabled && tsDB3 != nil {
		structuredTargets = append(structuredTargets, structuredReindexTarget{
			label:     "wiki",
			kinds:     []nostr.Kind{30818},
			recreate:  func() error { return tsDB3.RecreateCollection(tsDB3.Schema) },
			reproject: func(e nostr.Event) error { return reprojectStructured(tsDB3, e, nostrToWiki) },
		})
	}
	if calendarEnabled && tsDB4 != nil {
		structuredTargets = append(structuredTargets, structuredReindexTarget{
			label:    "calendar",
			kinds:    []nostr.Kind{31922, 31923, 31924, 31925},
			recreate: func() error { return tsDB4.RecreateCollection(tsDB4.Schema) },
			reproject: func(e nostr.Event) error {
				if calendar.IsCalendarEventKind(e.Kind) {
					if err := calStore.GetIndex().IndexEvent(e); err != nil {
						return err
					}
				}
				return reprojectStructured(tsDB4, e, nostrToCalendar)
			},
		})
	}

	// Reindexer for rebuilding Typesense from BoltDB
	reindexer := NewReindexer(&tsDB, &boltDB, &mgmt, contentStore, structuredTargets)

	relay.OnConnect = func(ctx context.Context) {
		khatru.RequestAuth(ctx)
	}

	// Signing identity for relay-originated kind-21142 snippet events.
	// RELAY_SECKEY must be a dedicated key — never the operator's. Without
	// it, a fresh key is generated per boot: snippets stay validly signed,
	// the identity just isn't stable across restarts.
	var relaySK nostr.SecretKey
	if hexKey := os.Getenv("RELAY_SECKEY"); hexKey != "" {
		var err error
		relaySK, err = nostr.SecretKeyFromHex(hexKey)
		if err != nil {
			panic(fmt.Sprintf("invalid RELAY_SECKEY: %v", err))
		}
	} else {
		relaySK = nostr.Generate()
		fmt.Println("RELAY_SECKEY not set — generated ephemeral snippet-signing key for this boot")
	}
	fmt.Printf("Snippet signer pubkey: %s\n", nostr.GetPublicKey(relaySK).Hex())

	// Optional chunk-level re-ranking of NIP-50 searches via amb-indexer.
	// Off by default; when enabled, searches are ranked by best matching
	// passage in the chunk index and fall back to plain Typesense search on
	// any indexer error (see chunk_rerank.go).
	var chunkSearcher semantic.ChunkSearcher
	if os.Getenv("CHUNK_RERANK_ENABLED") == "true" {
		indexerURL := os.Getenv("INDEXER_BASE_URL")
		if indexerURL == "" {
			indexerURL = "http://amb-indexer:8080"
		}
		token := os.Getenv("INDEXER_API_TOKEN")
		if token == "" {
			fmt.Println("Warning: CHUNK_RERANK_ENABLED but INDEXER_API_TOKEN unset — chunk re-ranking disabled")
		} else {
			timeout := semantic.DefaultChunkSearchTimeout
			if ms := os.Getenv("CHUNK_RERANK_TIMEOUT_MS"); ms != "" {
				if n, err := strconv.Atoi(ms); err == nil && n > 0 {
					timeout = time.Duration(n) * time.Millisecond
				}
			}
			chunkSearcher = semantic.NewHTTPChunkSearcherWithTimeout(indexerURL, token, timeout)
			fmt.Printf("Chunk re-ranking enabled via %s (timeout %s)\n", indexerURL, timeout)
		}
	}

	// Dual-write eventstore wiring (query from Typesense, persist to both)
	relay.QueryStored = func(ctx context.Context, filter nostr.Filter) iter.Seq[nostr.Event] {
		maxLimit := 250
		if khatru.IsNegentropySession(ctx) {
			maxLimit = 250 * 20
		}
		// Chunk-rerank only owns searches that can be served by the chunk index.
		// Calendar-only (non-chunked) searches go straight to plain full-text;
		// otherwise the rerank would drop them whenever the term also matched a
		// chunk from another content type.
		if reg.targetsChunked(filter) {
			return semantic.ChunkRerankQuery(ctx, filter, chunkSearcher, reg.fetch, maxLimit, relaySK)
		}
		return reg.fetch(filter, maxLimit)
	}
	relay.Count = func(ctx context.Context, filter nostr.Filter) (uint32, error) {
		return reg.count(filter)
	}
	relay.StoreEvent = func(ctx context.Context, event nostr.Event) error {
		boltBuf.Queue(event, false)
		reg.store(event)
		return nil
	}
	relay.ReplaceEvent = func(ctx context.Context, event nostr.Event) error {
		boltBuf.Queue(event, true)
		reg.store(event)
		return nil
	}
	relay.DeleteEvent = func(ctx context.Context, id nostr.ID) error {
		boltDB.DeleteEvent(id)
		_ = contentStore.Delete(id.Hex()) // idempotent; safe when no content row existed
		// The id carries no kind, so try every collection (each delete is a
		// no-op when the id lives elsewhere).
		return reg.deleteEverywhere(id, func(err error) {
			fmt.Printf("delete %s (secondary collection): %v\n", id.Hex(), err)
		})
	}

	relay.Negentropy = true

	// Event validation + ban check
	relay.OnEvent = func(ctx context.Context, event nostr.Event) (reject bool, msg string) {
		if mgmt.IsPubKeyBanned(event.PubKey) {
			return true, "pubkey is banned"
		}
		if mgmt.IsEventBanned(event.ID) {
			return true, "event is banned"
		}
		if acl.IsWriteRestricted() && !admins.IsAdmin(event.PubKey) && !acl.IsWriteAllowed(event.PubKey.Hex()) {
			return true, "restricted: pubkey not on write allowlist"
		}
		if event.Kind == nostr.KindDeletion {
			return false, ""
		}
		return reg.validate(event)
	}

	// Read access enforcement (NIP-42 auth required when read-restricted)
	checkReadAccess := func(ctx context.Context, filter nostr.Filter) (reject bool, msg string) {
		if !acl.IsReadRestricted() {
			return false, ""
		}
		if khatru.IsInternalCall(ctx) {
			return false, ""
		}
		authed, ok := khatru.GetAuthed(ctx)
		if !ok {
			return true, "auth-required: authentication required to read"
		}
		if admins.IsAdmin(authed) {
			return false, ""
		}
		if !acl.IsReadAllowed(authed.Hex()) {
			return true, "restricted: pubkey not on read allowlist"
		}
		return false, ""
	}
	relay.OnRequest = checkReadAccess
	relay.OnCount = checkReadAccess

	// Prevent broadcasting to non-allowed connections when read-restricted
	relay.PreventBroadcast = func(ws *khatru.WebSocket, filter nostr.Filter, event nostr.Event) bool {
		if !acl.IsReadRestricted() {
			return false
		}
		for _, pk := range ws.AuthedPublicKeys {
			if admins.IsAdmin(pk) || acl.IsReadAllowed(pk.Hex()) {
				return false
			}
		}
		return true
	}

	// Dynamic NIP-11 — set auth_required when read-restricted
	relay.OverwriteRelayInformation = func(ctx context.Context, r *http.Request, info nip11.RelayInformationDocument) nip11.RelayInformationDocument {
		if acl.IsReadRestricted() {
			if info.Limitation == nil {
				info.Limitation = &nip11.RelayLimitationDocument{}
			}
			info.Limitation.AuthRequired = true
		}
		return info
	}

	// NIP-86 Management API
	relay.ManagementAPI.OnAPICall = func(ctx context.Context, mp nip86.MethodParams) (reject bool, msg string) {
		authed, ok := khatru.GetAuthed(ctx)
		if !ok {
			return true, "not authenticated"
		}
		if !admins.IsAllowed(authed, mp.MethodName()) {
			return true, "not authorized"
		}
		return false, ""
	}

	relay.ManagementAPI.BanPubKey = func(ctx context.Context, pubkey nostr.PubKey, reason string) error {
		return mgmt.BanPubKey(pubkey, reason)
	}
	relay.ManagementAPI.ListBannedPubKeys = func(ctx context.Context) ([]nip86.PubKeyReason, error) {
		return mgmt.ListBannedPubKeys()
	}
	relay.ManagementAPI.AllowPubKey = func(ctx context.Context, pubkey nostr.PubKey, reason string) error {
		return mgmt.AllowPubKey(pubkey)
	}
	relay.ManagementAPI.BanEvent = func(ctx context.Context, id nostr.ID, reason string) error {
		boltDB.DeleteEvent(id)
		_ = contentStore.Delete(id.Hex()) // idempotent
		if err := reg.deleteEverywhere(id, func(e error) {
			fmt.Printf("ban-delete %s (secondary collection): %v\n", id.Hex(), e)
		}); err != nil {
			return err
		}
		return mgmt.BanEvent(id, reason)
	}
	relay.ManagementAPI.ListBannedEvents = func(ctx context.Context) ([]nip86.IDReason, error) {
		return mgmt.ListBannedEvents()
	}
	relay.ManagementAPI.AllowEvent = func(ctx context.Context, id nostr.ID, reason string) error {
		return mgmt.AllowEvent(id)
	}

	relay.ManagementAPI.GrantAdmin = func(ctx context.Context, pubkey nostr.PubKey, methods []string) error {
		if err := mgmt.AddAdmin(pubkey.Hex(), methods); err != nil {
			return err
		}
		admins.Grant(pubkey, methods)
		return nil
	}
	relay.ManagementAPI.RevokeAdmin = func(ctx context.Context, pubkey nostr.PubKey, methods []string) error {
		if admins.IsStatic(pubkey) {
			return fmt.Errorf("cannot revoke env-configured admin")
		}
		if err := mgmt.RemoveAdmin(pubkey.Hex(), methods); err != nil {
			return err
		}
		admins.Revoke(pubkey, methods)
		return nil
	}

	relay.ManagementAPI.ChangeRelayName = func(ctx context.Context, name string) error {
		relay.Info.Name = name
		return nil
	}
	relay.ManagementAPI.ChangeRelayDescription = func(ctx context.Context, desc string) error {
		relay.Info.Description = desc
		return nil
	}
	relay.ManagementAPI.ChangeRelayIcon = func(ctx context.Context, icon string) error {
		relay.Info.Icon = icon
		return nil
	}

	relay.ManagementAPI.Stats = func(ctx context.Context) (nip86.Response, error) {
		count, err := tsDB.CountEvents(nostr.Filter{Kinds: []nostr.Kind{30142}})
		if err != nil {
			return nip86.Response{}, err
		}
		return nip86.Response{
			Result: map[string]any{
				"event_count": count,
				"uptime":      time.Since(startTime).String(),
			},
		}, nil
	}

	// Custom Typesense management methods via Generic handler
	relay.ManagementAPI.Generic = func(ctx context.Context, request nip86.Request) (nip86.Response, error) {
		switch request.Method {
		case "getcollectionschema":
			schema, err := mgmt.LoadSchema()
			if err != nil {
				return nip86.Response{}, err
			}
			if schema == nil {
				def := typesense30142.DefaultSchema()
				schema = &def
			}
			return nip86.Response{Result: schema}, nil

		case "updatecollectionschema":
			if len(request.Params) == 0 {
				return nip86.Response{Error: "missing schema parameter"}, nil
			}
			schemaJSON, err := json.Marshal(request.Params[0])
			if err != nil {
				return nip86.Response{Error: fmt.Sprintf("invalid schema: %v", err)}, nil
			}
			var schema typesense30142.CollectionSchema
			if err := json.Unmarshal(schemaJSON, &schema); err != nil {
				return nip86.Response{Error: fmt.Sprintf("invalid schema JSON: %v", err)}, nil
			}
			if err := mgmt.SaveSchema(schema); err != nil {
				return nip86.Response{}, err
			}
			return nip86.Response{Result: true}, nil

		case "resetcollectionschema":
			if err := mgmt.DeleteSchema(); err != nil {
				return nip86.Response{}, err
			}
			return nip86.Response{Result: true}, nil

		case "reindex":
			if err := reindexer.Start(); err != nil {
				return nip86.Response{Error: err.Error()}, nil
			}
			return nip86.Response{Result: "reindex started"}, nil

		case "getreindexstatus":
			return nip86.Response{Result: reindexer.GetStatus()}, nil

		case "getsemanticsearchconfig":
			cfg, err := mgmt.LoadSemanticConfig()
			if err != nil {
				return nip86.Response{}, err
			}
			return nip86.Response{Result: cfg}, nil

		case "updatesemanticsearchconfig":
			if len(request.Params) == 0 {
				return nip86.Response{Error: "missing config parameter"}, nil
			}
			cfgJSON, err := json.Marshal(request.Params[0])
			if err != nil {
				return nip86.Response{Error: fmt.Sprintf("invalid config: %v", err)}, nil
			}
			var cfg SemanticConfig
			if err := json.Unmarshal(cfgJSON, &cfg); err != nil {
				return nip86.Response{Error: fmt.Sprintf("invalid config JSON: %v", err)}, nil
			}
			if err := mgmt.SaveSemanticConfig(cfg); err != nil {
				return nip86.Response{}, err
			}
			// Update TSBackend config at runtime
			if cfg.Enabled && embedder != nil {
				tsDB.Embedder = embedder
				tsDB.EmbedFields = cfg.EmbedFields
			} else {
				tsDB.Embedder = nil
				tsDB.EmbedFields = nil
			}
			return nip86.Response{Result: true}, nil

		case "enablesemanticsearch":
			if embedder == nil {
				return nip86.Response{Error: "embedding service not configured (EMBED_ENDPOINT not set)"}, nil
			}
			cfg, _ := mgmt.LoadSemanticConfig()
			cfg.Enabled = true
			if err := mgmt.SaveSemanticConfig(cfg); err != nil {
				return nip86.Response{}, err
			}
			tsDB.Embedder = embedder
			tsDB.EmbedFields = cfg.EmbedFields
			return nip86.Response{Result: true}, nil

		case "disablesemanticsearch":
			cfg, _ := mgmt.LoadSemanticConfig()
			cfg.Enabled = false
			if err := mgmt.SaveSemanticConfig(cfg); err != nil {
				return nip86.Response{}, err
			}
			tsDB.Embedder = nil
			tsDB.EmbedFields = nil
			return nip86.Response{Result: true}, nil

		case "getaccesscontrol":
			writeList, _ := acl.ListWriteAllowPubkeys()
			readList, _ := acl.ListReadAllowPubkeys()
			refs := acl.LoadListReferences()
			refSlice := make([]ListReference, 0, len(refs))
			for _, ref := range refs {
				refSlice = append(refSlice, ref)
			}
			return nip86.Response{Result: map[string]any{
				"write_restricted": acl.IsWriteRestricted(),
				"read_restricted":  acl.IsReadRestricted(),
				"write_allowlist":  writeList,
				"read_allowlist":   readList,
				"list_references":  refSlice,
			}}, nil

		case "setaccesscontrol":
			if len(request.Params) == 0 {
				return nip86.Response{Error: "missing config parameter"}, nil
			}
			cfgJSON, err := json.Marshal(request.Params[0])
			if err != nil {
				return nip86.Response{Error: fmt.Sprintf("invalid config: %v", err)}, nil
			}
			var cfg AccessControlConfig
			if err := json.Unmarshal(cfgJSON, &cfg); err != nil {
				return nip86.Response{Error: fmt.Sprintf("invalid config JSON: %v", err)}, nil
			}
			if err := acl.SetAccessControl(cfg); err != nil {
				return nip86.Response{}, err
			}
			return nip86.Response{Result: true}, nil

		case "addtowriteallowlist":
			if len(request.Params) == 0 {
				return nip86.Response{Error: "missing pubkey parameter"}, nil
			}
			pubkey, _ := request.Params[0].(string)
			if pubkey == "" {
				return nip86.Response{Error: "invalid pubkey"}, nil
			}
			var reason string
			if len(request.Params) > 1 {
				reason, _ = request.Params[1].(string)
			}
			if err := acl.AddWriteAllowPubkey(pubkey, reason); err != nil {
				return nip86.Response{}, err
			}
			return nip86.Response{Result: true}, nil

		case "removefromwriteallowlist":
			if len(request.Params) == 0 {
				return nip86.Response{Error: "missing pubkey parameter"}, nil
			}
			pubkey, _ := request.Params[0].(string)
			if pubkey == "" {
				return nip86.Response{Error: "invalid pubkey"}, nil
			}
			if err := acl.RemoveWriteAllowPubkey(pubkey); err != nil {
				return nip86.Response{}, err
			}
			return nip86.Response{Result: true}, nil

		case "addtoreadallowlist":
			if len(request.Params) == 0 {
				return nip86.Response{Error: "missing pubkey parameter"}, nil
			}
			pubkey, _ := request.Params[0].(string)
			if pubkey == "" {
				return nip86.Response{Error: "invalid pubkey"}, nil
			}
			var reason string
			if len(request.Params) > 1 {
				reason, _ = request.Params[1].(string)
			}
			if err := acl.AddReadAllowPubkey(pubkey, reason); err != nil {
				return nip86.Response{}, err
			}
			return nip86.Response{Result: true}, nil

		case "removefromreadallowlist":
			if len(request.Params) == 0 {
				return nip86.Response{Error: "missing pubkey parameter"}, nil
			}
			pubkey, _ := request.Params[0].(string)
			if pubkey == "" {
				return nip86.Response{Error: "invalid pubkey"}, nil
			}
			if err := acl.RemoveReadAllowPubkey(pubkey); err != nil {
				return nip86.Response{}, err
			}
			return nip86.Response{Result: true}, nil

		case "addlistreference":
			if len(request.Params) == 0 {
				return nip86.Response{Error: "missing list reference parameter"}, nil
			}
			refJSON, err := json.Marshal(request.Params[0])
			if err != nil {
				return nip86.Response{Error: fmt.Sprintf("invalid list reference: %v", err)}, nil
			}
			var ref ListReference
			if err := json.Unmarshal(refJSON, &ref); err != nil {
				return nip86.Response{Error: fmt.Sprintf("invalid list reference JSON: %v", err)}, nil
			}
			if ref.Pubkey == "" || len(ref.Relays) == 0 || (ref.Kind != 3 && ref.Kind != 30000) {
				return nip86.Response{Error: "list reference requires pubkey, relays, and kind (3 or 30000)"}, nil
			}
			if ref.Direction == "" {
				ref.Direction = "both"
			}
			if err := acl.AddListReference(ref); err != nil {
				return nip86.Response{}, err
			}
			return nip86.Response{Result: true}, nil

		case "removelistreference":
			if len(request.Params) == 0 {
				return nip86.Response{Error: "missing pubkey parameter"}, nil
			}
			pubkey, _ := request.Params[0].(string)
			if pubkey == "" {
				return nip86.Response{Error: "invalid pubkey"}, nil
			}
			kind := 3
			if len(request.Params) > 1 {
				if k, ok := request.Params[1].(float64); ok {
					kind = int(k)
				}
			}
			var dtag string
			if len(request.Params) > 2 {
				dtag, _ = request.Params[2].(string)
			}
			if err := acl.RemoveListReference(pubkey, kind, dtag); err != nil {
				return nip86.Response{}, err
			}
			return nip86.Response{Result: true}, nil

		case "refreshlistreferences":
			n := acl.RefreshAllLists()
			return nip86.Response{Result: map[string]any{"refreshed": n}}, nil

		case "setcontent":
			if len(request.Params) < 4 {
				return nip86.Response{Error: "setcontent requires [event_id, text, fetched_at, status, (source_url)]"}, nil
			}
			eventIDHex, ok := request.Params[0].(string)
			if !ok || eventIDHex == "" {
				return nip86.Response{Error: "event_id must be a non-empty string"}, nil
			}
			text, ok := request.Params[1].(string)
			if !ok {
				return nip86.Response{Error: "text must be a string"}, nil
			}
			fetchedAtFloat, ok := request.Params[2].(float64)
			if !ok {
				return nip86.Response{Error: "fetched_at must be a number (unix seconds)"}, nil
			}
			status, ok := request.Params[3].(string)
			if !ok || status == "" {
				return nip86.Response{Error: "status must be a non-empty string"}, nil
			}
			var sourceURL string
			if len(request.Params) > 4 && request.Params[4] != nil {
				s, ok := request.Params[4].(string)
				if !ok {
					return nip86.Response{Error: "source_url must be a string or null"}, nil
				}
				sourceURL = s
			}

			// Verify the event exists in BoltDB — the indexer must only
			// setcontent for events the relay actually holds.
			id, err := nostr.IDFromHex(eventIDHex)
			if err != nil {
				return nip86.Response{Error: fmt.Sprintf("invalid event id: %v", err)}, nil
			}
			var event nostr.Event
			var found bool
			for e := range boltDB.QueryEvents(nostr.Filter{IDs: []nostr.ID{id}, Limit: 1}, 1) {
				event = e
				found = true
			}
			if !found {
				return nip86.Response{Error: "event not found"}, nil
			}
			entry := ContentEntry{
				Text:      text,
				FetchedAt: int64(fetchedAtFloat),
				Status:    status,
				SourceURL: sourceURL,
			}
			if err := contentStore.Put(eventIDHex, entry); err != nil {
				return nip86.Response{Error: fmt.Sprintf("content store put: %v", err)}, nil
			}
			// Queue projection. The buffer flushes the pending event
			// batch (including this event) before issuing the patch, so
			// the doc is guaranteed to exist in Typesense by patch time.
			// Direct PATCH would race with tsBuf's batched flush.
			tsBuf.QueueContent(event, entry)
			return nip86.Response{Result: true}, nil

		case "refetchcontent":
			if len(request.Params) == 0 {
				return nip86.Response{Error: "refetchcontent requires [event_id]"}, nil
			}
			eventIDHex, ok := request.Params[0].(string)
			if !ok || eventIDHex == "" {
				return nip86.Response{Error: "event_id must be a non-empty string"}, nil
			}
			id, err := nostr.IDFromHex(eventIDHex)
			if err != nil {
				return nip86.Response{Error: fmt.Sprintf("invalid event id: %v", err)}, nil
			}
			var event nostr.Event
			var found bool
			for e := range boltDB.QueryEvents(nostr.Filter{IDs: []nostr.ID{id}, Limit: 1}, 1) {
				event = e
				found = true
			}
			if !found {
				return nip86.Response{Error: "event not found"}, nil
			}
			if err := contentStore.Delete(eventIDHex); err != nil {
				return nip86.Response{Error: fmt.Sprintf("content store delete: %v", err)}, nil
			}
			// Project an empty content entry through the buffer so
			// Typesense reflects the clear. Direct PATCH would race
			// with tsBuf's pending batches.
			tsBuf.QueueContent(event, ContentEntry{})
			// Signal the indexer to re-ingest this event. Best-effort:
			// the operator's intent (clear content) already succeeded,
			// so a failure here is logged but does not fail the call.
			if err := mgmt.MarkNeedsRefetch(eventIDHex); err != nil {
				fmt.Printf("refetchcontent: mark needs_refetch %s: %v\n", eventIDHex, err)
			}
			return nip86.Response{Result: true}, nil

		case "listrefetch":
			ids, err := mgmt.ListNeedsRefetch()
			if err != nil {
				return nip86.Response{Error: fmt.Sprintf("list needs_refetch: %v", err)}, nil
			}
			if ids == nil {
				ids = []string{}
			}
			return nip86.Response{Result: map[string]any{"event_ids": ids}}, nil

		case "acknowledgerefetch":
			if len(request.Params) == 0 {
				return nip86.Response{Error: "acknowledgerefetch requires [event_ids]"}, nil
			}
			rawIDs, ok := request.Params[0].([]any)
			if !ok {
				return nip86.Response{Error: "event_ids must be an array of strings"}, nil
			}
			acked := 0
			for _, raw := range rawIDs {
				id, ok := raw.(string)
				if !ok || id == "" {
					return nip86.Response{Error: "event_ids must contain non-empty strings"}, nil
				}
				if err := mgmt.RemoveNeedsRefetch(id); err != nil {
					return nip86.Response{Error: fmt.Sprintf("remove needs_refetch %s: %v", id, err)}, nil
				}
				acked++
			}
			return nip86.Response{Result: map[string]any{"acknowledged": acked}}, nil

		default:
			return nip86.Response{Error: fmt.Sprintf("unknown method '%s'", request.Method)}, nil
		}
	}

	landing.Setup(relay)

	port := os.Getenv("PORT")
	if port == "" {
		port = "3334"
	}
	fmt.Printf("running on :%s\n", port)
	http.ListenAndServe(":"+port, relay)
}

var startTime = time.Now()

// ensureContentFields appends the three content fields to the schema if they
// are not already present. Idempotent: safe to call on default or custom schemas.
func ensureContentFields(schema *typesense30142.CollectionSchema) {
	have := make(map[string]bool, len(schema.Fields))
	for _, f := range schema.Fields {
		have[f.Name] = true
	}
	if !have["content"] {
		schema.Fields = append(schema.Fields, typesense30142.Field{
			Name: "content", Type: "string", Optional: true,
		})
	}
	if !have["content_fetched_at"] {
		schema.Fields = append(schema.Fields, typesense30142.Field{
			Name: "content_fetched_at", Type: "int64", Optional: true,
		})
	}
	if !have["content_status"] {
		schema.Fields = append(schema.Fields, typesense30142.Field{
			Name: "content_status", Type: "string", Optional: true,
		})
	}
}

// getVersion returns the git commit hash from build info, or "dev" if unavailable.
func getVersion() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			if setting.Key == "vcs.revision" {
				if len(setting.Value) > 7 {
					return setting.Value[:7]
				}
				return setting.Value
			}
		}
	}
	return "dev"
}
