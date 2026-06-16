package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/eventstore/typesense30142"
	"fiatjaf.com/nostr/khatru/semantic"
)

func sampleAMBEvent(t *testing.T) nostr.Event {
	t.Helper()
	evt := nostr.Event{
		Kind:      30142,
		PubKey:    nostr.MustPubKeyFromHex("0000000000000000000000000000000000000000000000000000000000000001"),
		CreatedAt: 1700000000,
		Tags: nostr.Tags{
			{"d", "resource-123"},
			{"name", "Bruchrechnung Grundlagen"},
			{"description", "Eine Einführung in die Bruchrechnung."},
			{"keywords", "Mathematik"},
			{"keywords", "Brüche"},
			{"about:id", "https://w3id.org/kim/schulfaecher/s1017"},
		},
		Content: "",
	}
	evt.ID = evt.GetID()
	return evt
}

func TestGoldenAMBProjection(t *testing.T) {
	evt := sampleAMBEvent(t)
	amb, err := typesense30142.NostrToAMB(&evt)
	if err != nil {
		t.Fatalf("NostrToAMB: %v", err)
	}
	got, err := json.MarshalIndent(amb, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	goldenAssert(t, "amb_projection.json", got)
}

func TestGoldenSnippetEvent(t *testing.T) {
	sk := nostr.MustSecretKeyFromHex("0000000000000000000000000000000000000000000000000000000000000abc")
	hit := semantic.ChunkHit{
		EventID:    "abc123",
		EventCoord: "30142:0000000000000000000000000000000000000000000000000000000000000001:resource-123",
		Score:      0.9876,
		Snippet:    "Brüche sind Teile eines Ganzen.",
		Page:       3,
		Heading:    "Einführung",
		SourceURL:  "https://example.org/bruch.pdf",
	}
	snip, ok := semantic.BuildSnippetEvent(sk, hit)
	if !ok {
		t.Fatal("buildSnippetEvent returned ok=false")
	}
	snip.CreatedAt = 0
	snip.ID = nostr.ID{}
	snip.Sig = [64]byte{}
	got, err := json.MarshalIndent(snip, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	goldenAssert(t, "snippet_event.json", got)
}

func goldenAssert(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", "golden", name)
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote golden %s", path)
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s (run with UPDATE_GOLDEN=1 to create): %v", path, err)
	}
	if string(want) != string(got) {
		t.Errorf("golden %s mismatch:\n--- want ---\n%s\n--- got ---\n%s", name, want, got)
	}
}
