package main

import (
	"encoding/json"
	"fmt"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/eventstore/typesense30142"
	"fiatjaf.com/nostr/khatru/relaykit"
	"fiatjaf.com/nostr/nip86"
	"go.etcd.io/bbolt"
)

var (
	bucketTypesenseSchema = []byte("typesense_schema")
	bucketSemanticConfig  = []byte("semantic_config")
	bucketAccessControl   = []byte("access_control")
	bucketWriteAllowlist  = []byte("write_allowlist")
	bucketReadAllowlist   = []byte("read_allowlist")
	bucketListReferences  = []byte("list_references")
	bucketFetchedContent  = []byte("fetched_content") // resource fulltext keyed by event_id
	bucketNeedsRefetch    = []byte("needs_refetch")   // event_ids flagged for re-ingestion
	bucketProfileQueue    = []byte("profile_queue")   // author pubkeys awaiting kind-0 fetch
)

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
	*relaykit.Store
}

func (m *ManagementStore) Init(db *bbolt.DB) error {
	store, err := relaykit.NewStore(db)
	if err != nil {
		return err
	}
	m.Store = store
	return db.Update(func(tx *bbolt.Tx) error {
		for _, bucket := range [][]byte{bucketTypesenseSchema, bucketSemanticConfig, bucketAccessControl, bucketWriteAllowlist, bucketReadAllowlist, bucketListReferences, bucketFetchedContent, bucketNeedsRefetch, bucketProfileQueue} {
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

// MarkNeedsRefetch flags an event id for re-ingestion by the indexer.
// Idempotent: marking the same id twice is a no-op.
func (m *ManagementStore) MarkNeedsRefetch(eventID string) error {
	return m.DB.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketNeedsRefetch).Put([]byte(eventID), []byte{})
	})
}

// RemoveNeedsRefetch clears the refetch flag for an event id.
// Idempotent: removing a non-existent flag is a no-op.
func (m *ManagementStore) RemoveNeedsRefetch(eventID string) error {
	return m.DB.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketNeedsRefetch).Delete([]byte(eventID))
	})
}

// ListNeedsRefetch returns all event ids currently flagged for refetch.
func (m *ManagementStore) ListNeedsRefetch() ([]string, error) {
	var result []string
	err := m.DB.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketNeedsRefetch).ForEach(func(k, _ []byte) error {
			result = append(result, string(k))
			return nil
		})
	})
	return result, err
}

// EnqueueProfileCandidate queues an author pubkey (hex) for kind-0 fetch.
// Idempotent: queuing the same pubkey twice is a no-op (dedup).
func (m *ManagementStore) EnqueueProfileCandidate(pubkey string) error {
	return m.DB.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketProfileQueue).Put([]byte(pubkey), []byte{})
	})
}

// RemoveProfileCandidate clears a pubkey from the profile fetch queue.
// Idempotent: removing a non-existent pubkey is a no-op.
func (m *ManagementStore) RemoveProfileCandidate(pubkey string) error {
	return m.DB.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketProfileQueue).Delete([]byte(pubkey))
	})
}

// ListProfileQueue returns all author pubkeys currently queued for kind-0 fetch.
func (m *ManagementStore) ListProfileQueue() ([]string, error) {
	var result []string
	err := m.DB.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketProfileQueue).ForEach(func(k, _ []byte) error {
			result = append(result, string(k))
			return nil
		})
	})
	return result, err
}

