# Write-path durability fix bundle — implementation report

Branch: `fix/write-path-durability` (worktree `.claude/worktrees/write-path`, based on `dev` @ `940f3e9`, matching the deployed rev in the root-cause report).

Source: `.superpowers/sdd/rootcause-30142-drop.md`.

## 1. Graceful shutdown (`main.go`, new `server.go`)

- `main.go`: replaced the bare `http.ListenAndServe(":"+port, relay)` with an `*http.Server{Addr: ":"+port, Handler: relay}`, `signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)`, and a call to the new `runServer(srv, sigCh, shutdownDrainTimeout)` (`shutdownDrainTimeout = 25s`). `runServer` returning lets `main()` fall through to its existing `defer tsBuf.Close()` / `defer boltBuf.Close()` (and every other deferred cleanup), which is the actual fix — those defers never ran on SIGTERM before.
- New `server.go`: `runServer(srv gracefulServer, sigCh <-chan os.Signal, shutdownTimeout time.Duration) error`. `gracefulServer` is a 2-method interface (`ListenAndServe`/`Shutdown`) satisfied by `*http.Server`, extracted specifically so the signal→Shutdown path is unit-testable without binding a real listener. On signal, it calls `srv.Shutdown(ctx)` with the bounded context and waits for the `ListenAndServe` goroutine to actually return before returning itself, so callers can rely on "runServer returned" meaning "listener fully stopped."
- Verified the existing defer ordering claim in the root-cause report: `tsBuf.Close()` is deferred first (main.go), `boltBuf.Close()` second — LIFO means boltBuf drains before tsBuf, which the existing comment explains and is unchanged/correct.
- Added `CLAUDE.md` note under "Build & Run Commands" (new "Deployment" subsection): `stop_grace_period` should be ≥30s, and ideally ~60s given `shutdownDrainTimeout` (25s) plus the buffer-drain retry budget (~31s worst case via `upsertHTTPMaxRetries`/`upsertHTTPMaxBackoff`) both run in that window. Docker-compose/Ansible template changes are explicitly out of scope here (per task) — this is documentation of the requirement only.

**Test:** `server_test.go` — `TestRunServer_SignalTriggersShutdown` (signal → `Shutdown` called, `runServer` returns nil) and `TestRunServer_ListenErrorReturnsWithoutSignal` (a listener error surfaces without needing a signal, so `main()` can't hang). Both use a fake `gracefulServer`, no real network.

## 2. Batched embeds in the flush path

**Where the seam ended up:** entirely in `amb-relay`, NOT in nostrlib. The relay already exports what's needed: `typesense30142.NostrToAMB` (exported), `TSBackend.Embedder`/`TSBackend.EmbedFields` (exported fields), and the relay already carries its own duplicate of nostrlib's field-concatenation logic (`BuildEmbedText` in `embedding.go`, mirroring nostrlib's private `buildEmbedText`). So instead of touching nostrlib's `PrepareBatch` (which loops `ts.Embedder.Embed(ctx, []string{embedText}, EmbedPassage)` once per event — the actual root cause per `replace.go:173-185`), I added a relay-side `prepareBatch(tsDB *typesense30142.TSBackend, events []nostr.Event)` in `buffer.go` that:
1. Converts every event via `typesense30142.NostrToAMB` (same per-event conversion, same error collection contract as nostrlib's `PrepareBatch`).
2. Builds each doc's embed text via the existing `BuildEmbedText`.
3. Batches all non-empty texts into chunks of `maxEmbedBatchSize = 32` and issues ONE `Embedder.Embed` call per chunk (`ceil(N/32)` calls total instead of N), assigning the returned vectors back onto the corresponding docs.
4. On a chunk's embed failure, logs a warning and leaves that chunk's docs without a vector (same graceful-degradation contract as before) rather than failing the batch.

`productionWriter.Upsert` (buffer.go) now calls `prepareBatch(p.tsDB, events)` instead of `p.tsDB.PrepareBatch(events)`. Nothing in nostrlib changed — no push/bump needed.

**Test:** `buffer_embed_test.go` — `TestPrepareBatch_BatchesEmbedCalls` uses a `countingEmbedder` fake and 100 synthetic AMB events; asserts `embedder.callCount() == ceil(100/32) == 4` (not 100) and that every doc received an embedding. `TestPrepareBatch_NoEmbedderSkipsEmbedding` guards the no-embedder configuration (oersi-style deployments) stays a no-op.

## 3. Stop force-flushing 1-event batches for stamp tasks

`buffer.go`'s `run()` now tracks `pendingDocIDs map[string]struct{}` — the Typesense doc id of every event currently sitting unflushed in `batch` (populated by a new `addToBatch` helper via the existing `tsDocIDFromEvent`, cleared whenever the batch flushes).

- **Stamp (`QueueCommunityPatch`) tasks**, both in the live loop and the shutdown `drain()`: now check `pendingDocIDs[task.Stamp.DocID]`. Force a flush ONLY if that specific doc's own upsert is still pending; otherwise apply the `PatchCommunity` call directly with no flush at all, letting the batch ride its normal `batchSize`/`flushInterval` cadence. This is safe because the correctness requirement is narrower than "flush everything": the patch only races the *next* flush of *that doc's own* upsert (which re-derives `community` from the event's own h-tags and would clobber the patch if it lands after). If the doc already flushed earlier (the common case once the CommunityStamper's async `go stamper.reconcile(...)` — see `main.go`/`community_stamp.go` — completes its own Typesense query round-trip), there's nothing to race and no reason to force an unrelated flush.
- **Content (`QueueContent`) tasks: unchanged, deliberately.** The task's own event is *always* the thing just appended to the batch, so it is always "pending" by construction — there is no cadence to ride without breaking the upsert-before-patch guarantee the 404-repro test locks in. Kept the existing forced `flushBatch()` there.
- The `flushInterval` ticker (unchanged, 500ms in production via `NewTSWriteBuffer(&tsDB, 100, 500*time.Millisecond)` in `main.go`) still bounds worst-case latency for whatever partial batch is left once the queue goes idle — this is what "ride cadence unless idle" resolves to structurally, without adding a redundant explicit idle/channel-length check (kept it simple per repo convention of preferring the existing mechanism over new machinery).

**Tests (`buffer_test.go`):**
- Renamed `TestTSWriteBuffer_CommunityPatchForcesFlushThenPatch` → `TestTSWriteBuffer_CommunityPatchForcesFlushWhenPending`, retargeted at a stamp whose docID is one of the pending events' own doc id (previously used an unrelated synthetic string, which no longer exercises the pending-check correctly) — still asserts flush-before-patch ordering.
- New `TestTSWriteBuffer_CommunityPatchSkipsFlushWhenNotPending`: queues 2 events (not flushed, long ticker), sends a stamp for an unrelated (non-pending) doc id, asserts **zero** upsert batches occurred and the patch was still recorded.
- New `TestTSWriteBuffer_StampsUnderLoadFlushFarLessThanN`: batchSize=10, queues 10 events (auto-flushes once at the size threshold), then sends 10 stamp tasks (one per event, mirroring the real content-write→reconcile→QueueCommunityPatch flow) targeting those now-already-flushed docs. Asserts `communityPatches == 10` but `upsertBatches == 1` — flush count far below N, matching the task's acceptance criterion.
- `TestTSWriteBuffer_ContentTaskForcesFlushThenPatch` (the existing 404-repro guard) is untouched and still green.

## Test evidence

```
GOWORK=off GOCACHE=$PWD/.gocache go build ./...   # clean
GOWORK=off GOCACHE=$PWD/.gocache go vet ./...      # clean
GOWORK=off GOCACHE=$PWD/.gocache go clean -testcache && go test ./... -v
# ok  	github.com/edufeed-org/amb-relay	3.274s   (166 test funcs, all PASS)
# ok  	github.com/edufeed-org/amb-relay/internal/hydrate	0.006s
gofmt -l buffer.go buffer_test.go buffer_embed_test.go server.go server_test.go main.go   # no output — clean
```

`management.go` shows up under a bare `gofmt -l .` but was already unformatted before this branch (confirmed via `git stash`), untouched here — not part of this change.

## Concerns / follow-ups (not blocking this bundle)

- This bundle does not add durability to the in-memory queue itself (Bolt-backed queue was explicitly called "optional hardening, not necessary given 1+3" in the root-cause report) — a crash (not a graceful SIGTERM) still loses whatever is queued. `HYDRATE_ON_START=true` (ops-side, already existing machinery) remains the recommended mitigation for that residual risk, per the report's own recommendation #1.
- `shutdownDrainTimeout` (25s) only bounds the HTTP `Shutdown` call (draining in-flight connections); it does not bound the write-buffer close that runs after it returns. Actual worst-case total shutdown time is closer to ~55s (25s + ~31s buffer-drain retry budget) — reflected in the CLAUDE.md note recommending ~60s `stop_grace_period` where the orchestrator allows it, even though the task asked for a ≥30s floor.
- The `docker-compose.yml` `stop_grace_period` value itself was intentionally NOT changed here (task scoped that to CLAUDE.md documentation; the homelab/compose template change is tracked separately).

## 4. Cutover-gating review fixes: panic-safe queues + bounded drain deadlines

**CRITICAL — send-on-closed-channel panic during drain.** The prior drain design closed the producer channel (`close(b.ch)`) from the consumer side once `Close()` was called, then ranged over it to drain remaining items. Any producer still alive past `Close()` — a hijacked websocket goroutine (`http.Server.Shutdown` doesn't wait for hijacked conns), an in-flight `CommunityStamper.reconcile` goroutine (runs up to ~60s), or a late NIP-86 `setcontent` — that called `Queue`/`QueueContent`/`QueueCommunityPatch` (buffer.go) or `Queue` (bolt_buffer.go) after that close would panic.

Fix, both buffers: **never close the producer channel.** Added an `atomic.Bool` `closed` flag, set-then-drain in `Close()` (`b.closed.Store(true)` strictly before `close(b.done)`). `Queue*` methods check the flag first and log-and-drop instead of sending when set. Since the channel is never closed, a call that read `closed==false` a moment before the `Store` and is concurrently blocked on `b.ch <- task` still succeeds — there is no closed channel for that send to land on, so it can never panic; the task is picked up by the drain loop like any other queued item (or, in the rare case it lands after drain has already concluded the channel empty, sits unconsumed until process exit — harmless, verified below). `run()`'s drain path switched from `for task := range b.ch` (closed-channel semantics) to a `select`/`default` loop reading the still-open channel.

Verified the OK-path reasoning the task asked me to check rather than assume: `khatru/adding.go`'s `handleNormal` turns a non-nil `StoreEvent`/`ReplaceEvent` error into `OK=false` sent to the client. Previously `boltBuf.Queue` was fire-and-forget (void) and `main.go`'s `relay.StoreEvent`/`ReplaceEvent` always returned `nil` regardless — so a dropped enqueue would have produced a false-positive `OK=true`. Changed `BoltWriteBuffer.Queue` to return `bool` (queued/dropped), and wired both call sites in `main.go` to return a retryable error (`"error: relay is shutting down, please retry"`) when the buffer reports a drop — making "no OK → client retries" actually true. `TSWriteBuffer.Queue*` methods stay void: the event is already durable in BoltDB (or, for content/community patches, is a search-index refresh) by the time it reaches the Typesense buffer, so no OK-path implication applies there.

**IMPORTANT — bounded drain deadlines.** Added `tsDrainDeadline = 90 * time.Second` (buffer.go) and `boltDrainDeadline = 45 * time.Second` (bolt_buffer.go), both overridable per-instance (`drainDeadline` field) for tests. Rather than only checking the deadline between drained items (which wouldn't bound a single writer call that never returns — e.g. a genuinely wedged Typesense/BoltDB with no client-side timeout), each drained task (and the final flush) runs through a new `runBounded(deadline, fn)` helper that executes `fn` in a goroutine raced against the deadline; on timeout the goroutine is abandoned (leaked — there's no way to preempt a synchronous HTTP/BoltDB call, and the process is exiting anyway) and the remaining queue depth is logged. This bounds the review's cited nested-retry worst case (~12.7min/flush against a hanging Typesense, from many stamp/content tasks each forcing their own ~31s-retry flush) down to the deadline constant.

Updated CLAUDE.md's "Deployment: `stop_grace_period`" section: 60s remains the right common-case target (queue empty/near-empty at shutdown resolves in low single-digit seconds), but it is not a worst-case bound — combined with `shutdownDrainTimeout` (25s) and LIFO close order (`boltBuf` drains first, then `tsBuf`), the theoretical worst case if both buffers hit their full deadline is ~25s + 45s + 90s ≈ 160s. That's an accepted trade-off (SIGKILL can still cut a worst-case drain short), not a regression: whatever's abandoned is logged with queue depth and recovered via reindex/`HYDRATE_ON_START`.

**Tests added** (`buffer_test.go`, `bolt_buffer_test.go`), both buffers:
- `Test*Buffer_QueueAfterCloseDropsWithoutPanic` — Queue/QueueContent/QueueCommunityPatch (or Queue, bolt) called after `Close()` has fully returned: no panic, dropped (bolt: returns `false`), nothing reaches the writer.
- `Test*Buffer_HammerCloseNoPanic` — N=20 goroutines spam `Queue` while `Close()` runs concurrently; asserts no panic via `recover()`. Runs clean under `-race` (verified 90/90 iterations across both buffers, `-count=15`).
- `Test*Buffer_DrainDeadlineHonored` — a writer/boltDB fake that blocks forever; asserts `Close()`/`drain()` returns within the (overridden, millisecond-scale) deadline and logs a "drain deadline" line. The bolt version calls `drain()` directly (bypassing `Close()`/`run()`) to sidestep an inherent scheduling race: BoltDB processes ops one at a time with no batching, so if the live loop's normal (unbounded) path happened to dequeue the op before `Close()` ran, the call would hang outside the deadline-bounded drain path entirely — a pre-existing, out-of-scope property of an already-in-flight op (bounded only by `process()`'s own ~31s retry budget, not the new deadline). The TS version doesn't need this workaround: a single plain `Queue()`'d event with `batchSize>1` never triggers the writer from the live loop (it only appends to the in-memory batch below the size threshold), so the blocking call deterministically only happens inside `drain()`'s final flush.

**-race and the pre-existing nostrlib checkptr bug**, per the task's heads-up: confirmed independently — `nostr.Event.Sign`/`SetID` (via `serializedHash`/`writeJSONString`, event.go) intermittently trips `checkptr: pointer arithmetic result points to invalid allocation` under `go test -race`, even in single-threaded, pre-existing tests unrelated to this change (reproduced on `TestBoltBuffer_WorkerContinuesAfterPermanentRejection` and on early drafts of the new tests that still called the Sign-based `mkEvent` helper). This is a nostrlib bug, not a concurrency bug in the buffers — not fixed here (out of scope, upstream). Worked around it for the new tests specifically: added `unsignedEvent(sk, d)` in `buffer_test.go`, which builds a `nostr.Event` with a synthetic `[32]byte` ID and no signature, never calling `Sign`/`SetID`. The buffer/Queue machinery under test doesn't validate signatures or recompute IDs, so this is sufficient. All six new tests (three per buffer) now use it and pass cleanly under `-race` across many repeated runs; the broader pre-existing test suite (untouched) may still occasionally trip the unrelated nostrlib bug under `-race` and was therefore verified via the plain (non-race) `go test ./...` run below, per repo convention.

### Test evidence

```
GOWORK=off GOCACHE=$PWD/.gocache go build ./...   # clean
GOWORK=off GOCACHE=$PWD/.gocache go vet ./...      # clean
GOWORK=off GOCACHE=$PWD/.gocache go clean -testcache && go test ./...
# ok  	github.com/edufeed-org/amb-relay	3.565s   (192 test funcs, all PASS)
# ok  	github.com/edufeed-org/amb-relay/internal/hydrate	0.008s
GOWORK=off GOCACHE=$PWD/.gocache go test -race -run 'TestTSWriteBuffer_(HammerCloseNoPanic|QueueAfterCloseDropsWithoutPanic|DrainDeadlineHonored)$|TestBoltBuffer_(HammerCloseNoPanic|QueueAfterCloseDropsWithoutPanic|DrainDeadlineHonored)$' -count=15 .
# 90/90 PASS, no DATA RACE, no panic
gofmt -l buffer.go buffer_test.go bolt_buffer.go bolt_buffer_test.go main.go   # no output — clean
```

`management.go` still shows up under a bare `gofmt -l .`, pre-existing and untouched (same as noted in section 1 above).
