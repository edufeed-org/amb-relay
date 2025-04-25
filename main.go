package main

import (
	"context"
	"fmt"
	"net/http"
	"os"

	"github.com/edufeed-org/eventstore/typesense30142"
	"github.com/fiatjaf/khatru"
	"github.com/joho/godotenv"
	"github.com/nbd-wtf/go-nostr"
)

func main() {
	// Load .env file
	err := godotenv.Load()
	if err != nil {
		fmt.Printf("Error loading .env file: %v", err)
	}

	relay := khatru.NewRelay()
	relay.Info.Name = os.Getenv("NAME")
	relay.Info.PubKey = os.Getenv("PUBKEY")
	relay.Info.Description = os.Getenv("DESCRIPTION")
	relay.Info.Icon = os.Getenv("ICON")
	db := typesense30142.TSBackend{
		ApiKey:         os.Getenv("TS_APIKEY"),
    Host:           os.Getenv("TS_HOST"),
		CollectionName: os.Getenv("TS_COLLECTION"),
	}
	if err := db.Init(); err != nil {
		panic(err)
	}

	relay.OnConnect = append(relay.OnConnect, func(ctx context.Context) {
		khatru.RequestAuth(ctx)
	})

	relay.QueryEvents = append(relay.QueryEvents, db.QueryEvents)
	relay.DeleteEvent = append(relay.DeleteEvent, db.DeleteEvent)
	relay.ReplaceEvent = append(relay.ReplaceEvent, db.ReplaceEvent)
	relay.Negentropy = true

	relay.RejectEvent = append(relay.RejectEvent,
		func(ctx context.Context, event *nostr.Event) (reject bool, msg string) {
			if event.Kind != 30142 {
				return true, "we don't allow these kinds here. It's a 30142 only place."
			}
			return false, ""
		},
	)

	fmt.Println("running on :3334")
	http.ListenAndServe(":3334", relay)
}
