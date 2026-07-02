package main

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed web/viz
var vizAssets embed.FS

// VizCollections names the Typesense collections the viz endpoints read.
// Empty names are skipped (feature not enabled).
type VizCollections struct {
	AMB           string
	Longform      string
	Wiki          string
	Calendar      string
	Shares        string
	Transferkiosk string
}

// VizConfig bundles everything vizSetup needs. All fields are read-only use.
type VizConfig struct {
	Host        string
	ApiKey      string
	Collections VizCollections
	TopAuthors  int
	CacheTTL    time.Duration
}

// ambScanCap bounds the client-side aggregate scan of the AMB collection. The
// AMB `publisher`/`about` object[] fields are not facetable, so authors and
// subjects are rolled up by scanning docs. (Post-demo: declare those subfields
// facetable and switch to facet_by — see project memory.)
const ambScanCap = 2000

// tkScanCap bounds the transferkiosk pull (medium scale → a few hundred docs).
const tkScanCap = 1000

// vizCache is a tiny TTL cache for the two expensive endpoints. A per-key build
// lock collapses concurrent misses into a single build (no thundering herd);
// errors are never cached.
type vizCache struct {
	mu      sync.Mutex
	entries map[string]vizCacheEntry
	locks   map[string]*sync.Mutex
}

type vizCacheEntry struct {
	val    any
	expiry time.Time
}

func newVizCache() *vizCache {
	return &vizCache{entries: map[string]vizCacheEntry{}, locks: map[string]*sync.Mutex{}}
}

func (c *vizCache) fresh(key string) (any, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.entries[key]; ok && time.Now().Before(e.expiry) {
		return e.val, true
	}
	return nil, false
}

func (c *vizCache) get(key string, ttl time.Duration, build func() (any, error)) (any, error) {
	if v, ok := c.fresh(key); ok {
		return v, nil
	}

	c.mu.Lock()
	kl := c.locks[key]
	if kl == nil {
		kl = &sync.Mutex{}
		c.locks[key] = kl
	}
	c.mu.Unlock()

	// Serialize builds per key; the winner populates, the rest read the result.
	kl.Lock()
	defer kl.Unlock()
	if v, ok := c.fresh(key); ok {
		return v, nil
	}

	val, err := build()
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.entries[key] = vizCacheEntry{val: val, expiry: time.Now().Add(ttl)}
	c.mu.Unlock()
	return val, nil
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// vizSetup registers all /viz routes on mux. Call ONLY when VIZ_ENABLED.
func vizSetup(mux *http.ServeMux, cfg VizConfig) {
	cache := newVizCache()

	sub, err := fs.Sub(vizAssets, "web/viz")
	if err != nil {
		panic(err) // embed path is a compile-time constant; this cannot fail
	}
	fileServer := http.FileServer(http.FS(sub))

	// API routes first (longer, more specific patterns win in ServeMux).
	mux.HandleFunc("/viz/stats", func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithoutCancel(r.Context())
		v, err := cache.get("stats", cfg.CacheTTL, func() (any, error) {
			return cfg.computeStats(ctx)
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		writeJSON(w, v)
	})

	mux.HandleFunc("/viz/graph", func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithoutCancel(r.Context())
		v, err := cache.get("graph", cfg.CacheTTL, func() (any, error) {
			return cfg.computeGraph(ctx)
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		writeJSON(w, v)
	})

	mux.HandleFunc("/viz/node/", func(w http.ResponseWriter, r *http.Request) {
		// Split on the escaped path so a %2F inside a segment (e.g. a
		// publisher name containing "/") isn't mistaken for a separator.
		rest := strings.TrimPrefix(r.URL.EscapedPath(), "/viz/node/")
		parts := strings.SplitN(rest, "/", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			http.Error(w, "bad node ref", http.StatusBadRequest)
			return
		}
		typ, err := url.PathUnescape(parts[0])
		if err != nil {
			http.Error(w, "bad node ref", http.StatusBadRequest)
			return
		}
		ref, err := url.PathUnescape(parts[1])
		if err != nil {
			http.Error(w, "bad node ref", http.StatusBadRequest)
			return
		}
		items, err := cfg.computeNode(r.Context(), typ, ref)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		writeJSON(w, items)
	})

	// GET /viz → index; /viz/<asset> → static file.
	mux.HandleFunc("/viz", func(w http.ResponseWriter, r *http.Request) {
		r.URL.Path = "/"
		fileServer.ServeHTTP(w, r)
	})
	mux.HandleFunc("/viz/", func(w http.ResponseWriter, r *http.Request) {
		http.StripPrefix("/viz/", fileServer).ServeHTTP(w, r)
	})
}

// contentCollections returns the non-empty content collections keyed by kind.
func (cfg VizConfig) contentCollections() map[int]struct {
	name  string
	label string
} {
	out := map[int]struct {
		name  string
		label string
	}{}
	add := func(kind int, name, label string) {
		if name != "" {
			out[kind] = struct {
				name  string
				label string
			}{name, label}
		}
	}
	add(30142, cfg.Collections.AMB, "AMB resource")
	add(30023, cfg.Collections.Longform, "Long-form")
	add(30818, cfg.Collections.Wiki, "Wiki")
	add(31923, cfg.Collections.Calendar, "Calendar")
	return out
}

// computeStats builds the /viz/stats payload from Typesense reads.
func (cfg VizConfig) computeStats(ctx context.Context) (Stats, error) {
	var typeCounts []TypeCount
	for kind, c := range cfg.contentCollections() {
		n, err := vizCount(ctx, cfg.Host, cfg.ApiKey, c.name)
		if err != nil {
			return Stats{}, err
		}
		typeCounts = append(typeCounts, TypeCount{Kind: kind, Label: c.label, Count: n})
	}
	// Deterministic order (map iteration is random).
	sort.SliceStable(typeCounts, func(i, j int) bool { return typeCounts[i].Kind < typeCounts[j].Kind })

	authors, subjects, _ := cfg.scanAMBAggregate(ctx, ambScanCap)
	if len(subjects) > 12 {
		subjects = subjects[:12]
	}
	communities, projects := cfg.communityAndProjectCounts(ctx)

	return buildStats(StatsInput{
		TypeCounts:  typeCounts,
		Subjects:    subjects,
		Authors:     len(authors),
		Communities: communities,
		Projects:    projects,
	}), nil
}

// communityAndProjectCounts returns the distinct community count (from the
// shares collection `community` facet) and the project count (30143 docs in
// transferkiosk). Both degrade to 0 when their collection is disabled/absent.
func (cfg VizConfig) communityAndProjectCounts(ctx context.Context) (int, int) {
	communities := 0
	if cfg.Collections.Shares != "" {
		if f, err := vizFacet(ctx, cfg.Host, cfg.ApiKey, cfg.Collections.Shares, "community", 1000); err == nil {
			communities = len(f)
		}
	}
	projects := 0
	if cfg.Collections.Transferkiosk != "" {
		if n, err := cfg.filteredCount(ctx, cfg.Collections.Transferkiosk, "eventKind:=30143"); err == nil {
			projects = n
		}
	}
	return communities, projects
}

// computeGraph builds the /viz/graph payload. Authors + author→community edges
// come from a client-side AMB scan; communities from the shares `community`
// facet; the transferkiosk tree from a transferkiosk pull. Missing collections
// degrade gracefully (that slice stays empty).
func (cfg VizConfig) computeGraph(ctx context.Context) (Graph, error) {
	authors, _, authorCommunities := cfg.scanAMBAggregate(ctx, ambScanCap)

	in := GraphInput{TopAuthors: cfg.TopAuthors, AuthorCommunities: authorCommunities}
	for _, f := range authors {
		in.Authors = append(in.Authors, AuthorStat{Pubkey: f.Value, Label: f.Value, Count: f.Count})
	}

	if cfg.Collections.Shares != "" {
		if facets, err := vizFacet(ctx, cfg.Host, cfg.ApiKey, cfg.Collections.Shares, "community", 1000); err == nil {
			for _, f := range facets {
				in.Communities = append(in.Communities, CommunityStat{Pubkey: f.Value, Label: shortLabel(f.Value), Content: f.Count})
			}
		}
	}

	if cfg.Collections.Transferkiosk != "" {
		docs := pagedSearch(ctx, cfg.Host, cfg.ApiKey, cfg.Collections.Transferkiosk, "", "id,eventKind,name,partOf,publisher,community", tkScanCap)
		for _, d := range docs {
			in.TK = append(in.TK, tkItemFromDoc(d))
		}
	}

	return buildGraph(in), nil
}

// computeNode returns the capped list of real events behind a node.
func (cfg VizConfig) computeNode(ctx context.Context, typ, id string) ([]VizDoc, error) {
	const capItems = 50
	switch typ {
	case "author":
		if cfg.Collections.AMB == "" {
			return []VizDoc{}, nil
		}
		return vizSearch(ctx, cfg.Host, cfg.ApiKey, cfg.Collections.AMB, "publisher.name:="+tsEscape(id), "id,name,community", capItems)
	case "community":
		if cfg.Collections.AMB == "" {
			return []VizDoc{}, nil
		}
		return vizSearch(ctx, cfg.Host, cfg.ApiKey, cfg.Collections.AMB, "community:="+tsEscape(id), "id,name,community", capItems)
	case "project", "measure", "publication":
		if cfg.Collections.Transferkiosk == "" {
			return []VizDoc{}, nil
		}
		return vizSearch(ctx, cfg.Host, cfg.ApiKey, cfg.Collections.Transferkiosk, "partOf:="+tsEscape(id), "id,name,eventKind,partOf", capItems)
	default:
		return []VizDoc{}, nil
	}
}

// scanAMBAggregate pages through up to maxDocs AMB docs and tallies distinct
// publisher names (authors), about prefLabel:de values (subjects), and
// author→community pairs. The AMB object[] fields are not facetable, so these
// are rolled up client-side rather than via facet_by.
func (cfg VizConfig) scanAMBAggregate(ctx context.Context, maxDocs int) (authors, subjects []FacetValue, acs []AuthorCommunity) {
	if cfg.Collections.AMB == "" {
		return nil, nil, nil
	}
	docs := pagedSearch(ctx, cfg.Host, cfg.ApiKey, cfg.Collections.AMB, "", "publisher,about,community", maxDocs)

	authorCount := map[string]int{}
	subjectCount := map[string]int{}
	acCount := map[[2]string]int{}
	for _, d := range docs {
		names := publisherNames(d["publisher"])
		comms := stringSlice(d["community"])
		for _, n := range names {
			authorCount[n]++
			for _, c := range comms {
				acCount[[2]string{n, c}]++
			}
		}
		for _, label := range aboutLabels(d["about"]) {
			subjectCount[label]++
		}
	}
	authors = facetsFromCount(authorCount)
	subjects = facetsFromCount(subjectCount)
	for pair, w := range acCount {
		acs = append(acs, AuthorCommunity{Author: pair[0], Community: pair[1], Weight: w})
	}
	sort.SliceStable(acs, func(i, j int) bool {
		if acs[i].Author != acs[j].Author {
			return acs[i].Author < acs[j].Author
		}
		return acs[i].Community < acs[j].Community
	})
	return authors, subjects, acs
}

// filteredCount returns the total number of documents in a collection matching
// filterBy, via a per_page=0 search reading `found` (not truncated at a page
// size, unlike vizSearch).
func (cfg VizConfig) filteredCount(ctx context.Context, collection, filterBy string) (int, error) {
	u := tsSearchURL(cfg.Host, collection, url.Values{
		"per_page":  {"0"},
		"filter_by": {filterBy},
	})
	body, err := tsGet(ctx, u, cfg.ApiKey)
	if err != nil {
		return 0, err
	}
	var parsed struct {
		Found int `json:"found"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return 0, err
	}
	return parsed.Found, nil
}

// pagedSearch pages a q=* search (per_page=250) up to maxDocs and returns the
// raw documents. Stops on the first error, empty page, or short page.
func pagedSearch(ctx context.Context, host, apiKey, collection, filterBy, includeFields string, maxDocs int) []VizDoc {
	const perPage = 250
	var out []VizDoc
	for page := 1; len(out) < maxDocs; page++ {
		extra := url.Values{
			"per_page": {strconv.Itoa(perPage)},
			"page":     {strconv.Itoa(page)},
		}
		if filterBy != "" {
			extra.Set("filter_by", filterBy)
		}
		if includeFields != "" {
			extra.Set("include_fields", includeFields)
		}
		body, err := tsGet(ctx, tsSearchURL(host, collection, extra), apiKey)
		if err != nil {
			break
		}
		var parsed struct {
			Hits []struct {
				Document VizDoc `json:"document"`
			} `json:"hits"`
		}
		if json.Unmarshal(body, &parsed) != nil || len(parsed.Hits) == 0 {
			break
		}
		for _, h := range parsed.Hits {
			out = append(out, h.Document)
		}
		if len(parsed.Hits) < perPage {
			break
		}
	}
	return out
}

// tkItemFromDoc maps a transferkiosk Typesense doc to a TKItem. The doc `id` is
// `<pubkey>:<d>` (no kind prefix); the graph's part_of edges reference the
// kind-prefixed coord (`<eventKind>:<pubkey>:<d>`, matching the `a`-tag partOf
// value), so Coord is rebuilt with the eventKind prefix.
func tkItemFromDoc(d VizDoc) TKItem {
	id, _ := d["id"].(string)
	name, _ := d["name"].(string)
	pub, _ := d["publisher"].(string)
	kind := 0
	if k, ok := d["eventKind"].(float64); ok {
		kind = int(k)
	}
	parent, _ := d["partOf"].(string)
	community := ""
	if cs := stringSlice(d["community"]); len(cs) > 0 {
		community = cs[0]
	}
	coord := id
	if kind > 0 && id != "" {
		coord = fmt.Sprintf("%d:%s", kind, id)
	}
	return TKItem{Coord: coord, Kind: kind, Label: name, Author: pub, ParentCoord: parent, Community: community}
}

// publisherNames extracts the `name` of each publisher object in an AMB doc's
// publisher[] field.
func publisherNames(v any) []string {
	arr, _ := v.([]any)
	var out []string
	for _, e := range arr {
		if m, ok := e.(map[string]any); ok {
			if n, _ := m["name"].(string); n != "" {
				out = append(out, n)
			}
		}
	}
	return out
}

// aboutLabels extracts a display label (prefLabel.de, falling back to id) for
// each about object in an AMB doc's about[] field.
func aboutLabels(v any) []string {
	arr, _ := v.([]any)
	var out []string
	for _, e := range arr {
		m, ok := e.(map[string]any)
		if !ok {
			continue
		}
		label := ""
		if pl, ok := m["prefLabel"].(map[string]any); ok {
			if de, ok := pl["de"].(string); ok {
				label = de
			}
		}
		if label == "" {
			label, _ = m["id"].(string)
		}
		if label != "" {
			out = append(out, label)
		}
	}
	return out
}

// stringSlice coerces a JSON any into a []string, dropping non-string/empty.
func stringSlice(v any) []string {
	arr, _ := v.([]any)
	var out []string
	for _, e := range arr {
		if s, ok := e.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}

// facetsFromCount turns a value→count map into FacetValues sorted by count
// desc, then value asc (stable, deterministic).
func facetsFromCount(m map[string]int) []FacetValue {
	out := make([]FacetValue, 0, len(m))
	for k, c := range m {
		out = append(out, FacetValue{Value: k, Count: c})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Value < out[j].Value
	})
	return out
}

// shortLabel trims a pubkey to a compact display label.
func shortLabel(s string) string {
	if len(s) > 12 {
		return s[:8] + "…"
	}
	return s
}

// tsEscape wraps a filter_by value in backticks (stripping embedded backticks),
// so values containing ':' or spaces are treated literally by Typesense.
func tsEscape(v string) string {
	return "`" + strings.ReplaceAll(v, "`", "") + "`"
}
