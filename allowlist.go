package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/nip86"
)

type AllowlistManager struct {
	mgmt *ManagementStore

	mu           sync.RWMutex
	config       AccessControlConfig
	writeAllowed map[string]bool            // merged: direct + list-resolved
	readAllowed  map[string]bool            // merged: direct + list-resolved
	listPubkeys  map[string]map[string]bool // listRefKey → resolved pubkeys
	listRefs     map[string]ListReference

	pool     *nostr.Pool
	stopCh   chan struct{}
	stopOnce sync.Once
}

func NewAllowlistManager(mgmt *ManagementStore) *AllowlistManager {
	return &AllowlistManager{
		mgmt:         mgmt,
		writeAllowed: make(map[string]bool),
		readAllowed:  make(map[string]bool),
		listPubkeys:  make(map[string]map[string]bool),
		listRefs:     make(map[string]ListReference),
		pool:         nostr.NewPool(),
		stopCh:       make(chan struct{}),
	}
}

func (a *AllowlistManager) Init() error {
	cfg, err := a.mgmt.LoadAccessControlConfig()
	if err != nil {
		return fmt.Errorf("loading access control config: %w", err)
	}
	a.config = cfg

	refs, err := a.mgmt.LoadListReferences()
	if err != nil {
		return fmt.Errorf("loading list references: %w", err)
	}
	a.listRefs = refs

	// Initial list fetch with timeout
	if len(refs) > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		a.refreshAllLists(ctx)
		cancel()
	}

	a.rebuildMergedSets()
	return nil
}

func (a *AllowlistManager) StartRefreshLoop(interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				a.refreshAllLists(ctx)
				cancel()
				a.mu.Lock()
				a.rebuildMergedSetsLocked()
				a.mu.Unlock()
			case <-a.stopCh:
				return
			}
		}
	}()
}

// Stop is idempotent: a second call must not re-close the channel.
func (a *AllowlistManager) Stop() {
	a.stopOnce.Do(func() { close(a.stopCh) })
}

// IsWriteRestricted returns whether write access is restricted.
func (a *AllowlistManager) IsWriteRestricted() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.config.WriteRestricted
}

// IsReadRestricted returns whether read access is restricted.
func (a *AllowlistManager) IsReadRestricted() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.config.ReadRestricted
}

// IsWriteAllowed checks if a pubkey is on the write allowlist.
func (a *AllowlistManager) IsWriteAllowed(pk string) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.writeAllowed[pk]
}

// IsReadAllowed checks if a pubkey is on the read allowlist.
func (a *AllowlistManager) IsReadAllowed(pk string) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.readAllowed[pk]
}

// SetAccessControl updates the access control configuration.
func (a *AllowlistManager) SetAccessControl(cfg AccessControlConfig) error {
	if err := a.mgmt.SaveAccessControlConfig(cfg); err != nil {
		return err
	}
	a.mu.Lock()
	a.config = cfg
	a.mu.Unlock()
	return nil
}

// AddWriteAllowPubkey adds a pubkey to the write allowlist.
func (a *AllowlistManager) AddWriteAllowPubkey(pubkey string, reason string) error {
	if err := a.mgmt.AddWriteAllowPubkey(pubkey, reason); err != nil {
		return err
	}
	a.mu.Lock()
	a.rebuildMergedSetsLocked()
	a.mu.Unlock()
	return nil
}

// RemoveWriteAllowPubkey removes a pubkey from the write allowlist.
func (a *AllowlistManager) RemoveWriteAllowPubkey(pubkey string) error {
	if err := a.mgmt.RemoveWriteAllowPubkey(pubkey); err != nil {
		return err
	}
	a.mu.Lock()
	a.rebuildMergedSetsLocked()
	a.mu.Unlock()
	return nil
}

// ListWriteAllowPubkeys returns directly-added write-allowed pubkeys.
func (a *AllowlistManager) ListWriteAllowPubkeys() ([]nip86.PubKeyReason, error) {
	return a.mgmt.ListWriteAllowPubkeys()
}

// AddReadAllowPubkey adds a pubkey to the read allowlist.
func (a *AllowlistManager) AddReadAllowPubkey(pubkey string, reason string) error {
	if err := a.mgmt.AddReadAllowPubkey(pubkey, reason); err != nil {
		return err
	}
	a.mu.Lock()
	a.rebuildMergedSetsLocked()
	a.mu.Unlock()
	return nil
}

// RemoveReadAllowPubkey removes a pubkey from the read allowlist.
func (a *AllowlistManager) RemoveReadAllowPubkey(pubkey string) error {
	if err := a.mgmt.RemoveReadAllowPubkey(pubkey); err != nil {
		return err
	}
	a.mu.Lock()
	a.rebuildMergedSetsLocked()
	a.mu.Unlock()
	return nil
}

// ListReadAllowPubkeys returns directly-added read-allowed pubkeys.
func (a *AllowlistManager) ListReadAllowPubkeys() ([]nip86.PubKeyReason, error) {
	return a.mgmt.ListReadAllowPubkeys()
}

// AddListReference adds a list reference and fetches it immediately.
func (a *AllowlistManager) AddListReference(ref ListReference) error {
	key := ListRefKey(ref.Pubkey, ref.Kind, ref.DTag)
	if err := a.mgmt.SaveListReference(key, ref); err != nil {
		return err
	}

	a.mu.Lock()
	a.listRefs[key] = ref
	a.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	a.fetchList(ctx, key, ref)

	a.mu.Lock()
	a.rebuildMergedSetsLocked()
	a.mu.Unlock()
	return nil
}

// RemoveListReference removes a list reference.
func (a *AllowlistManager) RemoveListReference(pubkey string, kind int, dtag string) error {
	key := ListRefKey(pubkey, kind, dtag)
	if err := a.mgmt.DeleteListReference(key); err != nil {
		return err
	}

	a.mu.Lock()
	delete(a.listRefs, key)
	delete(a.listPubkeys, key)
	a.rebuildMergedSetsLocked()
	a.mu.Unlock()
	return nil
}

// LoadListReferences returns all stored list references.
func (a *AllowlistManager) LoadListReferences() map[string]ListReference {
	a.mu.RLock()
	defer a.mu.RUnlock()
	result := make(map[string]ListReference, len(a.listRefs))
	for k, v := range a.listRefs {
		result[k] = v
	}
	return result
}

// RefreshAllLists fetches all list references from remote relays.
func (a *AllowlistManager) RefreshAllLists() int {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	n := a.refreshAllLists(ctx)
	a.mu.Lock()
	a.rebuildMergedSetsLocked()
	a.mu.Unlock()
	return n
}

func (a *AllowlistManager) refreshAllLists(ctx context.Context) int {
	a.mu.RLock()
	refs := make(map[string]ListReference, len(a.listRefs))
	for k, v := range a.listRefs {
		refs[k] = v
	}
	a.mu.RUnlock()

	count := 0
	for key, ref := range refs {
		if a.fetchList(ctx, key, ref) {
			count++
		}
	}
	return count
}

func (a *AllowlistManager) fetchList(ctx context.Context, key string, ref ListReference) bool {
	pk, err := nostr.PubKeyFromHex(ref.Pubkey)
	if err != nil {
		fmt.Printf("allowlist: invalid pubkey in list ref %s: %v\n", key, err)
		return false
	}

	var filter nostr.Filter
	if ref.Kind == 3 {
		filter = nostr.Filter{
			Kinds:   []nostr.Kind{3},
			Authors: []nostr.PubKey{pk},
			Limit:   1,
		}
	} else {
		filter = nostr.Filter{
			Kinds:   []nostr.Kind{30000},
			Authors: []nostr.PubKey{pk},
			Tags:    nostr.TagMap{"d": []string{ref.DTag}},
			Limit:   1,
		}
	}

	result := a.pool.QuerySingle(ctx, ref.Relays, filter, nostr.SubscriptionOptions{})
	if result == nil {
		fmt.Printf("allowlist: no event found for list ref %s\n", key)
		return false
	}

	pubkeys := make(map[string]bool)
	for _, tag := range result.Tags {
		if len(tag) >= 2 && tag[0] == "p" {
			pubkeys[tag[1]] = true
		}
	}

	a.mu.Lock()
	a.listPubkeys[key] = pubkeys
	a.mu.Unlock()

	fmt.Printf("allowlist: resolved %d pubkeys from list ref %s\n", len(pubkeys), key)
	return true
}

// rebuildMergedSets rebuilds writeAllowed/readAllowed from direct pubkeys + list-resolved pubkeys.
// Caller must NOT hold the lock.
func (a *AllowlistManager) rebuildMergedSets() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.rebuildMergedSetsLocked()
}

// rebuildMergedSetsLocked rebuilds merged sets. Caller must hold a.mu write lock.
func (a *AllowlistManager) rebuildMergedSetsLocked() {
	writeSet := make(map[string]bool)
	readSet := make(map[string]bool)

	// Add direct pubkeys from BoltDB
	if wpks, err := a.mgmt.ListWriteAllowPubkeys(); err == nil {
		for _, pk := range wpks {
			writeSet[pk.PubKey.Hex()] = true
		}
	}
	if rpks, err := a.mgmt.ListReadAllowPubkeys(); err == nil {
		for _, pk := range rpks {
			readSet[pk.PubKey.Hex()] = true
		}
	}

	// Add list-resolved pubkeys
	for key, pubkeys := range a.listPubkeys {
		ref, ok := a.listRefs[key]
		if !ok {
			continue
		}
		for pk := range pubkeys {
			switch ref.Direction {
			case "write":
				writeSet[pk] = true
			case "read":
				readSet[pk] = true
			case "both":
				writeSet[pk] = true
				readSet[pk] = true
			}
		}
	}

	a.writeAllowed = writeSet
	a.readAllowed = readSet
}
