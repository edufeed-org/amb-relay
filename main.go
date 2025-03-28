package main

import (
	"context"
	"fmt"
	"log"
	"net/http"

	"github.com/fiatjaf/eventstore/badger"
	"github.com/fiatjaf/khatru"
	"github.com/nbd-wtf/go-nostr"
)

const (
	apiKey         string = "xyz"
	typesenseHost  string = "http://localhost:8108"
	collectionName string = "amb-test"
)

func main() {
	err := CheckOrCreateCollection("amb-test")
	if err != nil {
		log.Fatalf("Failed to check/create collection: %v", err)
	}

	relay := khatru.NewRelay()
	relay.Info.Name = "my typesense relay"
	// tsRelay.Info.PubKey = "79be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798"
	relay.Info.Description = "this is the typesense custom relay"
	// tsRelay.Info.Icon = "https://external-content.duckduckgo.com/iu/?u=https%3A%2F%2Fliquipedia.net%2Fcommons%2Fimages%2F3%2F35%2FSCProbe.jpg&f=1&nofb=1&ipt=0cbbfef25bce41da63d910e86c3c343e6c3b9d63194ca9755351bb7c2efa3359&ipo=images"
	dbts := badger.BadgerBackend{Path: "/tmp/khatru-badgern-tmp-2"}
	if err := dbts.Init(); err != nil {
		panic(err)
	}

	relay.OnConnect = append(relay.OnConnect, func(ctx context.Context) {
		khatru.RequestAuth(ctx)
	})

	relay.QueryEvents = append(relay.QueryEvents, handleQuery)
	relay.CountEvents = append(relay.CountEvents, handleCount) 
	relay.DeleteEvent = append(relay.DeleteEvent, handleDelete)
	relay.ReplaceEvent = append(relay.ReplaceEvent, handleReplaceEvent)
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

func handleQuery(ctx context.Context, filter nostr.Filter) (chan *nostr.Event, error) {
	ch := make(chan *nostr.Event)

	nostrs, err := SearchResources(collectionName, filter.Search)
	if err != nil {
		log.Printf("Search failed: %v", err)
		return ch, err
	}

	go func() {
		for _, evt := range nostrs {
			ch <- &evt
		}
		close(ch)
	}()
	return ch, nil
}

func handleCount(ctx context.Context, filter nostr.Filter) (int64, error) {
  CountEvents(collectionName, filter)
  return 0, nil
}

func handleDelete(ctx context.Context, event *nostr.Event) error {
  fmt.Println("delete event", event)
	DeleteNostrEvent(collectionName, event)
	return nil
}

func handleReplaceEvent(ctx context.Context, event *nostr.Event) error {
	IndexNostrEvent(collectionName, event)
	return nil
}

