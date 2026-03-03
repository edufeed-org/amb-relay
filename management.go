package main

import (
	"encoding/json"
	"fmt"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/eventstore/typesense30142"
	"fiatjaf.com/nostr/nip86"
	"go.etcd.io/bbolt"
)

var (
	bucketBannedPubKeys   = []byte("banned_pubkeys")
	bucketBannedEvents    = []byte("banned_events")
	bucketTypesenseSchema = []byte("typesense_schema")
	bucketSemanticConfig  = []byte("semantic_config")
	bucketAccessControl   = []byte("access_control")
	bucketWriteAllowlist  = []byte("write_allowlist")
	bucketReadAllowlist   = []byte("read_allowlist")
	bucketListReferences  = []byte("list_references")
	bucketAdmins          = []byte("admins")
)

type adminEntry struct {
	FullAccess bool     `json:"full_access"`
	Methods    []string `json:"methods,omitempty"`
}

const schemaKey = "current"
const semanticConfigKey = "config"
const accessControlKey = "config"

// SemanticConfig stores the configuration for semantic search.
type SemanticConfig struct {
	Enabled     bool     `json:"enabled"`
	EmbedFields []string `json:"embed_fields"`
}

// DefaultSemanticConfig returns the default semantic search configuration.
func DefaultSemanticConfig() SemanticConfig {
	return SemanticConfig{
		Enabled:     false, // Disabled by default until explicitly enabled
		EmbedFields: []string{"name", "description", "keywords", "about"},
	}
}

type ManagementStore struct {
	DB *bbolt.DB
}

func (m *ManagementStore) Init(db *bbolt.DB) error {
	m.DB = db
	return db.Update(func(tx *bbolt.Tx) error {
		for _, bucket := range [][]byte{bucketBannedPubKeys, bucketBannedEvents, bucketTypesenseSchema, bucketSemanticConfig, bucketAccessControl, bucketWriteAllowlist, bucketReadAllowlist, bucketListReferences, bucketAdmins} {
			if _, err := tx.CreateBucketIfNotExists(bucket); err != nil {
				return err
			}
		}
		return nil
	})
}

type reasonEntry struct {
	Reason string `json:"reason,omitempty"`
}

// BanPubKey adds a pubkey to the ban list.
func (m *ManagementStore) BanPubKey(pubkey nostr.PubKey, reason string) error {
	val, _ := json.Marshal(reasonEntry{Reason: reason})
	return m.DB.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketBannedPubKeys).Put([]byte(pubkey.Hex()), val)
	})
}

// AllowPubKey removes a pubkey from the ban list.
func (m *ManagementStore) AllowPubKey(pubkey nostr.PubKey) error {
	return m.DB.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketBannedPubKeys).Delete([]byte(pubkey.Hex()))
	})
}

// ListBannedPubKeys returns all banned pubkeys.
func (m *ManagementStore) ListBannedPubKeys() ([]nip86.PubKeyReason, error) {
	var result []nip86.PubKeyReason
	err := m.DB.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketBannedPubKeys).ForEach(func(k, v []byte) error {
			pk, err := nostr.PubKeyFromHex(string(k))
			if err != nil {
				return nil // skip invalid entries
			}
			var entry reasonEntry
			json.Unmarshal(v, &entry)
			result = append(result, nip86.PubKeyReason{PubKey: pk, Reason: entry.Reason})
			return nil
		})
	})
	return result, err
}

// IsPubKeyBanned checks if a pubkey is banned.
func (m *ManagementStore) IsPubKeyBanned(pubkey nostr.PubKey) bool {
	var banned bool
	m.DB.View(func(tx *bbolt.Tx) error {
		if tx.Bucket(bucketBannedPubKeys).Get([]byte(pubkey.Hex())) != nil {
			banned = true
		}
		return nil
	})
	return banned
}

// BanEvent adds an event ID to the ban list.
func (m *ManagementStore) BanEvent(id nostr.ID, reason string) error {
	val, _ := json.Marshal(reasonEntry{Reason: reason})
	return m.DB.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketBannedEvents).Put([]byte(id.Hex()), val)
	})
}

// AllowEvent removes an event ID from the ban list.
func (m *ManagementStore) AllowEvent(id nostr.ID) error {
	return m.DB.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketBannedEvents).Delete([]byte(id.Hex()))
	})
}

// ListBannedEvents returns all banned event IDs.
func (m *ManagementStore) ListBannedEvents() ([]nip86.IDReason, error) {
	var result []nip86.IDReason
	err := m.DB.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketBannedEvents).ForEach(func(k, v []byte) error {
			id, err := nostr.IDFromHex(string(k))
			if err != nil {
				return nil // skip invalid entries
			}
			var entry reasonEntry
			json.Unmarshal(v, &entry)
			result = append(result, nip86.IDReason{ID: id, Reason: entry.Reason})
			return nil
		})
	})
	return result, err
}

// SaveSchema stores a custom Typesense collection schema in BoltDB.
func (m *ManagementStore) SaveSchema(schema typesense30142.CollectionSchema) error {
	val, err := json.Marshal(schema)
	if err != nil {
		return err
	}
	return m.DB.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketTypesenseSchema).Put([]byte(schemaKey), val)
	})
}

// LoadSchema loads the custom Typesense schema from BoltDB. Returns nil if none stored.
func (m *ManagementStore) LoadSchema() (*typesense30142.CollectionSchema, error) {
	var schema *typesense30142.CollectionSchema
	err := m.DB.View(func(tx *bbolt.Tx) error {
		val := tx.Bucket(bucketTypesenseSchema).Get([]byte(schemaKey))
		if val == nil {
			return nil
		}
		schema = &typesense30142.CollectionSchema{}
		return json.Unmarshal(val, schema)
	})
	return schema, err
}

// DeleteSchema removes the custom schema, reverting to defaults.
func (m *ManagementStore) DeleteSchema() error {
	return m.DB.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketTypesenseSchema).Delete([]byte(schemaKey))
	})
}

// SaveSemanticConfig stores the semantic search configuration in BoltDB.
func (m *ManagementStore) SaveSemanticConfig(cfg SemanticConfig) error {
	val, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	return m.DB.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketSemanticConfig).Put([]byte(semanticConfigKey), val)
	})
}

// LoadSemanticConfig loads the semantic search configuration from BoltDB.
// Returns the default configuration if none is stored.
func (m *ManagementStore) LoadSemanticConfig() (SemanticConfig, error) {
	var cfg SemanticConfig
	err := m.DB.View(func(tx *bbolt.Tx) error {
		val := tx.Bucket(bucketSemanticConfig).Get([]byte(semanticConfigKey))
		if val == nil {
			cfg = DefaultSemanticConfig()
			return nil
		}
		return json.Unmarshal(val, &cfg)
	})
	return cfg, err
}

// AccessControlConfig controls whether write/read access is restricted to allowlisted pubkeys.
type AccessControlConfig struct {
	WriteRestricted bool `json:"write_restricted"`
	ReadRestricted  bool `json:"read_restricted"`
}

// ListReference defines an external Nostr list to resolve pubkeys from.
type ListReference struct {
	Direction string   `json:"direction"` // "write", "read", or "both"
	Pubkey    string   `json:"pubkey"`
	Kind      int      `json:"kind"` // 3 or 30000
	DTag      string   `json:"d_tag,omitempty"`
	Relays    []string `json:"relays"`
	Reason    string   `json:"reason,omitempty"`
}

// ListRefKey returns the storage key for a ListReference.
func ListRefKey(pubkey string, kind int, dtag string) string {
	return fmt.Sprintf("%s:%d:%s", pubkey, kind, dtag)
}

// SaveAccessControlConfig stores the access control configuration.
func (m *ManagementStore) SaveAccessControlConfig(cfg AccessControlConfig) error {
	val, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	return m.DB.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketAccessControl).Put([]byte(accessControlKey), val)
	})
}

// LoadAccessControlConfig loads the access control configuration.
func (m *ManagementStore) LoadAccessControlConfig() (AccessControlConfig, error) {
	var cfg AccessControlConfig
	err := m.DB.View(func(tx *bbolt.Tx) error {
		val := tx.Bucket(bucketAccessControl).Get([]byte(accessControlKey))
		if val == nil {
			return nil
		}
		return json.Unmarshal(val, &cfg)
	})
	return cfg, err
}

// AddWriteAllowPubkey adds a pubkey to the write allowlist.
func (m *ManagementStore) AddWriteAllowPubkey(pubkey string, reason string) error {
	val, _ := json.Marshal(reasonEntry{Reason: reason})
	return m.DB.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketWriteAllowlist).Put([]byte(pubkey), val)
	})
}

// RemoveWriteAllowPubkey removes a pubkey from the write allowlist.
func (m *ManagementStore) RemoveWriteAllowPubkey(pubkey string) error {
	return m.DB.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketWriteAllowlist).Delete([]byte(pubkey))
	})
}

// ListWriteAllowPubkeys returns all directly-added write-allowed pubkeys.
func (m *ManagementStore) ListWriteAllowPubkeys() ([]nip86.PubKeyReason, error) {
	var result []nip86.PubKeyReason
	err := m.DB.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketWriteAllowlist).ForEach(func(k, v []byte) error {
			pk, err := nostr.PubKeyFromHex(string(k))
			if err != nil {
				return nil
			}
			var entry reasonEntry
			json.Unmarshal(v, &entry)
			result = append(result, nip86.PubKeyReason{PubKey: pk, Reason: entry.Reason})
			return nil
		})
	})
	return result, err
}

// AddReadAllowPubkey adds a pubkey to the read allowlist.
func (m *ManagementStore) AddReadAllowPubkey(pubkey string, reason string) error {
	val, _ := json.Marshal(reasonEntry{Reason: reason})
	return m.DB.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketReadAllowlist).Put([]byte(pubkey), val)
	})
}

// RemoveReadAllowPubkey removes a pubkey from the read allowlist.
func (m *ManagementStore) RemoveReadAllowPubkey(pubkey string) error {
	return m.DB.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketReadAllowlist).Delete([]byte(pubkey))
	})
}

// ListReadAllowPubkeys returns all directly-added read-allowed pubkeys.
func (m *ManagementStore) ListReadAllowPubkeys() ([]nip86.PubKeyReason, error) {
	var result []nip86.PubKeyReason
	err := m.DB.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketReadAllowlist).ForEach(func(k, v []byte) error {
			pk, err := nostr.PubKeyFromHex(string(k))
			if err != nil {
				return nil
			}
			var entry reasonEntry
			json.Unmarshal(v, &entry)
			result = append(result, nip86.PubKeyReason{PubKey: pk, Reason: entry.Reason})
			return nil
		})
	})
	return result, err
}

// SaveListReference stores a list reference.
func (m *ManagementStore) SaveListReference(key string, ref ListReference) error {
	val, err := json.Marshal(ref)
	if err != nil {
		return err
	}
	return m.DB.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketListReferences).Put([]byte(key), val)
	})
}

// DeleteListReference removes a list reference.
func (m *ManagementStore) DeleteListReference(key string) error {
	return m.DB.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketListReferences).Delete([]byte(key))
	})
}

// LoadListReferences returns all stored list references.
func (m *ManagementStore) LoadListReferences() (map[string]ListReference, error) {
	result := make(map[string]ListReference)
	err := m.DB.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketListReferences).ForEach(func(k, v []byte) error {
			var ref ListReference
			if err := json.Unmarshal(v, &ref); err != nil {
				return nil
			}
			result[string(k)] = ref
			return nil
		})
	})
	return result, err
}

// AddAdmin grants admin access to a pubkey. If methods is empty, full access is granted.
// If methods is non-empty, they are merged with any existing methods.
func (m *ManagementStore) AddAdmin(pubkey string, methods []string) error {
	return m.DB.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketAdmins)
		var entry adminEntry
		if existing := b.Get([]byte(pubkey)); existing != nil {
			json.Unmarshal(existing, &entry)
		}
		if len(methods) == 0 {
			entry.FullAccess = true
			entry.Methods = nil
		} else if !entry.FullAccess {
			seen := make(map[string]bool)
			for _, m := range entry.Methods {
				seen[m] = true
			}
			for _, m := range methods {
				if !seen[m] {
					entry.Methods = append(entry.Methods, m)
				}
			}
		}
		val, _ := json.Marshal(entry)
		return b.Put([]byte(pubkey), val)
	})
}

// RemoveAdmin revokes admin access. If methods is empty, the admin is removed entirely.
// If methods is non-empty, only those methods are removed; if none remain, the admin is deleted.
func (m *ManagementStore) RemoveAdmin(pubkey string, methods []string) error {
	return m.DB.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketAdmins)
		if len(methods) == 0 {
			return b.Delete([]byte(pubkey))
		}
		existing := b.Get([]byte(pubkey))
		if existing == nil {
			return nil
		}
		var entry adminEntry
		json.Unmarshal(existing, &entry)
		if entry.FullAccess {
			return nil // cannot partially revoke a full-access admin via methods
		}
		remove := make(map[string]bool)
		for _, m := range methods {
			remove[m] = true
		}
		var remaining []string
		for _, m := range entry.Methods {
			if !remove[m] {
				remaining = append(remaining, m)
			}
		}
		if len(remaining) == 0 {
			return b.Delete([]byte(pubkey))
		}
		entry.Methods = remaining
		val, _ := json.Marshal(entry)
		return b.Put([]byte(pubkey), val)
	})
}

// ListAdmins returns all persisted admin entries.
func (m *ManagementStore) ListAdmins() (map[string]adminEntry, error) {
	result := make(map[string]adminEntry)
	err := m.DB.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketAdmins).ForEach(func(k, v []byte) error {
			var entry adminEntry
			if err := json.Unmarshal(v, &entry); err != nil {
				return nil
			}
			result[string(k)] = entry
			return nil
		})
	})
	return result, err
}
