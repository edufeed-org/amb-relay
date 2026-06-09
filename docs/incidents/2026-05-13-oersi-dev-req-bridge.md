# `dev.oersi.edufeed.org`: relay's REQ returned ~13 events from a 95 722-doc Typesense collection

**Date:** 2026-05-13
**Instance:** `amb-relay-oersi-dev` (full 5/6-container stack, `wss://dev.oersi.edufeed.org`)
**Relay image:** `git.edufeed.org/edufeed/amb-relay:dev` (built off dev branch)
**Status:** **RESOLVED on this instance** — operational fix, not an upstream code bug.

## TL;DR

The relay's REQ surface was yielding only a handful of kind-30142 events
even though its Typesense collection had 95 722 documents indexed under
that kind. Root cause: **the BoltDB raw event store was never hydrated
from the bulk Typesense import.** The dev branch already addresses this
exact case (commits `5af9b45` / `70bd0e0` / `2a3db34`, 2026-05-05/06)
— the relay reads payloads from BoltDB by ID, so Typesense-only docs
are silently unreachable. The fix tool `hydrate-bolt` is already baked
into the `:dev` image; we just had to run it against the live BoltDB.

After running it: 95 722 events written, 0 already-present (confirming
BoltDB had ~none of the kind-30142 raw events), and REQ now returns the
page-cap (250) per query — paginating clients walk the full corpus.

## What we observed

```
# Direct Typesense query — has the data:
$ curl …/collections/oersi/documents/search?filter_by=eventKind:=30142
{ "found": 95722, ... }

# Relay REQ — barely anything (before fix):
$ nak req -k 30142 --limit 500 ws://relay:3334
→ ~13 events
```

The relay log showed:
```
Processing query with search: , nostr filters: eventKind:=30142, limit 250
Search succeeded, found 250 events
```
…where "found 250" was Typesense's match count, but only the small
subset whose `eventRaw` payload had been written into BoltDB was
actually yielded over the websocket.

## Why this happened on this instance

The OERSI corpus was bulk-loaded into the relay's Typesense collection
some time ago, but the corresponding raw events never reached BoltDB.
This is exactly the skew that the dev branch commit messages document:

> "leaving 8555 docs in TS with no underlying raw event. The relay's
> REQ path treats Typesense as an index and reads payloads from the
> raw store by ID, so those docs became silently unreachable: 'Search
> succeeded, found N events' with nothing yielded over the websocket."
> — commit 2a3db34

On our deploy, the named volume for `/data/relay.db` is correctly
configured (homelab role's docker-compose template), so BoltDB has been
persistent across restarts — but it was empty of the historical
kind-30142 events because none of them flowed through the live
dual-write path.

The original misleading framing in this report ("13 yesterday vs 250
today") was wrong: yesterday's `found 250` was the Typesense match
count, not the events delivered. The actual deliveries-per-query
fluctuated based on how many events happened to have raw payloads in
BoltDB, which grew slowly as live events came in.

## Resolution applied

On `homelab-docker` as root, with the relay stopped (BoltDB exclusive
lock):

```bash
docker stop amb-relay-oersi-dev
docker run --rm \
  --network amb-relay-oersi-dev-internal \
  -v amb-relay-oersi-dev_relay_data:/data \
  -e TS_HOST=http://typesense:8108 \
  -e TS_APIKEY=$(sudo cat /opt/amb-relay-oersi-dev/.ts_apikey) \
  -e TS_COLLECTION=oersi \
  -e DB_PATH=/data/relay.db \
  --entrypoint /root/hydrate-bolt \
  git.edufeed.org/edufeed/amb-relay:dev
docker start amb-relay-oersi-dev
```

Output ended:
```
DONE scanned=95722 saved=95722 already=0 parseErr=0 saveErr=0
```

Post-fix REQ verification (raw websocket frame, no `since`/`until`):
```
events: 250 (EOSE)
```
Filter logged by relay: `eventKind:=30142, limit 250` — no spurious
`eventCreatedAt:>=…` injection. Downstream clients can now paginate
with `until=<page-oldest-1>` and walk the full 95 722-event history.

## No upstream action needed

The dev branch already shipped:
- BoltDB as RawEventStore (`5af9b45`)
- `cmd/hydrate-bolt` tool (`70bd0e0`)
- Image-baked binary + persistent volume in Dockerfile (`2a3db34`)

What might still be worth doing upstream (out of scope for this report):

1. **Operational doc.** Mention the hydrate-bolt recovery in the
   README mirroring section so operators of fresh instances seeded
   from a Typesense snapshot know to run it.
2. **Optional auto-hydrate on startup.** A `HYDRATE_ON_START=true`
   env that runs hydrate-bolt as the first step of the relay
   container's entrypoint would prevent this skew from going unnoticed
   on future bulk-seed deploys. Trade-off: long startup; idempotent
   though.

These are nice-to-haves. The immediate operational fix is sufficient
to unblock the downstream amb-indexer backfill.

## Related

- amb-indexer side fixes that paired with this:
  `~/coding/edufeed/amb-indexer/HEADLESS_DEPLOY_BUGS.md` (resolved)
- Three-tier headless fetch spec (deployed and live):
  `~/coding/edufeed/amb-indexer/HEADLESS_FETCH_SPEC.md`
- Homelab Ansible role: `~/coding/homelab/roles/amb-relay/`
