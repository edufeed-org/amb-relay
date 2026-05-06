package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestPatchDoc_EscapesDocIDInPath verifies that docIDs containing URL-unsafe
// characters (':', '/') are path-escaped so Typesense sees the whole string
// as a single document id, not multiple path segments. Regression test for
// the 404s hit when AMB events (whose d-tag is a URL) were patched.
func TestPatchDoc_EscapesDocIDInPath(t *testing.T) {
	const docID = "abc123:https://example.com/path/"
	const collection = "amb"

	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// RequestURI is the raw, un-decoded path as it came over the wire.
		gotPath = r.RequestURI
		w.WriteHeader(http.StatusOK)
		_, _ = io.Copy(io.Discard, r.Body)
	}))
	defer srv.Close()

	if err := patchDoc(srv.URL, "key", collection, docID, map[string]any{"x": 1}); err != nil {
		t.Fatalf("patchDoc: %v", err)
	}

	// '/' must be escaped (those were the real 404 cause — Typesense parsed
	// them as extra path segments). ':' need not be escaped; Typesense
	// accepts it literally in the doc-id segment.
	docSegment := strings.TrimPrefix(gotPath, "/collections/"+collection+"/documents/")
	if strings.Contains(docSegment, "/") {
		t.Errorf("docID segment still contains unescaped '/': %q", gotPath)
	}
	if !strings.Contains(docSegment, "%2F") {
		t.Errorf("docID segment missing %%2F (slash escape): %q", gotPath)
	}
}
