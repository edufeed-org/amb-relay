package main

import (
	"encoding/json"
	"fmt"

	"go.etcd.io/bbolt"
)

// ContentStatus values recorded alongside fetched content.
const (
	ContentStatusFetched       = "fetched"        // successfully fetched full body
	ContentStatusTruncated     = "truncated"      // body exceeded EXTRACT_MAX_BYTES; prefix stored
	ContentStatusLicenseDenied = "license_denied" // license not on allowlist; vectors-only (no text)
	ContentStatusUnsupported   = "unsupported"    // content-type not handled
	ContentStatusFailed        = "failed"         // fetch/extract exhausted retries
)

// ContentEntry is the BoltDB payload for fetched resource fulltext.
type ContentEntry struct {
	Text        string `json:"text,omitempty"`
	FetchedAt   int64  `json:"fetched_at"`
	Status      string `json:"status"`
	SourceURL   string `json:"source_url,omitempty"`
	ContentHash string `json:"content_hash,omitempty"`
}

// ContentStore persists fetched resource fulltext in BoltDB, keyed by event id.
type ContentStore struct {
	DB *bbolt.DB
}

// Put inserts or replaces a content entry for the given event id.
func (c *ContentStore) Put(eventID string, entry ContentEntry) error {
	val, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("marshal content entry: %w", err)
	}
	return c.DB.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketFetchedContent).Put([]byte(eventID), val)
	})
}

// Get returns (entry, true, nil) if present; (zero, false, nil) if missing.
func (c *ContentStore) Get(eventID string) (ContentEntry, bool, error) {
	var entry ContentEntry
	var found bool
	err := c.DB.View(func(tx *bbolt.Tx) error {
		val := tx.Bucket(bucketFetchedContent).Get([]byte(eventID))
		if val == nil {
			return nil
		}
		found = true
		return json.Unmarshal(val, &entry)
	})
	return entry, found, err
}

// Delete removes a content entry. Idempotent.
func (c *ContentStore) Delete(eventID string) error {
	return c.DB.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketFetchedContent).Delete([]byte(eventID))
	})
}

// ForEach invokes fn for each content entry in the bucket. If fn returns a
// non-nil error, iteration stops and the error is returned.
func (c *ContentStore) ForEach(fn func(eventID string, entry ContentEntry) error) error {
	return c.DB.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketFetchedContent).ForEach(func(k, v []byte) error {
			var entry ContentEntry
			if err := json.Unmarshal(v, &entry); err != nil {
				return nil // skip malformed rows rather than aborting
			}
			return fn(string(k), entry)
		})
	})
}
