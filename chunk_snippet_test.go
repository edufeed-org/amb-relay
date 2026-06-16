package main

import (
	"testing"

	"fiatjaf.com/nostr"
)

func fullHit() ChunkHit {
	return ChunkHit{
		EventID:    "aa11bb22cc33dd44ee55ff6600112233445566778899aabbccddeeff00112233",
		EventCoord: "30142:deadbeef:photosynthese-skript",
		Score:      0.9312,
		Snippet:    "…bei stärkerem Licht verdoppelt sich die Photosyntheserate…",
		Page:       12,
		Heading:    "Lichtabhängigkeit",
		SourceURL:  "https://example.org/skript.pdf",
	}
}

func tagValue(t *testing.T, e nostr.Event, key string) string {
	t.Helper()
	tag := e.Tags.Find(key)
	if tag == nil {
		t.Fatalf("missing %q tag in %v", key, e.Tags)
	}
	return tag[1]
}

func TestBuildSnippetEvent_ValidSignature(t *testing.T) {
	sk := nostr.Generate()

	evt, ok := buildSnippetEvent(sk, fullHit())
	if !ok {
		t.Fatal("buildSnippetEvent returned ok=false for a valid hit")
	}
	if evt.Kind != kindSearchSnippet {
		t.Errorf("kind = %d, want 21142", evt.Kind)
	}
	if evt.PubKey != nostr.GetPublicKey(sk) {
		t.Errorf("pubkey = %s, want signer pubkey", evt.PubKey.Hex())
	}
	if !evt.VerifySignature() {
		t.Error("snippet event signature does not verify")
	}
}

func TestBuildSnippetEvent_TagsCorrect(t *testing.T) {
	sk := nostr.Generate()
	hit := fullHit()

	evt, ok := buildSnippetEvent(sk, hit)
	if !ok {
		t.Fatal("ok=false")
	}
	if evt.Content != hit.Snippet {
		t.Errorf("content = %q, want snippet verbatim", evt.Content)
	}
	if got := tagValue(t, evt, "e"); got != hit.EventID {
		t.Errorf("e tag = %q, want %q", got, hit.EventID)
	}
	if got := tagValue(t, evt, "a"); got != hit.EventCoord {
		t.Errorf("a tag = %q, want %q", got, hit.EventCoord)
	}
	if got := tagValue(t, evt, "k"); got != "30142" {
		t.Errorf("k tag = %q, want 30142", got)
	}
	if got := tagValue(t, evt, "score"); got != "0.9312" {
		t.Errorf("score tag = %q, want 0.9312 (%%.4f)", got)
	}
	if got := tagValue(t, evt, "page"); got != "12" {
		t.Errorf("page tag = %q, want 12", got)
	}
	if got := tagValue(t, evt, "heading"); got != hit.Heading {
		t.Errorf("heading tag = %q, want %q", got, hit.Heading)
	}
	if got := tagValue(t, evt, "source_url"); got != hit.SourceURL {
		t.Errorf("source_url tag = %q, want %q", got, hit.SourceURL)
	}
}

func TestBuildSnippetEvent_OmitsEmptyLocators(t *testing.T) {
	sk := nostr.Generate()
	hit := fullHit()
	hit.Page = 0
	hit.Heading = ""
	hit.SourceURL = ""

	evt, ok := buildSnippetEvent(sk, hit)
	if !ok {
		t.Fatal("ok=false")
	}
	for _, key := range []string{"page", "heading", "source_url"} {
		if evt.Tags.Find(key) != nil {
			t.Errorf("%q tag present despite empty locator", key)
		}
	}
}

func TestBuildSnippetEvent_EmptySnippet_NotEmitted(t *testing.T) {
	sk := nostr.Generate()
	hit := fullHit()
	hit.Snippet = ""

	if _, ok := buildSnippetEvent(sk, hit); ok {
		t.Error("ok=true for empty snippet, want false")
	}
}

func TestBuildSnippetEventUsesParentKind(t *testing.T) {
	sk := nostr.MustSecretKeyFromHex("0000000000000000000000000000000000000000000000000000000000000abc")
	hit := ChunkHit{
		EventID:    "e1",
		EventCoord: "30023:pk:d1",
		Kind:       30023,
		Score:      0.5,
		Snippet:    "long-form passage",
	}
	snip, ok := buildSnippetEvent(sk, hit)
	if !ok {
		t.Fatal("ok=false")
	}
	var kTag string
	for _, tag := range snip.Tags {
		if len(tag) >= 2 && tag[0] == "k" {
			kTag = tag[1]
		}
	}
	if kTag != "30023" {
		t.Errorf("k tag = %q, want 30023", kTag)
	}
}
