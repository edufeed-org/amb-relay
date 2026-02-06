package main

import (
	"encoding/json"

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
)

const schemaKey = "current"
const semanticConfigKey = "config"

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
		for _, bucket := range [][]byte{bucketBannedPubKeys, bucketBannedEvents, bucketTypesenseSchema, bucketSemanticConfig} {
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
