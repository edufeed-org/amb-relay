package main

import (
	"context"
	"fmt"
	"net/http"
	"os"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/eventstore/typesense30142"
	"fiatjaf.com/nostr/khatru"
	"github.com/joho/godotenv"
)

func main() {
	// Load .env file
	err := godotenv.Load()
	if err != nil {
		fmt.Printf("Error loading .env file: %v", err)
	}

	relay := khatru.NewRelay()
	relay.Info.Name = os.Getenv("NAME")
	relay.Info.Description = os.Getenv("DESCRIPTION")
	relay.Info.Icon = os.Getenv("ICON")

	// Parse pubkey from hex string
	if pkHex := os.Getenv("PUBKEY"); pkHex != "" {
		pk, err := nostr.PubKeyFromHex(pkHex)
		if err != nil {
			fmt.Printf("Error parsing PUBKEY: %v\n", err)
		} else {
			relay.Info.PubKey = &pk
		}
	}

	db := typesense30142.TSBackend{
		ApiKey:         os.Getenv("TS_APIKEY"),
		Host:           os.Getenv("TS_HOST"),
		CollectionName: os.Getenv("TS_COLLECTION"),
	}
	if err := db.Init(); err != nil {
		panic(err)
	}

	relay.OnConnect = func(ctx context.Context) {
		khatru.RequestAuth(ctx)
	}

	relay.UseEventstore(&db, 250)
	relay.Negentropy = true

	relay.OnEvent = func(ctx context.Context, event nostr.Event) (reject bool, msg string) {
		if event.Kind != 30142 {
			return true, "only kind 30142 events are accepted"
		}
		// d tag required for addressable events (NIP-33)
		if event.Tags.GetD() == "" {
			return true, "missing required 'd' tag"
		}
		// name is a required AMB field
		if !event.Tags.Has("name") {
			return true, "missing required 'name' tag"
		}
		return false, ""
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = "3334"
	}
	fmt.Printf("running on :%s\n", port)
	http.ListenAndServe(":"+port, relay)
}
