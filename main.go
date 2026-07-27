package main

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"log"
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
	profilesEnabled := os.Getenv("PROFILES_ENABLED") == "true"
	sharesEnabled := os.Getenv("COMMUNITY_SHARES_ENABLED") == "true"
	transferkioskEnabled := os.Getenv("TRANSFERKIOSK_ENABLED") == "true"
	publicationsEnabled := os.Getenv("PUBLICATIONS_ENABLED") == "true"

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
	if sharesEnabled {
		retentionKinds = append(retentionKinds, []int{16}, []int{30222})
	}
	if transferkioskEnabled {
		retentionKinds = append(retentionKinds, []int{30143}, []int{30144})
	}
	if publicationsEnabled {
		retentionKinds = append(retentionKinds, []int{30040}, []int{30041})
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

	// German stopword set shared by every Typesense collection (sets are
	// global to a Typesense instance). Standard function words plus a few
	// domain filler words so a natural-language query like "Materialien zum
	// Thema Frieden im Religionsunterricht" reduces to its topical terms
	// ("Frieden Religionsunterricht") before lexical scoring.
	stopwordsList := append(append([]string{}, typesense30142.GermanStopwords...),
		"thema", "themen", "material", "materialien")
	const stopwordsSet = "amb_de"

	// Typesense backend (search index)
	tsDB := typesense30142.TSBackend{
		ApiKey:          os.Getenv("TS_APIKEY"),
		Host:            os.Getenv("TS_HOST"),
		CollectionName:  os.Getenv("TS_COLLECTION"),
		RawEventStore:   &boltDB,
		StopwordsSet:    stopwordsSet,
		StopwordsList:   stopwordsList,
		StopwordsLocale: "de",
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
			ApiKey:          os.Getenv("TS_APIKEY"),
			Host:            os.Getenv("TS_HOST"),
			CollectionName:  lfColl,
			RawEventStore:   &boltDB,
			Schema:          &lfSchema,
			SearchFields:    "title,summary,content",
			StopwordsSet:    stopwordsSet,
			StopwordsList:   stopwordsList,
			StopwordsLocale: "de",
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
			ApiKey:          os.Getenv("TS_APIKEY"),
			Host:            os.Getenv("TS_HOST"),
			CollectionName:  wikiColl,
			RawEventStore:   &boltDB,
			Schema:          &wSchema,
			SearchFields:    "title,summary,content",
			StopwordsSet:    stopwordsSet,
			StopwordsList:   stopwordsList,
			StopwordsLocale: "de",
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
			ApiKey:          os.Getenv("TS_APIKEY"),
			Host:            os.Getenv("TS_HOST"),
			CollectionName:  calColl,
			RawEventStore:   &boltDB,
			Schema:          &calSchema,
			SearchFields:    "title,summary,content,location",
			StopwordsSet:    stopwordsSet,
			StopwordsList:   stopwordsList,
			StopwordsLocale: "de",
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

	// Profiles (kind-0) Typesense backend — gated behind PROFILES_ENABLED. One
	// document per author pubkey, populated by ProfileManager (not by clients).
	// RawEventStore is intentionally nil: profiles are not relay events in
	// BoltDB, so QueryEvents reconstructs them from the stored eventRaw field.
	var profilesDB *typesense30142.TSBackend
	if profilesEnabled {
		pColl := os.Getenv("TS_COLLECTION_PROFILES")
		if pColl == "" {
			pColl = "profiles_0"
		}
		pSchema := profileSchema(pColl)
		profilesDB = &typesense30142.TSBackend{
			ApiKey:         os.Getenv("TS_APIKEY"),
			Host:           os.Getenv("TS_HOST"),
			CollectionName: pColl,
			Schema:         &pSchema,
			SearchFields:   "name,display_name,about,nip05",
			// Verified-first ranking: at equal text relevance a nip05-verified
			// profile outranks an unverified one; explicit client sort: wins.
			SearchSortBy:    "_text_match:desc,nip05_verified:desc,eventCreatedAt:desc",
			StopwordsSet:    stopwordsSet,
			StopwordsList:   stopwordsList,
			StopwordsLocale: "de",
		}
		if err := profilesDB.Init(); err != nil {
			panic(fmt.Sprintf("profiles TSBackend init: %v", err))
		}
		fmt.Printf("Profiles (kind 0) enabled — collection %s\n", pColl)
	}

	// Community shares (kind 16 repost + legacy kind 30222) Typesense backend —
	// gated behind COMMUNITY_SHARES_ENABLED. Disjoint id-keyed collection; nil
	// when the flag is off so the registry omits it. SearchFields is set to a
	// real indexed string field (eventID) because shares carry no fulltext —
	// they are queried by #h / community:<pubkey> / kind, never by free text.
	var tsDB5 *typesense30142.TSBackend
	if sharesEnabled {
		shColl := os.Getenv("TS_COLLECTION_SHARES")
		if shColl == "" {
			shColl = "community_shares"
		}
		shSchema := sharesSchema(shColl)
		tsDB5 = &typesense30142.TSBackend{
			ApiKey:          os.Getenv("TS_APIKEY"),
			Host:            os.Getenv("TS_HOST"),
			CollectionName:  shColl,
			RawEventStore:   &boltDB,
			Schema:          &shSchema,
			SearchFields:    "eventID",
			StopwordsSet:    stopwordsSet,
			StopwordsList:   stopwordsList,
			StopwordsLocale: "de",
		}
		if err := tsDB5.Init(); err != nil {
			panic(fmt.Sprintf("shares TSBackend init: %v", err))
		}
		fmt.Printf("Community shares (kinds 16, 30222) enabled — collection %s\n", shColl)
	}

	// Transferkiosk (NIP-DIDACTIC kinds 30143/30144) backend — gated behind
	// TRANSFERKIOSK_ENABLED. One shared collection for projekt/massnahme; nil
	// when the flag is off so the registry omits the kinds. Publikationen
	// (formerly kind 30145) moved to NKBIP-01 kind 30040 — see publications.go.
	var tsDB6 *typesense30142.TSBackend
	if transferkioskEnabled {
		tkColl := os.Getenv("TS_COLLECTION_TRANSFERKIOSK")
		if tkColl == "" {
			tkColl = "transferkiosk"
		}
		tkSchema := transferkioskSchema(tkColl)
		tsDB6 = &typesense30142.TSBackend{
			ApiKey:          os.Getenv("TS_APIKEY"),
			Host:            os.Getenv("TS_HOST"),
			CollectionName:  tkColl,
			RawEventStore:   &boltDB,
			Schema:          &tkSchema,
			SearchFields:    "name,searchText,content",
			StopwordsSet:    stopwordsSet,
			StopwordsList:   stopwordsList,
			StopwordsLocale: "de",
		}
		if err := tsDB6.Init(); err != nil {
			panic(fmt.Sprintf("transferkiosk TSBackend init: %v", err))
		}
		fmt.Printf("Transferkiosk (kinds 30143/30144) enabled — collection %s\n", tkColl)
	}

	// Publications (NKBIP-01 kinds 30040/30041) backend — gated behind
	// PUBLICATIONS_ENABLED. One shared collection for indices + sections; nil
	// when the flag is off so the registry omits the kinds.
	var tsDB7 *typesense30142.TSBackend
	if publicationsEnabled {
		pubColl := os.Getenv("TS_COLLECTION_PUBLICATIONS")
		if pubColl == "" {
			pubColl = "publications"
		}
		pubSchema := publicationsSchema(pubColl)
		tsDB7 = &typesense30142.TSBackend{
			ApiKey:          os.Getenv("TS_APIKEY"),
			Host:            os.Getenv("TS_HOST"),
			CollectionName:  pubColl,
			RawEventStore:   &boltDB,
			Schema:          &pubSchema,
			SearchFields:    "title,summary,searchText,content",
			StopwordsSet:    stopwordsSet,
			StopwordsList:   stopwordsList,
			StopwordsLocale: "de",
		}
		if err := tsDB7.Init(); err != nil {
			panic(fmt.Sprintf("publications TSBackend init: %v", err))
		}
		fmt.Printf("Publications (kinds 30040/30041) enabled — collection %s\n", pubColl)
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
			chunked:  true,
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
	if profilesEnabled && profilesDB != nil {
		contentTypes = append(contentTypes, contentType{
			kinds:    []nostr.Kind{0},
			validate: func(nostr.Event) (bool, string) { return true, "kind not accepted" }, // reject client kind-0 writes
			store:    func(nostr.Event) {},                                                  // never reached: validate rejects first
			fetch:    profilesDB.QueryEvents,
			count:    profilesDB.CountEvents,
			deleteID: profilesDB.DeleteEvent,
			chunked:  false,
		})
	}
	if sharesEnabled && tsDB5 != nil {
		contentTypes = append(contentTypes, contentType{
			kinds:    []nostr.Kind{16, 30222},
			validate: validateShare,
			store:    func(e nostr.Event) { storeShare(true, tsDB5, e) },
			fetch:    tsDB5.QueryEvents,
			count:    tsDB5.CountEvents,
			deleteID: tsDB5.DeleteEvent,
			chunked:  false,
		})
	}
	if transferkioskEnabled && tsDB6 != nil {
		contentTypes = append(contentTypes, contentType{
			kinds:    []nostr.Kind{30143, 30144},
			validate: validateTransferkiosk,
			store:    func(e nostr.Event) { storeTransferkiosk(true, tsDB6, e) },
			fetch:    tsDB6.QueryEvents,
			count:    tsDB6.CountEvents,
			deleteID: tsDB6.DeleteEvent,
			chunked:  true,
		})
	}
	if publicationsEnabled && tsDB7 != nil {
		contentTypes = append(contentTypes, contentType{
			kinds:    []nostr.Kind{30040, 30041},
			validate: validatePublication,
			store:    func(e nostr.Event) { storePublication(true, tsDB7, e) },
			fetch:    tsDB7.QueryEvents,
			count:    tsDB7.CountEvents,
			deleteID: tsDB7.DeleteEvent,
			chunked:  true,
		})
	}
	// NIP-09 (issue #1): serve stored kind-5 deletion requests. Always on —
	// deletion *processing* has always been unconditional, so the deletion
	// trail is too. Registered last so content types keep fan-out priority;
	// servedKinds is snapshotted here so the write policy scopes 'a'-only
	// deletions to kinds this relay actually serves.
	servedKinds := make(map[nostr.Kind]bool)
	for _, ct := range contentTypes {
		for _, k := range ct.kinds {
			servedKinds[k] = true
		}
	}
	contentTypes = append(contentTypes, deletionContentType(&boltDB, servedKinds))
	reg := newRegistry(contentTypes...)

	var profileContentKinds []nostr.Kind
	for _, k := range reg.kinds() {
		if k != 0 && k != nostr.KindDeletion {
			profileContentKinds = append(profileContentKinds, k)
		}
	}

	// Discovery scope for communities: share kinds + content kinds (content can
	// carry its own h tag). Declared here so the profile backfill closure below
	// can union communities in; reused by the community registry further down.
	communityKinds := append([]nostr.Kind{16, 30222}, profileContentKinds...)

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
		// Structured collections (long-form, wiki, calendar) reuse the same
		// hybrid query path; setting Embedder activates BM25⊕vector RRF for
		// them. They embed on write via EmbedText(), so no EmbedFields needed.
		// Each backend is nil when its feature flag is off, hence the guards.
		if tsDB2 != nil {
			tsDB2.Embedder = embedder
		}
		if tsDB3 != nil {
			tsDB3.Embedder = embedder
		}
		if tsDB4 != nil {
			tsDB4.Embedder = embedder
		}
		if tsDB6 != nil {
			tsDB6.Embedder = embedder
		}
		if tsDB7 != nil {
			tsDB7.Embedder = embedder
		}
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
	if sharesEnabled && tsDB5 != nil {
		structuredTargets = append(structuredTargets, structuredReindexTarget{
			label:     "shares",
			kinds:     []nostr.Kind{16, 30222},
			recreate:  func() error { return tsDB5.RecreateCollection(tsDB5.Schema) },
			reproject: func(e nostr.Event) error { return reprojectStructured(tsDB5, e, nostrToShare) },
		})
	}
	if transferkioskEnabled && tsDB6 != nil {
		structuredTargets = append(structuredTargets, structuredReindexTarget{
			label:     "transferkiosk",
			kinds:     []nostr.Kind{30143, 30144},
			recreate:  func() error { return tsDB6.RecreateCollection(tsDB6.Schema) },
			reproject: func(e nostr.Event) error { return reprojectStructured(tsDB6, e, nostrToTransferkiosk) },
		})
	}
	if publicationsEnabled && tsDB7 != nil {
		structuredTargets = append(structuredTargets, structuredReindexTarget{
			label:     "publications",
			kinds:     []nostr.Kind{30040, 30041},
			recreate:  func() error { return tsDB7.RecreateCollection(tsDB7.Schema) },
			reproject: func(e nostr.Event) error { return reprojectStructured(tsDB7, e, nostrToPublication) },
			// Replay indexer-written 30040 fulltext from the ContentStore onto
			// the rebuilt docs (reprojection writes content="" for 30040).
			after: func() (patched, errs int64) {
				type row struct {
					id    string
					entry ContentEntry
				}
				var rows []row // collect under the read txn, patch outside it
				_ = contentStore.ForEach(func(id string, e ContentEntry) error {
					rows = append(rows, row{id, e})
					return nil
				})
				for _, rw := range rows {
					id, err := nostr.IDFromHex(rw.id)
					if err != nil {
						continue
					}
					var ev nostr.Event
					var found bool
					for e := range boltDB.QueryEvents(nostr.Filter{IDs: []nostr.ID{id}, Limit: 1}, 1) {
						ev, found = e, true
					}
					if !found || ev.Kind != 30040 {
						continue // AMB rows are replayed by the AMB path
					}
					docID := docIDFor(ev.Kind, ev.PubKey.Hex(), ev.Tags.GetD())
					if err := PatchContent(tsDB7.Host, tsDB7.ApiKey, tsDB7.CollectionName, docID, rw.entry); err != nil {
						log.Printf("reindex: publication content patch failed for %s: %v", rw.id, err)
						errs++
						continue
					}
					patched++
				}
				return
			},
		})
	}

	// Reindexer for rebuilding Typesense from BoltDB
	reindexer := NewReindexer(&tsDB, &boltDB, &mgmt, contentStore, structuredTargets)

	var profileMgr *ProfileManager
	if profilesEnabled && profilesDB != nil {
		profileRelays := []string{"wss://relay.edufeed.org"}
		if raw := os.Getenv("PROFILE_RELAYS"); raw != "" {
			profileRelays = nil
			for _, r := range strings.Split(raw, ",") {
				if r = strings.TrimSpace(r); r != "" {
					profileRelays = append(profileRelays, r)
				}
			}
		}
		fallbackRelays := []string{"wss://purplepag.es", "wss://relay.damus.io", "wss://relay.nostr.band"}
		if raw, ok := os.LookupEnv("PROFILE_FALLBACK_RELAYS"); ok {
			fallbackRelays = nil
			for _, r := range strings.Split(raw, ",") {
				if r = strings.TrimSpace(r); r != "" {
					fallbackRelays = append(fallbackRelays, r)
				}
			}
		}
		refreshInterval := 6 * time.Hour
		if raw := os.Getenv("PROFILE_REFRESH_INTERVAL"); raw != "" {
			if d, err := time.ParseDuration(raw); err == nil {
				refreshInterval = d
			} else {
				fmt.Printf("profile: bad PROFILE_REFRESH_INTERVAL %q, using %s\n", raw, refreshInterval)
			}
		}
		profileMgr = NewProfileManager(
			&mgmt,
			poolSource{pool: nostr.NewPool()},
			func(e nostr.Event, nip05Verified bool) { storeProfile(profilesEnabled, profilesDB, e, nip05Verified) },
			func() []nostr.PubKey {
				return backfillProfileCandidates(&boltDB, profileContentKinds, communityKinds, 1_000_000)
			},
			verifyNIP05,
			profileRelays,
			fallbackRelays,
			50,
		)
		if err := profileMgr.Init(); err != nil {
			fmt.Printf("profile: init: %v\n", err)
		}
		profileMgr.StartRefreshLoop(refreshInterval)
		defer profileMgr.Stop()
		fmt.Printf("Profiles: fetching from %v (fallback %v), refresh every %s\n", profileRelays, fallbackRelays, refreshInterval)
	}

	var communityRelays []string
	var communityRefresh time.Duration
	var communityReg *CommunityRegistry
	if sharesEnabled {
		communityRelays = []string{"wss://relay.edufeed.org"}
		if raw := os.Getenv("COMMUNITY_RELAYS"); raw != "" {
			communityRelays = nil
			for _, r := range strings.Split(raw, ",") {
				if r = strings.TrimSpace(r); r != "" {
					communityRelays = append(communityRelays, r)
				}
			}
		}
		communityRefresh = 6 * time.Hour
		if raw := os.Getenv("COMMUNITY_REFRESH_INTERVAL"); raw != "" {
			if d, err := time.ParseDuration(raw); err == nil {
				communityRefresh = d
			} else {
				fmt.Printf("community: bad COMMUNITY_REFRESH_INTERVAL %q, using %s\n", raw, communityRefresh)
			}
		}
		communityReg = NewCommunityRegistry(
			poolCommunitySource{pool: nostr.NewPool()},
			func() []string { return backfillCommunities(&boltDB, communityKinds, 1_000_000) },
			communityRelays,
		)
		if err := communityReg.Init(); err != nil {
			fmt.Printf("community: init: %v\n", err)
		}
		communityReg.StartRefreshLoop(communityRefresh)
		defer communityReg.Stop()
		fmt.Printf("Community registry: resolving from %v, refresh every %s\n", communityRelays, communityRefresh)
	}
	var stamper *CommunityStamper
	if sharesEnabled && communityReg != nil && tsDB5 != nil {
		// Per-kind stamp target: which Typesense backend holds each content kind.
		stampTargets := map[nostr.Kind]*typesense30142.TSBackend{30142: &tsDB}
		if longformEnabled && tsDB2 != nil {
			stampTargets[30023] = tsDB2
		}
		if wikiEnabled && tsDB3 != nil {
			stampTargets[30818] = tsDB3
		}
		if calendarEnabled && tsDB4 != nil {
			for _, k := range []nostr.Kind{31922, 31923, 31924, 31925} {
				stampTargets[k] = tsDB4
			}
		}
		if transferkioskEnabled && tsDB6 != nil {
			for _, k := range []nostr.Kind{30143, 30144} {
				stampTargets[k] = tsDB6
			}
		}
		if publicationsEnabled && tsDB7 != nil {
			for _, k := range []nostr.Kind{30040, 30041} {
				stampTargets[k] = tsDB7
			}
		}

		stampPool := nostr.NewPool()
		// Reuse the Phase-3 community relays for fetch fallback.
		stampRelays := communityRelays

		// Populate stampKinds for the kind guard in triggers.
		stampKinds := make(map[nostr.Kind]bool, len(stampTargets))
		for k := range stampTargets {
			stampKinds[k] = true
		}

		stamper = &CommunityStamper{
			isMember: communityReg.IsMember,
			sharesFor: func(coord string) []nostr.Event {
				// refA is the filterable facet that records each share's referenced
				// content coord. The nostr `#a` tag filter maps to `nostr_a`, which
				// the share projection (nostrToShare) does NOT populate — it writes
				// refA — so query the facet directly via a raw filter expression.
				// Limit is 250: Typesense's hard per_page max (a larger value 422s
				// inside the 200-OK multi_search envelope, silently yielding zero
				// hits). One content coord realistically never has more shares.
				out, err := tsDB5.SearchResourcesWithLimitAndFilter("", 250, fmt.Sprintf("refA:=`%s`", coord))
				if err != nil {
					fmt.Printf("community stamp: sharesFor %s: %v\n", coord, err)
					return nil
				}
				return out
			},
			allShares: func() []nostr.Event {
				var out []nostr.Event
				for ev := range boltDB.QueryEvents(nostr.Filter{Kinds: []nostr.Kind{16, 30222}}, 1_000_000) {
					out = append(out, ev)
				}
				return out
			},
			lookup: func(coord string) (nostr.Event, bool) {
				parts := strings.SplitN(coord, ":", 3)
				if len(parts) != 3 {
					return nostr.Event{}, false
				}
				kn, err := strconv.Atoi(parts[0])
				if err != nil {
					return nostr.Event{}, false
				}
				pk, err := nostr.PubKeyFromHex(parts[1])
				if err != nil {
					return nostr.Event{}, false
				}
				for ev := range boltDB.QueryEvents(nostr.Filter{
					Kinds:   []nostr.Kind{nostr.Kind(kn)},
					Authors: []nostr.PubKey{pk},
					Tags:    nostr.TagMap{"d": []string{parts[2]}},
					Limit:   1,
				}, 1) {
					return ev, true
				}
				return nostr.Event{}, false
			},
			fetch: func(ref shareRef, hints []string) (nostr.Event, bool) {
				pk, err := nostr.PubKeyFromHex(ref.Pubkey)
				if err != nil {
					return nostr.Event{}, false
				}
				relays := stampRelays
				if len(hints) > 0 {
					relays = append(append([]string{}, hints...), stampRelays...)
				}
				ctx, cancel := context.WithTimeout(context.Background(), communityStampTimeout)
				defer cancel()
				res := stampPool.QuerySingle(ctx, relays, nostr.Filter{
					Kinds:   []nostr.Kind{ref.Kind},
					Authors: []nostr.PubKey{pk},
					Tags:    nostr.TagMap{"d": []string{ref.DTag}},
					Limit:   1,
				}, nostr.SubscriptionOptions{})
				if res == nil {
					return nostr.Event{}, false
				}
				return res.Event, true
			},
			validate: reg.validate,
			store: func(e nostr.Event) {
				boltBuf.Queue(e, false)
				reg.store(e)
				if profileMgr != nil {
					profileMgr.Enqueue(e.PubKey)
				}
			},
			patch: func(kind nostr.Kind, docID string, communities []string) error {
				be, ok := stampTargets[kind]
				if !ok {
					return nil // not a stampable kind
				}
				if communities == nil {
					communities = []string{}
				}
				// AMB (30142) writes through the async tsBuf; a direct PATCH would race
				// its batched flush (which re-projects community from own h-tags only),
				// clobbering the stamp. Route through the buffer so the patch serializes
				// after the flush — the same idiom as setcontent's QueueContent. The
				// other content kinds use synchronous stores, so a direct PATCH is safe.
				if kind == 30142 {
					tsBuf.QueueCommunityPatch(docID, communities)
					return nil
				}
				return patchDoc(be.Host, be.ApiKey, be.CollectionName, docID, map[string]any{"community": communities})
			},
			stampKinds: stampKinds,
			stopCh:     make(chan struct{}),
		}
		stamper.Init()
		reindexer.afterRun = func() { stamper.reconcileAll() }
		stamper.StartSweepLoop(communityRefresh) // reuse Phase-3 interval
		defer stamper.Stop()
		fmt.Printf("Community stamper: member-gated denormalization active (sweep every %s)\n", communityRefresh)
	}

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

	// Query fetch budget: bounds how long a single REQ waits on the search
	// backend before khatru is allowed to terminate the subscription
	// handshake (EOSE) with whatever was collected so far. Each Typesense
	// call is already bounded by its own client timeout, but registry.fetch
	// fans a kind-less filter out across every registered content type
	// SERIALLY — with several collections on one (possibly degraded)
	// Typesense instance, that serial worst case stacks up to minutes, which
	// is indistinguishable from "hangs forever" to a real client. See
	// query_budget.go.
	queryFetchBudget := DefaultQueryFetchBudget
	if ms := os.Getenv("QUERY_FETCH_TIMEOUT_MS"); ms != "" {
		if n, err := strconv.Atoi(ms); err == nil && n > 0 {
			queryFetchBudget = time.Duration(n) * time.Millisecond
		}
	}

	// Dual-write eventstore wiring (query from Typesense, persist to both)
	relay.QueryStored = func(ctx context.Context, filter nostr.Filter) iter.Seq[nostr.Event] {
		maxLimit := 250
		if khatru.IsNegentropySession(ctx) {
			maxLimit = 250 * 20
		}
		// Chunk-rerank only owns searches that (a) can be served by the chunk
		// index and (b) carry a free-text term to rank. Calendar-only
		// (non-chunked) searches go straight to plain full-text; otherwise the
		// rerank would drop them whenever the term also matched a chunk from
		// another content type. Pure field-filter searches (e.g.
		// "community:<pubkey>") have no semantic term, so rerank would send the
		// raw string to the chunk index and drop the field filter — those must
		// take the plain field-filter path too.
		var results iter.Seq[nostr.Event]
		if reg.targetsChunked(filter) && searchHasFreeText(filter.Search) {
			results = calendarRerankQuery(ctx, filter, chunkSearcher, reg.fetch, maxLimit, relaySK)
		} else {
			results = reg.fetch(filter, maxLimit)
		}
		return boundedSeq(results, queryFetchBudget)
	}
	relay.Count = func(ctx context.Context, filter nostr.Filter) (uint32, error) {
		return reg.count(filter)
	}
	relay.StoreEvent = func(ctx context.Context, event nostr.Event) error {
		boltBuf.Queue(event, false)
		reg.store(event)
		if profileMgr != nil {
			profileMgr.Enqueue(event.PubKey)
		}
		if stamper != nil {
			switch event.Kind {
			case 16, 30222:
				enqueueShareCommunities(profileMgr, event)
				go stamper.reconcileShare(event)
			default:
				if stamper.isStampKind(event.Kind) {
					if coord, k, pk, d, ok := contentCoord(event); ok {
						go stamper.reconcile(coord, k, pk, d)
					}
				}
			}
		}
		return nil
	}
	relay.ReplaceEvent = func(ctx context.Context, event nostr.Event) error {
		boltBuf.Queue(event, true)
		reg.store(event)
		if profileMgr != nil {
			profileMgr.Enqueue(event.PubKey)
		}
		if stamper != nil {
			switch event.Kind {
			case 16, 30222:
				enqueueShareCommunities(profileMgr, event)
				go stamper.reconcileShare(event)
			default:
				if stamper.isStampKind(event.Kind) {
					if coord, k, pk, d, ok := contentCoord(event); ok {
						go stamper.reconcile(coord, k, pk, d)
					}
				}
			}
		}
		return nil
	}
	relay.DeleteEvent = func(ctx context.Context, id nostr.ID) error {
		var deletedShareRef *shareRef
		if stamper != nil {
			for ev := range boltDB.QueryEvents(nostr.Filter{IDs: []nostr.ID{id}, Limit: 1}, 1) {
				if ev.Kind == 16 || ev.Kind == 30222 {
					if ref, ok := shareToRef(ev); ok {
						r := ref
						deletedShareRef = &r
					}
				}
			}
		}
		boltDB.DeleteEvent(id)
		if err := mgmt.MarkEventDeleted(id.Hex()); err != nil {
			fmt.Printf("mark deleted %s: %v\n", id.Hex(), err)
		}
		_ = contentStore.Delete(id.Hex()) // idempotent; safe when no content row existed
		// The id carries no kind, so try every collection (each delete is a
		// no-op when the id lives elsewhere).
		err := reg.deleteEverywhere(id, func(err error) {
			fmt.Printf("delete %s (secondary collection): %v\n", id.Hex(), err)
		})
		if deletedShareRef != nil {
			r := *deletedShareRef
			go stamper.reconcile(r.Coord, r.Kind, r.Pubkey, r.DTag)
		}
		return err
	}

	// NIP-09: a deletion request must not delete another deletion request
	// ("publishing a deletion request event against a deletion request has
	// no effect"). Serving kind 5 made stored deletions findable by the
	// target lookup, so guard here; other kinds keep the default
	// same-author rule this hook replaces.
	relay.AllowDeleting = func(ctx context.Context, target, deletion nostr.Event) bool {
		return target.Kind != nostr.KindDeletion && target.PubKey == deletion.PubKey
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
		if mgmt.IsEventDeleted(event.ID.Hex()) {
			return true, "blocked: event was deleted by its author"
		}
		if acl.IsWriteRestricted() && !admins.IsAdmin(event.PubKey) && !acl.IsWriteAllowed(event.PubKey.Hex()) {
			return true, "restricted: pubkey not on write allowlist"
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
			// Kind-routed content projection: 30040 patches the publications
			// collection directly; everything else keeps the buffered AMB path.
			if event.Kind == 30040 {
				if !publicationsEnabled || tsDB7 == nil {
					return nip86.Response{Error: "publications not enabled"}, nil
				}
				if err := setPublicationContent(tsDB7, contentStore, event, entry); err != nil {
					return nip86.Response{Error: fmt.Sprintf("set publication content: %v", err)}, nil
				}
				return nip86.Response{Result: true}, nil
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
			if event.Kind == 30040 {
				if !publicationsEnabled || tsDB7 == nil {
					return nip86.Response{Error: "publications not enabled"}, nil
				}
				if err := clearPublicationContent(tsDB7, contentStore, event); err != nil {
					return nip86.Response{Error: fmt.Sprintf("clear publication content: %v", err)}, nil
				}
				if err := mgmt.MarkNeedsRefetch(eventIDHex); err != nil {
					fmt.Printf("refetchcontent: mark needs_refetch %s: %v\n", eventIDHex, err)
				}
				return nip86.Response{Result: true}, nil
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
