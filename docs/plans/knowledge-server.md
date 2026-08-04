# Knowledge server — central store, web curation, thin subscribers

Status: accepted direction (operator decision, 2026-08-04). This plan
supersedes the git-transport delivery model and the separate-identity
delivery trust boundary (`delivery-trust-boundary.md`). Implementation
has not started; shipped code is not yet retired.

## Operator constraints (fixed)

1. Single OS user on every host. No second principal, no OS-level
   publisher identity, no privileged fixtures or tests.
2. Knowledge is not stored in Git or on a Git service. The server's
   database is the store of record.
3. The server is the fleet's one dependency. Consumer hosts run one
   thin subscriber and hold no writable synced state.
4. Humans curate through the server's web UI, not through files.

## Decision

Replace the git-clone transport with a master/replica knowledge server:

- **Server** (one deployable): document store in an embedded database,
  web curation UI, conflict management, an append-only release stream,
  and a consumer-host registry.
- **Subscriber** (per host): a thin client that materializes released
  corpus bytes to `$KNOWLEDGE_HOME` and reports status. Adapters and
  the Tier A/B/C injection contract are unchanged.

The failure mode that shaped this: peer-synchronized fat clients with
no central arbiter corrupt shared state and strand hosts. The
corrective asymmetry is absolute — the server's database is the only
writable state; every write from any source goes through the server's
API and is serialized there. Hosts never reconcile, merge, or repair
anything locally. Operationally that means merge logic exists in
exactly one process: one database to back up, one write path to
debug, and conflicts appear as records on a page instead of corrupted
client state discovered after the fact.

Scale follows from the same asymmetry. Write contention is independent
of fleet size — hosts are not writers, so 5 or 90 hosts changes
nothing on the write path. Reads are stateless and linear: one idle
stream connection and one small release fetch per host. Client
credentials are one revocable token per host; content encryption is
TLS to a single endpoint, so key rotation and host revocation are
server-side operations, never fleet-wide re-key events.

## Server

### Store

Collections hold documents. Each collection fixes three policies —
write path (web UI or server API), delivery (pushed, trigger-loaded,
or query-on-demand), and lifecycle. Knowledge types map to
collections:

| Collection | Writer | Delivery | Lifecycle |
|---|---|---|---|
| kernel (Tier A) | human, web UI | pushed into every session | supersede in place; cap-linted |
| procedure docs (Tier B) | human, web UI | trigger-loaded | supersession; staleness review |
| episodes (session records) | agents, API | query-on-demand | revision chains; decay |
| facts of record | humans + automation | query-on-demand | current-state validity |
| feedback / behavioral patterns | agent-proposed, human-confirmed | pushed or trigger-loaded | promoted from episodes |
| research notes | agents, API | query-on-demand | source staleness checks |

v1 ships the curated collections (kernel, procedure docs); the schema
anticipates the rest without redesign. Only push-tier collections
enter the release snapshot subscribers materialize.

- A document row carries the schema fields (`family-id`, `version`,
  `title`, `status`, `owner`, `audience`, `tier`, `triggers`, body)
  plus timestamps and editor identity. Every save is a new immutable
  version row; supersession is a status change, never a delete.
- Lifecycle is enforced by the database, not convention: `draft` never
  enters a release; exactly one `active` version per family.
- Promotion across collections is a first-class UI action: an episode
  that proves durable becomes a behavioral pattern or Tier B doc, with
  provenance links back to its source episodes.

### Web curation UI

- Browse by tier and status; edit in a markdown editor.
- Kernel edits show a live token count against the Tier A cap;
  publish is blocked over cap.
- Per-document version history with diffs.
- **Publish** cuts a release: an immutable snapshot (release id,
  content hash, manifest, corpus archive) written in one transaction
  and appended to the release stream.

### Conflict management

First-class conflict records with a resolution audit trail: what
conflicted, both versions, who resolved it, the winning version, when.

- *Edit conflicts* — optimistic locking on save; the later writer is
  redirected to a merge view instead of overwriting.
- *Claim conflicts* — "one claim, one home" violations flagged for
  resolution (v1: manual flagging; automated detection later).
- *Policy conflicts* — publish-blocking lints: kernel over cap, a
  draft referenced by an active doc, dangling supersession.
- *Cross-collection conflicts* — an API-submitted episode that
  contradicts an active curated doc surfaces for human resolution:
  supersede the doc or mark the episode wrong. Lands with the
  episodes collection, after v1.

### Consumer host registry

- Subscribers heartbeat `{host id, applied release, timestamp, last
  error}`.
- Fleet page: which hosts are current, stale, or dark; per-host error
  detail; a force-resync action.

## Subscriber contract

Receive "release N exists" (stream push; poll fallback) → fetch N →
verify content hash → write to a fresh versioned dir → atomically flip
`current` → heartbeat. Idempotent; no local state beyond the applied
release. On any failure keep the previous release and report the
error. Fail-soft is preserved: an unreachable server means
stale-but-working sessions, never broken ones.

## Stack

- Server: single Go binary; embedded SQLite (Postgres later as a
  config swap, not a v1 concern); embedded web UI; SSE release stream.
- Subscriber: single static Go binary per host.
- This ends the sh-only convention for the delivery layer. Adapters
  stay POSIX sh and unchanged.

## What this supersedes

- Git remotes, the `sync.sh` transport, and Git-service hosting of
  knowledge: retire after cutover; keep until the server path runs
  the full loop end to end.
- `delivery-trust-boundary.md` and the two-principal publisher: the
  single-user constraint withdraws the local-integrity claims that
  plan made; do not cite them as guarantees. Revise the
  `docs/architecture.md` decision record in the same slice that
  retires the code.
- "Kernel edits are PR-only" becomes: kernel edits go through the web
  UI's diff-and-confirm publish with the cap lint enforced.

## Build order (v1)

1. Store + schema + release cut (server, API only): publish a fixture
   corpus; regression tests.
2. Subscriber: materialize + flip + heartbeat against a local server;
   fail-soft tests (server down, hash mismatch, partial fetch).
3. Curation UI: browse, edit, history, diff, publish with lints.
4. Conflict records + merge view + resolution audit.
5. Fleet page + force resync.
6. Cutover: adapters read the subscriber-materialized corpus (no
   adapter change expected); retire the git transport; update
   `architecture.md`, `REPO_MAP.md`, README.

## Out of scope (v1)

- Multi-instance HA, Postgres, authentication beyond one operator
  account and per-host subscriber tokens.
- Automated claim-conflict detection.
- Additional collections and machine-submitted streams (the schema
  anticipates them; nothing ships).
- Environment deployment, host inventory, and consumer migration
  plans — those belong to environment repositories.
