package main

import (
	"reflect"
	"testing"

	"fiatjaf.com/nostr/eventstore/typesense30142"
	"fiatjaf.com/nostr/nip11"
)

// NIP-11 advertises the NIP-50 search-string keys this relay understands:
// the sort directive plus every top-level field a field:value token can
// filter on, across all enabled collections — so clients can discover the
// grammar instead of reading NIP-AMB. Internal payload fields are not
// filters anyone should build.
func TestNip50SearchExtensionsUnionsSchemasAndSkipsInternals(t *testing.T) {
	amb := typesense30142.CollectionSchema{Fields: []typesense30142.Field{
		{Name: "name", Type: "string"},
		{Name: "about", Type: "object[]"},
		{Name: "community", Type: "string[]"},
		{Name: "eventRaw", Type: "string"},
		{Name: "eventSignature", Type: "string"},
		{Name: "embedding", Type: "float[]"},
	}}
	profiles := typesense30142.CollectionSchema{Fields: []typesense30142.Field{
		{Name: "title", Type: "string"},
		{Name: "community", Type: "string[]"},
		{Name: "nip05_verified", Type: "bool"},
		{Name: "content", Type: "string"},
		{Name: "eventContent", Type: "string"},
	}}
	got := nip50SearchExtensions(amb, profiles)
	want := []string{"sort", "about", "community", "name", "nip05_verified", "title"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("nip50SearchExtensions = %q, want %q", got, want)
	}
}

func TestAdvertiseSearchExtensionsWritesBothSpellings(t *testing.T) {
	info := nip11.RelayInformationDocument{}
	advertiseSearchExtensions(&info, []string{"sort", "about"})
	if info.Limitation == nil || !reflect.DeepEqual(info.Limitation.SearchExtensions, []string{"sort", "about"}) {
		t.Errorf("limitation.search_extensions = %+v", info.Limitation)
	}
	if !reflect.DeepEqual(info.Nip50, []string{"ext sort", "ext about"}) {
		t.Errorf("nip50 = %q", info.Nip50)
	}
	// An existing limitation block keeps its other values.
	info = nip11.RelayInformationDocument{Limitation: &nip11.RelayLimitationDocument{MaxLimit: 250}}
	advertiseSearchExtensions(&info, []string{"sort"})
	if info.Limitation.MaxLimit != 250 {
		t.Errorf("MaxLimit clobbered: %+v", info.Limitation)
	}
}
