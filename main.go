package main

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/eventstore/boltdb"
	"fiatjaf.com/nostr/eventstore/typesense30142"
	"fiatjaf.com/nostr/khatru"
	"fiatjaf.com/nostr/nip86"
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
	relay.Info.Name = os.Getenv("NAME")
	relay.Info.Description = os.Getenv("DESCRIPTION")
	relay.Info.Icon = os.Getenv("ICON")

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
	admins := map[nostr.PubKey]bool{}
	if operatorPK != (nostr.PubKey{}) {
		admins[operatorPK] = true
	}
	if adminList := os.Getenv("ADMIN_PUBKEYS"); adminList != "" {
		for _, hex := range strings.Split(adminList, ",") {
			hex = strings.TrimSpace(hex)
			if pk, err := nostr.PubKeyFromHex(hex); err == nil {
				admins[pk] = true
			} else {
				fmt.Printf("Error parsing admin pubkey %q: %v\n", hex, err)
			}
		}
	}

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

	// Typesense backend (search index)
	tsDB := typesense30142.TSBackend{
		ApiKey:         os.Getenv("TS_APIKEY"),
		Host:           os.Getenv("TS_HOST"),
		CollectionName: os.Getenv("TS_COLLECTION"),
	}

	// Load custom schema from BoltDB if one was stored
	if customSchema, err := mgmt.LoadSchema(); err != nil {
		fmt.Printf("Warning: failed to load custom schema: %v\n", err)
	} else if customSchema != nil {
		tsDB.Schema = customSchema
		fmt.Println("Using custom Typesense schema from BoltDB")
	}

	if err := tsDB.Init(); err != nil {
		panic(err)
	}

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

	// Reindexer for rebuilding Typesense from BoltDB
	reindexer := NewReindexer(&tsDB, &boltDB, &mgmt)

	relay.OnConnect = func(ctx context.Context) {
		khatru.RequestAuth(ctx)
	}

	// Dual-write eventstore wiring (query from Typesense, persist to both)
	relay.QueryStored = func(ctx context.Context, filter nostr.Filter) iter.Seq[nostr.Event] {
		maxLimit := 250
		if khatru.IsNegentropySession(ctx) {
			maxLimit = 250 * 20
		}
		return tsDB.QueryEvents(filter, maxLimit)
	}
	relay.Count = func(ctx context.Context, filter nostr.Filter) (uint32, error) {
		return tsDB.CountEvents(filter)
	}
	relay.StoreEvent = func(ctx context.Context, event nostr.Event) error {
		boltDB.SaveEvent(event)
		if event.Kind == 30142 {
			return tsDB.SaveEvent(event)
		}
		return nil
	}
	relay.ReplaceEvent = func(ctx context.Context, event nostr.Event) error {
		boltDB.ReplaceEvent(event)
		return tsDB.ReplaceEvent(event)
	}
	relay.DeleteEvent = func(ctx context.Context, id nostr.ID) error {
		boltDB.DeleteEvent(id)
		return tsDB.DeleteEvent(id)
	}

	relay.Negentropy = true

	// Event validation + ban check
	relay.OnEvent = func(ctx context.Context, event nostr.Event) (reject bool, msg string) {
		if mgmt.IsPubKeyBanned(event.PubKey) {
			return true, "pubkey is banned"
		}
		if event.Kind == nostr.KindDeletion {
			return false, ""
		}
		if event.Kind != 30142 {
			return true, "only kind 30142 events are accepted"
		}
		if event.Tags.GetD() == "" {
			return true, "missing required 'd' tag"
		}
		if !event.Tags.Has("name") {
			return true, "missing required 'name' tag"
		}
		return false, ""
	}

	// NIP-86 Management API
	relay.ManagementAPI.OnAPICall = func(ctx context.Context, mp nip86.MethodParams) (reject bool, msg string) {
		authed, ok := khatru.GetAuthed(ctx)
		if !ok {
			return true, "not authenticated"
		}
		if !admins[authed] {
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
		return mgmt.BanEvent(id, reason)
	}
	relay.ManagementAPI.ListBannedEvents = func(ctx context.Context) ([]nip86.IDReason, error) {
		return mgmt.ListBannedEvents()
	}
	relay.ManagementAPI.AllowEvent = func(ctx context.Context, id nostr.ID, reason string) error {
		return mgmt.AllowEvent(id)
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

		default:
			return nip86.Response{Error: fmt.Sprintf("unknown method '%s'", request.Method)}, nil
		}
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = "3334"
	}
	fmt.Printf("running on :%s\n", port)
	http.ListenAndServe(":"+port, relay)
}

var startTime = time.Now()
