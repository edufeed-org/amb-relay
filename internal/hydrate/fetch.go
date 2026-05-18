package hydrate

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// FetchPage hits Typesense /documents/search with q=* and pulls both the
// eventRaw and eventID fields. Returning both is required so the verify
// pass can cross-check that BoltDB has an entry keyed at TS.eventID — see
// VerifyOne for why TS.eventID can drift from the hash of eventRaw. Per_page
// is the page-size flag; page is 1-indexed.
func FetchPage(ctx context.Context, host, apiKey, collection string, page, perPage int) ([]TSDocRef, error) {
	q := url.Values{}
	q.Set("q", "*")
	q.Set("query_by", "name")
	q.Set("include_fields", "eventRaw,eventID")
	q.Set("per_page", fmt.Sprintf("%d", perPage))
	q.Set("page", fmt.Sprintf("%d", page))

	endpoint := fmt.Sprintf("%s/collections/%s/documents/search?%s", host, collection, q.Encode())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-TYPESENSE-API-KEY", apiKey)

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, body)
	}

	var parsed struct {
		Hits []struct {
			Document struct {
				EventRaw string `json:"eventRaw"`
				EventID  string `json:"eventID"`
			} `json:"document"`
		} `json:"hits"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	out := make([]TSDocRef, 0, len(parsed.Hits))
	for _, h := range parsed.Hits {
		if h.Document.EventRaw != "" {
			out = append(out, TSDocRef{
				EventID:  h.Document.EventID,
				EventRaw: h.Document.EventRaw,
			})
		}
	}
	return out, nil
}
