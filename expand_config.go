package main

import (
	"fmt"
	"log"
	"strings"
	"time"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/nip19"
	"github.com/edufeed-org/amb-relay/vocabmap"
)

const (
	defaultExpandRelay   = "wss://relay.edufeed.org"
	defaultExpandRefresh = 10 * time.Minute
)

// conceptExpansionConfig reads EXPAND_CONCORDANCES (naddrs of kind 39741),
// EXPAND_READ_RELAYS, EXPAND_REFRESH_INTERVAL and EXPAND_DEBUG. Invalid
// entries are logged and skipped.
func conceptExpansionConfig(getenv func(string) string) (relays []string, defaults Trust, refresh time.Duration, debug bool) {
	for _, raw := range strings.Split(getenv("EXPAND_CONCORDANCES"), ",") {
		if raw = strings.TrimSpace(raw); raw == "" {
			continue
		}
		prefix, data, err := nip19.Decode(raw)
		p, ok := data.(nostr.EntityPointer)
		if err != nil || prefix != "naddr" || !ok || p.Kind != vocabmap.KindConcordance {
			log.Printf("expand: EXPAND_CONCORDANCES: skipping %q (not a kind-39741 naddr)", raw)
			continue
		}
		defaults.Concordances = append(defaults.Concordances, fmt.Sprintf("%d:%s:%s", p.Kind, p.PublicKey.Hex(), p.Identifier))
	}
	for _, r := range strings.Split(getenv("EXPAND_READ_RELAYS"), ",") {
		if r = strings.TrimSpace(r); r != "" {
			relays = append(relays, r)
		}
	}
	if len(relays) == 0 {
		relays = []string{defaultExpandRelay}
	}
	refresh = defaultExpandRefresh
	if raw := getenv("EXPAND_REFRESH_INTERVAL"); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil && d > 0 {
			refresh = d
		} else {
			log.Printf("expand: bad EXPAND_REFRESH_INTERVAL %q, using %s", raw, refresh)
		}
	}
	debug = getenv("EXPAND_DEBUG") == "true"
	return
}
