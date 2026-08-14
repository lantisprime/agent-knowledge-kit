# Subscriber protocol compatibility negotiation

Status: **approved post-v1 implementation slice** (operator decision
2026-08-11).

This slice prevents a server and subscriber with incompatible wire semantics
from silently materializing a release. It establishes a rolling-upgrade-safe
v1 negotiation contract without changing release bytes, the database schema,
or the polling convergence model.

## Contract

The subscriber sends this header on current-release, archive, and heartbeat
requests:

```text
Agent-Knowledge-Protocol-Version: 1
```

The server applies the compatibility gate only to those three
subscriber-facing routes, after authentication and before the route handler.
It advertises the same header on every gated response reached after successful
authentication.

- A missing request header means legacy protocol v1. This lets a new server
  roll out before existing subscribers.
- A present request header must contain exactly one value equal to `1`.
  Empty, duplicate, or unsupported values return HTTP 409 with
  `{"error":"incompatible_protocol",...}` before a manifest, archive, or
  heartbeat is processed.
- Operator/UI routes are not gated. Authentication remains the outer gate, so
  an unauthenticated incompatible request is still 401 rather than a protocol
  version oracle.

The subscriber accepts a missing response header as legacy v1. Once a server
advertises the header, it must contain exactly one value equal to `1`.
Incompatible current-release or archive responses fail soft before corpus
selection changes: the last-good corpus pointer and bytes remain intact, the
failure is logged, and an error heartbeat is attempted. The `.applied` marker
keeps its existing force-resync belief-erasure semantics. An incompatible
heartbeat response is logged and otherwise remains best-effort observability.

Future servers receiving a missing header must continue to treat it as a v1
client. They may serve a v1-compatible representation or reject it with the
same incompatible-protocol envelope, but must never silently send newer wire
semantics to that client.

## Writable set

- `knowledge-server/api.go`
- `knowledge-server/api_test.go`
- `knowledge-server/subscriber/main.go`
- `knowledge-server/subscriber/main_test.go`
- `docs/plans/subscriber-protocol-compatibility.md`
- `README.md`, `CLAUDE.md`, `docs/architecture.md`, `docs/REPO_MAP.md`
- `docs/plans/knowledge-server.md`

No store, database, manifest, archive, UI, adapter, authentication, fleet,
deployment, or consumer-repository behavior changes in this slice.

## Exclusions

- authenticated binary distribution and release signing;
- database or document-schema versioning;
- SSE release notification;
- new collections, machine-submitted streams, or provenance;
- environment deployment and consumer migration.

## Verification

Negative tests first prove that the baseline server accepts unsupported
protocol requests and the baseline subscriber applies a response advertising
version `2`. Completion requires:

```sh
(cd knowledge-server && go test -race -count=1 ./...)
(cd knowledge-server && node --check ui/app.js && node --test ui/lib_test.mjs)
sh -n tests/run.sh && sh tests/run.sh
shellcheck -x adapters/lib/kernel-path.sh adapters/claude/install.sh \
  adapters/codex/update-agents-md.sh adapters/pi/run.sh tests/run.sh
git diff --check
```
