# multilingual-e5-base Embedding Migration Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Swap the symmetric `paraphrase-multilingual-MiniLM-L12-v2` embedder for the asymmetric `intfloat/multilingual-e5-base` (768-dim, `query:`/`passage:` prefixes) across the embed service, nostrlib, the relay, and amb-indexer, so short topic queries retrieve semantically-related long passages on the dev stack.

**Architecture:** The `/embed` HTTP contract gains an `input_type` field; the embed service owns the e5 prefix logic. The two Go consumers thread `query` vs `passage` through their embed call sites. Both Typesense `embedding` fields move 384→768-dim, forcing a collection recreate; the indexer folds an embedding-version tag into its idempotency hash so a full re-chunk happens automatically.

**Tech Stack:** Python (FastAPI, sentence-transformers), Go (khatru/eventstore fork `fiatjaf.com/nostr`, amb-indexer), Typesense, Docker Compose.

## Global Constraints

- e5 prefixes are the exact strings `"query: "` and `"passage: "` (colon + single space), prepended to each text. (Verbatim — required by the e5 model family.)
- Both `embedding` fields use `num_dim: 768`, `vec_dist_metric: "cosine"`.
- Default model id (both embed service and indexer config): `intfloat/multilingual-e5-base`.
- `input_type` absent on `/embed` ⇒ default `passage`. Query call sites set `query` explicitly.
- Dev stack only. No prod changes. All shared-infra actions (nostrlib push, deploy, reindex, collection drop, cursor reset) are operator-gated — list them, do NOT auto-run.
- Never print live credentials (TS_APIKEY, EMBED_TOKEN, nsecs, indexer tokens) into output; read them into server-side shell vars used inline.
- Local Go builds resolve nostrlib via the parent `edufeed/go.work` (→ `./nostrlib`); deployment builds use the `go.mod` replace pseudo-version. Per-task Go validation uses the default (go.work) build; the `GOWORK=off` build is verified in Task 7 after nostrlib is pushed.
- Stage files by name (never `git add -A`); never skip hooks; new commits only. Git writes to the worktree git-dir require sandbox disabled.

---

### Task 1: Embed service — `input_type` + e5-base prefixes

**Files:**
- Modify: `embed/embed_service.py`
- Modify: `embed/tests/test_embed_service.py`
- Modify: `embed/Dockerfile:14-15` (cache comment only)
- Test: `embed/tests/test_embed_service.py`

**Interfaces:**
- Produces: `POST /embed {"texts":[...],"input_type":"query"|"passage"}` → `{"embeddings":[[float]*768,...],"model":str,"dimensions":768}`. Prefix `"query: "`/`"passage: "` prepended per `input_type`; default `passage`.

- [ ] **Step 1: Write the failing prefix tests**

Add to `embed/tests/test_embed_service.py`:

```python
def test_embed_applies_passage_prefix_by_default(client, monkeypatch):
    import numpy as np
    import embed_service

    captured = {}

    def fake_encode(texts, normalize_embeddings=True):
        captured["texts"] = list(texts)
        return np.zeros((len(texts), embed_service._dim), dtype="float32")

    monkeypatch.setattr(embed_service._model, "encode", fake_encode)
    r = client.post(
        "/embed",
        headers={"Authorization": f"Bearer {TEST_TOKEN}"},
        json={"texts": ["hallo welt"]},
    )
    assert r.status_code == 200
    assert captured["texts"] == ["passage: hallo welt"]


def test_embed_applies_query_prefix(client, monkeypatch):
    import numpy as np
    import embed_service

    captured = {}

    def fake_encode(texts, normalize_embeddings=True):
        captured["texts"] = list(texts)
        return np.zeros((len(texts), embed_service._dim), dtype="float32")

    monkeypatch.setattr(embed_service._model, "encode", fake_encode)
    r = client.post(
        "/embed",
        headers={"Authorization": f"Bearer {TEST_TOKEN}"},
        json={"texts": ["hallo welt"], "input_type": "query"},
    )
    assert r.status_code == 200
    assert captured["texts"] == ["query: hallo welt"]
```

Update the two existing dimension assertions from `384` to `768`:
- `test_health_returns_384_dim` → rename to `test_health_returns_768_dim`, assert `body["dimensions"] == 768`.
- `test_embed_returns_one_vector_per_input` → assert `len(vec) == 768`.

- [ ] **Step 2: Run tests to verify the prefix tests fail**

Run: `cd embed && python -m pytest tests/test_embed_service.py -v`
Expected: the two new prefix tests FAIL (texts captured as `["hallo welt"]`, no prefix); dim tests may also fail until Step 3 sets the model.

- [ ] **Step 3: Implement prefix + e5-base default**

In `embed/embed_service.py`, change the default model and add prefix logic:

```python
MODEL_NAME = os.environ.get(
    "EMBED_MODEL", "intfloat/multilingual-e5-base"
)
EXPECTED_TOKEN = os.environ.get("EMBED_TOKEN", "")

# e5 models are asymmetric: a query and the passage it should match get
# different prefixes. This is the only place model-specific prefix knowledge
# lives — a prefix-free model (e.g. bge-m3) would set both to "".
E5_PREFIXES = {"query": "query: ", "passage": "passage: "}
```

Change `EmbedRequest`:

```python
class EmbedRequest(BaseModel):
    texts: list[str]
    input_type: str = "passage"
```

Change `embed()`:

```python
@app.post("/embed", response_model=EmbedResponse, dependencies=[Depends(verify_bearer)])
def embed(req: EmbedRequest) -> EmbedResponse:
    prefix = E5_PREFIXES.get(req.input_type, E5_PREFIXES["passage"])
    prefixed = [prefix + t for t in req.texts]
    vectors = _model.encode(prefixed, normalize_embeddings=True)
    return EmbedResponse(
        embeddings=vectors.tolist(),
        model=MODEL_NAME,
        dimensions=_dim,
    )
```

Update the `embed/Dockerfile:14-15` comment to reference the new model and size:

```dockerfile
# HuggingFace cache lives here; mount a volume to persist the model
# (~1.1 GB for intfloat/multilingual-e5-base) across container restarts.
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd embed && python -m pytest tests/test_embed_service.py -v`
Expected: all PASS. (First run downloads e5-base ~1.1 GB into the HF cache; the session fixture loads the real model, so the dim tests assert the live 768.)

- [ ] **Step 5: Commit**

```bash
git add embed/embed_service.py embed/tests/test_embed_service.py embed/Dockerfile
git commit -m "feat(embed): add input_type prefixes and default to multilingual-e5-base"
```

---

### Task 2: nostrlib — `Embedder` carries `input_type`; AMB field 768-dim

**Repo:** `../../../../nostrlib` (i.e. `/home/laoc/coding/edufeed/nostrlib`)

**Files:**
- Modify: `eventstore/typesense30142/lib.go:15-20` (Embedder interface)
- Modify: `eventstore/typesense30142/query.go:383` (query embed)
- Modify: `eventstore/typesense30142/replace.go:43` and `:177` (passage embeds)
- Modify: `eventstore/typesense30142/typesense.go:237-238` (num_dim + comment)
- Modify: `eventstore/typesense30142/types.go:257` (comment)
- Test: `eventstore/typesense30142` package build (`go build ./...`)

**Interfaces:**
- Produces: `type EmbedInput string`; consts `EmbedQuery EmbedInput = "query"`, `EmbedPassage EmbedInput = "passage"`; interface method `Embed(ctx context.Context, texts []string, input EmbedInput) ([][]float32, error)`. AMB `embedding` field `NumDim: 768`.

> Note: nostrlib has no test-side Embedder mock and no test calls `Embed` (confirmed by grep), so this is a compile-level change validated by `go build ./...`. The relay (Task 3) provides the only concrete implementation.

- [ ] **Step 1: Change the interface**

In `eventstore/typesense30142/lib.go`, replace the `Embedder` block (lines 15-20):

```go
// EmbedInput distinguishes asymmetric retrieval roles for models (e.g. e5)
// that prepend a "query:"/"passage:" prefix. Symmetric models receive it and
// embed identically.
type EmbedInput string

const (
	EmbedQuery   EmbedInput = "query"
	EmbedPassage EmbedInput = "passage"
)

// Embedder is an interface for text embedding services.
// Implementations should be thread-safe.
type Embedder interface {
	// Embed computes embedding vectors for the given texts.
	// Returns one vector per input text. input selects the asymmetric role.
	Embed(ctx context.Context, texts []string, input EmbedInput) ([][]float32, error)
}
```

- [ ] **Step 2: Verify the build breaks at call sites**

Run: `cd /home/laoc/coding/edufeed/nostrlib && go build ./eventstore/typesense30142/`
Expected: FAIL — `not enough arguments in call to ts.Embedder.Embed` at query.go and replace.go.

- [ ] **Step 3: Thread the input role through the three call sites**

`query.go:383` (query path):

```go
		embeddings, err := ts.Embedder.Embed(ctx, []string{mainQuery}, EmbedQuery)
```

`replace.go:43` (single passage):

```go
			embeddings, err := ts.Embedder.Embed(ctx, []string{embedText}, EmbedPassage)
```

`replace.go:177` (batch passage):

```go
				embeddings, eerr := ts.Embedder.Embed(ctx, []string{embedText}, EmbedPassage)
```

- [ ] **Step 4: Bump the AMB vector field to 768-dim**

`typesense.go:237-238`:

```go
			// Semantic search embedding vector (768-dim for intfloat/multilingual-e5-base)
			{Name: "embedding", Type: "float[]", NumDim: 768, VecDistMetric: "cosine", Optional: true},
```

`types.go:257` — update the comment to read `768-dim for intfloat/multilingual-e5-base`.

- [ ] **Step 5: Verify build + existing tests pass**

Run: `cd /home/laoc/coding/edufeed/nostrlib && go build ./... && go test ./eventstore/typesense30142/`
Expected: PASS.

- [ ] **Step 6: Commit (in the nostrlib repo)**

```bash
cd /home/laoc/coding/edufeed/nostrlib
git add eventstore/typesense30142/lib.go eventstore/typesense30142/query.go eventstore/typesense30142/replace.go eventstore/typesense30142/typesense.go eventstore/typesense30142/types.go
git commit -m "feat(typesense30142): Embedder input_type role; AMB embedding 768-dim"
```

---

### Task 3: Relay — `EmbeddingClient` implements the new interface

**Files:**
- Modify: `embedding.go:34-36` (request struct), `:53-89` (Embed method)
- Test: relay package build `go build ./...` and `go test ./...` (via go.work)

**Interfaces:**
- Consumes: `typesense30142.EmbedInput`, `typesense30142.Embedder` (Task 2).
- Produces: `EmbeddingClient.Embed(ctx, texts, input typesense30142.EmbedInput)` forwarding `"input_type"` in the request body.

- [ ] **Step 1: Verify the relay build breaks against new nostrlib**

Run: `go build ./...`
Expected: FAIL — `*EmbeddingClient does not implement typesense30142.Embedder (wrong type for method Embed)` at the `var _` assertion (embedding.go:32).

- [ ] **Step 2: Update the request struct and Embed signature**

`embedding.go` — add `InputType` to `embedRequest`:

```go
type embedRequest struct {
	Texts     []string `json:"texts"`
	InputType string   `json:"input_type"`
}
```

Change the method signature and body marshal (`embedding.go:54` and `:59`):

```go
// Embed computes embedding vectors for the given texts.
func (c *EmbeddingClient) Embed(ctx context.Context, texts []string, input typesense30142.EmbedInput) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}

	reqBody, err := json.Marshal(embedRequest{Texts: texts, InputType: string(input)})
```

(The rest of the method is unchanged.)

- [ ] **Step 3: Verify build + tests pass**

Run: `go build ./... && go test ./...`
Expected: PASS (calendar_rerank_test.go and the rest unaffected — they use a fake chunk searcher, not the Embedder).

- [ ] **Step 4: Commit**

```bash
git add embedding.go
git commit -m "feat(relay): EmbeddingClient sends input_type to embed service"
```

---

### Task 4: Indexer — embed `input_type` wire + query/passage threading + chunk 768-dim

**Repo:** `../../../../amb-indexer` (i.e. `/home/laoc/coding/edufeed/amb-indexer`)

**Files:**
- Modify: `embed.go:26-28` (request struct), `:41` + `:70` (Embed/embedBatch signatures)
- Modify: `worker.go:204` and `:333` (chunk pipeline → passage)
- Modify: `search.go:50` (embedderIface), `:106` (query path → query)
- Modify: `schema.go:34` (chunk `num_dim` → 768)
- Modify: `embed_test.go` (7 `Embed` call sites), `search_test.go` (fake embedder if present)
- Test: `embed_test.go`, `search_test.go`, `schema_test.go`

**Interfaces:**
- Produces: indexer-local `type EmbedInput string`; consts `EmbedQuery`/`EmbedPassage`; `(*Embedder).Embed(ctx, inputs, input EmbedInput)`; `embedderIface.Embed(ctx, inputs, input EmbedInput)`. Chunk schema `embedding` `num_dim: 768`.

- [ ] **Step 1: Update the embedder tests to the new signature (write failing)**

In `embed.go` add the role type near the top (after imports):

```go
// EmbedInput selects the e5 asymmetric role sent to the embed service.
type EmbedInput string

const (
	EmbedQuery   EmbedInput = "query"
	EmbedPassage EmbedInput = "passage"
)
```

In `embed_test.go`, update every `e.Embed(context.Background(), <texts>)` call (lines 48, 92, 126, 147, 164, 184, 208) to pass a role, e.g.:

```go
	out, err := e.Embed(context.Background(), []string{"hello"}, EmbedPassage)
```

Add one assertion test that the wire body carries `input_type` (place near the existing batch tests; reuse the test server pattern already in `embed_test.go`):

```go
func TestEmbedSendsInputType(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"embeddings":[[0.1]]}`))
	}))
	defer srv.Close()

	e := &Embedder{Endpoint: srv.URL}
	if _, err := e.Embed(context.Background(), []string{"x"}, EmbedQuery); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if !strings.Contains(gotBody, `"input_type":"query"`) {
		t.Fatalf("body missing input_type=query: %s", gotBody)
	}
}
```

(Ensure `net/http/httptest`, `io`, `net/http`, `strings` are imported in the test file.)

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd /home/laoc/coding/edufeed/amb-indexer && go test ./... 2>&1 | head -30`
Expected: FAIL — too many arguments to `Embed`, and `TestEmbedSendsInputType` references undefined behavior.

- [ ] **Step 3: Implement the wire change**

`embed.go` — add `InputType` to the request struct:

```go
type embedRequest struct {
	Texts     []string `json:"texts"`
	InputType string   `json:"input_type"`
}
```

Change `Embed` and `embedBatch` signatures to thread the role:

```go
func (e *Embedder) Embed(ctx context.Context, inputs []string, input EmbedInput) ([][]float32, error) {
```

In `Embed`, pass `input` into the `embedBatch` call inside the loop:

```go
		embs, err := e.embedBatch(ctx, batch, input)
```

And:

```go
func (e *Embedder) embedBatch(ctx context.Context, batch []string, input EmbedInput) ([][]float32, error) {
```

In `embedBatch`, set the field on marshal:

```go
	payload, err := json.Marshal(embedRequest{Texts: batch, InputType: string(input)})
```

- [ ] **Step 4: Thread roles through the indexer call sites**

`search.go:50` (interface) — add the param:

```go
	Embed(ctx context.Context, inputs []string, input EmbedInput) ([][]float32, error)
```

`search.go:106` (query path):

```go
	embs, err := q.Embedder.Embed(ctx, []string{req.Q}, EmbedQuery)
```

`worker.go:204` and `worker.go:333` (both chunk-pipeline passage embeds):

```go
		embeddings, eerr = w.Embedder.Embed(ctx, texts, EmbedPassage)
```

If `search_test.go` has a fake implementing `embedderIface`, update its `Embed` method to the new signature (ignore the new arg).

- [ ] **Step 5: Bump the chunk vector field to 768-dim**

`schema.go:34`:

```go
		{"name": "embedding", "type": "float[]", "num_dim": 768, "vec_dist_metric": "cosine"},
```

- [ ] **Step 6: Run tests to verify they pass**

Run: `cd /home/laoc/coding/edufeed/amb-indexer && go test ./...`
Expected: PASS.

- [ ] **Step 7: Commit (in the amb-indexer repo)**

```bash
cd /home/laoc/coding/edufeed/amb-indexer
git add embed.go embed_test.go search.go worker.go schema.go search_test.go
git commit -m "feat(indexer): send input_type to embed service; chunk embedding 768-dim"
```

---

### Task 5: Indexer — embedding-version in idempotency hash

**Repo:** `../../../../amb-indexer`

**Files:**
- Modify: `config.go:10-40` (add `EmbedModel`), `:94-108` (load it)
- Modify: `worker.go:90` and `:296` (fold version into hash)
- Test: `worker_test.go` (or `store_test.go`) — new test that a model change invalidates the hash

**Interfaces:**
- Consumes: `Config.EmbedModel` (env `EMBED_MODEL`, default `intfloat/multilingual-e5-base`).
- Produces: content hash = `sha256Hex(sourceURL + "\x00" + w.Cfg.EmbedModel)`; changing `EmbedModel` changes the stored hash for every event, forcing a re-chunk.

- [ ] **Step 1: Write the failing test**

Add to `worker_test.go`:

```go
func TestEmbeddingVersionChangesHash(t *testing.T) {
	src := "nostr:naddr1example"
	h1 := sha256Hex(src + "\x00" + "intfloat/multilingual-e5-base")
	h2 := sha256Hex(src + "\x00" + "sentence-transformers/paraphrase-multilingual-MiniLM-L12-v2")
	if h1 == h2 {
		t.Fatal("expected different idempotency hashes for different embed models")
	}
}
```

- [ ] **Step 2: Run to verify it passes trivially, then assert wiring via a guard test**

Run: `cd /home/laoc/coding/edufeed/amb-indexer && go test -run TestEmbeddingVersionChangesHash ./...`
Expected: PASS (it tests the hash formula directly). This locks the formula; Steps 3-4 wire it into the worker so production hashing uses it.

- [ ] **Step 3: Add the config field**

`config.go` — add to the `Config` struct (near `EmbedEndpoint`, line ~28):

```go
	EmbedModel    string
```

In `LoadConfig` (line ~102, beside `EmbedEndpoint`):

```go
		EmbedModel:        envDefault("EMBED_MODEL", "intfloat/multilingual-e5-base"),
```

- [ ] **Step 4: Fold the version into both hash sites**

`worker.go:90` (standard path):

```go
	hashHex := sha256Hex(sourceURL + "\x00" + w.Cfg.EmbedModel)
```

`worker.go:296` (content-direct path):

```go
	hashHex := sha256Hex(sourceURL + "\x00" + w.Cfg.EmbedModel)
```

- [ ] **Step 5: Run the full suite**

Run: `cd /home/laoc/coding/edufeed/amb-indexer && go test ./...`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
cd /home/laoc/coding/edufeed/amb-indexer
git add config.go worker.go worker_test.go
git commit -m "feat(indexer): fold embed model into idempotency hash to force re-chunk on model swap"
```

---

### Task 6: Stack wiring + docs

**Files:**
- Modify: `docker-compose.yml` (embed service: `EMBED_MODEL`, `mem_limit`, `start_period`, cache comment)
- Modify: `CLAUDE.md` (embed model references)
- Modify: `../../../../nostrlib/eventstore/typesense30142/README.md:191,202,205` (768-dim notes)
- Modify: `../../../../amb-indexer/.env.indexer.example` (document `EMBED_MODEL`)
- Test: `docker compose config` (compose file parses)

**Interfaces:**
- Consumes: the runtime envs read by Tasks 1 and 5.

- [ ] **Step 1: Update the embed service in `docker-compose.yml`**

In the `embed:` service block, set the model and raise the ceiling (e5-base resident ≈1.1–1.5 GB vs MiniLM ≈0.12 GB):

```yaml
    environment:
      - EMBED_TOKEN=${EMBED_TOKEN}
      - EMBED_MODEL=intfloat/multilingual-e5-base
```

Replace the `mem_limit`/comment block:

```yaml
    # e5-base is ~1.1 GB resident; activations add headroom. The 2026-06-11
    # runaway (retry-amplification, since fixed in nostrlib replace.go) hit
    # ~2.0 GB RSS, so keep a hard ceiling but above the model footprint.
    mem_limit: 3g
    mem_reservation: 1g
```

Raise the healthcheck `start_period` (first boot downloads ~1.1 GB):

```yaml
      # First boot pulls ~1.1 GB (e5-base) and loads it; allow a long cold start.
      start_period: 300s
```

- [ ] **Step 2: Update docs**

- `CLAUDE.md`: in the embed paragraph, replace `paraphrase-multilingual-MiniLM-L12-v2` (384-dim) with `intfloat/multilingual-e5-base` (768-dim), and note the `input_type` (`query:`/`passage:`) prefix contract.
- `../../../../nostrlib/eventstore/typesense30142/README.md`: lines ~191/202/205 — change `384` → `768` and the model name; note the `num_dim` must match the embed model.
- `../../../../amb-indexer/.env.indexer.example`: add `EMBED_MODEL=intfloat/multilingual-e5-base` with a comment that it must match the embed service so the idempotency hash invalidates on a model swap.

- [ ] **Step 3: Verify compose parses**

Run: `docker compose config >/dev/null && echo OK`
Expected: `OK`.

- [ ] **Step 4: Commit (split across repos)**

```bash
git add docker-compose.yml CLAUDE.md
git commit -m "chore(embed): wire e5-base model + raise mem ceiling; docs"
cd /home/laoc/coding/edufeed/nostrlib && git add eventstore/typesense30142/README.md && git commit -m "docs(typesense30142): embedding field is 768-dim (e5-base)"
cd /home/laoc/coding/edufeed/amb-indexer && git add .env.indexer.example && git commit -m "docs(indexer): document EMBED_MODEL for hash invalidation"
```

---

### Task 7: Integration & dev migration runbook (OPERATOR-GATED)

> Every step here is a shared-infra action. Present it to the user and get an explicit go-ahead before running it. Do NOT auto-run. Keep all credentials server-side; never print them.

**Files:**
- Modify: `go.mod` (relay nostrlib pseudo-version), `go.mod` tidy
- Create: `scripts/validate_e5.py` (throwaway validation; remove after)

**Interfaces:**
- Consumes: all prior tasks landed and committed locally.

- [ ] **Step 1: Push nostrlib, then bump the relay go.mod**

Operator-gated. After the user approves pushing the nostrlib commits to git.edufeed.org:

```bash
cd /home/laoc/coding/edufeed/nostrlib && git push   # operator-approved
GOWORK=off go list -m git.edufeed.org/edufeed/nostrlib@latest   # capture pseudo-version
```

Update the `replace` directive in the relay `go.mod` with the captured version, then:

```bash
# from the relay worktree root (.../amb-relay/.claude/worktrees/profile-fallback)
GOWORK=off go mod tidy && GOWORK=off go build .
```

Expected: standalone build succeeds (deployment-faithful). The indexer needs no nostrlib bump (it does not consume the changed interface or the AMB schema).

- [ ] **Step 2: Deploy the embed service with e5-base (dev)**

Operator-gated (homelab ansible — sandbox disabled for that call). Redeploy the dev embed container so it pulls e5-base. Verify:

```bash
# server-side; ETOK read inline, never printed
curl -s -H "Authorization: Bearer $ETOK" http://<embed-ip>:8100/health
```

Expected: `{"status":"ok","model":"intfloat/multilingual-e5-base","dimensions":768}`.

- [ ] **Step 3: Recreate + re-embed the AMB collection (dev)**

Operator-gated. Trigger the relay NIP-86 `reindex` (drops + recreates the AMB schema at 768-dim and re-embeds every kind-30142 event via the write-path). Poll `getreindexstatus` to completion.

- [ ] **Step 4: Drop + re-chunk the chunk collection (dev)**

Operator-gated. Drop `amb_chunks_30142` (dimension change); the indexer's `EnsureCollection` recreates it at 768-dim. Reset the indexer cursor so it rescans all events; the embedding-version hash (Task 5) forces a re-chunk/re-embed of every event. Confirm the indexer's EMBED_MODEL matches the service (`intfloat/multilingual-e5-base`).

- [ ] **Step 5: Validate retrieval (dev)**

Create `scripts/validate_e5.py` mirroring the diagnostic used to find the root cause: embed each query via the dev embed service with `input_type=query`, run pure-vector KNN over `amb_chunks_30142` filtered to `kind:=31923`, and report SCALE-UP's rank (event_id prefix `9ded31a4` / `055e5630`).

Pass criteria (run server-side, creds inline):
- `"Aktivierung Studierende"` → SCALE-UP rank ≤ 10 (was: not in top 50).
- `"kooperatives Lernen Hochschule"` → SCALE-UP rank ≤ 10 (was: not in top 50).

Also re-run the `zzraw` topic+time REQ against `wss://dev.amb-relay.edufeed.org`:
`{"kinds":[31923],"search":"Aktivierung Studierende","#start_after":["<now>"],"#start_before":["<now+28d>"],"limit":10}`
Expected: SCALE-UP returned in-window.

- [ ] **Step 6: Clean up and report**

Remove `scripts/validate_e5.py` and any `zz*` debug dirs. Report before/after ranks to the user. Prod cutover (spec §7) remains a separate, later, operator-gated action.

---

## Notes for the executor

- The embed pytest session fixture loads the real model — first run downloads e5-base (~1.1 GB). The prefix tests monkeypatch `encode`, so they don't depend on model output; only the dim tests need the live model.
- nostrlib changes are committed in the nostrlib repo, not the relay worktree. The relay picks them up immediately via `go.work` for local builds; the `go.mod` bump (Task 7) is only for deployment.
- The indexer is independent of the nostrlib interface change — do not bump its go.mod for this work.
