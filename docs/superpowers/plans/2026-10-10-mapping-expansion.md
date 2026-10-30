# Concept Filter Expansion Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A concept filter in an amb-relay query (e.g. KIM educationalLevel `level_2`) also matches resources tagged with concepts mapped to it via trusted NIP-VOCAB concordances (e.g. EKW Klassenstufe `5-6`).

**Architecture:** The vocabs repo publishes the B1–B3 crosswalks. nostrlib indexes every concept id of a resource into one `concepts` field. amb-relay loads concordances, mappings and scheme hierarchies into an in-memory graph (`MappingRegistry`). Before each REQ or COUNT, an expander rewrites concept filters into synthetic `#concepts_<n>` tag filters holding the expanded id sets. TagFields maps these keys onto `concepts`, so each filter stays its own `&&` clause, ORed inside.

**Tech Stack:** Go 1.25 (amb-relay, nostrlib fork `fiatjaf.com/nostr` = `git.edufeed.org/edufeed/nostrlib`), Typesense 28, Node/vitest (vocabs repo), `golang.org/x/text/unicode/norm`.

**Spec:** `docs/superpowers/specs/2026-10-10-mapping-expansion-design.md` (amb-relay, incl. its "Implementation amendments" section).

## Global Constraints

- Default relations: `exactMatch` (transitive), `closeMatch` (one hop from the exact closure), everything narrower (mappings in the narrowing direction + in-scheme hierarchy, transitive, each reached concept taking its exact closure). `relatedMatch` only with `expand:related`, one hop from the exact closure. Never add a broader concept.
- Caps: depth 4 for the narrowing step, 200 identifiers per expanded value (`vocabmap.MaxDepth = 4`, `vocabmap.MaxResults = 200`).
- Only 1:1 mappings (`len(to) == 1`) feed the graph.
- Conflict rule: a pair that is `exactMatch` and also broad/narrow/related within the trust set contributes no exact edge.
- Env: `EXPAND_CONCORDANCES` (default empty), `EXPAND_READ_RELAYS` (default `wss://relay.edufeed.org`), `EXPAND_REFRESH_INTERVAL` (default `10m`), `EXPAND_DEBUG` (default `false`).
- NIP-50 tokens: `expand:off`, `expand:related`, `expand:<naddr1…>` (kind 39741 only), `expand:<npub1…>`. Values are bech32 only. `expand:` tokens are always stripped from `search`, even when expansion is disabled.
- Expansion never fails closed: on any error the query runs unexpanded.
- Concept identifiers are compared verbatim. No http/https or trailing-slash normalization.
- Expansion applies only when `filter.Kinds` is non-empty and every kind is 30142.
- No push, merge, publish or deploy without the user's explicit go. These are marked **GATE** below.
- Commit messages end with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.

## Review Focus

1. **Free text plus a concept filter on chunked AMB.** The rewritten filter must not be routed to chunk-rerank: its `filter.Matches` post-filter would drop every hit, because events carry no `concepts_<n>` tag. Test in Task 7 (`rerankOwns`).
2. **Unresolvable `expand:<naddr>` or a dead read relay.** The query must return unexpanded results within ~3 s, and later queries must not pay the timeout again for a minute. Test in Task 6 (negative cache).
3. **A refresh where the relay returns nothing** (connection dropped, so empty and not an error). It must not wipe a good graph. Test in Task 6.
4. **`about.id:X about.name:Y` in one search.** These are OR semantics within the `about` base, so they must not be rewritten into an ANDed clause. Test in Task 7.
5. **Quoted phrases and unknown tokens in `search`.** They must survive the `expand:` stripping and field rewriting verbatim. Test in Task 7.

## Repositories and workspaces

| Repo | Workspace | Branch |
|---|---|---|
| vocabs | `/home/laoc/coding/edufeed/vocabs` (main checkout, clean) | `feat/level-crosswalks` |
| nostrlib | `/home/laoc/coding/edufeed/nostrlib.worktrees/concepts-field` (new worktree) | `feat/concepts-field` from `edufeed-git/edufeed` |
| amb-relay | `/home/laoc/coding/edufeed/amb-relay.worktrees/mapping-expansion` (exists, has the spec) | `mapping-expansion` |

Never touch the main checkouts of nostrlib (detached HEAD, used by other worktrees) or amb-relay (holds the user's untracked plan files).

---

### Task 1: B1–B3 crosswalk data (vocabs)

**Files:**
- Create: `mappings/klassenstufen--educational-level.json`
- Create: `mappings/schulart--educational-level.json`
- Modify: `mappings/ekw-lrt--hcrt.json` (append 13 rows)
- Modify: `test/build-mappings.test.mjs:11-17`
- Modify: `test/fix.test.mjs:22-23`

**Interfaces:**
- Consumes: the existing mapping file format `{ concordance: { d, titles, fromScheme, toScheme, license }, rows: [{ sourceD, marker, targetD }] }`; concept d-tags as published. KIM levels have d `level_0…level_4` and `level_a` (lowercase; the `i` is `…/level_A`).
- Produces: 7 concordances and 74 rows (33 existing + 9 + 19 + 13).

- [ ] **Step 1: Branch**

```bash
cd /home/laoc/coding/edufeed/vocabs && git checkout -b feat/level-crosswalks
```

- [ ] **Step 2: Update the count tests first (they must fail)**

In `test/build-mappings.test.mjs`, change the first test to:

```js
  it('loads the ported, rename and level/lrt crosswalk rows in seven concordances', () => {
    const files = loadMappings(ROOT);
    expect(files.map((f) => f.concordance.d)).toEqual([
      'ekw-fach--schulfaecher', 'ekw-keywords--konfi-themen', 'ekw-lrt--hcrt',
      'klassenstufen--educational-level', 'konfi-lernformat--konfi-methode',
      'methode--konfi-methode', 'schulart--educational-level'
    ]);
    expect(files.reduce((n, f) => n + f.rows.length, 0)).toBe(74);
  });
```

(Keep the existing variable names of that file. If it uses a different root constant than `ROOT`, use that one.)

In `test/fix.test.mjs`, change the pinned summary to `{ create: 89, update: 385, unchanged: 634, orphan: 0 }`. Also add this comment above it: `// 89 = 8 restored concepts + 7 concordances + 74 mappings (none on the pre-fix relay).`

Run: `pnpm vitest run test/build-mappings.test.mjs test/fix.test.mjs`
Expected: FAIL (5 concordances / 33 rows / create 46).

- [ ] **Step 3: Create `mappings/klassenstufen--educational-level.json`**

```json
{
  "concordance": {
    "d": "klassenstufen--educational-level",
    "titles": [{ "value": "Klassenstufen ↔ KIM Bildungsstufe", "lang": "de" }],
    "fromScheme": "klassenstufen",
    "toScheme": "educational-level",
    "license": "CC0-1.0"
  },
  "rows": [
    { "sourceD": "1", "marker": "broadMatch", "targetD": "level_1" },
    { "sourceD": "2", "marker": "broadMatch", "targetD": "level_1" },
    { "sourceD": "3", "marker": "broadMatch", "targetD": "level_1" },
    { "sourceD": "4", "marker": "broadMatch", "targetD": "level_1" },
    { "sourceD": "5-6", "marker": "broadMatch", "targetD": "level_2" },
    { "sourceD": "7-8", "marker": "broadMatch", "targetD": "level_2" },
    { "sourceD": "9-10", "marker": "broadMatch", "targetD": "level_2" },
    { "sourceD": "ef", "marker": "broadMatch", "targetD": "level_3" },
    { "sourceD": "q1-q2", "marker": "broadMatch", "targetD": "level_3" }
  ]
}
```

- [ ] **Step 4: Create `mappings/schulart--educational-level.json`**

```json
{
  "concordance": {
    "d": "schulart--educational-level",
    "titles": [{ "value": "Schulart ↔ KIM Bildungsstufe", "lang": "de" }],
    "fromScheme": "schulart",
    "toScheme": "educational-level",
    "license": "CC0-1.0"
  },
  "rows": [
    { "sourceD": "elementar", "marker": "exactMatch", "targetD": "level_0" },
    { "sourceD": "krippe", "marker": "broadMatch", "targetD": "level_0" },
    { "sourceD": "kita", "marker": "broadMatch", "targetD": "level_0" },
    { "sourceD": "hort", "marker": "relatedMatch", "targetD": "level_1" },
    { "sourceD": "grundschule", "marker": "broadMatch", "targetD": "level_1" },
    { "sourceD": "hauptschule", "marker": "broadMatch", "targetD": "level_2" },
    { "sourceD": "realschule", "marker": "broadMatch", "targetD": "level_2" },
    { "sourceD": "gymnasium", "marker": "relatedMatch", "targetD": "level_2" },
    { "sourceD": "gymnasium", "marker": "relatedMatch", "targetD": "level_3" },
    { "sourceD": "gesamtschule", "marker": "relatedMatch", "targetD": "level_2" },
    { "sourceD": "gesamtschule", "marker": "relatedMatch", "targetD": "level_3" },
    { "sourceD": "gemeinschaftsschule", "marker": "relatedMatch", "targetD": "level_2" },
    { "sourceD": "gemeinschaftsschule", "marker": "relatedMatch", "targetD": "level_3" },
    { "sourceD": "foerderschule", "marker": "relatedMatch", "targetD": "level_1" },
    { "sourceD": "foerderschule", "marker": "relatedMatch", "targetD": "level_2" },
    { "sourceD": "berufliche-schule", "marker": "broadMatch", "targetD": "level_3" },
    { "sourceD": "universitaere-lehre", "marker": "broadMatch", "targetD": "level_a" },
    { "sourceD": "fachhochschule", "marker": "closeMatch", "targetD": "level_a" },
    { "sourceD": "berufliche-schule", "marker": "relatedMatch", "targetD": "level_4" }
  ]
}
```

- [ ] **Step 5: Append the 13 B3 rows to `mappings/ekw-lrt--hcrt.json`**

Append these objects to the end of its `rows` array (keep the existing rows unchanged):

```json
    { "sourceD": "textbaustein-zum-wiederverwenden", "marker": "broadMatch", "targetD": "text" },
    { "sourceD": "biblische-geschichte", "marker": "broadMatch", "targetD": "text" },
    { "sourceD": "erzahlung-geschichte", "marker": "broadMatch", "targetD": "text" },
    { "sourceD": "quelle", "marker": "relatedMatch", "targetD": "text" },
    { "sourceD": "bebilderte-geschichte", "marker": "relatedMatch", "targetD": "image" },
    { "sourceD": "interaktives-medium/lernpfad", "marker": "relatedMatch", "targetD": "course" },
    { "sourceD": "interaktives-medium/adaptives-lernangebot", "marker": "relatedMatch", "targetD": "application" },
    { "sourceD": "interaktives-medium/virtual-reality", "marker": "relatedMatch", "targetD": "simulation" },
    { "sourceD": "interaktives-medium/augmented-reality", "marker": "relatedMatch", "targetD": "application" },
    { "sourceD": "spiel", "marker": "closeMatch", "targetD": "educational_game" },
    { "sourceD": "stationenlernen", "marker": "relatedMatch", "targetD": "lesson_plan" },
    { "sourceD": "bild/mal-und-bastelvorlage", "marker": "relatedMatch", "targetD": "worksheet" },
    { "sourceD": "ubung-lernkontrolle/selbsttest-aufgabe", "marker": "relatedMatch", "targetD": "drill_and_practice" }
```

- [ ] **Step 6: Run the whole suite**

Run: `pnpm test`
Expected: all tests pass, including the offline guards (the concepts exist, no duplicate mapping d).

If another test pins a count derived from the mapping data, update it to the new data and note it in the report. The new values are: 7 concordances, 74 mapping events. Never change a guard to make the data pass.

- [ ] **Step 7: Read-only plan against the relay**

Run: `pnpm plan` (it reads wss://relay.edufeed.org and publishes nothing)
Expected: `create 43` (2 concordances + 41 mappings) · `update 0` · `orphan 0`, no errors.

- [ ] **Step 8: Commit**

```bash
git add mappings test
git commit -m "data(mappings): Klassenstufen/Schulart → KIM level, remaining ekw-lrt → HCRT (B1–B3)

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

- [ ] **Step 9: GATE: publish (user go required)**

Stop and ask the user before anything leaves the machine: pushing the branch, merging it, and `pnpm apply --relays wss://relay.edufeed.org` (signs with `EDUFEED_PUBLISHER_NSEC`; never print it). After the apply, the re-plan must show `create 0 · update 0`.

---

### Task 2: `concepts` field (nostrlib)

**Files:**
- Modify: `eventstore/typesense30142/types.go` (`AMBMetadata`, after `Community`)
- Modify: `eventstore/typesense30142/typesense.go` (`DefaultSchema`, after `community`)
- Modify: `eventstore/typesense30142/nostr_amb.go` (`NostrToAMB` before `return amb, nil`, plus the new func `conceptIDs`)
- Test: `eventstore/typesense30142/concepts_test.go` (new)

**Interfaces:**
- Produces: `AMBMetadata.Concepts []string` (JSON `concepts`, omitempty); schema field `{Name: "concepts", Type: "string[]", Optional: true}`. `NostrToAMB` fills it, so both the live path (amb-relay `prepareBatch`) and reindex (`BatchUpsertEvents` → `PrepareBatch`) get it with no amb-relay change.

- [ ] **Step 1: Worktree**

```bash
cd /home/laoc/coding/edufeed/nostrlib && git fetch -q edufeed-git && git worktree add /home/laoc/coding/edufeed/nostrlib.worktrees/concepts-field -b feat/concepts-field edufeed-git/edufeed
cd /home/laoc/coding/edufeed/nostrlib.worktrees/concepts-field/eventstore/typesense30142
```

- [ ] **Step 2: Write the failing tests** (`concepts_test.go`)

```go
package typesense30142

import (
	"testing"

	"fiatjaf.com/nostr"
	"github.com/stretchr/testify/assert"
)

// TestNostrToAMB_Concepts locks the concepts field: every concept id of the
// resource (core vocab fields and ext concept facets), deduplicated in
// first-seen order, scalar ext facets excluded.
func TestNostrToAMB_Concepts(t *testing.T) {
	assert := assert.New(t)

	event := createTestEvent(nostr.Tags{
		{"d", "concepts"},
		{"name", "Concepts"},
		{"educationalLevel:id", "https://w3id.org/kim/educationalLevel/level_2"},
		{"about:id", "http://w3id.org/kim/schulfaecher/s1024"},
		{"about:id", "http://w3id.org/kim/schulfaecher/s1055"},
		{"learningResourceType:id", "https://w3id.org/kim/hcrt/worksheet"},
		{"audience:id", "http://purl.org/dcx/lrmi-vocabs/educationalAudienceRole/student"},
		{"ext:ekw:gradeLevel:id", "nostr:39738:pk:5-6"},
		{"ext:ekw:gradeLevel:prefLabel:de", "5–6"},
		{"ext:ekw:bibleReference", "Mt 5,1-12"},
		{"ext:ekw:schoolType:id", "https://w3id.org/kim/educationalLevel/level_2"},
	})

	amb, err := NostrToAMB(event)
	assert.NoError(err)
	assert.Equal([]string{
		"https://w3id.org/kim/educationalLevel/level_2",
		"http://w3id.org/kim/schulfaecher/s1024",
		"http://w3id.org/kim/schulfaecher/s1055",
		"https://w3id.org/kim/hcrt/worksheet",
		"http://purl.org/dcx/lrmi-vocabs/educationalAudienceRole/student",
		"nostr:39738:pk:5-6",
	}, amb.Concepts)
}

func TestNostrToAMB_NoConcepts(t *testing.T) {
	amb, err := NostrToAMB(createTestEvent(nostr.Tags{{"d", "plain"}, {"name", "Plain"}}))
	assert.NoError(t, err)
	assert.Nil(t, amb.Concepts)
}

func TestDefaultSchema_ConceptsField(t *testing.T) {
	for _, f := range DefaultSchema().Fields {
		if f.Name == "concepts" {
			assert.Equal(t, "string[]", f.Type)
			assert.True(t, f.Optional)
			assert.False(t, f.Facet)
			return
		}
	}
	t.Fatal("DefaultSchema has no concepts field")
}
```

The expected order is: the core fields in the order educationalLevel, about, learningResourceType, audience; then ext namespaces and facets sorted by name. `gradeLevel` sorts before `schoolType`, and the schoolType id duplicates level_2, so it is deduplicated away.

Run: `go test -run 'Concepts' ./...`
Expected: FAIL (`amb.Concepts undefined`).

- [ ] **Step 3: Implement**

`types.go`, after the `Community` field:

```go
	// Every concept identifier of the resource (educationalLevel, about,
	// learningResourceType, audience and ext concept facets), deduplicated.
	// One filterable field, so a concept filter expanded through vocabulary
	// mappings can match any of those fields with a single clause.
	Concepts []string `json:"concepts,omitempty"`
```

`typesense.go` `DefaultSchema`, after the `community` field:

```go
			// Concept identifiers across all vocab fields (see AMBMetadata.Concepts).
			{Name: "concepts", Type: "string[]", Optional: true},
```

`nostr_amb.go`: replace the final `return amb, nil` of `NostrToAMB` with:

```go
	amb.Concepts = conceptIDs(amb)

	return amb, nil
}

// conceptIDs collects the concept identifiers of a projected resource: the id
// of every core controlled-vocabulary value, then of every ext concept facet
// (namespaces and facets in sorted order). Scalar ext facets carry no id and
// are skipped. Deduplicated in first-seen order; nil when there are none.
func conceptIDs(amb *AMBMetadata) []string {
	var out []string
	seen := map[string]bool{}
	add := func(id string) {
		if id != "" && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	for _, v := range amb.EducationalLevel {
		if v != nil {
			add(v.ID)
		}
	}
	for _, v := range amb.About {
		if v != nil {
			add(v.ID)
		}
	}
	for _, v := range amb.LearningResourceType {
		if v != nil {
			add(v.ID)
		}
	}
	for _, v := range amb.Audience {
		if v != nil {
			add(v.ID)
		}
	}
	namespaces := make([]string, 0, len(amb.Ext))
	for ns := range amb.Ext {
		namespaces = append(namespaces, ns)
	}
	sort.Strings(namespaces)
	for _, ns := range namespaces {
		facets := make([]string, 0, len(amb.Ext[ns]))
		for facet := range amb.Ext[ns] {
			facets = append(facets, facet)
		}
		sort.Strings(facets)
		for _, facet := range facets {
			for _, value := range amb.Ext[ns][facet] {
				if m, ok := value.(map[string]interface{}); ok {
					if id, ok := m["id"].(string); ok {
						add(id)
					}
				}
			}
		}
	}
	return out
```

(Note the closing: the old `return amb, nil` and `}` are replaced by the block above, which ends `conceptIDs` with `return out` and its own `}`. Add `"sort"` to the imports if it is not already there.)

- [ ] **Step 4: Run tests**

Run: `go test ./...` (in `eventstore/typesense30142`)
Expected: PASS. Existing golden or schema tests that pin the full field list may need the new field added; update them and say so in the report.

- [ ] **Step 5: Integration test (skips without Typesense)**

Append to `concepts_test.go`:

```go
// TestConceptsAliasIntegration proves the query shape amb-relay's expander
// emits: several tag keys aliased onto `concepts` via TagFields stay separate
// && clauses, each ORing its values — across what were different fields.
func TestConceptsAliasIntegration(t *testing.T) {
	ts := getTestBackend(t)
	defer cleanupTestBackend(t, ts)
	ts.TagFields = map[string]string{"concepts_0": "concepts", "concepts_1": "concepts"}

	sk := nostr.Generate()
	mk := func(d string, tags ...nostr.Tag) nostr.Event {
		ev := nostr.Event{Kind: 30142, CreatedAt: nostr.Now(), Tags: append(nostr.Tags{{"d", d}, {"name", d}}, tags...)}
		ev.Sign(sk)
		return ev
	}
	a := mk("a", nostr.Tag{"educationalLevel:id", "L2"})
	b := mk("b", nostr.Tag{"ext:ekw:gradeLevel:id", "G56"}, nostr.Tag{"about:id", "S1"})
	c := mk("c", nostr.Tag{"about:id", "S1"})
	if _, errs := ts.BatchUpsertEvents([]nostr.Event{a, b, c}); len(errs) > 0 {
		t.Fatalf("upsert: %v", errs)
	}

	names := func(f nostr.Filter) []string {
		var out []string
		for ev := range ts.QueryEvents(f, 10) {
			out = append(out, ev.Tags.GetD())
		}
		sort.Strings(out)
		return out
	}
	or := nostr.Filter{Kinds: []nostr.Kind{30142}, Tags: nostr.TagMap{"concepts_0": {"L2", "G56"}}}
	assert.Equal(t, []string{"a", "b"}, names(or))
	and := nostr.Filter{Kinds: []nostr.Kind{30142}, Tags: nostr.TagMap{"concepts_0": {"L2", "G56"}, "concepts_1": {"S1"}}}
	assert.Equal(t, []string{"b"}, names(and))
	n, err := ts.CountEvents(and)
	assert.NoError(t, err)
	assert.Equal(t, uint32(1), n)
}
```

(Add `"sort"` to the test imports. If `ev.Tags.GetD()` is not available in this fork, read the `d` tag with `ev.Tags.Find("d")[1]`. Follow `search_integration_test.go` if it waits after upserting.)

Run, with Typesense: `docker compose -f docker-compose.test.yml up -d && TS_TEST_HOST=http://localhost:8108 TS_TEST_APIKEY=test-key go test -run TestConceptsAliasIntegration ./... ; docker compose -f docker-compose.test.yml down`
Expected: PASS. Without the env vars the test SKIPs.

- [ ] **Step 6: Commit**

```bash
git add -A && git commit -m "feat(typesense30142): concepts field with every concept id of a resource

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 3: Point amb-relay at the nostrlib branch; schema and TagFields wiring

**Files (amb-relay worktree):**
- Modify: `go.mod` / `go.sum` (temporary local replace)
- Create: `expand.go` (only `conceptTagPrefix`, `maxConceptFilters`, `conceptTagFields`, `hasConceptTags`, `ensureConceptsField` in this task)
- Modify: `main.go:189-197` (tsDB literal: `TagFields: conceptTagFields(),`), `main.go:213` and `reindex.go:118` (call `ensureConceptsField` right after `ensureContentFields`)
- Test: `expand_test.go` (new), `facet_filters_test.go` (one new test)

**Interfaces:**
- Consumes: `typesense30142.DefaultSchema()` with `concepts` (Task 2).
- Produces: `const conceptTagPrefix = "concepts_"`, `const maxConceptFilters = 8`, `func conceptTagFields() map[string]string`, `func hasConceptTags(f nostr.Filter) bool`, `func ensureConceptsField(schema *typesense30142.CollectionSchema)`.

- [ ] **Step 1: Local replace**

```bash
cd /home/laoc/coding/edufeed/amb-relay.worktrees/mapping-expansion
go mod edit -replace fiatjaf.com/nostr=/home/laoc/coding/edufeed/nostrlib.worktrees/concepts-field
go build ./...
```

Expected: builds. Task 9 swaps this for the published commit.

- [ ] **Step 2: Failing tests** (`expand_test.go`)

```go
package main

import (
	"testing"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/eventstore/typesense30142"
)

func TestConceptTagFields(t *testing.T) {
	m := conceptTagFields()
	if len(m) != maxConceptFilters {
		t.Fatalf("len = %d, want %d", len(m), maxConceptFilters)
	}
	if m["concepts_0"] != "concepts" || m["concepts_7"] != "concepts" {
		t.Fatalf("aliases = %v", m)
	}
}

func TestHasConceptTags(t *testing.T) {
	if hasConceptTags(nostr.Filter{Tags: nostr.TagMap{"about:id": {"x"}}}) {
		t.Fatal("about:id is not a synthetic concept tag")
	}
	if !hasConceptTags(nostr.Filter{Tags: nostr.TagMap{"concepts_3": {"x"}}}) {
		t.Fatal("concepts_3 is a synthetic concept tag")
	}
}

func TestEnsureConceptsField(t *testing.T) {
	s := typesense30142.CollectionSchema{Fields: []typesense30142.Field{{Name: "d", Type: "string"}}}
	ensureConceptsField(&s)
	ensureConceptsField(&s)
	n := 0
	for _, f := range s.Fields {
		if f.Name == "concepts" {
			n++
			if f.Type != "string[]" || !f.Optional {
				t.Fatalf("concepts field = %+v", f)
			}
		}
	}
	if n != 1 {
		t.Fatalf("concepts fields = %d, want 1", n)
	}
}
```

Append to `facet_filters_test.go`:

```go
// The AMB backend aliases the expander's synthetic tag keys onto `concepts`;
// the schema must declare that field or every expanded query fails.
func TestAMBConceptTagFieldsDeclared(t *testing.T) {
	assertTagFieldsDeclared(t, "DefaultSchema", typesense30142.DefaultSchema(), conceptTagFields())
}
```

Run: `go test -run 'Concept' ./...`
Expected: FAIL (undefined).

- [ ] **Step 3: Implement** (`expand.go`)

```go
package main

import (
	"fmt"
	"strings"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/eventstore/typesense30142"
)

// Concept filter expansion (spec: docs/superpowers/specs/2026-10-10-mapping-expansion-design.md).
// The expander rewrites each concept filter into a synthetic tag filter
// concepts_<n>; TagFields aliases every such key onto the one `concepts`
// field, so separate filters stay separate && clauses.
const (
	conceptTagPrefix  = "concepts_"
	maxConceptFilters = 8
)

func conceptTagFields() map[string]string {
	m := make(map[string]string, maxConceptFilters)
	for i := 0; i < maxConceptFilters; i++ {
		m[fmt.Sprintf("%s%d", conceptTagPrefix, i)] = "concepts"
	}
	return m
}

// hasConceptTags reports whether a filter carries synthetic concept tags.
// Events never carry them, so filter.Matches would reject every hit.
func hasConceptTags(f nostr.Filter) bool {
	for name := range f.Tags {
		if strings.HasPrefix(name, conceptTagPrefix) {
			return true
		}
	}
	return false
}

// ensureConceptsField adds `concepts` to a custom schema stored before the
// field existed (DefaultSchema already has it).
func ensureConceptsField(schema *typesense30142.CollectionSchema) {
	for _, f := range schema.Fields {
		if f.Name == "concepts" {
			return
		}
	}
	schema.Fields = append(schema.Fields, typesense30142.Field{Name: "concepts", Type: "string[]", Optional: true})
}
```

`main.go`: add `TagFields: conceptTagFields(),` to the `tsDB := typesense30142.TSBackend{…}` literal. After `ensureContentFields(&effectiveSchema)` add `ensureConceptsField(&effectiveSchema)`.
`reindex.go`: after `ensureContentFields(schema)` add `ensureConceptsField(schema)`.

- [ ] **Step 4: Run tests**

Run: `go vet ./... && go test ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add -A && git commit -m "feat(expand): concepts field wiring — TagFields aliases, schema guard

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 4: `vocabmap` parsing (amb-relay)

**Files:**
- Create: `vocabmap/parse.go`
- Create: `vocabmap/vocabmaptest/events.go`
- Test: `vocabmap/parse_test.go` (package `vocabmap_test`)
- Modify: `go.mod` (`golang.org/x/text` becomes a direct require via `go mod tidy`)

**Interfaces:**
- Produces (package `github.com/edufeed-org/amb-relay/vocabmap`):
  - `const KindScheme, KindConcept, KindMapping, KindConcordance nostr.Kind = 39737, 39738, 39740, 39741`
  - `type Concept struct { ID, Address string; Schemes, Broader, Narrower []string }`
  - `type Mapping struct { D, Relation, From string; To []string; FromScheme, ToScheme, PartOf, Author string; Schemes []string }`
  - `type Concordance struct { Address, Author, FromScheme, ToScheme string; Maintainers, Schemes []string }`
  - `func Address(ev nostr.Event) string`
  - `func MappingIdentifier(from string, to []string, relation string) string`
  - `func ParseConcept(ev nostr.Event) (Concept, bool)`, `func ParseMapping(ev nostr.Event) (Mapping, bool)`, `func ParseConcordance(ev nostr.Event) (Concordance, bool)`
  - `func Members(c Concordance, ms []Mapping) []Mapping`
  - `func LatestByAddress(evs []nostr.Event) []nostr.Event`
- Produces (package `…/vocabmap/vocabmaptest`): `Owner, Other, Maintainer nostr.PubKey`; `SchemeAddr(pk, d)`, `ConcordanceAddr(pk, d)`, `ID(pk, d)` strings; `Concept(pk, scheme, d, uri string, broader ...string) nostr.Event`; `Concordance(pk, d, fromScheme, toScheme string, schemeAddrs []string, maintainers ...nostr.PubKey) nostr.Event`; `Mapping(pk, from, relation, to, fromScheme, toScheme, partOf string, schemeAddrs ...string) nostr.Event`.

- [ ] **Step 1: Test helpers** (`vocabmap/vocabmaptest/events.go`)

```go
// Package vocabmaptest builds NIP-VOCAB events for tests.
package vocabmaptest

import (
	"fmt"

	"fiatjaf.com/nostr"
	"github.com/edufeed-org/amb-relay/vocabmap"
)

var (
	Owner      = nostr.GetPublicKey([32]byte{31: 1})
	Other      = nostr.GetPublicKey([32]byte{31: 2})
	Maintainer = nostr.GetPublicKey([32]byte{31: 3})
)

func SchemeAddr(pk nostr.PubKey, d string) string      { return fmt.Sprintf("39737:%s:%s", pk.Hex(), d) }
func ConcordanceAddr(pk nostr.PubKey, d string) string { return fmt.Sprintf("39741:%s:%s", pk.Hex(), d) }
func ID(pk nostr.PubKey, d string) string              { return fmt.Sprintf("nostr:39738:%s:%s", pk.Hex(), d) }

// Concept builds a 39738 in scheme; uri becomes its `i` tag when non-empty;
// broader are d-tags of concepts by the same author.
func Concept(pk nostr.PubKey, scheme, d, uri string, broader ...string) nostr.Event {
	tags := nostr.Tags{{"d", d}, {"a", SchemeAddr(pk, scheme), "wss://relay.example", "inScheme"}}
	for _, b := range broader {
		tags = append(tags, nostr.Tag{"a", fmt.Sprintf("39738:%s:%s", pk.Hex(), b), "wss://relay.example", "broader"})
	}
	if uri != "" {
		tags = append(tags, nostr.Tag{"i", uri})
	}
	return nostr.Event{Kind: vocabmap.KindConcept, PubKey: pk, CreatedAt: 1, Tags: tags}
}

func Concordance(pk nostr.PubKey, d, fromScheme, toScheme string, schemeAddrs []string, maintainers ...nostr.PubKey) nostr.Event {
	tags := nostr.Tags{{"d", d}, {"title", d, "de"}, {"fromScheme", fromScheme}, {"toScheme", toScheme}}
	for _, a := range schemeAddrs {
		tags = append(tags, nostr.Tag{"a", a})
	}
	for _, m := range maintainers {
		tags = append(tags, nostr.Tag{"p", m.Hex(), "", "maintainer"})
	}
	return nostr.Event{Kind: vocabmap.KindConcordance, PubKey: pk, CreatedAt: 1, Tags: tags}
}

// Mapping builds a valid 1:1 39740 (d = sameness identifier).
func Mapping(pk nostr.PubKey, from, relation, to, fromScheme, toScheme, partOf string, schemeAddrs ...string) nostr.Event {
	tags := nostr.Tags{
		{"d", vocabmap.MappingIdentifier(from, []string{to}, relation)},
		{"relation", relation}, {"from", from}, {"to", to},
		{"fromScheme", fromScheme}, {"toScheme", toScheme},
	}
	for _, a := range schemeAddrs {
		tags = append(tags, nostr.Tag{"a", a, "wss://relay.example"})
	}
	if partOf != "" {
		tags = append(tags, nostr.Tag{"a", partOf, "", "partOf"})
	}
	return nostr.Event{Kind: vocabmap.KindMapping, PubKey: pk, CreatedAt: 1, Tags: tags}
}
```

- [ ] **Step 2: Failing tests** (`vocabmap/parse_test.go`)

```go
package vocabmap_test

import (
	"reflect"
	"testing"

	"fiatjaf.com/nostr"
	"github.com/edufeed-org/amb-relay/vocabmap"
	vt "github.com/edufeed-org/amb-relay/vocabmap/vocabmaptest"
)

// Vectors computed with nostr-vocab-core 0.3.1 mappingIdentifier (the JS
// reference implementation of the NIP-VOCAB sameness identifier).
func TestMappingIdentifierVectors(t *testing.T) {
	cases := []struct {
		from, rel string
		to        []string
		want      string
	}{
		{"https://example.org/a", "exactMatch", []string{"https://example.org/b"},
			"mapping:1a14a04c5ab1c27eafc07b372852f727bf73f5332da3536ed4c18ee7db254960"},
		{"nostr:39738:pk:5-6", "broadMatch", []string{"https://w3id.org/kim/educationalLevel/level_2"},
			"mapping:b7f659c41777195ddfb0024fa49fcaa37b278f250049606e84f2e57d61eed241"},
		// decomposed é (NFD) + duplicate and unsorted targets
		{"https://x/Café", "closeMatch", []string{"https://y/z", "https://y/b", "https://y/z"},
			"mapping:68c8b1a4a104ebe7319644a3e9eff24fd576da1f8f4d70fcea3adda553bf5c7d"},
	}
	for _, c := range cases {
		if got := vocabmap.MappingIdentifier(c.from, c.to, c.rel); got != c.want {
			t.Errorf("MappingIdentifier(%q,%v,%q) = %s, want %s", c.from, c.to, c.rel, got, c.want)
		}
	}
}

func TestParseMapping(t *testing.T) {
	part := vt.ConcordanceAddr(vt.Owner, "c")
	ev := vt.Mapping(vt.Owner, "A", "broadMatch", "B", "S1", "S2", part, vt.SchemeAddr(vt.Owner, "s1"))
	m, ok := vocabmap.ParseMapping(ev)
	if !ok {
		t.Fatal("valid mapping rejected")
	}
	want := vocabmap.Mapping{D: ev.Tags.Find("d")[1], Relation: "broadMatch", From: "A", To: []string{"B"},
		FromScheme: "S1", ToScheme: "S2", PartOf: part, Author: vt.Owner.Hex(), Schemes: []string{vt.SchemeAddr(vt.Owner, "s1")}}
	if !reflect.DeepEqual(m, want) {
		t.Fatalf("got %+v\nwant %+v", m, want)
	}

	bad := ev
	bad.Tags = append(nostr.Tags{}, ev.Tags...)
	bad.Tags[0] = nostr.Tag{"d", "mapping:wrong"}
	if _, ok := vocabmap.ParseMapping(bad); ok {
		t.Error("d != sameness identifier must be rejected")
	}
	twoRel := ev
	twoRel.Tags = append(append(nostr.Tags{}, ev.Tags...), nostr.Tag{"relation", "exactMatch"})
	if _, ok := vocabmap.ParseMapping(twoRel); ok {
		t.Error("two relation tags must be rejected")
	}
	unknown := vt.Mapping(vt.Owner, "A", "sameAs", "B", "S1", "S2", "")
	unknown.Tags[0] = nostr.Tag{"d", vocabmap.MappingIdentifier("A", []string{"B"}, "exactMatch")}
	if _, ok := vocabmap.ParseMapping(unknown); ok {
		t.Error("unknown relation must be rejected")
	}
}

func TestParseConcept(t *testing.T) {
	c, ok := vocabmap.ParseConcept(vt.Concept(vt.Owner, "schulart", "berufsschule", "", "berufliche-schule"))
	if !ok {
		t.Fatal("rejected")
	}
	if c.ID != vt.ID(vt.Owner, "berufsschule") || c.Address != "39738:"+vt.Owner.Hex()+":berufsschule" {
		t.Errorf("id/address = %q %q", c.ID, c.Address)
	}
	if !reflect.DeepEqual(c.Broader, []string{"39738:" + vt.Owner.Hex() + ":berufliche-schule"}) ||
		!reflect.DeepEqual(c.Schemes, []string{vt.SchemeAddr(vt.Owner, "schulart")}) {
		t.Errorf("broader/schemes = %v %v", c.Broader, c.Schemes)
	}
	withURI, _ := vocabmap.ParseConcept(vt.Concept(vt.Owner, "educational-level", "level_a", "https://w3id.org/kim/educationalLevel/level_A"))
	if withURI.ID != "https://w3id.org/kim/educationalLevel/level_A" {
		t.Errorf("i tag must win: %q", withURI.ID)
	}
}

func TestMembers(t *testing.T) {
	addr := vt.ConcordanceAddr(vt.Owner, "c")
	conc, ok := vocabmap.ParseConcordance(vt.Concordance(vt.Owner, "c", "S1", "S2", nil, vt.Maintainer))
	if !ok || conc.Address != addr || !reflect.DeepEqual(conc.Maintainers, []string{vt.Maintainer.Hex()}) {
		t.Fatalf("concordance = %+v ok=%v", conc, ok)
	}
	parse := func(ev nostr.Event) vocabmap.Mapping { m, _ := vocabmap.ParseMapping(ev); return m }
	owner := parse(vt.Mapping(vt.Owner, "A", "exactMatch", "B", "S1", "S2", addr))
	maint := parse(vt.Mapping(vt.Maintainer, "C", "exactMatch", "D", "S1", "S2", addr))
	stranger := parse(vt.Mapping(vt.Other, "E", "exactMatch", "F", "S1", "S2", addr))
	wrongScheme := parse(vt.Mapping(vt.Owner, "G", "exactMatch", "H", "S1", "S9", addr))
	otherConc := parse(vt.Mapping(vt.Owner, "I", "exactMatch", "J", "S1", "S2", vt.ConcordanceAddr(vt.Owner, "x")))
	got := vocabmap.Members(conc, []vocabmap.Mapping{owner, maint, stranger, wrongScheme, otherConc, owner})
	if !reflect.DeepEqual(got, []vocabmap.Mapping{owner, maint}) {
		t.Fatalf("members = %+v", got)
	}
}

func TestLatestByAddress(t *testing.T) {
	old := vt.Concept(vt.Owner, "s", "a", "")
	newer := vt.Concept(vt.Owner, "s", "a", "https://new")
	newer.CreatedAt = 2
	other := vt.Concept(vt.Owner, "s", "b", "")
	got := vocabmap.LatestByAddress([]nostr.Event{old, other, newer})
	if len(got) != 2 || got[0].Tags.Find("i")[1] != "https://new" || got[1].Tags.Find("d")[1] != "b" {
		t.Fatalf("latest = %+v", got)
	}
}
```

Run: `go test ./vocabmap/...`
Expected: FAIL (package does not exist / undefined).

- [ ] **Step 3: Implement** (`vocabmap/parse.go`)

```go
// Package vocabmap reads NIP-VOCAB concepts (39738), standalone mappings
// (39740) and concordances (39741), and expands a concept identifier through
// them (graph.go). It does no I/O; amb-relay's MappingRegistry feeds it.
package vocabmap

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"fiatjaf.com/nostr"
	"golang.org/x/text/unicode/norm"
)

const (
	KindScheme      nostr.Kind = 39737
	KindConcept     nostr.Kind = 39738
	KindMapping     nostr.Kind = 39740
	KindConcordance nostr.Kind = 39741

	skosNS = "http://www.w3.org/2004/02/skos/core#"
)

var relations = map[string]bool{
	"exactMatch": true, "closeMatch": true, "broadMatch": true, "narrowMatch": true, "relatedMatch": true,
}

// Concept is a 39738's identifier (its `i` URI, else nostr:<address>), its
// address, the scheme addresses it belongs to and its hierarchy links
// (concept addresses).
type Concept struct {
	ID       string
	Address  string
	Schemes  []string
	Broader  []string
	Narrower []string
}

// Mapping is a valid 39740. Schemes are the 39737 addresses among its a tags.
type Mapping struct {
	D          string
	Relation   string
	From       string
	To         []string
	FromScheme string
	ToScheme   string
	PartOf     string
	Author     string
	Schemes    []string
}

// Concordance is a 39741. Schemes are the 39737 addresses among its a tags.
type Concordance struct {
	Address     string
	Author      string
	FromScheme  string
	ToScheme    string
	Maintainers []string
	Schemes     []string
}

func first(ev nostr.Event, name string) (string, bool) {
	for _, t := range ev.Tags {
		if len(t) >= 2 && t[0] == name {
			return t[1], true
		}
	}
	return "", false
}

// single returns the value of a tag that occurs exactly once.
func single(ev nostr.Event, name string) (string, bool) {
	value, n := "", 0
	for _, t := range ev.Tags {
		if len(t) >= 2 && t[0] == name {
			value = t[1]
			n++
		}
	}
	return value, n == 1
}

// Address is kind:pubkey:d of an addressable event.
func Address(ev nostr.Event) string {
	d, _ := first(ev, "d")
	return fmt.Sprintf("%d:%s:%s", ev.Kind, ev.PubKey.Hex(), d)
}

func uniqueNFC(values []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range values {
		if v == "" {
			continue
		}
		v = norm.NFC.String(v)
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

// canonicalSet: NFC, deduplicated, code-point sorted (byte order of UTF-8 is
// code-point order), joined with "|".
func canonicalSet(values []string) string {
	set := uniqueNFC(values)
	sort.Strings(set)
	return strings.Join(set, "|")
}

// MappingIdentifier is the NIP-VOCAB mapping d: the TS4NFDI sameness
// identifier over subject {from}, predicate skos:<relation>, objects to.
func MappingIdentifier(from string, to []string, relation string) string {
	input := canonicalSet([]string{from}) + " " + skosNS + relation + " " + canonicalSet(to)
	sum := sha256.Sum256([]byte(input))
	return "mapping:" + hex.EncodeToString(sum[:])
}

func ParseConcept(ev nostr.Event) (Concept, bool) {
	if ev.Kind != KindConcept {
		return Concept{}, false
	}
	if d, ok := first(ev, "d"); !ok || d == "" {
		return Concept{}, false
	}
	c := Concept{Address: Address(ev)}
	if i, ok := first(ev, "i"); ok && i != "" {
		c.ID = norm.NFC.String(i)
	} else {
		c.ID = "nostr:" + c.Address
	}
	for _, t := range ev.Tags {
		if len(t) < 2 || t[0] != "a" {
			continue
		}
		marker := ""
		if len(t) >= 4 {
			marker = t[3]
		}
		switch {
		case strings.HasPrefix(t[1], "39737:"):
			c.Schemes = append(c.Schemes, t[1])
		case strings.HasPrefix(t[1], "39738:") && marker == "broader":
			c.Broader = append(c.Broader, t[1])
		case strings.HasPrefix(t[1], "39738:") && marker == "narrower":
			c.Narrower = append(c.Narrower, t[1])
		}
	}
	return c, true
}

// ParseMapping validates a 39740 like nostr-vocab-core parseMapping: single
// d/relation/from/fromScheme/toScheme, known relation, ≥1 to, and d equal to
// the recomputed sameness identifier.
func ParseMapping(ev nostr.Event) (Mapping, bool) {
	if ev.Kind != KindMapping {
		return Mapping{}, false
	}
	d, ok1 := single(ev, "d")
	rel, ok2 := single(ev, "relation")
	from, ok3 := single(ev, "from")
	fromScheme, ok4 := single(ev, "fromScheme")
	toScheme, ok5 := single(ev, "toScheme")
	if !(ok1 && ok2 && ok3 && ok4 && ok5) || d == "" || from == "" || fromScheme == "" || toScheme == "" || !relations[rel] {
		return Mapping{}, false
	}
	var rawTo []string
	for _, t := range ev.Tags {
		if len(t) >= 2 && t[0] == "to" {
			rawTo = append(rawTo, t[1])
		}
	}
	to := uniqueNFC(rawTo)
	if len(to) == 0 || MappingIdentifier(from, to, rel) != d {
		return Mapping{}, false
	}
	m := Mapping{D: d, Relation: rel, From: norm.NFC.String(from), To: to,
		FromScheme: fromScheme, ToScheme: toScheme, Author: ev.PubKey.Hex()}
	for _, t := range ev.Tags {
		if len(t) < 2 || t[0] != "a" {
			continue
		}
		if len(t) >= 4 && t[3] == "partOf" {
			if m.PartOf == "" {
				m.PartOf = t[1]
			}
		} else if strings.HasPrefix(t[1], "39737:") {
			m.Schemes = append(m.Schemes, t[1])
		}
	}
	return m, true
}

func ParseConcordance(ev nostr.Event) (Concordance, bool) {
	if ev.Kind != KindConcordance {
		return Concordance{}, false
	}
	d, _ := first(ev, "d")
	fromScheme, _ := first(ev, "fromScheme")
	toScheme, _ := first(ev, "toScheme")
	if d == "" || fromScheme == "" || toScheme == "" {
		return Concordance{}, false
	}
	c := Concordance{Address: Address(ev), Author: ev.PubKey.Hex(), FromScheme: fromScheme, ToScheme: toScheme}
	for _, t := range ev.Tags {
		switch {
		case len(t) >= 4 && t[0] == "p" && t[3] == "maintainer":
			c.Maintainers = append(c.Maintainers, t[1])
		case len(t) >= 2 && t[0] == "a" && strings.HasPrefix(t[1], "39737:"):
			c.Schemes = append(c.Schemes, t[1])
		}
	}
	return c, true
}

// Members are the mappings that belong to c: partOf c, authored by its owner
// or a maintainer, with matching schemes; first occurrence per d wins.
// Callers pass only the latest event per address.
func Members(c Concordance, ms []Mapping) []Mapping {
	allowed := map[string]bool{c.Author: true}
	for _, m := range c.Maintainers {
		allowed[m] = true
	}
	seen := map[string]bool{}
	var out []Mapping
	for _, m := range ms {
		if m.PartOf != c.Address || !allowed[m.Author] || m.FromScheme != c.FromScheme || m.ToScheme != c.ToScheme || seen[m.D] {
			continue
		}
		seen[m.D] = true
		out = append(out, m)
	}
	return out
}

// LatestByAddress keeps the newest event per address (ties: larger id),
// sorted by address.
func LatestByAddress(evs []nostr.Event) []nostr.Event {
	best := map[string]nostr.Event{}
	for _, ev := range evs {
		a := Address(ev)
		prev, ok := best[a]
		if !ok || ev.CreatedAt > prev.CreatedAt || (ev.CreatedAt == prev.CreatedAt && ev.ID.Hex() > prev.ID.Hex()) {
			best[a] = ev
		}
	}
	keys := make([]string, 0, len(best))
	for a := range best {
		keys = append(keys, a)
	}
	sort.Strings(keys)
	out := make([]nostr.Event, 0, len(keys))
	for _, a := range keys {
		out = append(out, best[a])
	}
	return out
}
```

Run: `go mod tidy && go test ./vocabmap/...`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add -A && git commit -m "feat(vocabmap): parse NIP-VOCAB concepts, mappings, concordances

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 5: `vocabmap` graph and `Expand`

**Files:**
- Create: `vocabmap/graph.go`
- Test: `vocabmap/graph_test.go` (package `vocabmap_test`)

**Interfaces:**
- Consumes: `Concept`, `Mapping` (Task 4).
- Produces: `const MaxDepth = 4`, `const MaxResults = 200`, `type Graph`, `func NewGraph(concepts []Concept, mappings []Mapping) *Graph`, `func (g *Graph) Expand(id string, related bool) (ids []string, truncated bool)`. `ids[0] == id` always.

- [ ] **Step 1: Failing tests** (`vocabmap/graph_test.go`)

```go
package vocabmap_test

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/edufeed-org/amb-relay/vocabmap"
)

func mp(from, rel, to string) vocabmap.Mapping {
	return vocabmap.Mapping{From: from, Relation: rel, To: []string{to}}
}

func expand(t *testing.T, g *vocabmap.Graph, id string, related bool, want ...string) {
	t.Helper()
	got, _ := g.Expand(id, related)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Expand(%s, related=%v) = %v, want %v", id, related, got, want)
	}
}

func TestExpandNarrowingDirection(t *testing.T) {
	g := vocabmap.NewGraph(nil, []vocabmap.Mapping{mp("G56", "broadMatch", "L2"), mp("L2", "narrowMatch", "HS")})
	expand(t, g, "L2", false, "L2", "G56", "HS")
	expand(t, g, "G56", false, "G56") // never broader
	expand(t, g, "HS", false, "HS")
}

func TestExpandExactTransitiveCloseOneHop(t *testing.T) {
	g := vocabmap.NewGraph(nil, []vocabmap.Mapping{
		mp("A", "exactMatch", "B"), mp("B", "exactMatch", "C"),
		mp("C", "closeMatch", "D"), mp("D", "closeMatch", "E"),
	})
	expand(t, g, "A", false, "A", "B", "C", "D") // E is two close hops away
}

func TestExpandInSchemeHierarchyWithURIs(t *testing.T) {
	parent := vocabmap.Concept{ID: "https://p", Address: "39738:pk:p"}
	child := vocabmap.Concept{ID: "nostr:39738:pk:c", Address: "39738:pk:c", Broader: []string{"39738:pk:p"}}
	grandchild := vocabmap.Concept{ID: "nostr:39738:pk:g", Address: "39738:pk:g", Broader: []string{"39738:pk:c"}}
	g := vocabmap.NewGraph([]vocabmap.Concept{parent, child, grandchild}, []vocabmap.Mapping{mp("L3", "narrowMatch", "https://p")})
	expand(t, g, "L3", false, "L3", "https://p", "nostr:39738:pk:c", "nostr:39738:pk:g")
}

func TestExpandNarrowerTakesExactClosure(t *testing.T) {
	g := vocabmap.NewGraph(nil, []vocabmap.Mapping{mp("G56", "broadMatch", "L2"), mp("G56", "exactMatch", "X")})
	expand(t, g, "L2", false, "L2", "G56", "X")
}

func TestExpandRelatedOnlyOnRequestAndNotChained(t *testing.T) {
	g := vocabmap.NewGraph(nil, []vocabmap.Mapping{mp("GYM", "relatedMatch", "L3"), mp("GYM", "exactMatch", "Y")})
	expand(t, g, "L3", false, "L3")
	expand(t, g, "L3", true, "L3", "GYM")
}

func TestExpandConflictDropsExact(t *testing.T) {
	g := vocabmap.NewGraph(nil, []vocabmap.Mapping{mp("A", "exactMatch", "B"), mp("A", "broadMatch", "B")})
	expand(t, g, "A", false, "A")
	expand(t, g, "B", false, "B", "A")
}

func TestExpandIgnoresOneToMany(t *testing.T) {
	g := vocabmap.NewGraph(nil, []vocabmap.Mapping{{From: "A", Relation: "exactMatch", To: []string{"B", "C"}}})
	expand(t, g, "A", false, "A")
}

func TestExpandCyclesTerminate(t *testing.T) {
	g := vocabmap.NewGraph(nil, []vocabmap.Mapping{
		mp("A", "exactMatch", "B"), mp("B", "exactMatch", "A"),
		mp("C", "broadMatch", "A"), mp("A", "broadMatch", "C"),
	})
	expand(t, g, "A", false, "A", "B", "C")
}

func TestExpandDepthCap(t *testing.T) {
	var ms []vocabmap.Mapping
	for i := 0; i < 6; i++ {
		ms = append(ms, mp(fmt.Sprintf("N%d", i+1), "broadMatch", fmt.Sprintf("N%d", i)))
	}
	g := vocabmap.NewGraph(nil, ms)
	expand(t, g, "N0", false, "N0", "N1", "N2", "N3", "N4") // depth 4
}

func TestExpandResultCap(t *testing.T) {
	var ms []vocabmap.Mapping
	for i := 0; i < 250; i++ {
		ms = append(ms, mp(fmt.Sprintf("C%03d", i), "broadMatch", "ROOT"))
	}
	ids, truncated := vocabmap.NewGraph(nil, ms).Expand("ROOT", false)
	if len(ids) != vocabmap.MaxResults || !truncated || ids[0] != "ROOT" {
		t.Fatalf("len=%d truncated=%v first=%s", len(ids), truncated, ids[0])
	}
}

func TestExpandUnknownID(t *testing.T) {
	ids, truncated := vocabmap.NewGraph(nil, nil).Expand("Z", true)
	if !reflect.DeepEqual(ids, []string{"Z"}) || truncated {
		t.Fatalf("got %v %v", ids, truncated)
	}
}
```

Run: `go test ./vocabmap/...`
Expected: FAIL (`NewGraph` undefined).

- [ ] **Step 2: Implement** (`vocabmap/graph.go`)

```go
package vocabmap

const (
	// MaxDepth bounds the narrowing step of Expand.
	MaxDepth = 4
	// MaxResults bounds the identifiers Expand returns per queried identifier.
	MaxResults = 200
)

// Graph holds the trusted relations between concept identifiers. exact, close
// and related are symmetric; narrower maps a concept to its narrower ones.
type Graph struct {
	exact, close, related, narrower map[string][]string
	has                             map[[3]string]bool
}

func (g *Graph) link(rel, from, to string) {
	key := [3]string{rel, from, to}
	if from == to || g.has[key] {
		return
	}
	g.has[key] = true
	switch rel {
	case "exact":
		g.exact[from] = append(g.exact[from], to)
	case "close":
		g.close[from] = append(g.close[from], to)
	case "related":
		g.related[from] = append(g.related[from], to)
	case "narrower":
		g.narrower[from] = append(g.narrower[from], to)
	}
}

func (g *Graph) both(rel, a, b string) {
	g.link(rel, a, b)
	g.link(rel, b, a)
}

// NewGraph builds the graph from in-scheme hierarchy links and 1:1 mappings.
// A pair that is exactMatch and also broad/narrow/related contributes no
// exact edge (NIP-VOCAB conflict rule).
func NewGraph(concepts []Concept, mappings []Mapping) *Graph {
	g := &Graph{
		exact: map[string][]string{}, close: map[string][]string{},
		related: map[string][]string{}, narrower: map[string][]string{},
		has: map[[3]string]bool{},
	}
	idByAddr := make(map[string]string, len(concepts))
	for _, c := range concepts {
		idByAddr[c.Address] = c.ID
	}
	resolve := func(addr string) string {
		if id, ok := idByAddr[addr]; ok {
			return id
		}
		return "nostr:" + addr
	}
	for _, c := range concepts {
		for _, b := range c.Broader {
			g.link("narrower", resolve(b), c.ID)
		}
		for _, n := range c.Narrower {
			g.link("narrower", c.ID, resolve(n))
		}
	}

	pair := func(a, b string) [2]string {
		if a < b {
			return [2]string{a, b}
		}
		return [2]string{b, a}
	}
	nonExact := map[[2]string]bool{}
	for _, m := range mappings {
		if len(m.To) == 1 && m.Relation != "exactMatch" && m.Relation != "closeMatch" {
			nonExact[pair(m.From, m.To[0])] = true
		}
	}
	for _, m := range mappings {
		if len(m.To) != 1 {
			continue
		}
		a, b := m.From, m.To[0]
		switch m.Relation {
		case "exactMatch":
			if !nonExact[pair(a, b)] {
				g.both("exact", a, b)
			}
		case "closeMatch":
			g.both("close", a, b)
		case "relatedMatch":
			g.both("related", a, b)
		case "broadMatch": // b is broader than a
			g.link("narrower", b, a)
		case "narrowMatch": // b is narrower than a
			g.link("narrower", a, b)
		}
	}
	return g
}

func (g *Graph) exactClosure(start string) []string {
	out := []string{start}
	seen := map[string]bool{start: true}
	for i := 0; i < len(out); i++ {
		for _, n := range g.exact[out[i]] {
			if !seen[n] {
				seen[n] = true
				out = append(out, n)
			}
		}
	}
	return out
}

// Expand returns id followed by what a filter on id should also match:
//  1. E: the exact closure of id (exactMatch is transitive)
//  2. the direct closeMatch neighbours of E (one hop, not chained)
//  3. everything narrower than those, transitively up to MaxDepth, each newly
//     reached concept with its exact closure
//  4. if related: the direct relatedMatch neighbours of E
//
// Broader concepts are never added. At most MaxResults identifiers, in that
// order; truncated reports whether the cap cut the result.
func (g *Graph) Expand(id string, related bool) (ids []string, truncated bool) {
	seen := map[string]bool{}
	add := func(x string) bool {
		if seen[x] {
			return true
		}
		if len(ids) >= MaxResults {
			truncated = true
			return false
		}
		seen[x] = true
		ids = append(ids, x)
		return true
	}

	exact := g.exactClosure(id)
	for _, x := range exact {
		if !add(x) {
			return
		}
	}
	frontier := append([]string{}, exact...)
	for _, x := range exact {
		for _, n := range g.close[x] {
			if !seen[n] {
				if !add(n) {
					return
				}
				frontier = append(frontier, n)
			}
		}
	}
	for depth := 0; depth < MaxDepth && len(frontier) > 0; depth++ {
		var next []string
		for _, x := range frontier {
			for _, child := range g.narrower[x] {
				if seen[child] {
					continue
				}
				for _, y := range g.exactClosure(child) {
					if seen[y] {
						continue
					}
					if !add(y) {
						return
					}
					next = append(next, y)
				}
			}
		}
		frontier = next
	}
	if related {
		for _, x := range exact {
			for _, n := range g.related[x] {
				if !add(n) {
					return
				}
			}
		}
	}
	return
}
```

- [ ] **Step 3: Run tests**

Run: `go test ./vocabmap/...`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add vocabmap && git commit -m "feat(vocabmap): expansion graph (exact closure, close hop, narrower, related on request)

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 6: `MappingRegistry`

**Files:**
- Create: `expand_registry.go`
- Test: `expand_registry_test.go`

**Interfaces:**
- Consumes: `vocabmap.*` (Tasks 4–5), `vocabmaptest` helpers.
- Produces:
  - `type Trust struct { Concordances []string /* 39741 addresses */; Authors []string /* hex pubkeys */ }`
  - `type mappingSource interface { Fetch(ctx context.Context, relays []string, filter nostr.Filter) ([]nostr.Event, error) }`
  - `type poolMappingSource struct{ pool *nostr.Pool }` (paged, per relay)
  - `func NewMappingRegistry(src mappingSource, relays []string, defaults Trust) *MappingRegistry`
  - `func (r *MappingRegistry) Defaults() Trust`
  - `func (r *MappingRegistry) Graph(ctx context.Context, trust Trust) (*vocabmap.Graph, bool)`
  - `func (r *MappingRegistry) Init()`, `StartRefreshLoop(time.Duration)`, `Stop()`; unexported `refresh(ctx)`.

- [ ] **Step 1: Failing tests** (`expand_registry_test.go`)

```go
package main

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"sync"
	"testing"

	"fiatjaf.com/nostr"
	vt "github.com/edufeed-org/amb-relay/vocabmap/vocabmaptest"
)

type fakeMappingSource struct {
	mu     sync.Mutex
	events []nostr.Event
	err    error
	calls  int
}

func (f *fakeMappingSource) Fetch(_ context.Context, _ []string, filter nostr.Filter) ([]nostr.Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	var out []nostr.Event
	for _, ev := range f.events {
		if filter.Matches(ev) {
			out = append(out, ev)
		}
	}
	return out, nil
}

func (f *fakeMappingSource) set(events []nostr.Event, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events, f.err = events, err
}

func (f *fakeMappingSource) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

const level2 = "https://w3id.org/kim/educationalLevel/level_2"

// levelWorld: Klassenstufen 5-6 broadMatch KIM level_2 in concordance "k",
// plus an author-only mapping by Other (no concordance).
func levelWorld() ([]nostr.Event, string) {
	o := vt.Owner
	conc := vt.ConcordanceAddr(o, "k")
	ks, el := vt.SchemeAddr(o, "klassenstufen"), vt.SchemeAddr(o, "educational-level")
	return []nostr.Event{
		vt.Concept(o, "klassenstufen", "5-6", ""),
		vt.Concept(o, "educational-level", "level_2", level2),
		vt.Concordance(o, "k", "nostr:"+ks, "nostr:"+el, []string{ks, el}),
		vt.Mapping(o, vt.ID(o, "5-6"), "broadMatch", level2, "nostr:"+ks, "nostr:"+el, conc, ks, el),
		// a stranger's mapping claiming membership: must be ignored
		vt.Mapping(vt.Other, "https://evil", "broadMatch", level2, "nostr:"+ks, "nostr:"+el, conc),
		// Other's own mapping, trusted only via expand:<npub>
		vt.Mapping(vt.Other, "https://other/x", "broadMatch", level2, "S", "T", ""),
	}, conc
}

func expandIDs(t *testing.T, r *MappingRegistry, trust Trust) []string {
	t.Helper()
	g, ok := r.Graph(context.Background(), trust)
	if !ok {
		t.Fatalf("Graph(%+v) not ok", trust)
	}
	ids, _ := g.Expand(level2, false)
	return ids
}

func TestMappingRegistryDefaults(t *testing.T) {
	events, conc := levelWorld()
	src := &fakeMappingSource{events: events}
	r := NewMappingRegistry(src, []string{"wss://r"}, Trust{Concordances: []string{conc}})

	if _, ok := r.Graph(context.Background(), r.Defaults()); ok {
		t.Fatal("cold start: defaults are loaded by refresh, not on the query path")
	}
	r.refresh(context.Background())
	want := []string{level2, vt.ID(vt.Owner, "5-6")}
	if got := expandIDs(t, r, r.Defaults()); !reflect.DeepEqual(got, want) {
		t.Fatalf("expand = %v, want %v", got, want)
	}

	calls := src.callCount()
	expandIDs(t, r, r.Defaults())
	if src.callCount() != calls {
		t.Fatal("a loaded trust set must not refetch")
	}
}

func TestMappingRegistryKeepsLastGoodGraph(t *testing.T) {
	events, conc := levelWorld()
	src := &fakeMappingSource{events: events}
	r := NewMappingRegistry(src, []string{"wss://r"}, Trust{Concordances: []string{conc}})
	r.refresh(context.Background())

	src.set(nil, errors.New("relay down"))
	r.refresh(context.Background())
	if got := expandIDs(t, r, r.Defaults()); len(got) != 2 {
		t.Fatalf("after failed refresh: %v", got)
	}

	src.set(nil, nil) // relay answers, but with nothing (dropped connection)
	r.refresh(context.Background())
	if got := expandIDs(t, r, r.Defaults()); len(got) != 2 {
		t.Fatalf("after empty refresh: %v", got)
	}
}

func TestMappingRegistryOnDemandAuthor(t *testing.T) {
	events, _ := levelWorld()
	src := &fakeMappingSource{events: events}
	r := NewMappingRegistry(src, []string{"wss://r"}, Trust{})
	got := expandIDs(t, r, Trust{Authors: []string{vt.Other.Hex()}})
	sort.Strings(got[1:]) // narrower order follows mapping d (a hash)
	if !reflect.DeepEqual(got, []string{level2, "https://evil", "https://other/x"}) {
		t.Fatalf("author trust = %v", got)
	}
}

func TestMappingRegistryNegativeCache(t *testing.T) {
	src := &fakeMappingSource{}
	r := NewMappingRegistry(src, []string{"wss://r"}, Trust{})
	missing := Trust{Concordances: []string{vt.ConcordanceAddr(vt.Owner, "nope")}}
	if _, ok := r.Graph(context.Background(), missing); ok {
		t.Fatal("unresolvable concordance must not yield a graph")
	}
	calls := src.callCount()
	r.Graph(context.Background(), missing)
	if src.callCount() != calls {
		t.Fatal("a failed source must not be retried on every query")
	}
}
```

Run: `go test -run MappingRegistry ./...`
Expected: FAIL (undefined).

- [ ] **Step 2: Implement** (`expand_registry.go`)

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"fiatjaf.com/nostr"
	"github.com/edufeed-org/amb-relay/vocabmap"
)

const (
	mappingRefreshTimeout  = 60 * time.Second
	mappingOnDemandTimeout = 3 * time.Second
	mappingRetryAfter      = time.Minute
	maxOnDemandSources     = 32
	mappingPageSize        = 300
)

// Trust is the set of mapping sources a query expands with: concordances
// (39741 addresses) and authors whose every mapping is trusted (hex pubkeys).
type Trust struct {
	Concordances []string
	Authors      []string
}

// sourceKeys are "c:<address>" and "p:<hex>", deduplicated and sorted.
func (t Trust) sourceKeys() []string {
	set := map[string]bool{}
	for _, c := range t.Concordances {
		set["c:"+c] = true
	}
	for _, a := range t.Authors {
		set["p:"+a] = true
	}
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

type mappingSource interface {
	Fetch(ctx context.Context, relays []string, filter nostr.Filter) ([]nostr.Event, error)
}

type sourceEntry struct {
	concepts []vocabmap.Concept
	mappings []vocabmap.Mapping
	lastUsed time.Time
}

// MappingRegistry keeps the concordances, mappings and scheme hierarchies
// that concept expansion trusts, as in-memory graphs. Defaults are loaded by
// Init/refresh only; per-query sources load on first use (bounded by
// mappingOnDemandTimeout) and are refreshed with the defaults. A failed load
// keeps the last good data. Modeled on CommunityRegistry.
type MappingRegistry struct {
	src      mappingSource
	relays   []string
	defaults Trust

	mu       sync.Mutex
	entries  map[string]*sourceEntry
	failedAt map[string]time.Time
	graphs   map[string]*vocabmap.Graph
	stopCh   chan struct{}
	stopOnce sync.Once
}

func NewMappingRegistry(src mappingSource, relays []string, defaults Trust) *MappingRegistry {
	return &MappingRegistry{
		src: src, relays: relays, defaults: defaults,
		entries:  map[string]*sourceEntry{},
		failedAt: map[string]time.Time{},
		graphs:   map[string]*vocabmap.Graph{},
		stopCh:   make(chan struct{}),
	}
}

func (r *MappingRegistry) Defaults() Trust { return r.defaults }

func (r *MappingRegistry) isDefault(key string) bool {
	for _, k := range r.defaults.sourceKeys() {
		if k == key {
			return true
		}
	}
	return false
}

// Graph returns the expansion graph for trust. ok is false when none of its
// sources is loaded; the caller then runs the query unexpanded.
func (r *MappingRegistry) Graph(ctx context.Context, trust Trust) (*vocabmap.Graph, bool) {
	keys := trust.sourceKeys()
	if len(keys) == 0 {
		return nil, false
	}
	for _, k := range r.toLoad(keys) {
		lctx, cancel := context.WithTimeout(ctx, mappingOnDemandTimeout)
		e, err := r.load(lctx, k)
		cancel()
		if err != nil {
			log.Printf("expand: load %s: %v", k, err)
			r.markFailed(k)
			continue
		}
		r.store(k, e)
	}
	return r.graphFor(strings.Join(keys, ","), keys)
}

// toLoad: keys not loaded yet, not defaults, not failed within mappingRetryAfter.
func (r *MappingRegistry) toLoad(keys []string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, k := range keys {
		if _, ok := r.entries[k]; ok || r.isDefault(k) {
			continue
		}
		if t, failed := r.failedAt[k]; failed && time.Since(t) < mappingRetryAfter {
			continue
		}
		out = append(out, k)
	}
	return out
}

func (r *MappingRegistry) markFailed(key string) {
	r.mu.Lock()
	r.failedAt[key] = time.Now()
	r.mu.Unlock()
}

func (r *MappingRegistry) store(key string, e *sourceEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if old, ok := r.entries[key]; ok {
		e.lastUsed = old.lastUsed
	} else {
		e.lastUsed = time.Now()
	}
	r.entries[key] = e
	delete(r.failedAt, key)
	r.graphs = map[string]*vocabmap.Graph{}
	for {
		var oldest string
		n := 0
		for k, v := range r.entries {
			if r.isDefault(k) {
				continue
			}
			n++
			if oldest == "" || v.lastUsed.Before(r.entries[oldest].lastUsed) {
				oldest = k
			}
		}
		if n <= maxOnDemandSources {
			return
		}
		delete(r.entries, oldest)
	}
}

func (r *MappingRegistry) graphFor(cacheKey string, keys []string) (*vocabmap.Graph, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	var present []*sourceEntry
	for _, k := range keys {
		if e, ok := r.entries[k]; ok {
			e.lastUsed = now
			present = append(present, e)
		}
	}
	if len(present) == 0 {
		return nil, false
	}
	if g, ok := r.graphs[cacheKey]; ok {
		return g, true
	}
	var concepts []vocabmap.Concept
	var mappings []vocabmap.Mapping
	seenC, seenM := map[string]bool{}, map[string]bool{}
	for _, e := range present {
		for _, c := range e.concepts {
			if !seenC[c.Address] {
				seenC[c.Address] = true
				concepts = append(concepts, c)
			}
		}
		for _, m := range e.mappings {
			if k := m.Author + "|" + m.D; !seenM[k] {
				seenM[k] = true
				mappings = append(mappings, m)
			}
		}
	}
	g := vocabmap.NewGraph(concepts, mappings)
	r.graphs[cacheKey] = g
	return g, true
}

func (r *MappingRegistry) load(ctx context.Context, key string) (*sourceEntry, error) {
	kind, value, _ := strings.Cut(key, ":")
	switch kind {
	case "c":
		return r.loadConcordance(ctx, value)
	case "p":
		return r.loadAuthor(ctx, value)
	}
	return nil, fmt.Errorf("unknown source %q", key)
}

func (r *MappingRegistry) loadConcordance(ctx context.Context, addr string) (*sourceEntry, error) {
	parts := strings.SplitN(addr, ":", 3)
	if len(parts) != 3 || parts[0] != "39741" {
		return nil, fmt.Errorf("not a concordance address: %s", addr)
	}
	owner, err := nostr.PubKeyFromHex(parts[1])
	if err != nil {
		return nil, err
	}
	evs, err := r.src.Fetch(ctx, r.relays, nostr.Filter{
		Kinds: []nostr.Kind{vocabmap.KindConcordance}, Authors: []nostr.PubKey{owner},
		Tags: nostr.TagMap{"d": []string{parts[2]}},
	})
	if err != nil {
		return nil, err
	}
	latest := vocabmap.LatestByAddress(evs)
	if len(latest) == 0 {
		return nil, errors.New("concordance not found")
	}
	conc, ok := vocabmap.ParseConcordance(latest[0])
	if !ok {
		return nil, errors.New("invalid concordance event")
	}
	authors := []nostr.PubKey{owner}
	for _, m := range conc.Maintainers {
		if pk, err := nostr.PubKeyFromHex(m); err == nil {
			authors = append(authors, pk)
		}
	}
	mevs, err := r.src.Fetch(ctx, r.relays, nostr.Filter{
		Kinds: []nostr.Kind{vocabmap.KindMapping}, Authors: authors,
		Tags: nostr.TagMap{"a": []string{addr}},
	})
	if err != nil {
		return nil, err
	}
	members := vocabmap.Members(conc, parseMappings(mevs))
	if len(members) == 0 {
		return nil, errors.New("concordance has no member mappings")
	}
	concepts, err := r.loadConcepts(ctx, conc.Schemes)
	if err != nil {
		return nil, err
	}
	return &sourceEntry{concepts: concepts, mappings: members}, nil
}

func (r *MappingRegistry) loadAuthor(ctx context.Context, hexKey string) (*sourceEntry, error) {
	pk, err := nostr.PubKeyFromHex(hexKey)
	if err != nil {
		return nil, err
	}
	evs, err := r.src.Fetch(ctx, r.relays, nostr.Filter{Kinds: []nostr.Kind{vocabmap.KindMapping}, Authors: []nostr.PubKey{pk}})
	if err != nil {
		return nil, err
	}
	mappings := parseMappings(evs)
	if len(mappings) == 0 {
		return nil, errors.New("author has no mappings")
	}
	var schemes []string
	seen := map[string]bool{}
	for _, m := range mappings {
		for _, s := range m.Schemes {
			if !seen[s] {
				seen[s] = true
				schemes = append(schemes, s)
			}
		}
	}
	concepts, err := r.loadConcepts(ctx, schemes)
	if err != nil {
		return nil, err
	}
	return &sourceEntry{concepts: concepts, mappings: mappings}, nil
}

func parseMappings(evs []nostr.Event) []vocabmap.Mapping {
	var out []vocabmap.Mapping
	for _, ev := range vocabmap.LatestByAddress(evs) {
		if m, ok := vocabmap.ParseMapping(ev); ok {
			out = append(out, m)
		}
	}
	return out
}

// loadConcepts fetches the concepts of schemes (for the in-scheme hierarchy).
// An empty answer for a non-empty scheme list is treated as a failed fetch.
func (r *MappingRegistry) loadConcepts(ctx context.Context, schemes []string) ([]vocabmap.Concept, error) {
	if len(schemes) == 0 {
		return nil, nil
	}
	evs, err := r.src.Fetch(ctx, r.relays, nostr.Filter{Kinds: []nostr.Kind{vocabmap.KindConcept}, Tags: nostr.TagMap{"a": schemes}})
	if err != nil {
		return nil, err
	}
	var out []vocabmap.Concept
	for _, ev := range vocabmap.LatestByAddress(evs) {
		if c, ok := vocabmap.ParseConcept(ev); ok {
			out = append(out, c)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("no concepts for schemes")
	}
	return out, nil
}

// refresh reloads the defaults and every cached source; failures keep the
// last good data.
func (r *MappingRegistry) refresh(ctx context.Context) {
	keys := r.defaults.sourceKeys()
	r.mu.Lock()
	for k := range r.entries {
		if !r.isDefault(k) {
			keys = append(keys, k)
		}
	}
	r.mu.Unlock()
	for _, k := range keys {
		e, err := r.load(ctx, k)
		if err != nil {
			log.Printf("expand: refresh %s: %v (keeping last good)", k, err)
			continue
		}
		r.store(k, e)
	}
}

func (r *MappingRegistry) Init() {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), mappingRefreshTimeout)
		defer cancel()
		r.refresh(ctx)
	}()
}

func (r *MappingRegistry) StartRefreshLoop(interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				ctx, cancel := context.WithTimeout(context.Background(), mappingRefreshTimeout)
				r.refresh(ctx)
				cancel()
			case <-r.stopCh:
				return
			}
		}
	}()
}

func (r *MappingRegistry) Stop() { r.stopOnce.Do(func() { close(r.stopCh) }) }

// poolMappingSource pages each relay separately (until-paging a merged
// multi-relay stream skips events only one relay holds) and dedupes by id.
type poolMappingSource struct {
	pool *nostr.Pool
}

func (s poolMappingSource) Fetch(ctx context.Context, relays []string, filter nostr.Filter) ([]nostr.Event, error) {
	byID := map[nostr.ID]nostr.Event{}
	for _, url := range relays {
		var until nostr.Timestamp
		for {
			f := filter
			f.Limit = mappingPageSize
			f.Until = until
			n, unseen := 0, 0
			var oldest nostr.Timestamp
			for ev := range s.pool.FetchMany(ctx, []string{url}, f, nostr.SubscriptionOptions{}) {
				n++
				if oldest == 0 || ev.CreatedAt < oldest {
					oldest = ev.CreatedAt
				}
				if _, ok := byID[ev.ID]; !ok {
					byID[ev.ID] = ev.Event
					unseen++
				}
			}
			if err := ctx.Err(); err != nil {
				return nil, fmt.Errorf("%s: %w", url, err)
			}
			if n < mappingPageSize {
				break
			}
			if unseen == 0 {
				return nil, fmt.Errorf("%s: more than %d events share created_at %d", url, mappingPageSize, oldest)
			}
			until = oldest
		}
	}
	out := make([]nostr.Event, 0, len(byID))
	for _, ev := range byID {
		out = append(out, ev)
	}
	return out, nil
}
```

- [ ] **Step 3: Run tests**

Run: `go vet ./... && go test -run MappingRegistry ./...`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add expand_registry.go expand_registry_test.go && git commit -m "feat(expand): MappingRegistry — trusted concordances/authors as graphs

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 7: Expander (filter rewrite and `expand:` tokens) and the rerank guard

**Files:**
- Modify: `expand.go` (add the rewrite)
- Modify: `content_registry.go:170-174` (`rerankOwns`)
- Test: `expand_test.go`, `content_registry_test.go` (one case)

**Interfaces:**
- Consumes: `Trust`, `*MappingRegistry` via `graphProvider` (Task 6); `vocabmap.Graph.Expand`, `vocabmap.MaxResults`.
- Produces:
  - `type graphProvider interface { Defaults() Trust; Graph(ctx context.Context, trust Trust) (*vocabmap.Graph, bool) }`
  - `type expandOptions struct { off, related bool; concordances, authors []string }`
  - `func expandConceptFilters(ctx context.Context, filter nostr.Filter, reg graphProvider, debug bool) nostr.Filter`

- [ ] **Step 1: Failing tests** (append to `expand_test.go`; merge the import block with the one from Task 3)

```go
import (
	"context"
	"reflect"
	"testing"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/eventstore/typesense30142"
	"fiatjaf.com/nostr/nip19"
	"github.com/edufeed-org/amb-relay/vocabmap"
	vt "github.com/edufeed-org/amb-relay/vocabmap/vocabmaptest"
)

type fakeGraphs struct {
	g        *vocabmap.Graph
	ok       bool
	gotTrust Trust
}

func (f *fakeGraphs) Defaults() Trust { return Trust{Concordances: []string{"39741:def:d"}} }
func (f *fakeGraphs) Graph(_ context.Context, t Trust) (*vocabmap.Graph, bool) {
	f.gotTrust = t
	return f.g, f.ok
}

func testGraphs() *fakeGraphs {
	m := func(from, rel, to string) vocabmap.Mapping {
		return vocabmap.Mapping{From: from, Relation: rel, To: []string{to}}
	}
	return &fakeGraphs{ok: true, g: vocabmap.NewGraph(nil, []vocabmap.Mapping{
		m("G56", "broadMatch", "L2"), m("SA", "exactMatch", "SB"), m("GYM", "relatedMatch", "L2"),
	})}
}

var amb = []nostr.Kind{30142}

func TestExpandTagFilter(t *testing.T) {
	got := expandConceptFilters(context.Background(),
		nostr.Filter{Kinds: amb, Tags: nostr.TagMap{"educationalLevel:id": {"L2"}, "t": {"x"}}}, testGraphs(), false)
	want := nostr.TagMap{"concepts_0": {"L2", "G56"}, "t": {"x"}}
	if !reflect.DeepEqual(got.Tags, want) {
		t.Fatalf("tags = %v, want %v", got.Tags, want)
	}
}

func TestExpandSearchFieldAndPhrase(t *testing.T) {
	got := expandConceptFilters(context.Background(),
		nostr.Filter{Kinds: amb, Search: `"heilige schrift" educationalLevel.id:L2 sort:datePublished`}, testGraphs(), false)
	if got.Search != `"heilige schrift" sort:datePublished` {
		t.Errorf("search = %q", got.Search)
	}
	if !reflect.DeepEqual(got.Tags["concepts_0"], []string{"L2", "G56"}) {
		t.Errorf("tags = %v", got.Tags)
	}
}

func TestExpandTwoFiltersStayAnded(t *testing.T) {
	got := expandConceptFilters(context.Background(),
		nostr.Filter{Kinds: amb, Tags: nostr.TagMap{"educationalLevel:id": {"L2"}}, Search: "about.id:SA"}, testGraphs(), false)
	if !reflect.DeepEqual(got.Tags, nostr.TagMap{"concepts_0": {"L2", "G56"}, "concepts_1": {"SA", "SB"}}) || got.Search != "" {
		t.Fatalf("tags = %v search = %q", got.Tags, got.Search)
	}
}

func TestExpandExtTag(t *testing.T) {
	got := expandConceptFilters(context.Background(),
		nostr.Filter{Kinds: amb, Tags: nostr.TagMap{"ext:ekw:subject:id": {"SB"}}}, testGraphs(), false)
	if !reflect.DeepEqual(got.Tags, nostr.TagMap{"concepts_0": {"SB", "SA"}}) {
		t.Fatalf("tags = %v", got.Tags)
	}
}

func TestExpandLeavesUnmappedAndForeign(t *testing.T) {
	g := testGraphs()
	cases := []nostr.Filter{
		{Kinds: amb, Tags: nostr.TagMap{"educationalLevel:id": {"NOMAP"}}},              // nothing mapped
		{Kinds: amb, Search: "about.id:SA about.name:Foo"},                               // same-base OR group
		{Kinds: []nostr.Kind{30142, 31923}, Tags: nostr.TagMap{"educationalLevel:id": {"L2"}}}, // not AMB-only
		{Tags: nostr.TagMap{"educationalLevel:id": {"L2"}}},                              // no kinds
		{Kinds: amb, Tags: nostr.TagMap{"keywords:id": {"L2"}}},                          // not a concept field
	}
	for _, f := range cases {
		if got := expandConceptFilters(context.Background(), f, g, false); !reflect.DeepEqual(got, f) {
			t.Errorf("%+v rewritten to %+v", f, got)
		}
	}
}

func TestExpandTokens(t *testing.T) {
	naddr := nip19.EncodeNaddr(vt.Owner, 39741, "k", nil)
	npub := nip19.EncodeNpub(vt.Other)
	g := testGraphs()

	off := expandConceptFilters(context.Background(),
		nostr.Filter{Kinds: amb, Tags: nostr.TagMap{"educationalLevel:id": {"L2"}}, Search: "mathe expand:off"}, g, false)
	if off.Search != "mathe" || !reflect.DeepEqual(off.Tags, nostr.TagMap{"educationalLevel:id": {"L2"}}) {
		t.Errorf("expand:off -> %+v", off)
	}

	rel := expandConceptFilters(context.Background(),
		nostr.Filter{Kinds: amb, Search: "educationalLevel.id:L2 expand:related expand:" + naddr + " expand:" + npub + " expand:bogus"}, g, false)
	if rel.Search != "" || !reflect.DeepEqual(rel.Tags["concepts_0"], []string{"L2", "G56", "GYM"}) {
		t.Errorf("expand:related -> %+v", rel)
	}
	wantTrust := Trust{Concordances: []string{vt.ConcordanceAddr(vt.Owner, "k")}, Authors: []string{vt.Other.Hex()}}
	if !reflect.DeepEqual(g.gotTrust, wantTrust) {
		t.Errorf("trust = %+v, want %+v", g.gotTrust, wantTrust)
	}

	nilReg := expandConceptFilters(context.Background(), nostr.Filter{Kinds: amb, Search: "x expand:off"}, nil, false)
	if nilReg.Search != "x" {
		t.Errorf("tokens must be stripped without a registry: %q", nilReg.Search)
	}
}

func TestExpandDefaultTrustAndUnavailableGraph(t *testing.T) {
	g := testGraphs()
	expandConceptFilters(context.Background(), nostr.Filter{Kinds: amb, Tags: nostr.TagMap{"about:id": {"SA"}}}, g, false)
	if !reflect.DeepEqual(g.gotTrust, g.Defaults()) {
		t.Errorf("default trust = %+v", g.gotTrust)
	}
	g.ok = false
	f := nostr.Filter{Kinds: amb, Tags: nostr.TagMap{"about:id": {"SA"}}}
	if got := expandConceptFilters(context.Background(), f, g, false); !reflect.DeepEqual(got, f) {
		t.Errorf("unavailable graph must leave the filter: %+v", got)
	}
}

func TestExpandSkipsClientConceptSlots(t *testing.T) {
	got := expandConceptFilters(context.Background(),
		nostr.Filter{Kinds: amb, Tags: nostr.TagMap{"concepts_0": {"Q"}, "about:id": {"SA"}}}, testGraphs(), false)
	if !reflect.DeepEqual(got.Tags, nostr.TagMap{"concepts_0": {"Q"}, "concepts_1": {"SA", "SB"}}) {
		t.Fatalf("tags = %v", got.Tags)
	}
}
```

In `content_registry_test.go` `TestRerankOwnsSearch`, add the field `tags nostr.TagMap` to the case struct, pass `Tags: c.tags` into the filter, and add this case:

```go
		{name: "free text plus expanded concept filter", kinds: []nostr.Kind{30142}, search: "mathematik", tags: nostr.TagMap{"concepts_0": {"L2"}}, want: false},
```

(Convert the existing positional case literals to keyed literals, or append `nil` as the `tags` value to each, whichever keeps the diff smaller.)

Run: `go test -run 'Expand|RerankOwns' ./...`
Expected: FAIL.

- [ ] **Step 2: Implement** (append to `expand.go`; extend its imports with `context`, `log`, `sort`, `fiatjaf.com/nostr/nip19`, `github.com/edufeed-org/amb-relay/vocabmap`)

```go
type graphProvider interface {
	Defaults() Trust
	Graph(ctx context.Context, trust Trust) (*vocabmap.Graph, bool)
}

type expandOptions struct {
	off, related          bool
	concordances, authors []string
}

func (o *expandOptions) parse(value string) {
	switch value {
	case "off":
		o.off = true
		return
	case "related":
		o.related = true
		return
	}
	prefix, data, err := nip19.Decode(value)
	if err != nil {
		log.Printf("expand: ignoring %q: %v", value, err)
		return
	}
	switch prefix {
	case "naddr":
		if p, ok := data.(nostr.EntityPointer); ok && p.Kind == vocabmap.KindConcordance {
			o.concordances = append(o.concordances, fmt.Sprintf("%d:%s:%s", p.Kind, p.PublicKey.Hex(), p.Identifier))
		}
	case "npub":
		if pk, ok := data.(nostr.PubKey); ok {
			o.authors = append(o.authors, pk.Hex())
		}
	}
}

func (o expandOptions) trust(defaults Trust) Trust {
	t := Trust{Concordances: defaults.Concordances, Authors: o.authors}
	if len(o.concordances) > 0 {
		t.Concordances = o.concordances
	}
	return t
}

var conceptBases = map[string]bool{"educationalLevel": true, "about": true, "learningResourceType": true, "audience": true}

// isConceptKey: "<base><sep>id" for a core vocab field, or
// "ext<sep><ns><sep><facet><sep>id" (sep ":" for tags, "." for NIP-50 fields).
func isConceptKey(key, sep string) bool {
	parts := strings.Split(key, sep)
	if len(parts) == 2 {
		return parts[1] == "id" && conceptBases[parts[0]]
	}
	return len(parts) == 4 && parts[0] == "ext" && parts[1] != "" && parts[2] != "" && parts[3] == "id"
}

// searchTokens splits like typesense30142's NIP-50 tokenizer, but keeps each
// token verbatim (quotes included) so kept tokens re-join unchanged.
func searchTokens(s string) []string {
	var tokens []string
	i := 0
	for i < len(s) {
		for i < len(s) && s[i] == ' ' {
			i++
		}
		if i >= len(s) {
			break
		}
		start := i
		if s[i] == '"' {
			if end := strings.Index(s[i+1:], `"`); end >= 0 {
				i += end + 2
				tokens = append(tokens, s[start:i])
				continue
			}
		}
		for i < len(s) && s[i] != ' ' {
			if s[i] == '"' {
				if end := strings.Index(s[i+1:], `"`); end >= 0 {
					i += end + 2
					continue
				}
			}
			i++
		}
		tokens = append(tokens, s[start:i])
	}
	return tokens
}

// fieldToken mirrors typesense30142.parseFieldFilter; phrases are never fields.
func fieldToken(tok string) (field, value string, ok bool) {
	if strings.HasPrefix(tok, `"`) {
		return "", "", false
	}
	i := strings.Index(tok, ":")
	if i <= 0 {
		return "", "", false
	}
	value = strings.Trim(tok[i+1:], `"`)
	return tok[:i], value, value != ""
}

func onlyAMB(kinds []nostr.Kind) bool {
	if len(kinds) == 0 {
		return false
	}
	for _, k := range kinds {
		if k != 30142 {
			return false
		}
	}
	return true
}

// expandConceptFilters rewrites the concept filters of an AMB-only query into
// synthetic concepts_<n> tag filters holding the values expanded through the
// trusted mappings (spec §1.4/§1.5). `expand:` tokens are always stripped from
// Search. Anything that cannot be expanded is left as it was — expansion never
// narrows a query and never fails it.
func expandConceptFilters(ctx context.Context, filter nostr.Filter, reg graphProvider, debug bool) nostr.Filter {
	out := filter
	var opts expandOptions
	type token struct{ raw, field, value string }
	var tokens []token
	fieldsByBase := map[string]map[string]bool{}
	stripped := false
	for _, raw := range searchTokens(filter.Search) {
		t := token{raw: raw}
		if field, value, ok := fieldToken(raw); ok {
			if field == "expand" {
				opts.parse(value)
				stripped = true
				continue
			}
			t.field, t.value = field, value
			base, _, _ := strings.Cut(field, ".")
			if fieldsByBase[base] == nil {
				fieldsByBase[base] = map[string]bool{}
			}
			fieldsByBase[base][field] = true
		}
		tokens = append(tokens, t)
	}
	join := func(drop map[string]bool) string {
		raws := make([]string, 0, len(tokens))
		for _, t := range tokens {
			if !drop[t.field] || t.field == "" {
				raws = append(raws, t.raw)
			}
		}
		return strings.Join(raws, " ")
	}
	if stripped {
		out.Search = join(nil)
	}
	if reg == nil || opts.off || !onlyAMB(filter.Kinds) {
		return out
	}

	type group struct{ tag, field string; values []string }
	var groups []group
	var tagNames []string
	for name := range filter.Tags {
		if isConceptKey(name, ":") {
			tagNames = append(tagNames, name)
		}
	}
	sort.Strings(tagNames)
	for _, name := range tagNames {
		groups = append(groups, group{tag: name, values: filter.Tags[name]})
	}
	seenField := map[string]int{}
	for _, t := range tokens {
		base, _, _ := strings.Cut(t.field, ".")
		if t.field == "" || !isConceptKey(t.field, ".") || len(fieldsByBase[base]) != 1 {
			continue
		}
		if i, ok := seenField[t.field]; ok {
			groups[i].values = append(groups[i].values, t.value)
			continue
		}
		seenField[t.field] = len(groups)
		groups = append(groups, group{field: t.field, values: []string{t.value}})
	}
	if len(groups) == 0 {
		return out
	}
	g, ok := reg.Graph(ctx, opts.trust(reg.Defaults()))
	if !ok {
		return out
	}

	tags := make(nostr.TagMap, len(filter.Tags)+len(groups))
	for k, v := range filter.Tags {
		tags[k] = v
	}
	dropFields := map[string]bool{}
	slot := 0
	for _, gr := range groups {
		expanded, changed := expandValues(g, gr.values, opts.related)
		if !changed {
			continue
		}
		for slot < maxConceptFilters {
			if _, taken := tags[fmt.Sprintf("%s%d", conceptTagPrefix, slot)]; !taken {
				break
			}
			slot++
		}
		if slot >= maxConceptFilters {
			break
		}
		if debug {
			log.Printf("expand: %v -> %v", gr.values, expanded)
		}
		tags[fmt.Sprintf("%s%d", conceptTagPrefix, slot)] = expanded
		slot++
		if gr.tag != "" {
			delete(tags, gr.tag)
		} else {
			dropFields[gr.field] = true
		}
	}
	if slot == 0 || reflect.DeepEqual(tags, filter.Tags) {
		return out
	}
	out.Tags = tags
	if len(dropFields) > 0 {
		out.Search = join(dropFields)
	}
	return out
}

// expandValues ORs the expansions of values: the values first, then each
// expansion's additions. changed is false when nothing was added.
func expandValues(g *vocabmap.Graph, values []string, related bool) ([]string, bool) {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, v := range values {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	changed := false
	for _, v := range values {
		ids, truncated := g.Expand(v, related)
		if truncated {
			log.Printf("expand: %s capped at %d identifiers", v, vocabmap.MaxResults)
		}
		for _, id := range ids {
			if !seen[id] {
				seen[id] = true
				out = append(out, id)
				changed = true
			}
		}
	}
	return out, changed
}
```

(Add `"reflect"` to the imports. Drop the `reflect.DeepEqual(tags, filter.Tags)` check if `slot == 0` already covers it in your reading; keep exactly one no-op guard.)

`content_registry.go` `rerankOwns`:

```go
func (r *registry) rerankOwns(filter nostr.Filter) bool {
	return r.targetsChunked(filter) &&
		searchHasFreeText(filter.Search) &&
		!searchHasFieldFilters(filter.Search) &&
		!hasConceptTags(filter)
}
```

Add one sentence to its doc comment: "Expanded concept filters (concepts_<n> tags, expand.go) also refuse it: events never carry those tags, so the rerank post-filter would drop every hit."

- [ ] **Step 3: Run tests**

Run: `go vet ./... && go test ./...`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add -A && git commit -m "feat(expand): rewrite concept filters through trusted mappings; expand: tokens

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 8: Configuration, wiring, docs

**Files:**
- Create: `expand_config.go`
- Test: `expand_config_test.go`
- Modify: `main.go` (registry setup before `relay.QueryStored`; `QueryStored` and `Count` call the expander)
- Modify: `.env.example`, `README.md`

**Interfaces:**
- Consumes: `NewMappingRegistry`, `poolMappingSource`, `expandConceptFilters`.
- Produces: `func conceptExpansionConfig(getenv func(string) string) (relays []string, defaults Trust, refresh time.Duration, debug bool)`.

- [ ] **Step 1: Failing test** (`expand_config_test.go`)

```go
package main

import (
	"reflect"
	"testing"
	"time"

	"fiatjaf.com/nostr/nip19"
	vt "github.com/edufeed-org/amb-relay/vocabmap/vocabmaptest"
)

func TestConceptExpansionConfig(t *testing.T) {
	env := map[string]string{
		"EXPAND_CONCORDANCES": nip19.EncodeNaddr(vt.Owner, 39741, "k", nil) + ", bogus ," +
			nip19.EncodeNaddr(vt.Owner, 30023, "article", nil),
		"EXPAND_READ_RELAYS":      "wss://a, wss://b",
		"EXPAND_REFRESH_INTERVAL": "nonsense",
		"EXPAND_DEBUG":            "true",
	}
	relays, defaults, refresh, debug := conceptExpansionConfig(func(k string) string { return env[k] })
	if !reflect.DeepEqual(relays, []string{"wss://a", "wss://b"}) {
		t.Errorf("relays = %v", relays)
	}
	if !reflect.DeepEqual(defaults, Trust{Concordances: []string{vt.ConcordanceAddr(vt.Owner, "k")}}) {
		t.Errorf("defaults = %+v", defaults)
	}
	if refresh != 10*time.Minute || !debug {
		t.Errorf("refresh = %s debug = %v", refresh, debug)
	}

	relays, defaults, refresh, debug = conceptExpansionConfig(func(string) string { return "" })
	if !reflect.DeepEqual(relays, []string{"wss://relay.edufeed.org"}) || len(defaults.Concordances) != 0 || refresh != 10*time.Minute || debug {
		t.Errorf("defaults: %v %+v %s %v", relays, defaults, refresh, debug)
	}
}
```

Run: `go test -run ConceptExpansionConfig ./...`
Expected: FAIL.

- [ ] **Step 2: Implement** (`expand_config.go`)

```go
package main

import (
	"fmt"
	"log"
	"strings"
	"time"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/nip19"
	"github.com/edufeed-org/amb-relay/vocabmap"
)

const (
	defaultExpandRelay   = "wss://relay.edufeed.org"
	defaultExpandRefresh = 10 * time.Minute
)

// conceptExpansionConfig reads EXPAND_CONCORDANCES (naddrs of kind 39741),
// EXPAND_READ_RELAYS, EXPAND_REFRESH_INTERVAL and EXPAND_DEBUG. Invalid
// entries are logged and skipped.
func conceptExpansionConfig(getenv func(string) string) (relays []string, defaults Trust, refresh time.Duration, debug bool) {
	for _, raw := range strings.Split(getenv("EXPAND_CONCORDANCES"), ",") {
		if raw = strings.TrimSpace(raw); raw == "" {
			continue
		}
		prefix, data, err := nip19.Decode(raw)
		p, ok := data.(nostr.EntityPointer)
		if err != nil || prefix != "naddr" || !ok || p.Kind != vocabmap.KindConcordance {
			log.Printf("expand: EXPAND_CONCORDANCES: skipping %q (not a kind-39741 naddr)", raw)
			continue
		}
		defaults.Concordances = append(defaults.Concordances, fmt.Sprintf("%d:%s:%s", p.Kind, p.PublicKey.Hex(), p.Identifier))
	}
	for _, r := range strings.Split(getenv("EXPAND_READ_RELAYS"), ",") {
		if r = strings.TrimSpace(r); r != "" {
			relays = append(relays, r)
		}
	}
	if len(relays) == 0 {
		relays = []string{defaultExpandRelay}
	}
	refresh = defaultExpandRefresh
	if raw := getenv("EXPAND_REFRESH_INTERVAL"); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil && d > 0 {
			refresh = d
		} else {
			log.Printf("expand: bad EXPAND_REFRESH_INTERVAL %q, using %s", raw, refresh)
		}
	}
	debug = getenv("EXPAND_DEBUG") == "true"
	return
}
```

`main.go`: directly before `relay.QueryStored = func(…)` insert:

```go
	// Concept filter expansion through trusted NIP-VOCAB mappings (expand.go).
	expandRelays, expandDefaults, expandRefresh, expandDebug := conceptExpansionConfig(os.Getenv)
	expandReg := NewMappingRegistry(poolMappingSource{pool: nostr.NewPool()}, expandRelays, expandDefaults)
	expandReg.Init()
	expandReg.StartRefreshLoop(expandRefresh)
	defer expandReg.Stop()
	fmt.Printf("Concept expansion: %d default concordance(s) from %v, refresh every %s\n",
		len(expandDefaults.Concordances), expandRelays, expandRefresh)
```

In the `relay.QueryStored` closure, make the first statement:

```go
		filter = expandConceptFilters(ctx, filter, expandReg, expandDebug)
```

Replace the `relay.Count` closure body with:

```go
		return boundedRegCount(expandConceptFilters(ctx, filter, expandReg, expandDebug))
```

(If `nostr.NewPool` needs options in this fork, copy the exact call the CommunityRegistry setup uses.)

`.env.example`, append:

```
# Concept filter expansion (NIP-VOCAB mappings). Comma-separated naddrs of
# kind-39741 concordances used by default; empty = expand only on request
# (expand:<naddr> / expand:<npub> in a search).
EXPAND_CONCORDANCES=
EXPAND_READ_RELAYS=wss://relay.edufeed.org
EXPAND_REFRESH_INTERVAL=10m
EXPAND_DEBUG=false
```

`README.md`: add a section `## Concept expansion` covering:
- what it does, with the level_2 → Klassenstufe 5–6 example;
- that it applies to `kinds:[30142]` queries only, for the concept filters `#educationalLevel:id`, `#about:id`, `#learningResourceType:id`, `#audience:id`, `#ext:<ns>:<facet>:id` and their NIP-50 `*.id:` forms;
- the token table from the spec (§1.5);
- the env table (§3);
- that live subscriptions (events arriving after EOSE) are matched unexpanded;
- that NIP-11 advertisement waits for the search-extensions PR.

- [ ] **Step 3: Run tests and build**

Run: `go vet ./... && go test ./... && go build ./...`
Expected: PASS.

- [ ] **Step 4: Local smoke test (no deploy)**

```bash
docker compose up -d typesense
EXPAND_CONCORDANCES=<naddr of klassenstufen--educational-level, from `nak encode naddr …` once Task 1 is published; otherwise leave empty> go run . &
```

Publish one test 30142 to the local relay with `ext:ekw:gradeLevel:id=nostr:39738:d2689e2f41dabfba953da26655a94ce2aa4e029c383ee921c6a4deafab99a612:5-6` (use a throwaway key from `nak key generate`, never a real one). Query `nak req -k 30142 -t 'educationalLevel:id=https://w3id.org/kim/educationalLevel/level_2' ws://localhost:<port>`: it must return the test event when the concordance is published and configured, and nothing with `--search 'expand:off'`. Stop the relay and `docker compose down` afterwards. If Task 1 is not published yet, record the smoke test as skipped in the report.

- [ ] **Step 5: Commit**

```bash
git add -A && git commit -m "feat(expand): env configuration, REQ/COUNT wiring, docs

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 9: GATE: release nostrlib and pin amb-relay to it (user go required)

- [ ] **Step 1:** Ask the user before any push. With their go, push `feat/concepts-field` to `edufeed-git` and open a Forgejo PR into `edufeed`, via the API with the token at `~/.config/claude-forgejo/token` (never print it). The user, or the agent on their instruction, merges it.
- [ ] **Step 2:** After the merge, in the amb-relay worktree run:

```bash
go mod edit -replace fiatjaf.com/nostr=git.edufeed.org/edufeed/nostrlib@<merge commit sha>
go mod tidy && go vet ./... && go test ./... && go build ./...
git add go.mod go.sum && git commit -m "deps: nostrlib with the concepts field

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

If the module fetch fails, retry with `GOPRIVATE=git.edufeed.org`.

- [ ] **Step 3:** With the user's go, push `mapping-expansion` and open the amb-relay PR. Merge and deploy are the user's call.

## Rollout (after merge, each step with the user's go)

1. vocabs: Task 1 Step 9 (publish B1–B3).
2. Deploy amb-relay with `EXPAND_CONCORDANCES` empty, then run NIP-86 `reindex` and spot-check that documents carry `concepts`.
3. Verify explicit queries: `educationalLevel.id:https://w3id.org/kim/educationalLevel/level_2 expand:<naddr klassenstufen--educational-level>` finds EKW resources tagged 5–6, 7–8 or 9–10. COUNT must match REQ.
4. Set `EXPAND_CONCORDANCES` to the 7 concordances' naddrs, restart, and verify the default expansion and `expand:off`.
