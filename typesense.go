package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/nbd-wtf/go-nostr"
)

// TODO fix all the request stuff and put it in a dedicated function

type CollectionSchema struct {
	Name                string  `json:"name"`
	Fields              []Field `json:"fields"`
	DefaultSortingField string  `json:"default_sorting_field"`
	EnableNestedFields  bool    `json:"enable_nested_fields"`
}

type Field struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Facet    bool   `json:"facet,omitempty"`
	Optional bool   `json:"optional,omitempty"`
}

type SearchResponse struct {
	Found   int                      `json:"found"`
	Hits    []map[string]interface{} `json:"hits"`
	Page    int                      `json:"page"`
	Request map[string]interface{}   `json:"request"`
}

// CheckOrCreateCollection checks if a collection exists and creates it if it doesn't
func CheckOrCreateCollection(collectionName string) error {
	exists, err := collectionExists(collectionName)
	if err != nil {
		log.Fatalf("Error checking collection: %v", err)
	}

	if !exists {
		fmt.Printf("Collection %s does not exist. Creating...\n", collectionName)
		if err := createCollection(collectionName); err != nil {
			log.Fatalf("Error creating collection: %v", err)
		}
		fmt.Printf("Collection %s created successfully\n", collectionName)
	} else {
		fmt.Printf("Collection %s already exists\n", collectionName)
	}

	return nil
}

func collectionExists(name string) (bool, error) {
	url := fmt.Sprintf("%s/collections/%s", typesenseHost, name)

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return false, err
	}

	req.Header.Set("X-TYPESENSE-API-KEY", apiKey)

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	// 404 means collection doesn't exist
	if resp.StatusCode == http.StatusNotFound {
		return false, nil
	}

	// Any status code other than 200 is an error
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return false, fmt.Errorf("unexpected status code: %d, body: %s", resp.StatusCode, string(body))
	}

	return true, nil
}

// create a typesense collection
func createCollection(name string) error {
	url := fmt.Sprintf("%s/collections", typesenseHost)

	schema := CollectionSchema{
		Name: name,
		Fields: []Field{
			// Base information
			{Name: "id", Type: "string"},
			{Name: "d", Type: "string"},
			{Name: "type", Type: "string"},
			{Name: "name", Type: "string"},
			{Name: "description", Type: "string", Optional: true},
			{Name: "about", Type: "object[]", Optional: true},
			{Name: "keywords", Type: "string[]", Optional: true},
			{Name: "inLanguage", Type: "string[]", Optional: true},
			{Name: "image", Type: "string", Optional: true},
			{Name: "trailer", Type: "object[]", Optional: true},

			// Provenience
			{Name: "creator", Type: "object[]", Optional: true},
			{Name: "contributor", Type: "object[]", Optional: true},
			{Name: "dateCreated", Type: "string", Optional: true},
			{Name: "datePublished", Type: "string", Optional: true},
			{Name: "dateModified", Type: "string", Optional: true},
			{Name: "publisher", Type: "object[]", Optional: true},
			{Name: "funder", Type: "object[]", Optional: true},

			// Costs and Rights
			{Name: "isAccessibleForFree", Type: "bool", Optional: true},
			{Name: "license", Type: "object", Optional: true},
			{Name: "conditionsOfAccess", Type: "object", Optional: true},

			// Educational Metadata
			{Name: "learningResourceType", Type: "object[]", Optional: true},
			{Name: "audience", Type: "object[]", Optional: true},
			{Name: "teaches", Type: "object[]", Optional: true},
			{Name: "assesses", Type: "object[]", Optional: true},
			{Name: "competencyRequired", Type: "object[]", Optional: true},
			{Name: "educationalLevel", Type: "object[]", Optional: true},
			{Name: "interactivityType", Type: "object", Optional: true},

			// Relation
			{Name: "isBasedOn", Type: "object[]", Optional: true},
			{Name: "isPartOf", Type: "object[]", Optional: true},
			{Name: "hasPart", Type: "object[]", Optional: true},

			// Technical
			{Name: "duration", Type: "string", Optional: true},

			// Nostr Event
			{Name: "eventID", Type: "string"},
			{Name: "eventKind", Type: "int32"},
			{Name: "eventPubKey", Type: "string"},
			{Name: "eventSignature", Type: "string"},
			{Name: "eventCreatedAt", Type: "int64"},
			{Name: "eventContent", Type: "string"},
			{Name: "eventRaw", Type: "string"},
		},
		DefaultSortingField: "eventCreatedAt",
		EnableNestedFields:  true,
	}

	jsonData, err := json.Marshal(schema)
	if err != nil {
		return err
	}

	req, err := http.NewRequest(http.MethodPost, url, bytes.NewBuffer(jsonData))
	if err != nil {
		return err
	}

	req.Header.Set("X-TYPESENSE-API-KEY", apiKey)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("failed to create collection, status: %d, body: %s", resp.StatusCode, string(body))
	}

	return nil
}

// TODO Count events
func CountEvents(collectionName string, filter nostr.Filter) (int64, error) {
	fmt.Println("filter", filter)
	// search by author

	// search by d-tag

	return 0, nil
}

// Delete a nostr event from the index
func DeleteNostrEvent(collectionName string, event *nostr.Event) error {
  fmt.Println("deleting event")
	d := event.Tags.GetD()

	url := fmt.Sprintf(
		"%s/collections/%s/documents?filter_by=d:=%s&&eventPubKey:=%s",
		typesenseHost, collectionName, d, event.PubKey)
	fmt.Println("url", url)
	req, err := http.NewRequest(http.MethodDelete, url, nil)
	if err != nil {
		return err
	}

	req.Header.Set("X-TYPESENSE-API-KEY", apiKey)

	client := &http.Client{}
	resp, err := client.Do(req)

	if err != nil {
		return err
	}

	defer resp.Body.Close()

	// Any status code other than 200 is an error
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("unexpected status code: %d, body: %s", resp.StatusCode, string(body))
	}

	return nil
}

// IndexNostrEvent converts a Nostr event to AMB metadata and indexes it in Typesense
func IndexNostrEvent(collectionName string, event *nostr.Event) error {
	ambData, err := NostrToAMB(event)
	if err != nil {
		return fmt.Errorf("error converting Nostr event to AMB metadata: %v", err)
	}

	// check if event is already there, if so replace it, else index it
	alreadyIndexed, err := eventAlreadyIndexed(collectionName, ambData)
	return indexDocument(collectionName, ambData, alreadyIndexed)
}

func eventAlreadyIndexed(collectionName string, doc *AMBMetadata) (*nostr.Event, error) {
	url := fmt.Sprintf(
		"%s/collections/%s/documents/search?filter_by=d:=%s&&eventPubKey:=%s&q=&query_by=d,eventPubKey",
		typesenseHost, collectionName, doc.D, doc.EventPubKey)
	fmt.Println("url", url)

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("X-TYPESENSE-API-KEY", apiKey)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{}

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("failed to index document, status: %d, body: %s", resp.StatusCode, string(body))
	}

	events, err := parseSearchResponse(body)
	if err != nil {
    return nil, fmt.Errorf("got parsing search response: %v", err)
	}
	fmt.Println("response", events)

  // Check if we found any events
	if len(events) == 0 {
		return nil, nil
	}
	return &events[0], nil
}

// Index a document in Typesense
func indexDocument(collectionName string, doc *AMBMetadata, alreadyIndexedEvent *nostr.Event) error {
	if alreadyIndexedEvent != nil {
    // delete it
		fmt.Println("updating")
    DeleteNostrEvent(collectionName, alreadyIndexedEvent)
	}

	url := fmt.Sprintf("%s/collections/%s/documents", typesenseHost, collectionName)

	jsonData, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	method := http.MethodPost

	req, err := http.NewRequest(method, url, bytes.NewBuffer(jsonData))
	if err != nil {
		return err
	}

	req.Header.Set("X-TYPESENSE-API-KEY", apiKey)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{}

	// Do the request
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("failed to index document, status: %d, body: %s", resp.StatusCode, string(body))
	}

	return nil
}

// SearchQuery represents a parsed search query with raw terms and field filters
type SearchQuery struct {
	RawTerms     []string
	FieldFilters map[string]string
}

// ParseSearchQuery parses a search string with support for quoted terms and field:value pairs
func ParseSearchQuery(searchStr string) SearchQuery {
	var query SearchQuery
	query.RawTerms = []string{}
	query.FieldFilters = make(map[string]string)

	// Regular expression to match quoted strings and field:value pairs
	// This regex handles:
	// 1. Quoted strings (preserving spaces and everything inside)
	// 2. Field:value pairs
	// 3. Regular words
	re := regexp.MustCompile(`"([^"]+)"|(\S+\.\S+):(\S+)|(\S+)`)
	matches := re.FindAllStringSubmatch(searchStr, -1)

	for _, match := range matches {
		if match[1] != "" {
			// This is a quoted string, add it to raw terms
			query.RawTerms = append(query.RawTerms, match[1])
		} else if match[2] != "" && match[3] != "" {
			// This is a field:value pair
			fieldName := match[2]
			fieldValue := match[3]
			query.FieldFilters[fieldName] = fieldValue
		} else if match[4] != "" {
			// This is a regular word, check if it's a simple field:value
			parts := strings.SplitN(match[4], ":", 2)
			if len(parts) == 2 && !strings.Contains(parts[0], ".") {
				// Simple field:value without dot notation
				query.FieldFilters[parts[0]] = parts[1]
			} else {
				// Regular search term
				query.RawTerms = append(query.RawTerms, match[4])
			}
		}
	}

	return query
}

// BuildTypesenseQuery builds a Typesense search query from a parsed SearchQuery
func BuildTypesenseQuery(query SearchQuery) (string, map[string]string, error) {
	// Join raw terms for the main query
	mainQuery := strings.Join(query.RawTerms, " ")

	// Parameters for filter_by and other Typesense parameters
	params := make(map[string]string)

	// Build filter expressions for field filters
	var filterExpressions []string

	for field, value := range query.FieldFilters {
		// Handle special fields with dot notation
		if strings.Contains(field, ".") {
			parts := strings.SplitN(field, ".", 2)
			fieldName := parts[0]
			subField := parts[1]

			filterExpressions = append(filterExpressions, fmt.Sprintf("%s.%s:%s", fieldName, subField, value))
		} else {
			filterExpressions = append(filterExpressions, fmt.Sprintf("%s:%s", field, value))
		}
	}

	// Combine all filter expressions
	if len(filterExpressions) > 0 {
		params["filter_by"] = strings.Join(filterExpressions, " && ")
	}

	return mainQuery, params, nil
}

// SearchResources searches for resources and returns both the AMB metadata and converted Nostr events
func SearchResources(collectionName, searchStr string) ([]nostr.Event, error) {
	parsedQuery := ParseSearchQuery(searchStr)

	mainQuery, params, err := BuildTypesenseQuery(parsedQuery)
	if err != nil {
		return nil, fmt.Errorf("error building Typesense query: %v", err)
	}

	// URL encode the main query
	encodedQuery := url.QueryEscape(mainQuery)

	// Default fields to search in
	queryBy := "name,description"

	// Start building the search URL
	searchURL := fmt.Sprintf("%s/collections/%s/documents/search?q=%s&query_by=%s",
		typesenseHost, collectionName, encodedQuery, queryBy)

	// Add additional parameters
	for key, value := range params {
		searchURL += fmt.Sprintf("&%s=%s", key, url.QueryEscape(value))
	}

	// Debug information
	fmt.Printf("Search URL: %s\n", searchURL)

	// Create request
	req, err := http.NewRequest(http.MethodGet, searchURL, nil)
	if err != nil {
		return nil, fmt.Errorf("error creating search request: %v", err)
	}
	req.Header.Set("X-TYPESENSE-API-KEY", apiKey)

	// Execute the request
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("error executing search request: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("error reading response body: %v", err)
	}

	// Check for errors
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("search failed with status code %d: %s", resp.StatusCode, string(body))
	}

	return parseSearchResponse(body)
}

func parseSearchResponse(responseBody []byte) ([]nostr.Event, error) {
	var searchResponse SearchResponse
	if err := json.Unmarshal(responseBody, &searchResponse); err != nil {
		return nil, fmt.Errorf("error parsing search response: %v", err)
	}

	nostrResults := make([]nostr.Event, 0, len(searchResponse.Hits))

	for _, hit := range searchResponse.Hits {
		// Extract the document from the hit
		docMap, ok := hit["document"].(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("invalid document format in search results")
		}

		// Convert the map to AMB metadata
		docJSON, err := json.Marshal(docMap)
		if err != nil {
			return nil, fmt.Errorf("error marshaling document: %v", err)
		}

		var ambData AMBMetadata
		if err := json.Unmarshal(docJSON, &ambData); err != nil {
			return nil, fmt.Errorf("error unmarshaling to AMBMetadata: %v", err)
		}

		// Convert the AMB metadata to a Nostr event
		nostrEvent, err := StringifiedJSONToNostrEvent(ambData.EventRaw)
		if err != nil {
			fmt.Printf("Warning: failed to convert AMB to Nostr: %v\n", err)
			continue
		}

		nostrResults = append(nostrResults, nostrEvent)
	}

	// Print the number of results for logging
	fmt.Printf("Found %d results\n",
		len(nostrResults))

	return nostrResults, nil
}
