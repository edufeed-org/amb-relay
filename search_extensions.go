package main

import (
	"slices"

	"fiatjaf.com/nostr/eventstore/typesense30142"
	"fiatjaf.com/nostr/nip11"
)

// searchPayloadFields are stored for serving or ranking, never for a client
// to filter on by exact value, so they are not advertised as extensions.
var searchPayloadFields = map[string]bool{
	"content":            true,
	"content_fetched_at": true,
	"eventContent":       true,
	"eventRaw":           true,
	"eventSignature":     true,
}

// nip50SearchExtensions lists the NIP-50 search-string keys this relay
// understands: the `sort:` directive first, then — sorted — every top-level
// field a `field:value` token can filter on in any of the given collections.
// It mirrors what typesense30142's ParseSearch keeps: a key no schema
// declares is ignored there, so it is not advertised here.
func nip50SearchExtensions(schemas ...typesense30142.CollectionSchema) []string {
	seen := map[string]bool{}
	var fields []string
	for _, schema := range schemas {
		for _, f := range schema.Fields {
			if f.Type == "float[]" || searchPayloadFields[f.Name] || seen[f.Name] {
				continue
			}
			seen[f.Name] = true
			fields = append(fields, f.Name)
		}
	}
	slices.Sort(fields)
	return append([]string{"sort"}, fields...)
}

// advertiseSearchExtensions publishes the list in both NIP-11 spellings
// clients read today: limitation.search_extensions (Brainstorm's search
// relay) and the top-level nip50 list of "ext <token>" entries (NosFabrica
// vespa-relay). An existing limitation block keeps its other values.
func advertiseSearchExtensions(info *nip11.RelayInformationDocument, exts []string) {
	if info.Limitation == nil {
		info.Limitation = &nip11.RelayLimitationDocument{}
	}
	info.Limitation.SearchExtensions = exts
	info.Nip50 = make([]string, len(exts))
	for i, e := range exts {
		info.Nip50[i] = "ext " + e
	}
}
