package main

import (
	"reflect"
	"testing"
	"time"

	"fiatjaf.com/nostr/nip19"
	vt "github.com/edufeed-org/amb-relay/vocabmap/vocabmaptest"
)

func TestConceptExpansionConfig(t *testing.T) {
	env := map[string]string{
		"EXPAND_CONCORDANCES": nip19.EncodeNaddr(vt.Owner, 39741, "k", nil) + ", bogus ," +
			nip19.EncodeNaddr(vt.Owner, 30023, "article", nil),
		"EXPAND_READ_RELAYS":      "wss://a, wss://b",
		"EXPAND_REFRESH_INTERVAL": "nonsense",
		"EXPAND_DEBUG":            "true",
	}
	relays, defaults, refresh, debug := conceptExpansionConfig(func(k string) string { return env[k] })
	if !reflect.DeepEqual(relays, []string{"wss://a", "wss://b"}) {
		t.Errorf("relays = %v", relays)
	}
	if !reflect.DeepEqual(defaults, Trust{Concordances: []string{vt.ConcordanceAddr(vt.Owner, "k")}}) {
		t.Errorf("defaults = %+v", defaults)
	}
	if refresh != 10*time.Minute || !debug {
		t.Errorf("refresh = %s debug = %v", refresh, debug)
	}

	relays, defaults, refresh, debug = conceptExpansionConfig(func(string) string { return "" })
	if !reflect.DeepEqual(relays, []string{"wss://relay.edufeed.org"}) || len(defaults.Concordances) != 0 || refresh != 10*time.Minute || debug {
		t.Errorf("defaults: %v %+v %s %v", relays, defaults, refresh, debug)
	}
}
