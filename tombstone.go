// tombstone.go — the single "may this event exist on the relay?" gate.
//
// An event is blocked when its pubkey or id is banned (NIP-86), when its id
// was deleted by its author (NIP-09 'e'), or when it is a version of an
// addressable/replaceable coordinate its author deleted with an 'a' tag at or
// after the version's created_at (NIP-09: "delete all versions … up to the
// created_at timestamp of the deletion request").
//
// The gate is checked on every write path (OnEvent, both write buffers at
// flush time, the stamper/profile fetchers, startup hydration) and on the read
// path, so a ban or deletion cannot be undone by a write that was already in
// flight when it happened, and a search-index document that outlived its
// deletion is never served.
package main

import (
	"encoding/binary"
	"fmt"

	"fiatjaf.com/nostr"
	"go.etcd.io/bbolt"
)

// bucketDeletedCoords maps a NIP-09 'a' coordinate ("kind:pubkey:d") to the
// newest created_at (big-endian uint64) of a same-author deletion request for
// it. Versions with created_at <= that value are rejected on re-publication.
var bucketDeletedCoords = []byte("deleted_coords")

// bucketBannedPubKeys and bucketBannedEvents mirror the private bucket names
// relaykit.Store uses (khatru/relaykit/store.go). ManagementStore embeds that
// Store, which creates these buckets via NewStore, and m.DB is the same
// *bbolt.DB — so BlockReason reads them by the same literal names inside its
// own single transaction below instead of going through IsPubKeyBanned/
// IsEventBanned (each of which opens its own View). There is no exported
// handle to borrow these from relaykit; if it ever renames them, this needs
// to follow.
var (
	bucketBannedPubKeys = []byte("banned_pubkeys")
	bucketBannedEvents  = []byte("banned_events")
)

// eventCoord returns the NIP-01 coordinate of a replaceable ("kind:pubkey:")
// or addressable ("kind:pubkey:d") event. Regular events have none.
func eventCoord(ev nostr.Event) (string, bool) {
	switch {
	case ev.Kind.IsAddressable():
		return fmt.Sprintf("%d:%s:%s", ev.Kind, ev.PubKey.Hex(), ev.Tags.GetD()), true
	case ev.Kind.IsReplaceable():
		return fmt.Sprintf("%d:%s:", ev.Kind, ev.PubKey.Hex()), true
	}
	return "", false
}

// MarkCoordDeleted records a NIP-09 'a'-tag deletion of coord up to until.
// Monotonic: an older deletion request never lowers an existing tombstone.
func (m *ManagementStore) MarkCoordDeleted(coord string, until nostr.Timestamp) error {
	return m.DB.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketDeletedCoords)
		if cur := b.Get([]byte(coord)); len(cur) == 8 && nostr.Timestamp(binary.BigEndian.Uint64(cur)) >= until {
			return nil
		}
		var v [8]byte
		binary.BigEndian.PutUint64(v[:], uint64(until))
		return b.Put([]byte(coord), v[:])
	})
}

// CoordDeletedUntil returns the tombstone timestamp for coord, if any.
func (m *ManagementStore) CoordDeletedUntil(coord string) (nostr.Timestamp, bool) {
	var ts nostr.Timestamp
	var ok bool
	m.DB.View(func(tx *bbolt.Tx) error {
		if v := tx.Bucket(bucketDeletedCoords).Get([]byte(coord)); len(v) == 8 {
			ts, ok = nostr.Timestamp(binary.BigEndian.Uint64(v)), true
		}
		return nil
	})
	return ts, ok
}

// BlockReason returns why ev must not be stored or served, or "" when it may.
// The reasons are the exact OK-message strings OnEvent has always returned.
//
// All four checks (pubkey ban, event ban, NIP-09 'e' deletion, NIP-09 'a'
// tombstone) run inside one bbolt.View transaction rather than the four
// separate ones a naive call-through to IsPubKeyBanned/IsEventBanned/
// IsEventDeleted/CoordDeletedUntil would open — same behavior (identical
// bucket reads, same precedence/short-circuit order), one transaction
// instead of four per event checked. See BenchmarkBlockReason.
func (m *ManagementStore) BlockReason(ev nostr.Event) string {
	coord, hasCoord := eventCoord(ev)
	pubKeyKey := []byte(ev.PubKey.Hex())
	idKey := []byte(ev.ID.Hex())

	var reason string
	m.DB.View(func(tx *bbolt.Tx) error {
		if tx.Bucket(bucketBannedPubKeys).Get(pubKeyKey) != nil {
			reason = "pubkey is banned"
			return nil
		}
		if tx.Bucket(bucketBannedEvents).Get(idKey) != nil {
			reason = "event is banned"
			return nil
		}
		if tx.Bucket(bucketDeletedEvents).Get(idKey) != nil {
			reason = "blocked: event was deleted by its author"
			return nil
		}
		if hasCoord {
			if v := tx.Bucket(bucketDeletedCoords).Get([]byte(coord)); len(v) == 8 {
				if until := nostr.Timestamp(binary.BigEndian.Uint64(v)); ev.CreatedAt <= until {
					reason = "blocked: event was deleted by its author"
				}
			}
		}
		return nil
	})
	return reason
}

// isBlocked is BlockReason as a predicate, the shape the write buffers take.
func (m *ManagementStore) isBlocked(ev nostr.Event) bool { return m.BlockReason(ev) != "" }
