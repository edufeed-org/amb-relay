# P0.2: relay EOSE-on-search-failure fix

## Root-cause trace

### What already works (verified empirically, not assumed)

khatru's REQ handler (`nostrlib/khatru/handlers.go:295-324`) always sends
`EOSEEnvelope` after the per-filter loop **unless** `handleRequest` returns a
non-nil `error` — and `handleRequest` (`nostrlib/khatru/responding.go:11-37`)
only ever returns non-nil when the `OnRequest` hook rejects the filter. A
search-backend failure inside `rl.QueryStored`'s `iter.Seq` is **not** an
`error` in Go's sense — it's an iterator that just stops yielding — so it
cannot make `handleRequest` return an error. Concretely, in
`typesense30142/query.go:182-186`:

```go
nostrsearch, err := ts.SearchResourcesWithLimitAndFilter(searchStr, limit, nostrFilterBy)
if err != nil {
    log.Printf("Search failed: %v", err)
    return   // yields nothing, returns immediately
}
```

`SearchResourcesWithLimitAndFilter` (`query.go:456-458`) checks
`resp.StatusCode != http.StatusOK` and returns an error immediately for a
Typesense 503 "Not Ready or Lagging" response — before touching the response
body. I wrote `query_failure_repro_test.go::TestSearchBackendFailureStillTerminatesREQ`
(real `typesense30142.TSBackend` + real `khatru.Relay` + real
`nostr.RelayConnect` websocket client, backend host = an `httptest.Server`
that always answers 503) and it **passes on unmodified code**: EOSE arrives
immediately. I extended this to
`TestRegistryFanoutFailureStillTerminatesREQ` (this repo's `registry.fetch`,
`content_registry.go`, fanning a kind-less filter across 4 registered content
types, all backed by the same failing host) — also **passes on unmodified
code**. So the literal "TS returns 503 → client hangs forever" framing, taken
as a synchronous fast error, is **not** reproducible against the current
khatru/typesense30142/registry code: a clean, fast backend error already
terminates the REQ correctly. This needed saying explicitly since the task
brief assumed the opposite.

### The actual defect: unbounded SERIAL fan-out latency

`registry.fetch` (`content_registry.go:105-120`) iterates
`registry.selected(filter)` **serially** — for a kind-less filter (a broad
client search, or a Negentropy sync with no `search`/tag terms) that's
*every* registered content type, one after another:

```go
for _, i := range r.selected(filter) {
    for ev := range r.types[i].fetch(filter, maxLimit) { ... }
}
```

Each `contentType.fetch` is a `typesense30142.TSBackend.QueryEvents`, and its
underlying HTTP call is bounded only by `defaultHTTPClient.Timeout` = 30s
(`typesense30142/typesense.go:15,24-31`) — not by the incoming REQ's
context (`makehttpRequest`, `typesense.go:303-327`, uses `http.NewRequest`,
not `NewRequestWithContext`; `reqCtx` in `handlers.go:299` also carries no
deadline of its own, only cancellation on disconnect). Typesense's own
"Not Ready or Lagging" state is a **degraded/backed-up** condition — in
production it can genuinely take a while to answer, not just fail instantly.
This relay registers one content type per kind family (`main.go`: AMB,
long-form, wiki, calendar, profiles, community shares, transferkiosk,
publications — 8 today), **all pointed at the same single Typesense
instance** (`docker-compose.yml`). So a kind-less REQ during a real
degraded-Typesense outage could serially accumulate up to
`8 × 30s ≈ 4 minutes` before khatru's post-loop `EOSEEnvelope` write — far
past what any real nostr client waits before giving up, which is exactly
what "hangs forever" looks like from the client's side even though the relay
is, strictly, still working. I proved this is real and reproducible in
`TestSlowSearchBackendFanoutTerminatesWithinBudget` (see red/green transcript
below): 5 registered types, each backend sleeping 700ms before answering
(unbounded serial worst case ≈ 3.5s) — with no cap, a 2s client deadline is
missed.

## What I changed and why there

Fix lives entirely in **this repo** (`amb-relay`), not nostrlib — the defect
is in this repo's own multi-collection fan-out architecture
(`content_registry.go`), which nostrlib/khatru has no knowledge of.
nostrlib's `handlers.go`/`responding.go` already do the right thing given
what they're handed; I did not touch nostrlib.

- **`query_budget.go`** (new): `boundedSeq(seq iter.Seq[nostr.Event], budget time.Duration) iter.Seq[nostr.Event]`.
  Runs the wrapped sequence in its own goroutine, races it against a
  `time.Timer`, and stops waiting once `budget` elapses — the consumer
  (khatru's `handleRequest` for-range, which drives EOSE) gets only what
  arrived in time. The producer goroutine is told to stop via a `stop`
  channel and exits on its own once the underlying (already bounded, just
  slow) call returns; no goroutine leak, no change to `content_registry.go`'s
  ordering/dedup semantics.
- **`main.go`**: `relay.QueryStored` now wraps its final `iter.Seq` (from
  either `reg.fetch` or `calendarRerankQuery`) in `boundedSeq(..., queryFetchBudget)`.
  This is the single choke point regardless of which branch (chunk-rerank vs
  plain) or how many content types get added in the future — the guarantee
  is structural, not per-backend. `queryFetchBudget` defaults to
  `DefaultQueryFetchBudget = 20s` (comfortably under the sum of even 2
  backends' own 30s timeout, generous enough not to truncate a single
  legitimately slow-but-working collection), configurable via
  `QUERY_FETCH_TIMEOUT_MS` (same convention as `CHUNK_RERANK_TIMEOUT_MS`).
- **`.env.example`** / **`CLAUDE.md`**: documented `QUERY_FETCH_TIMEOUT_MS`.

I did **not** change `content_registry.go`'s serial-vs-parallel fan-out
strategy (a more invasive fix would parallelize `registry.fetch` across
content types to cut the worst case from `N×timeout` to `~1×timeout`) — the
task asked for a minimal hardening fix ahead of a cutover, and a global
budget wrapper achieves the actual requirement ("REQ handshake always
terminates") without touching registry semantics, ordering, or dedup. Noted
here as a follow-up worth considering if 20s still proves too slow under
real multi-collection degraded conditions.

## Test transcript: red → green

`TestSlowSearchBackendFanoutTerminatesWithinBudget` — 5 registered content
types, backend sleeps 700ms/request (unbounded serial worst case ≈ 3.5s),
2s client deadline.

**Red** (temporarily called `boundedSeq(reg.fetch(filter, 250), 0)` — `0`
budget means "no cap" per `boundedSeq`'s `if budget <= 0 { return seq }`,
i.e. today's un-hardened behavior):

```
=== RUN   TestSlowSearchBackendFanoutTerminatesWithinBudget
    query_failure_repro_test.go:225: timed out waiting for EOSE — search
    fetch budget did not terminate the REQ in time (this is the 07-24
    outage symptom: client hangs)
--- FAIL: TestSlowSearchBackendFanoutTerminatesWithinBudget (2.11s)
FAIL
```

**Green** (restored `boundedSeq(reg.fetch(filter, 250), budget)`, `budget = 500ms`):

```
=== RUN   TestSlowSearchBackendFanoutTerminatesWithinBudget
    query_failure_repro_test.go:218: EOSE arrived after 500.99ms (budget
    500ms, unbounded serial worst case ~3.5s)
--- PASS: TestSlowSearchBackendFanoutTerminatesWithinBudget (0.70s)
```

Also added (both pass on unmodified AND fixed code — they document that the
literal fast-503 case was never broken):
- `TestSearchBackendFailureStillTerminatesREQ` — single real `TSBackend`, 503.
- `TestRegistryFanoutFailureStillTerminatesREQ` — 4-type registry, all 503.

Full suite after the fix:

```
$ GOWORK=off GOCACHE=$PWD/.gocache go build ./... && go vet ./... && go test ./... -count=1 -timeout 300s
ok  	github.com/edufeed-org/amb-relay	1.149s
?   	github.com/edufeed-org/amb-relay/cmd/hydrate-bolt	[no test files]
ok  	github.com/edufeed-org/amb-relay/internal/hydrate	0.005s
```

`gofmt -l main.go query_budget.go query_failure_repro_test.go` — clean.

## nostrlib

Not touched. `git status --short` in `/home/laoc/coding/edufeed/nostrlib` is
clean. The defect is architecturally local to this repo's multi-collection
registry, which nostrlib doesn't know about; khatru's EOSE-after-loop
contract is correct given what it's handed.

## Files changed

- `main.go` — wrap `relay.QueryStored`'s result in `boundedSeq`; parse
  `QUERY_FETCH_TIMEOUT_MS`.
- `query_budget.go` (new) — `boundedSeq` + `DefaultQueryFetchBudget`.
- `query_failure_repro_test.go` (new) — 3 tests (2 fast-503 regression
  checks, 1 red/green slow-fan-out reproduction).
- `.env.example`, `CLAUDE.md` — document the new env var.
