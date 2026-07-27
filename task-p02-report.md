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

## Follow-up: bound COUNT, explicit budget-disable, pin single-backend hang

Three items from review, applied on top of the above.

### 1. Bound the COUNT path

`relay.Count = reg.count` (`main.go`, was line ~1039) had the exact same
hazard as the unhardened `QueryStored`, one envelope over: `registry.count`
(`content_registry.go:155-165`) loops `r.selected(filter)` **serially**,
calling each selected content type's own `count`, each bounded only by its
own HTTP client timeout. A kind-less COUNT during a degraded/lagging
Typesense can stack the same `N × (per-call timeout)` stall that
`boundedSeq` was written to fix for REQ.

Checked khatru's actual contract before choosing a shape
(`nostrlib/khatru/relay.go:83`, `responding.go:39-58`): `Count` is
`func(ctx, filter) (uint32, error)`; `handleCountRequest` sends `err.Error()`
as a `NOTICE` when non-nil and **still replies with a `CountEnvelope`**
carrying whatever `uint32` was returned (there's no path that lets an error
silently hang the COUNT round-trip — khatru already replies gracefully as
long as `Count` itself returns). That meant a straight port of `boundedSeq`'s
approach was safe: `boundedCount(fn func(nostr.Filter) (uint32, error),
budget time.Duration) func(nostr.Filter) (uint32, error)` (`query_budget.go`)
runs `fn` in a goroutine against a buffered result channel (capacity 1, so
the goroutine can never block on send even after the caller gives up — no
leak, mirroring `boundedSeq`'s `stop`-channel discipline via a different
mechanism suited to a single-shot, non-streaming call) and races it against
a timer. On timeout it returns `(0, fmt.Errorf("count timed out after %s",
budget))` — unlike a query result there is no partial value to stream back,
so zero-plus-error is the only honest answer, and khatru turns that into
exactly the graceful NOTICE+CountEnvelope(0) response the code already
supports. `budget<=0` returns `fn` unchanged, matching `boundedSeq`'s
convention.

Wired in `main.go`: `boundedRegCount := boundedCount(reg.count,
queryFetchBudget)`, then `relay.Count` calls through it. Same env-derived
budget value as `QueryStored` — no separate knob, since they share the exact
fan-out shape and there's no reason to tune them independently.

### 2. Budget disable semantics

Before: `QUERY_FETCH_TIMEOUT_MS=0` parsed fine (`strconv.Atoi` succeeds) but
failed the `n > 0` guard, so it silently fell back to the 20s default —
indistinguishable from leaving the variable unset, even though `boundedSeq`
(and now `boundedCount`) both already treat `budget<=0` as "no cap" and were
fully able to honor an explicit opt-out.

Fix is a one-character guard change, `n > 0` → `n >= 0`, in both places
`main.go` parses the var (the `QUERY_FETCH_TIMEOUT_MS` block feeding
`queryFetchBudget`). Semantics now: unset (empty string) → 20s default;
negative or non-numeric → 20s default (invalid input, not a valid opt-out
spelling); `"0"` → explicit unbounded, passed straight through as
`time.Duration(0)`, which both wrappers already treat as "run the wrapped
call directly, no timer." Documented in `.env.example` and `CLAUDE.md`
(both updated to state the `"0"` vs. unset distinction explicitly, since it's
exactly the kind of footgun a future reader would silently reintroduce
without the callout).

### 3. Pin the adjudicated incident shape

Added `TestSingleBackendSlowHangTerminatesWithinBudget`
(`query_failure_repro_test.go`): a registry with **one** registered content
type (`kinds:[30142]`, no fan-out at all — deliberately not the multi-type
registry the other tests in this file use) whose `httptest.Server` sleeps
2s (no error, no 503 — just slow) before answering 200 with zero hits, wired
exactly as `main.go` wires production (`relay.QueryStored` wraps
`reg.fetch`'s result in `boundedSeq`). Budget 200ms. Client still receives
`EndOfStoredEvents` at ~200ms, not at 2s. This isolates the "one slow
backend, one collection, no serial multiplier" mechanism from the
multi-collection serial-stacking mechanism the pre-existing tests
(`TestSlowSearchBackendFanoutTerminatesWithinBudget`, etc.) cover — i.e. the
literal incident shape: even a single unresponsive collection, on its own,
is enough to hang a client waiting on EOSE without the budget wrapper.

Also added, for the COUNT-side helper directly (unit-level, no
websocket/khatru harness needed since `boundedCount` has no dependency on
either): `TestBoundedCountTerminatesWithinBudget` (slow `fn`, budget 100ms,
backend 2s → returns within budget with a timeout error and `n=0`),
`TestBoundedCountPassesThroughFastResult` (fast `fn` → real value, no
error), `TestBoundedCountPropagatesUnderlyingError` (fast, clean error from
`fn` → propagated unchanged, not masked by the wrapper), and
`TestBoundedCountZeroBudgetDisables` (`budget=0` → `fn` called exactly once,
untouched — confirms the disable path doesn't add a no-op timer or double
invocation). New file: `query_budget_test.go`.

### Full suite after this follow-up

```
$ GOWORK=off GOCACHE=$PWD/.gocache go build ./... && go vet ./... \
    && go test ./... -count=1 -timeout 300s
ok  	github.com/edufeed-org/amb-relay	3.255s
?   	github.com/edufeed-org/amb-relay/cmd/hydrate-bolt	[no test files]
ok  	github.com/edufeed-org/amb-relay/internal/hydrate	0.004s
```

Targeted run of the new/changed tests (`-run
'TestBoundedCount|TestSingleBackendSlowHang|TestSlowSearchBackendFanout'
-v`): all 6 pass, including the two pre-existing slow-fan-out tests
(unaffected by this follow-up's changes).

`gofmt -l main.go query_budget.go query_budget_test.go
query_failure_repro_test.go` — clean.

### Files changed (this follow-up)

- `query_budget.go` — add `boundedCount`.
- `query_budget_test.go` (new) — 4 unit tests for `boundedCount`.
- `query_failure_repro_test.go` — add
  `TestSingleBackendSlowHangTerminatesWithinBudget`.
- `main.go` — wrap `relay.Count` with `boundedCount`; fix
  `QUERY_FETCH_TIMEOUT_MS` parse guard (`n > 0` → `n >= 0`) so `"0"` is a
  real opt-out.
- `.env.example`, `CLAUDE.md` — document COUNT bounding and the `"0"` vs.
  unset disable semantics.
