package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	typesense30142 "fiatjaf.com/nostr/eventstore/typesense30142"
)

const (
	// EmbedFieldSeparator is used to join multiple text fields for embedding.
	EmbedFieldSeparator = " | "
	// EmbedMaxLength is the maximum character length before truncation.
	EmbedMaxLength = 8192
)

// EmbeddingClient implements the typesense30142.Embedder interface
// for the edufeed embedding service.
type EmbeddingClient struct {
	Endpoint    string
	BearerToken string
	HTTPClient  *http.Client
}

// Verify EmbeddingClient implements Embedder interface
var _ typesense30142.Embedder = (*EmbeddingClient)(nil)

type embedRequest struct {
	Texts []string `json:"texts"`
}

type embedResponse struct {
	Embeddings [][]float32 `json:"embeddings"`
}

// NewEmbeddingClient creates a new embedding client.
func NewEmbeddingClient(endpoint, token string) *EmbeddingClient {
	return &EmbeddingClient{
		Endpoint:    endpoint,
		BearerToken: token,
		HTTPClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// Embed computes embedding vectors for the given texts.
func (c *EmbeddingClient) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}

	reqBody, err := json.Marshal(embedRequest{Texts: texts})
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Endpoint, bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.BearerToken)

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("embedding service error %d: %s", resp.StatusCode, string(body))
	}

	var result embedResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	return result.Embeddings, nil
}

// BuildEmbedText concatenates AMB metadata fields for embedding based on the configured field list.
// Supported fields: "name", "description", "keywords", "about", "creator", "publisher", "learningResourceType"
func BuildEmbedText(amb *typesense30142.AMBMetadata, fields []string) string {
	var parts []string

	for _, field := range fields {
		switch field {
		case "name":
			if amb.Name != "" {
				parts = append(parts, amb.Name)
			}
		case "description":
			if amb.Description != "" {
				parts = append(parts, amb.Description)
			}
		case "keywords":
			if len(amb.Keywords) > 0 {
				parts = append(parts, strings.Join(amb.Keywords, ", "))
			}
		case "about":
			// Extract prefLabel values from about objects (all languages)
			for _, about := range amb.About {
				if about != nil && about.PrefLabel != nil {
					for _, label := range about.PrefLabel {
						if label != "" {
							parts = append(parts, label)
						}
					}
				}
			}
		case "creator":
			for _, creator := range amb.Creator {
				if creator != nil && creator.Name != "" {
					parts = append(parts, creator.Name)
				}
			}
		case "publisher":
			for _, publisher := range amb.Publisher {
				if publisher != nil && publisher.Name != "" {
					parts = append(parts, publisher.Name)
				}
			}
		case "learningResourceType":
			for _, lrt := range amb.LearningResourceType {
				if lrt != nil && lrt.PrefLabel != nil {
					for _, label := range lrt.PrefLabel {
						if label != "" {
							parts = append(parts, label)
						}
					}
				}
			}
		}
	}

	text := strings.Join(parts, EmbedFieldSeparator)
	if len(text) > EmbedMaxLength {
		text = text[:EmbedMaxLength]
	}
	return text
}
