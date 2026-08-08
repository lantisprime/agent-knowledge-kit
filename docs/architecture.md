# Knowledge-server architecture

Status: **implemented v1 plus approved document-link lint slice** (v1 steps
1–7 cut over 2026-08-08; link slice approved 2026-08-08).

The accepted v1 contract is `docs/plans/knowledge-server.md`; the approved
link-slice contract is `docs/plans/document-link-lints.md`. This file records
the current architecture and separates it from post-v1 targets. The
earlier Git transport, event-propagation design, and two-principal publication
boundary are superseded; their historical contract remains in
`docs/plans/delivery-trust-boundary.md` and Git history only.

## Authority and repository boundary

This repository owns the generic server, subscriber, schemas, adapters, tests,
compatibility policy, and release lifecycle. Forgejo is the kit's only source
and release authority. It does not store or transport an environment's
knowledge.

Consumer repositories own deployment, service identities, TLS material,
backups, host inventory, monitoring, and migrations. Private knowledge lives
in the consumer-operated server database. The public kit must never contain
environment-specific content or credentials.

```text
agent-knowledge-kit release -> consumer pin -> server/subscriber deployment
                                           -> private database and releases
```

## System overview

```text
                         one writable path
human operator ──▶ web UI / authenticated API
                                 │
                                 ▼
                       ┌──────────────────┐
                       │ SQLite store     │
                       │ immutable docs   │
                       │ release records  │
                       │ conflicts        │
                       │ host registry    │
                       └────────┬─────────┘
                                │ manifest + tar archive
                                ▼
                      authenticated subscriber
                                │ verify + materialize
                                ▼
                 $KNOWLEDGE_HOME/releases/<release-id>[.<n>]
                                │ atomic relative symlink
                                ▼
                     $KNOWLEDGE_HOME/corpus
                         ┌──────┼──────┐
                         ▼      ▼      ▼
                      Claude  Codex    pi
                         └──────┬──────┘
                                ▼
                       fresh agent context
```

The server is the only component allowed to write knowledge. Subscribers do
not edit, merge, reconcile, or repair documents. Adapters do not call the
server and do not know which release is active; they consume the filesystem
contract below `corpus`.

## Store and release model

The v1 SQLite store seeds two release-bearing collections:

| Collection | Delivery policy | Release membership |
|---|---|---|
| `kernel` | pushed Tier A | included |
| `docs` | trigger-loaded Tier B | included |

Every save inserts an immutable document version. A database index allows one
active version per collection/family. Draft and superseded versions remain in
history but do not enter releases. Ordered `reference` and `supersedes` links
belong to the immutable source version and target an exact collection, family,
and version.

A release cut:

1. selects active documents from release-bearing collections;
2. applies the 2,000-word and 24-KiB kernel lints plus document-link lints;
3. computes a deterministic manifest and content hash;
4. checks the optional preview-hash precondition;
5. writes the immutable release and its document membership in one
   transaction.

The archive contains regular files at the exact manifest paths. Release
identities and hashes are server-issued and immutable. The database, not a Git
repository or local checkout, is the store of record.

## API and UI boundary

The HTTP API is the only store write door. The embedded UI is an ordinary API
client with no direct database access. It provides:

- document browse, edit, immutable history, and bounded diffs;
- JSON editing and merge comparison for version-specific document links;
- preview-and-confirm publishing with an expected-content-hash guard;
- edit, claim, and policy conflict records with resolution audit;
- fleet current/stale/dark classification and force-resync actions.

Operator endpoints cover document writes/reads, release cuts, host-token
issue/revoke, fleet state, resync, and conflicts. Subscriber endpoints expose
the current manifest, immutable archive, and heartbeat write.

With authentication enabled, every request needs a bearer token. Host tokens
are bound identities and may read releases and heartbeat only as their bound
host. Operator tokens may use administrative endpoints. Request bodies are
capped at 1 MiB. The server refuses non-loopback exposure without both
authentication and TLS unless the operator explicitly passes the dangerous
`--insecure-no-auth` override.

## Subscriber contract

The subscriber performs an idempotent polling convergence loop:

```text
GET current -> compare .applied -> GET archive -> validate/extract
-> recompute hash -> install fresh release dir -> atomically flip corpus
-> write .applied -> heartbeat
```

It independently reproduces the content-hash algorithm and does not import the
store package. Manifest paths and tar entries are untrusted. It rejects empty,
absolute, escaping, `..`, non-regular, unmanifested, or missing
entries before accepting a tree.

Every materialization uses a fresh directory. Force-resync removes the local
applied marker and re-fetches even when the release id matches; it never trusts
an existing release directory that may have drifted. A successful post-apply
heartbeat is the only resync acknowledgement.

All operational failures are fail-soft for sessions: the previous `corpus`
pointer and bytes remain available. The subscriber logs and heartbeats the
failure best-effort. Misconfigured command-line security settings fail hard at
startup.

V1 polls on a configurable interval. The SSE doorbell described in the
original target plan is not implemented and is not required for correctness;
it remains an unordered post-v1 latency optimization.

## Adapter contract

All adapters resolve either `corpus/kernel/kernel.md` or the legacy-compatible
nested `corpus/corpus/kernel/kernel.md`, preferring nested when both exist.
They require a regular kernel, canonical containment below the active corpus,
and—when the physical root happens to be a Git checkout—a regular tracked blob.

- Claude installs a SessionStart hook. Each fresh session resolves the current
  corpus pointer, so release changes need no hook rewrite.
- Codex copies kernel bytes into a global marker-delimited `AGENTS.md` block.
  The updater must run after each subscriber convergence pass.
- pi resolves the current kernel when launched and passes its physical path to
  `--append-system-prompt`.

The adapters preserve the last-good corpus behavior but are not an OS security
boundary. A same-uid process can mutate local release bytes or harness config.
Force-resync can restore server-authored bytes; it cannot isolate same-uid
processes.

## Conflict and fleet semantics

- Stale optimistic-lock saves open or increment an edit conflict.
- Operators may flag claim conflicts manually. Automated claim detection is
  post-v1.
- Kernel-cap and document-link lint failures open policy conflicts against the
  offending source document; a successful cut resolves open policy conflicts.
  Active references must resolve to the exact active target in the release,
  and supersession targets must exist with `superseded` status.
- Host rows are the union of heartbeats, pending resyncs, and issued tokens.
  The server supplies `now` so the UI does not trust the browser clock for age.
- Delivery is at-least-once and apply is idempotent. A resync flag survives an
  offline host and clears only after a successful resync-applied heartbeat.

## Trust model and residual risks

The v1 operator constraint is one OS user per host. Token files use restrictive
modes, but same-uid processes can still read them. Therefore:

- local-process heartbeat or operator impersonation remains possible;
- local materialized bytes can be changed until the next force-resync;
- a forged resync acknowledgement can clear a request, and v1 keeps no durable
  request/ack audit;
- an in-flight acknowledgement can clear a newer untagged request;
- live sessions retain the kernel they started with until restarted.

These are accepted v1 residual risks, not hardened guarantees. Remote callers
without valid tokens are excluded when authentication is enabled, and bearer
tokens are protected in transit when TLS is correctly deployed.

## Superseded design

The following are not current architecture and must not be reintroduced
without a new accepted decision:

- Git remotes or `sync.sh` as corpus transport;
- a separate publisher OS principal and protected publication root;
- the fixture-only publisher transaction and privileged two-principal tests;
- PR-only private-corpus editing;
- webhook/MQTT doorbells as a prerequisite for convergence.

Forgejo remains authoritative for kit development and releases. This
supersession applies only to private knowledge storage and delivery.

## Implemented verification

- `sh tests/run.sh`: adapter containment, marker preservation, supported
  layouts, and subscriber-materialized corpus cutover.
- `go test -race ./...` under `knowledge-server/`: store, API, authentication,
  UI handlers, and subscriber convergence/security regressions.
- `node --test ui/lib_test.mjs`: deterministic UI parsing (including link
  JSON), diffs, publish, conflict, and fleet helpers.

Fresh-session injection and production TLS/service deployment remain
consumer-environment checks.

## Post-v1 work

The document-link schema plus draft-reference/dangling-supersession lints are
the first approved post-v1 slice; `docs/plans/document-link-lints.md` is its
contract. Remaining candidate slices are unordered:

- automated claim-conflict detection;
- additional collections, machine-submitted streams, and provenance;
- an optional SSE release doorbell while retaining polling convergence;
- kit/schema versioning and authenticated binary release distribution;
- multi-instance/HA or Postgres, if scale requires them.

Environment deployment, backups, host inventory, and migration remain outside
the kit repository.
