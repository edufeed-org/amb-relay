package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
)

const (
	typesenseHost  = "http://localhost:8108"
	apiKey         = "xyz"
	// collectionName = "amb-test"
)

type CollectionSchema struct {
	Name                string  `json:"name"`
	Fields              []Field `json:"fields"`
	DefaultSortingField string  `json:"default_sorting_field"`
}

type Field struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Facet    bool   `json:"facet,omitempty"`
	Optional bool   `json:"optional,omitempty"`
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

func createCollection(name string) error {
	url := fmt.Sprintf("%s/collections", typesenseHost)

	schema := CollectionSchema{
		Name: name,
		Fields: []Field{
			{
				Name: "name",
				Type: "string",
			},
			{
				Name: "description",
				Type: "string",
			},
      {
				Name: "event_date_created",
				Type: "int64",
			},
		},
		DefaultSortingField: "event_date_created",
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
