package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// vizHTTPClient pools connections for the read-only viz Typesense calls.
var vizHTTPClient = &http.Client{Timeout: 15 * time.Second}

// FacetValue is one bucket of a Typesense facet_counts result.
type FacetValue struct {
	Value string
	Count int
}

// VizDoc is a decoded Typesense document (arbitrary fields).
type VizDoc map[string]any

// tsSearchURL builds a /documents/search URL with q=* and the given params.
func tsSearchURL(host, collection string, extra url.Values) string {
	q := url.Values{}
	q.Set("q", "*")
	q.Set("query_by", "name")
	// q=* ignores query_by for matching; validate_field_names=false keeps the
	// call from 400ing on collections that lack a `name` field (matches the
	// eventstore's own query convention).
	q.Set("validate_field_names", "false")
	for k, vs := range extra {
		for _, v := range vs {
			q.Add(k, v)
		}
	}
	return fmt.Sprintf("%s/collections/%s/documents/search?%s", host, collection, q.Encode())
}

func tsGet(ctx context.Context, endpoint, apiKey string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-TYPESENSE-API-KEY", apiKey)
	resp, err := vizHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("typesense status %d: %s", resp.StatusCode, body)
	}
	return body, nil
}

// vizCount returns the total document count of a collection via found on a
// q=* search with per_page=0.
func vizCount(ctx context.Context, host, apiKey, collection string) (int, error) {
	u := tsSearchURL(host, collection, url.Values{"per_page": {"0"}})
	body, err := tsGet(ctx, u, apiKey)
	if err != nil {
		return 0, err
	}
	var parsed struct {
		Found int `json:"found"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return 0, fmt.Errorf("decode count: %w", err)
	}
	return parsed.Found, nil
}

// vizFacet returns the facet buckets for one field, most-frequent first
// (Typesense default ordering).
func vizFacet(ctx context.Context, host, apiKey, collection, field string, maxValues int) ([]FacetValue, error) {
	u := tsSearchURL(host, collection, url.Values{
		"per_page":         {"0"},
		"facet_by":         {field},
		"max_facet_values": {strconv.Itoa(maxValues)},
	})
	body, err := tsGet(ctx, u, apiKey)
	if err != nil {
		return nil, err
	}
	var parsed struct {
		FacetCounts []struct {
			FieldName string `json:"field_name"`
			Counts    []struct {
				Value string `json:"value"`
				Count int    `json:"count"`
			} `json:"counts"`
		} `json:"facet_counts"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("decode facet: %w", err)
	}
	out := []FacetValue{}
	for _, fc := range parsed.FacetCounts {
		if fc.FieldName != field {
			continue
		}
		for _, c := range fc.Counts {
			out = append(out, FacetValue{Value: c.Value, Count: c.Count})
		}
	}
	return out, nil
}

// vizSearch runs a q=* search with an optional filter_by and returns the raw
// documents (capped by perPage). includeFields is a Typesense include_fields
// CSV; empty means all fields.
func vizSearch(ctx context.Context, host, apiKey, collection, filterBy, includeFields string, perPage int) ([]VizDoc, error) {
	extra := url.Values{"per_page": {strconv.Itoa(perPage)}}
	if filterBy != "" {
		extra.Set("filter_by", filterBy)
	}
	if includeFields != "" {
		extra.Set("include_fields", includeFields)
	}
	u := tsSearchURL(host, collection, extra)
	body, err := tsGet(ctx, u, apiKey)
	if err != nil {
		return nil, err
	}
	var parsed struct {
		Hits []struct {
			Document VizDoc `json:"document"`
		} `json:"hits"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("decode search: %w", err)
	}
	out := make([]VizDoc, 0, len(parsed.Hits))
	for _, h := range parsed.Hits {
		out = append(out, h.Document)
	}
	return out, nil
}
