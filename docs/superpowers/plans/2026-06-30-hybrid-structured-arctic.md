# Hybrid Search on Structured Collections + Arctic Model Swap — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make short German topic queries surface semantically-related events from the structured collections (calendar/long-form/wiki) by (A) swapping the embed model e5→Arctic and (B) extending native Typesense hybrid search to those collections.

**Architecture:** Two independent levers sharing only the embed service. Lever A flips the embed service to model-aware prompting and bumps `EMBED_MODEL`. Lever B adds an `embedding` field to the three structured schemas, computes a passage vector on write via a shared `embeddable` interface + `embedAndAttach` helper, and wires `tsDB2/3/4.Embedder`. The hybrid query path already exists in nostrlib `query.go` and keys off `ts.Embedder != nil` — **no nostrlib and no amb-indexer source change.**

**Tech Stack:** Go 1.25 (amb-relay), Python FastAPI + sentence-transformers (embed service), Typesense (BM25 ⊕ dense vector, RRF fusion).

## Global Constraints

- Embedding dimension stays **768** everywhere. No Typesense `num_dim` reset anywhere (Arctic-m-v2.0 is 768-dim like e5).
- Target model: `EMBED_MODEL=Snowflake/snowflake-arctic-embed-m-v2.0`.
- Every embed call uses `normalize_embeddings=True` (cosine = dot).
- Arctic prompting: query → `encode(texts, prompt_name="query", normalize_embeddings=True)`; passage → `encode(texts, normalize_embeddings=True)` (NO prefix). e5 path retained: `"query: "` / `"passage: "` string prefixes.
- Fusion weight is the existing fixed `alpha:0.3` in `query.go`; not parameterized per-collection.
- New schema field, identical on all three structured schemas: `{Name: "embedding", Type: "float[]", NumDim: 768, VecDistMetric: "cosine", Optional: true}`.
- The new doc field MUST be `Embedding []float32 \`json:"embedding,omitempty"\`` (omitempty) so existing golden/JSON-stable tests stay byte-identical when no vector is set.
- Relay build/verify command: `gofmt -w *.go && GOWORK=off go build ./... && go vet ./...` (adding fields to existing structs needs a gofmt realign). Embed service: `pytest` in `embed/`.
- **No source change** to nostrlib (`../nostrlib`) or amb-indexer (`../amb-indexer`). The query path is reused as-is.
- All shared-infra steps (Docker deploy, reindex, re-chunk, collection drop) are **dev-only and operator-gated** — never executed without explicit user approval. Prod is never touched.
- Commit trailer on every commit: `Co-Authored-By: Claude Opus 4.7 <noreply@anthropic.com>`.

---

## File Structure

- **`embed/embed_service.py`** (modify): add `_is_arctic`, `_load_model`, `encode_texts` (model-aware); `embed()` delegates to `encode_texts`. Lever A.
- **`embed/tests/test_embed_service.py`** (modify): add Arctic-path unit tests using a fake model (no download); keep e5 tests.
- **`structured.go`** (modify): add `embeddable` interface + `embedAndAttach(ts, doc any) error`; call it inside `reprojectStructured` before upsert. Lever B core.
- **`structured_test.go`** (modify): unit-test `embedAndAttach` (fake embedder) + that `reprojectStructured` embeds when `ts.Embedder` set.
- **`calendar.go` / `longform.go` / `wiki.go`** (modify): each doc gains `Embedding []float32`, `EmbedText()`, `SetEmbedding([]float32)`; each schema gains the `embedding` field.
- **`calendar_test.go` / `longform_test.go` / `wiki_test.go`** (modify): unit-test `EmbedText()` field concatenation + schema `embedding` field present with `NumDim 768`.
- **`main.go`** (modify): under the existing `semanticCfg.Enabled && embedder != nil` guard, set `tsDB2.Embedder = embedder`, `tsDB3.Embedder = embedder`, `tsDB4.Embedder = embedder`. Lever B wiring.
- **Migration runbook** (Task 7): operator-gated dev deploy + reindex + live NIP-50 validation.

---

### Task 1: Embed service — model-aware Arctic prompting

**Files:**
- Modify: `embed/embed_service.py`
- Test: `embed/tests/test_embed_service.py`

**Interfaces:**
- Consumes: nothing (entry task).
- Produces: HTTP `/embed` contract unchanged (`{"texts":[...],"input_type":"query"|"passage"}` → `{"embeddings":[[...]],"model":str,"dimensions":int}`). New module-level functions `_is_arctic(name) -> bool` and `encode_texts(model, model_name, texts, input_type) -> vectors`.

- [ ] **Step 1: Write the failing tests**

Append to `embed/tests/test_embed_service.py`:

```python
def test_is_arctic_detects_family():
    import embed_service

    assert embed_service._is_arctic("Snowflake/snowflake-arctic-embed-m-v2.0")
    assert embed_service._is_arctic("snowflake-ARCTIC-embed")
    assert not embed_service._is_arctic("intfloat/multilingual-e5-base")


def test_arctic_query_uses_prompt_name():
    import embed_service

    captured = {}

    class FakeModel:
        def encode(self, texts, prompt_name=None, normalize_embeddings=True):
            captured["texts"] = list(texts)
            captured["prompt_name"] = prompt_name
            captured["normalize"] = normalize_embeddings
            return [[0.0]]

    embed_service.encode_texts(
        FakeModel(), "Snowflake/snowflake-arctic-embed-m-v2.0", ["hallo"], "query"
    )
    assert captured["texts"] == ["hallo"]
    assert captured["prompt_name"] == "query"
    assert captured["normalize"] is True


def test_arctic_passage_encoded_bare():
    import embed_service

    captured = {}

    class FakeModel:
        def encode(self, texts, normalize_embeddings=True, **kw):
            captured["texts"] = list(texts)
            captured["kw"] = kw
            return [[0.0]]

    embed_service.encode_texts(
        FakeModel(), "Snowflake/snowflake-arctic-embed-m-v2.0", ["hallo"], "passage"
    )
    assert captured["texts"] == ["hallo"]  # NO prefix
    assert "prompt_name" not in captured["kw"]


def test_e5_passage_still_prefixed():
    import embed_service

    captured = {}

    class FakeModel:
        def encode(self, texts, normalize_embeddings=True):
            captured["texts"] = list(texts)
            return [[0.0]]

    embed_service.encode_texts(
        FakeModel(), "intfloat/multilingual-e5-base", ["hallo"], "passage"
    )
    assert captured["texts"] == ["passage: hallo"]


def test_e5_query_still_prefixed():
    import embed_service

    captured = {}

    class FakeModel:
        def encode(self, texts, normalize_embeddings=True):
            captured["texts"] = list(texts)
            return [[0.0]]

    embed_service.encode_texts(
        FakeModel(), "intfloat/multilingual-e5-base", ["hallo"], "query"
    )
    assert captured["texts"] == ["query: hallo"]
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd embed && python -m pytest tests/test_embed_service.py -k "arctic or is_arctic or still_prefixed or query_still" -v`
Expected: FAIL with `AttributeError: module 'embed_service' has no attribute '_is_arctic'` (and `encode_texts`).

- [ ] **Step 3: Refactor `embed_service.py` to model-aware prompting**

Replace the `_model = SentenceTransformer(MODEL_NAME)` load (line 30) and the `embed()` body (lines 55-63) so the model-family logic lives in `_is_arctic` / `_load_model` / `encode_texts`. The final state of the relevant region:

```python
# e5 models are asymmetric: a query and the passage it should match get
# different prefixes. Arctic v2.0 instead uses a built-in query prompt and
# encodes passages bare. Model-family knowledge lives only in the helpers below.
E5_PREFIXES = {"query": "query: ", "passage": "passage: "}


def _is_arctic(name: str) -> bool:
    return "arctic" in name.lower()


def _load_model(name: str) -> SentenceTransformer:
    # Arctic v2.0 needs the de-risk-proven xformers-free load (eager attention,
    # no memory-efficient attention / input unpadding) so it runs on CPU without
    # the optional xformers dependency.
    if _is_arctic(name):
        return SentenceTransformer(
            name,
            trust_remote_code=True,
            model_kwargs={"attn_implementation": "eager"},
            config_kwargs={
                "use_memory_efficient_attention": False,
                "unpad_inputs": False,
            },
        )
    return SentenceTransformer(name)


def encode_texts(model, model_name: str, texts: list[str], input_type: str):
    # All paths normalize so cosine == dot. Arctic: query uses the model's
    # built-in "query" prompt, passages encode bare. e5: prepend string prefix.
    if _is_arctic(model_name):
        if input_type == "query":
            return model.encode(texts, prompt_name="query", normalize_embeddings=True)
        return model.encode(texts, normalize_embeddings=True)
    prefix = E5_PREFIXES.get(input_type, E5_PREFIXES["passage"])
    return model.encode([prefix + t for t in texts], normalize_embeddings=True)


app = FastAPI()
_model = _load_model(MODEL_NAME)
_dim = _model.get_sentence_embedding_dimension()
```

And the `embed()` handler body becomes:

```python
@app.post("/embed", response_model=EmbedResponse, dependencies=[Depends(verify_bearer)])
def embed(req: EmbedRequest) -> EmbedResponse:
    vectors = encode_texts(_model, MODEL_NAME, req.texts, req.input_type)
    return EmbedResponse(
        embeddings=vectors.tolist(),
        model=MODEL_NAME,
        dimensions=_dim,
    )
```

(Leave `MODEL_NAME`, `EXPECTED_TOKEN`, `verify_bearer`, the pydantic models, and `/health` unchanged.)

- [ ] **Step 4: Run the full embed test suite to verify it passes**

Run: `cd embed && python -m pytest tests/ -v`
Expected: PASS — new Arctic tests pass; existing e5 tests (`test_embed_applies_passage_prefix_by_default`, `test_embed_applies_query_prefix`, health, token) still pass because the default model is e5 and `encode_texts` reproduces the prefix behavior.

- [ ] **Step 5: Commit**

```bash
git add embed/embed_service.py embed/tests/test_embed_service.py
git commit -m "$(cat <<'EOF'
feat(embed): model-aware prompting for Arctic + e5

Detect the model family by name: Arctic uses prompt_name="query" for
queries and bare passages with the xformers-free load; e5 keeps its
string prefixes. Both normalize. /embed contract unchanged.

Co-Authored-By: Claude Opus 4.7 <noreply@anthropic.com>
EOF
)"
```

---

### Task 2: `embeddable` interface + `embedAndAttach` helper

**Files:**
- Modify: `structured.go`
- Test: `structured_test.go`

**Interfaces:**
- Consumes: `typesense30142.TSBackend.Embedder` (existing field, type `typesense30142.Embedder`), `typesense30142.EmbedPassage`.
- Produces:
  - `type embeddable interface { EmbedText() string; SetEmbedding([]float32) }`
  - `func embedAndAttach(ts *typesense30142.TSBackend, doc any) error` — no-op (returns nil) when `ts.Embedder == nil` or `doc` does not implement `embeddable`; otherwise embeds `doc.EmbedText()` as a passage and calls `doc.SetEmbedding(vec)`.
  - `reprojectStructured` now calls `embedAndAttach` before upsert; an embed error is logged, not propagated (doc still upserted, BM25-searchable, vector omitted).

- [ ] **Step 1: Write the failing tests**

Append to `structured_test.go`:

```go
// fakeEmbedder returns a fixed vector and records the input role.
type fakeEmbedder struct {
	vec       []float32
	gotInput  typesense30142.EmbedInput
	gotTexts  []string
	err       error
}

func (f *fakeEmbedder) Embed(ctx context.Context, texts []string, input typesense30142.EmbedInput) ([][]float32, error) {
	f.gotInput = input
	f.gotTexts = texts
	if f.err != nil {
		return nil, f.err
	}
	return [][]float32{f.vec}, nil
}

// fakeDoc implements embeddable for helper tests.
type fakeDoc struct {
	text string
	vec  []float32
}

func (d *fakeDoc) EmbedText() string         { return d.text }
func (d *fakeDoc) SetEmbedding(v []float32)  { d.vec = v }

func TestEmbedAndAttach_SetsVector(t *testing.T) {
	fe := &fakeEmbedder{vec: []float32{0.1, 0.2, 0.3}}
	ts := &typesense30142.TSBackend{Embedder: fe}
	doc := &fakeDoc{text: "title summary content"}
	if err := embedAndAttach(ts, doc); err != nil {
		t.Fatalf("embedAndAttach: %v", err)
	}
	if fe.gotInput != typesense30142.EmbedPassage {
		t.Errorf("input role = %q, want passage", fe.gotInput)
	}
	if len(fe.gotTexts) != 1 || fe.gotTexts[0] != "title summary content" {
		t.Errorf("embed texts = %v", fe.gotTexts)
	}
	if len(doc.vec) != 3 || doc.vec[0] != 0.1 {
		t.Errorf("vector = %v", doc.vec)
	}
}

func TestEmbedAndAttach_NilEmbedderNoop(t *testing.T) {
	ts := &typesense30142.TSBackend{} // Embedder nil
	doc := &fakeDoc{text: "x"}
	if err := embedAndAttach(ts, doc); err != nil {
		t.Fatalf("embedAndAttach: %v", err)
	}
	if doc.vec != nil {
		t.Errorf("vector set despite nil embedder: %v", doc.vec)
	}
}

func TestEmbedAndAttach_NonEmbeddableNoop(t *testing.T) {
	fe := &fakeEmbedder{vec: []float32{1}}
	ts := &typesense30142.TSBackend{Embedder: fe}
	// a plain map does not implement embeddable
	if err := embedAndAttach(ts, map[string]string{"id": "x"}); err != nil {
		t.Fatalf("embedAndAttach: %v", err)
	}
	if fe.gotTexts != nil {
		t.Errorf("embedder called for non-embeddable doc")
	}
}
```

(The end-to-end test that `reprojectStructured` attaches a vector lives in
Task 5, once a real embeddable projection — `nostrToWiki` returning a
`WikiDocument` with `EmbedText`/`SetEmbedding` — exists. The `fakeEmbedder`
and `fakeDoc` helpers added here are reused there.)

- [ ] **Step 2: Run the three helper tests to verify they fail**

Run: `GOWORK=off go test ./... -run 'TestEmbedAndAttach' -v`
Expected: FAIL — `undefined: embedAndAttach` / `undefined: embeddable`.

- [ ] **Step 3: Add the interface, helper, and wiring to `structured.go`**

Add `"context"` to the import block, then add:

```go
// embeddable is implemented by every structured document that carries a dense
// vector. EmbedText returns the text to embed; SetEmbedding stores the result.
type embeddable interface {
	EmbedText() string
	SetEmbedding([]float32)
}

// embedAndAttach computes a passage embedding for doc and attaches it, when an
// embedder is wired on ts and doc supports embedding. No-op (nil error)
// otherwise, so collections with semantic disabled (or non-embeddable docs)
// project exactly as before.
func embedAndAttach(ts *typesense30142.TSBackend, doc any) error {
	if ts.Embedder == nil {
		return nil
	}
	emb, ok := doc.(embeddable)
	if !ok {
		return nil
	}
	vecs, err := ts.Embedder.Embed(context.Background(), []string{emb.EmbedText()}, typesense30142.EmbedPassage)
	if err != nil {
		return err
	}
	if len(vecs) > 0 {
		emb.SetEmbedding(vecs[0])
	}
	return nil
}
```

Then change `reprojectStructured` to embed before upsert (a TS/embed blip must never reject the durable write — log and upsert without the vector):

```go
func reprojectStructured[T any](ts *typesense30142.TSBackend, event nostr.Event, project func(*nostr.Event) (*T, error)) error {
	doc, err := project(&event)
	if err != nil {
		return fmt.Errorf("project: %w", err)
	}
	if err := embedAndAttach(ts, doc); err != nil {
		// Log and continue: the doc still upserts (BM25-searchable) without a
		// vector; the next reindex re-embeds.
		fmt.Printf("embed structured %s: %v\n", event.ID.Hex(), err)
	}
	if err := upsertStructuredDoc(ts, doc); err != nil {
		return fmt.Errorf("upsert: %w", err)
	}
	return nil
}
```

- [ ] **Step 4: Run the helper tests + build to verify they pass**

Run: `GOWORK=off go test ./... -run 'TestEmbedAndAttach' -v && GOWORK=off go build ./... && go vet ./...`
Expected: PASS, build clean.

- [ ] **Step 5: Commit**

```bash
git add structured.go structured_test.go
git commit -m "$(cat <<'EOF'
feat(structured): embeddable interface + embedAndAttach helper

reprojectStructured now computes a passage vector when the backend has an
Embedder, attaching it via the embeddable interface. Embed failures are
logged, not propagated — the doc still upserts BM25-searchable.

Co-Authored-By: Claude Opus 4.7 <noreply@anthropic.com>
EOF
)"
```

---

### Task 3: Calendar doc — embedding field, EmbedText, schema

**Files:**
- Modify: `calendar.go`
- Test: `calendar_test.go`

**Interfaces:**
- Consumes: `embeddable` interface (Task 2).
- Produces: `CalendarDocument` implements `embeddable`. `EmbedText()` concatenates non-empty `title`, `summary`, `location`, `content` (space-joined, in that order). `calendarSchema` includes the `embedding` field.

- [ ] **Step 1: Write the failing tests**

Append to `calendar_test.go`:

```go
func TestCalendarEmbedText(t *testing.T) {
	doc := &CalendarDocument{Title: "SCALE-UP", Summary: "aktivierung", Location: "Hochschule", Content: "kooperativ"}
	if got := doc.EmbedText(); got != "SCALE-UP aktivierung Hochschule kooperativ" {
		t.Errorf("EmbedText = %q", got)
	}
}

func TestCalendarEmbedTextSkipsEmpty(t *testing.T) {
	doc := &CalendarDocument{Title: "T", Content: "C"} // summary + location empty
	if got := doc.EmbedText(); got != "T C" {
		t.Errorf("EmbedText = %q", got)
	}
}

func TestCalendarSetEmbedding(t *testing.T) {
	doc := &CalendarDocument{}
	doc.SetEmbedding([]float32{1, 2})
	if len(doc.Embedding) != 2 || doc.Embedding[0] != 1 {
		t.Errorf("Embedding = %v", doc.Embedding)
	}
}

func TestCalendarSchemaHasEmbedding(t *testing.T) {
	for _, f := range calendarSchema("calendar_31922").Fields {
		if f.Name == "embedding" {
			if f.Type != "float[]" || f.NumDim != 768 || f.VecDistMetric != "cosine" || !f.Optional {
				t.Errorf("embedding field = %+v", f)
			}
			return
		}
	}
	t.Fatal("calendar schema missing embedding field")
}
```

- [ ] **Step 2: Run to verify failure**

Run: `GOWORK=off go test ./... -run 'TestCalendarEmbedText|TestCalendarSetEmbedding|TestCalendarSchemaHasEmbedding' -v`
Expected: FAIL — `doc.Embedding undefined`, `doc.EmbedText undefined`, schema field absent.

- [ ] **Step 3: Add the field, methods, and schema field**

Add `"strings"` to `calendar.go` imports. Add `Embedding` to the struct (immediately before the embedded `structuredEnvelope`):

```go
	Status   string    `json:"status,omitempty"` // RSVP (31925): accepted/declined/tentative
	Embedding []float32 `json:"embedding,omitempty"`
	structuredEnvelope
```

Add the methods (after `nostrToCalendar`, before `storeCalendar`):

```go
// EmbedText returns the passage text for this calendar event: non-empty
// title, summary, location, content joined by spaces.
func (d *CalendarDocument) EmbedText() string {
	parts := make([]string, 0, 4)
	for _, s := range []string{d.Title, d.Summary, d.Location, d.Content} {
		if s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, " ")
}

// SetEmbedding stores the computed dense vector.
func (d *CalendarDocument) SetEmbedding(v []float32) { d.Embedding = v }
```

Add the schema field — change the last field line in `calendarSchema` so it reads:

```go
			{Name: "status", Type: "string", Optional: true, Facet: true},
			{Name: "embedding", Type: "float[]", NumDim: 768, VecDistMetric: "cosine", Optional: true},
		}, structuredEnvelopeFields()...),
```

- [ ] **Step 4: Run to verify pass + build**

Run: `GOWORK=off go test ./... -run 'TestCalendar' -v && GOWORK=off go build ./... && go vet ./...`
Expected: PASS (including the pre-existing `TestCalendarSchemaFields` and `nostrToCalendar` tests).

- [ ] **Step 5: Commit**

```bash
git add calendar.go calendar_test.go
git commit -m "$(cat <<'EOF'
feat(calendar): embedding field + EmbedText for hybrid search

CalendarDocument now implements embeddable (title+summary+location+content)
and the schema carries a 768-dim cosine embedding field.

Co-Authored-By: Claude Opus 4.7 <noreply@anthropic.com>
EOF
)"
```

---

### Task 4: Long-form doc — embedding field, EmbedText, schema

**Files:**
- Modify: `longform.go`
- Test: `longform_test.go`

**Interfaces:**
- Consumes: `embeddable` interface (Task 2).
- Produces: `LongformDocument` implements `embeddable`. `EmbedText()` concatenates non-empty `title`, `summary`, `content`. `longformSchema` includes the `embedding` field. The existing `TestLongformDocumentJSONStable` golden test stays green (Embedding omitempty, unset).

- [ ] **Step 1: Write the failing tests**

Append to `longform_test.go`:

```go
func TestLongformEmbedText(t *testing.T) {
	doc := &LongformDocument{Title: "T", Summary: "S", Content: "C"}
	if got := doc.EmbedText(); got != "T S C" {
		t.Errorf("EmbedText = %q", got)
	}
}

func TestLongformEmbedTextSkipsEmpty(t *testing.T) {
	doc := &LongformDocument{Title: "T", Content: "C"} // summary empty
	if got := doc.EmbedText(); got != "T C" {
		t.Errorf("EmbedText = %q", got)
	}
}

func TestLongformSetEmbedding(t *testing.T) {
	doc := &LongformDocument{}
	doc.SetEmbedding([]float32{9})
	if len(doc.Embedding) != 1 || doc.Embedding[0] != 9 {
		t.Errorf("Embedding = %v", doc.Embedding)
	}
}

func TestLongformSchemaHasEmbedding(t *testing.T) {
	for _, f := range longformSchema("longform_30023").Fields {
		if f.Name == "embedding" {
			if f.Type != "float[]" || f.NumDim != 768 || f.VecDistMetric != "cosine" || !f.Optional {
				t.Errorf("embedding field = %+v", f)
			}
			return
		}
	}
	t.Fatal("longform schema missing embedding field")
}
```

- [ ] **Step 2: Run to verify failure**

Run: `GOWORK=off go test ./... -run 'TestLongformEmbedText|TestLongformSetEmbedding|TestLongformSchemaHasEmbedding' -v`
Expected: FAIL — undefined field/methods, schema field absent.

- [ ] **Step 3: Add the field, methods, and schema field**

Add `"strings"` to `longform.go` imports. Add `Embedding` immediately before the embedded `structuredEnvelope`:

```go
	Image       string    `json:"image,omitempty"`
	Embedding   []float32 `json:"embedding,omitempty"`
	structuredEnvelope
```

Add methods (after `nostrToLongform`, before `storeLongform`):

```go
// EmbedText returns the passage text: non-empty title, summary, content.
func (d *LongformDocument) EmbedText() string {
	parts := make([]string, 0, 3)
	for _, s := range []string{d.Title, d.Summary, d.Content} {
		if s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, " ")
}

// SetEmbedding stores the computed dense vector.
func (d *LongformDocument) SetEmbedding(v []float32) { d.Embedding = v }
```

Add the schema field — after the `image` field line in `longformSchema`:

```go
			{Name: "image", Type: "string", Optional: true},
			{Name: "embedding", Type: "float[]", NumDim: 768, VecDistMetric: "cosine", Optional: true},
		}, structuredEnvelopeFields()...),
```

- [ ] **Step 4: Run to verify pass (incl. golden) + build**

Run: `GOWORK=off go test ./... -run 'TestLongform' -v && GOWORK=off go build ./... && go vet ./...`
Expected: PASS — `TestLongformDocumentJSONStable` still passes (Embedding omitempty and unset → absent from JSON).

- [ ] **Step 5: Commit**

```bash
git add longform.go longform_test.go
git commit -m "$(cat <<'EOF'
feat(longform): embedding field + EmbedText for hybrid search

LongformDocument implements embeddable (title+summary+content); schema gains
a 768-dim cosine embedding field. Golden JSON stays byte-identical (omitempty).

Co-Authored-By: Claude Opus 4.7 <noreply@anthropic.com>
EOF
)"
```

---

### Task 5: Wiki doc — embedding field, EmbedText, schema (+ reproject embed test)

**Files:**
- Modify: `wiki.go`
- Test: `wiki_test.go`, `structured_test.go`

**Interfaces:**
- Consumes: `embeddable` interface (Task 2), `embedAndAttach`/`reprojectStructured` (Task 2).
- Produces: `WikiDocument` implements `embeddable` (`title`, `summary`, `content`). `wikiSchema` includes the `embedding` field. Adds the cross-cutting `TestReprojectStructured_EmbedsWhenEmbedderSet` (deferred from Task 2) now that a real embeddable doc + projection exist.

- [ ] **Step 1: Write the failing tests**

Append to `wiki_test.go`:

```go
func TestWikiEmbedText(t *testing.T) {
	doc := &WikiDocument{Title: "T", Summary: "S", Content: "C"}
	if got := doc.EmbedText(); got != "T S C" {
		t.Errorf("EmbedText = %q", got)
	}
}

func TestWikiEmbedTextSkipsEmpty(t *testing.T) {
	doc := &WikiDocument{Content: "C"} // title + summary empty
	if got := doc.EmbedText(); got != "C" {
		t.Errorf("EmbedText = %q", got)
	}
}

func TestWikiSetEmbedding(t *testing.T) {
	doc := &WikiDocument{}
	doc.SetEmbedding([]float32{7})
	if len(doc.Embedding) != 1 || doc.Embedding[0] != 7 {
		t.Errorf("Embedding = %v", doc.Embedding)
	}
}

func TestWikiSchemaHasEmbedding(t *testing.T) {
	for _, f := range wikiSchema("wiki_30818").Fields {
		if f.Name == "embedding" {
			if f.Type != "float[]" || f.NumDim != 768 || f.VecDistMetric != "cosine" || !f.Optional {
				t.Errorf("embedding field = %+v", f)
			}
			return
		}
	}
	t.Fatal("wiki schema missing embedding field")
}
```

Append to `structured_test.go` (the cross-cutting reproject test deferred from Task 2):

```go
func TestReprojectStructured_EmbedsWhenEmbedderSet(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"success":true}`))
	}))
	defer srv.Close()
	fe := &fakeEmbedder{vec: []float32{0.5, 0.6}}
	ts := &typesense30142.TSBackend{Host: srv.URL, CollectionName: "c", ApiKey: "k", Embedder: fe}
	evt := nostr.Event{Kind: 30818, Content: "body", Tags: nostr.Tags{{"d", "x"}, {"title", "T"}}}
	if err := reprojectStructured(ts, evt, nostrToWiki); err != nil {
		t.Fatalf("reprojectStructured: %v", err)
	}
	if !strings.Contains(string(gotBody), `"embedding":[0.5,0.6]`) {
		t.Errorf("upserted body missing embedding vector: %s", gotBody)
	}
	if fe.gotInput != typesense30142.EmbedPassage {
		t.Errorf("input role = %q, want passage", fe.gotInput)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `GOWORK=off go test ./... -run 'TestWiki|TestReprojectStructured_EmbedsWhenEmbedderSet' -v`
Expected: FAIL — undefined field/methods on `WikiDocument`; reproject test sees no embedding in body.

- [ ] **Step 3: Add the field, methods, and schema field to `wiki.go`**

Add `"strings"` to `wiki.go` imports. Add `Embedding` before the embedded `structuredEnvelope`:

```go
	Content string    `json:"content,omitempty"`
	Embedding []float32 `json:"embedding,omitempty"`
	structuredEnvelope
```

Add methods (after `nostrToWiki`, before `storeWiki`):

```go
// EmbedText returns the passage text: non-empty title, summary, content.
func (d *WikiDocument) EmbedText() string {
	parts := make([]string, 0, 3)
	for _, s := range []string{d.Title, d.Summary, d.Content} {
		if s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, " ")
}

// SetEmbedding stores the computed dense vector.
func (d *WikiDocument) SetEmbedding(v []float32) { d.Embedding = v }
```

Add the schema field — after the `content` field line in `wikiSchema`:

```go
			{Name: "content", Type: "string", Optional: true},
			{Name: "embedding", Type: "float[]", NumDim: 768, VecDistMetric: "cosine", Optional: true},
		}, structuredEnvelopeFields()...),
```

- [ ] **Step 4: Run full suite + build**

Run: `GOWORK=off go test ./... && GOWORK=off go build ./... && go vet ./...`
Expected: PASS across the package — confirms all three docs embed and the reproject path attaches vectors end-to-end.

- [ ] **Step 5: Commit**

```bash
git add wiki.go wiki_test.go structured_test.go
git commit -m "$(cat <<'EOF'
feat(wiki): embedding field + EmbedText; end-to-end reproject embed test

WikiDocument implements embeddable; schema gains the 768-dim cosine field.
Adds the reprojectStructured embed assertion now a real embeddable projection
exists.

Co-Authored-By: Claude Opus 4.7 <noreply@anthropic.com>
EOF
)"
```

---

### Task 6: Wire structured backends' Embedder in `main.go`

**Files:**
- Modify: `main.go:506-512` (the `semanticCfg.Enabled && embedder != nil` block)

**Interfaces:**
- Consumes: `embedder` (`*EmbeddingClient`), `tsDB2`/`tsDB3`/`tsDB4` (longform/wiki/calendar backends). **Each is nil when its feature flag is off** (confirmed at `main.go:211`, `:237`, `:265`), so every assignment MUST be nil-guarded.
- Produces: when semantic search is enabled, each non-nil structured backend gets `.Embedder` set, which activates the existing hybrid branch in `query.go:379` for that collection (write path embeds via `EmbedText()`; query path keys only off `ts.Embedder != nil`). No `EmbedFields` needed on these backends.

- [ ] **Step 1: Edit the wiring block**

Replace the existing block (`main.go:506-512`) so it reads:

```go
	if semanticCfg.Enabled && embedder != nil {
		tsDB.Embedder = embedder
		tsDB.EmbedFields = semanticCfg.EmbedFields
		// Structured collections (long-form, wiki, calendar) reuse the same
		// hybrid query path; setting Embedder activates BM25⊕vector RRF for
		// them. They embed on write via EmbedText(), so no EmbedFields needed.
		// Each backend is nil when its feature flag is off, hence the guards.
		if tsDB2 != nil {
			tsDB2.Embedder = embedder
		}
		if tsDB3 != nil {
			tsDB3.Embedder = embedder
		}
		if tsDB4 != nil {
			tsDB4.Embedder = embedder
		}
		fmt.Printf("Semantic search enabled with fields: %v\n", semanticCfg.EmbedFields)
	} else {
		fmt.Println("Semantic search disabled")
	}
```

- [ ] **Step 2: Build and vet**

Run: `GOWORK=off go build ./... && go vet ./...`
Expected: clean build.

- [ ] **Step 3: Run the full test suite**

Run: `GOWORK=off go test ./...`
Expected: PASS (no behavioral test depends on this wiring; this is integration glue verified live in Task 7).

- [ ] **Step 4: Commit**

```bash
git add main.go
git commit -m "$(cat <<'EOF'
feat: wire structured backends' Embedder to activate hybrid search

Under the existing semantic-enabled guard, set tsDB2/3/4.Embedder so the
shared query path runs BM25⊕vector RRF for long-form, wiki, and calendar.

Co-Authored-By: Claude Opus 4.7 <noreply@anthropic.com>
EOF
)"
```

---

### Task 7: Dev migration runbook + live validation (operator-gated)

**Files:** none (operational task). No code change; this task executes the dev cutover and validates the acceptance test. **Every shared-infra step requires explicit user approval before running.**

**Interfaces:**
- Consumes: Tasks 1-6 merged and building green.
- Produces: a re-embedded dev stack on Arctic with hybrid-enabled structured collections, and a pass/fail validation result against the SCALE-UP probes.

- [ ] **Step 1: Pre-flight verification (no infra)**

Run: `GOWORK=off go build ./... && go vet ./...` (relay green) and `cd embed && python -m pytest tests/` (embed green). Confirm no nostrlib/amb-indexer source changed: `git -C ../nostrlib status --short` and `git -C ../amb-indexer status --short` both clean.

- [ ] **Step 2: (OPERATOR-GATED) Deploy dev stack on Arctic**

Set `EMBED_MODEL=Snowflake/snowflake-arctic-embed-m-v2.0` for both relay and indexer in the dev `.env`. Rebuild the embed service `--no-cache` (so the new model downloads into `embed_model_cache`). Deploy. Then verify the model loaded:

```bash
curl -s http://<dev-embed>/health   # expect {"status":"ok","model":"Snowflake/snowflake-arctic-embed-m-v2.0","dimensions":768}
```

Pass: `/health` reports the arctic model, `dimensions: 768`.

- [ ] **Step 3: (OPERATOR-GATED) Re-embed everything (768→768, no schema reset)**

Trigger the relay NIP-86 `reindex`: it re-embeds the AMB collection with Arctic and drops+reprojects the structured targets (calendar/long-form/wiki), populating the new `embedding` field via `embedAndAttach`. Separately, the indexer auto-re-chunks `amb_chunks_30142` via `EmbedModel` hash invalidation (no manual cursor reset). Wait for both to complete (`getreindexstatus` for the relay; indexer logs/metrics for chunking).

- [ ] **Step 4: (OPERATOR-GATED) Validate the acceptance test against the live relay**

Run the two de-risk probes as live NIP-50 searches over the calendar kind (and `[30142]`):

```
search:"kooperatives Lernen Hochschule"  kinds:[31923]  limit:20
search:"Aktivierung Studierende"         kinds:[31923]  limit:20
```

- **Pass:** the e-teaching.org SCALE-UP event appears on page 1 (within `limit:20`) for both probes, where today it is absent.
- **No regression:** plain kind/range/`#h` queries and existing AMB search behave as before — spot-check a wildcard listing (`q="*"`) and a `#start_after`/`#start_before` range query still return the same shape.

- [ ] **Step 5: Record the result**

Append the pass/fail outcome (ranks observed) to `.superpowers/sdd/progress.md`. Prod cutover remains a separate, later, operator-gated action — do NOT execute it here.

---

## Notes on execution order & branch strategy

- Tasks 1-6 are local, reversible, and TDD-gated — execute them in order. Task 2 must precede Tasks 3-5 (they implement the `embeddable` interface it defines). Task 6 needs Tasks 3-5 (the docs must embed before wiring matters). Task 7 needs all prior tasks merged.
- **Branch decision (resolve with the user before execution):** whether to finalize the in-flight e5 branch (Task 7 / #57) first, roll this work onto the current branch, or start a fresh branch off the e5 foundation. This plan assumes the e5 migration (768-dim everywhere) is already merged/available as the foundation.
