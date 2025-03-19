package main

import (
	"context"
	"fmt"
	"net/http"
  "log"

	"github.com/fiatjaf/eventstore/badger"
	"github.com/fiatjaf/khatru"
	"github.com/nbd-wtf/go-nostr"
)

var typesense string = "http://localhost:8108"

func main() {
	fmt.Println("Hello, Go!")

  err:= CheckOrCreateCollection("amb-test")
  if err != nil {
		log.Fatalf("Failed to check/create collection: %v", err)
	}

	relay := khatru.NewRelay()
	relay.Info.Name = "my relay"
	// relay.Info.PubKey = "79be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798"
	relay.Info.Description = "this is my custom relay"
	// relay.Info.Icon = "https://external-content.duckduckgo.com/iu/?u=https%3A%2F%2Fliquipedia.net%2Fcommons%2Fimages%2F3%2F35%2FSCProbe.jpg&f=1&nofb=1&ipt=0cbbfef25bce41da63d910e86c3c343e6c3b9d63194ca9755351bb7c2efa3359&ipo=images"
	db := badger.BadgerBackend{Path: "/tmp/khatru-badgern-tmp"}
	if err := db.Init(); err != nil {
		panic(err)
	}

	relay.OnConnect = append(relay.OnConnect, func(ctx context.Context) {
		khatru.RequestAuth(ctx)
	})

	relay.StoreEvent = append(relay.StoreEvent, db.SaveEvent, handleEvent)
	relay.QueryEvents = append(relay.QueryEvents, db.QueryEvents, handleQuery)
	relay.CountEvents = append(relay.CountEvents, db.CountEvents)
	relay.DeleteEvent = append(relay.DeleteEvent, db.DeleteEvent)
	relay.ReplaceEvent = append(relay.ReplaceEvent, db.ReplaceEvent)
	relay.Negentropy = true

	fmt.Println("running on :3334")
	http.ListenAndServe(":3334", relay)
}

func handleEvent(ctx context.Context, event *nostr.Event) error {
	fmt.Println("got one", event)
  // TODO index md-event
	return nil
}

func handleQuery(ctx context.Context, filter nostr.Filter) (chan *nostr.Event, error) {
  ch := make(chan *nostr.Event)
  // TODO do stuff with search nips and look for an edufeed or amb something in the tags
  fmt.Println("a query!", filter)

  return ch, nil
}

