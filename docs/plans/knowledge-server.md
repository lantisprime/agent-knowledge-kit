# Knowledge server — central store, web curation, thin subscribers

Status: accepted direction (operator decision, 2026-08-04). This plan
supersedes the git-transport delivery model and the separate-identity
delivery trust boundary (`delivery-trust-boundary.md`). Implementation
has landed for build-order steps 1–3 (store + schema + release cut;
thin subscriber with token-bound identity; authN); the git transport
is not yet retired — cutover is step 7.

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

v1 ships the curated collections (kernel, procedure docs). The
non-v1 columns of the table above are the *target*, not what v1's
schema already holds: `collections` currently carries a `delivery`
policy and an `in_release` membership flag, so per-collection
write-path and lifecycle rules, the "draft referenced by an active
doc" reference lint, and promotion provenance each need an
**additive** schema change when their slice lands (e.g.
`collections.write_path`, `collections.lifecycle`, a `doc_links`
table, a `provenance` column on `doc_versions`). These are additive —
no rewrite of shipped tables — but they are not free. Release
membership is the `in_release` flag, not the delivery tier: v1 seeds
both `kernel` (push) and `docs` (trigger) with `in_release = 1`,
because Tier B procedure docs must be materialized on the host to be
trigger-loaded. Query-tier collections are not in the snapshot.

- A document row carries the schema fields (`family-id`, `version`,
  `title`, `status`, `owner`, `audience`, `tier`, `triggers`, body)
  plus timestamps and editor identity. Every save is a new immutable
  version row; supersession is a status change, never a delete.
- The store is the trust boundary: `collection` and `family` are
  validated at `SaveDoc` before any path is built from them — empty,
  a leading `.`, `/`, `\`, a `..` substring, NUL, an ASCII control
  char, or any non-ASCII byte is rejected (they become tar entry
  paths at release time; the identifier set is deliberately narrow).
- Writes are serialized in-process (WAL + a single SQLite connection),
  so concurrent saves cannot collide on `SQLITE_BUSY`. An optional
  `base_version` on a save is an optimistic-lock base: a stale value
  is rejected (409) rather than silently overwriting; omitting it is
  the explicit last-writer-wins opt-out.
- Lifecycle is enforced by the database, not convention: `draft` never
  enters a release; exactly one `active` version per family.
- Kernel release lint is two-door at cut time: the ~2000-word Tier A
  cap AND a hard `24576`-byte (24 KiB) backstop, so a whitespace- or
  zero-width-joined body that fools the word count still cannot ship.
- Promotion across collections is a first-class UI action (post-v1,
  needs the provenance column above): an episode that proves durable
  becomes a behavioral pattern or Tier B doc, linked to its sources.

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
  error}`. Heartbeats are observability only — a last-seen row per
  host, never delivery or routing state.
- Fleet page: which hosts are current, stale, or dark; per-host error
  detail; a force-resync action.
- **Force-resync (belief-erasure pull-flag).** Force-resync keeps the
  thin-client boundary: all intent is server-side in a dedicated
  `resync_requests(host, requested_at)` table, set by `POST
  /api/hosts/{host}/resync` and surviving restarts and offline hosts.
  The subscriber gains no logic — on its poll it reads `resync` from
  `GET /api/releases/current?host=<h>`; if true it deletes its local
  applied-release marker (its only permitted state) and the existing
  "applied != current → re-materialize with hash verification" path
  does the rest. The flag clears only on the post-apply heartbeat
  (`ok && resync_applied`), never on poll or on convergence, so an
  offline or crashed host retries on next boot. Delivery is
  at-least-once over an idempotent apply. (Server side ships in v1;
  the one-line client behavior is step-2 subscriber work.)

## Subscriber contract

Receive "release N exists" (stream push; poll fallback) → fetch N →
verify content hash → write to a fresh versioned dir → atomically flip
the `corpus` pointer (the symlink adapters read; `$KNOWLEDGE_HOME/
corpus` → `releases/<id>`) → heartbeat. Idempotent; no local state
beyond the applied release. On any failure keep the previous release
and report the
error. Fail-soft is preserved: an unreachable server means
stale-but-working sessions, never broken ones. The subscriber
assumes the server has auth enabled; against an auth-disabled server,
corpus sync still works but heartbeats and force-resync are inert
(the server treats the subscriber as the operator principal and
ignores any host-token semantics).

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

## Component boundaries and contracts

Each component has one responsibility, a declared input/output schema,
and an explicit non-goal. Nothing crosses a boundary except these
messages.

**Store (SQLite).** Consumes validated writes from the API layer only;
produces rows and release snapshots. No other process or component
opens the database file. Never: parse HTTP, render UI, talk to hosts.

**HTTP API — the only write door.** All schemas are JSON; errors are
always `{"error": "<code>", "detail": "<text>"}`.

- `PUT /api/docs/{collection}/{family}` — save a new doc version.
  Request: `{"title", "status": "draft|active", "tier", "triggers":
  [..], "owner", "audience", "body", "base_version"?}`. `base_version`
  is optional; when present and it does not equal the family's current
  max version, the save is rejected `409 {"error": "conflict", ...}`.
  Response: `{"collection", "family_id", "version", "status",
  "created_at", "editor"}`.
- `POST /api/releases` — cut a release from active docs; publish
  lints run here and reject with `409 {"error": "lint", ...}`.
  Response (the release manifest): `{"release_id", "content_hash":
  "sha256:..", "created_at", "docs": [{"path", "family_id",
  "version", "sha256"}]}`.
- `GET /api/releases/current[?host=<h>]` — latest manifest, same
  schema; with `?host=<h>` a *pending* force-resync flag adds
  `"resync": true` (see the registry). The field is `omitempty`: a
  false/absent flag omits it entirely, so clients must read absent as
  false.
- `GET /api/releases/{id}/archive` — tar stream; entry paths exactly
  match the manifest `path` fields. A missing id is `404 {"error":
  "not_found", ...}` (checked before any tar byte is written), never a
  200 with an empty body.
- `POST /api/heartbeats` — `{"host", "release_id", "ok",
  "error": null|"<text>", "resync_applied"?}`. `resync_applied: true`
  with `ok: true` is the sole signal that clears this host's resync
  flag. Response: `204`.
- `POST /api/hosts/{host}/resync` — operator/fleet-page action; sets
  the per-host force-resync flag. Response: `204`.
- `POST /api/hosts/{host}/token` — operator action; mints (or rotates,
  if a token already exists) the host's bearer token. The plaintext
  appears in this response exactly once; only its SHA-256 digest is
  stored. Response: `201 {"host", "token"}` (token is a 64-char hex
  string, 256 bits of entropy).
- `DELETE /api/hosts/{host}/token` — operator action; deletes the
  host's token — revocation is checked at request authentication,
  so new requests with that bearer fail immediately. An
  already-authenticated in-flight request may complete: request
  reading is bounded by `ReadTimeout` (30 s); the release archive
  stream has no server write bound (`WriteTimeout` is deliberately
  0) but serves only immutable release bytes, so a post-revocation
  completion cannot alter state or leak anything newer than the
  already-authorized release. Response: `204`, or `404 {"error":
  "not_found", ...}` when no token was issued.

With authentication enabled (`-operator-token-file` set), every
endpoint above requires `Authorization: Bearer <token>` on every
request; a missing or unknown token is `401 {"error": "unauthorized",
...}` with a `WWW-Authenticate: Bearer` challenge. Writes (docs,
release cuts, resync, token issue/revoke) are operator-only: a host
token on these is `403 {"error": "forbidden", ...}`. Release reads
and heartbeats accept any valid token, with the further rule that a
subscriber token is bound to exactly one host and *is* that host's
identity — a mismatched `?host=` on `/api/releases/current` or a
mismatched body `host` on `/api/heartbeats` is `403`. An empty
`?host=` (or empty body host) resolves to the token's bound host, so
a tokened subscriber needs no out-of-band hostname coordination. The
request body is capped at 1 MiB by `http.MaxBytesReader`; an
oversized payload is `413 {"error": "too_large", ...}` and is
distinguished from a plain malformed body (`400`). The server can
generate the operator credential itself (`-init-operator-token`,
CSPRNG, mode 0600); a minimum length is enforced for
hand-supplied tokens but their entropy cannot be verified — use the
generation path.

**Content-hash construction** (pinned so an independent subscriber
reproduces it bit-for-bit from the manifest alone). Doc order is the
release query order `ORDER BY collection, family_id`, and the manifest
lists docs in exactly that order. Start a SHA-256; for each doc in
order write its `path` bytes, then a single `0x00` byte, then the raw
32-byte SHA-256 of the doc body (not hex). `content_hash` is
`"sha256:" + hex(sum)`. The per-doc `sha256` in the manifest is that
same digest in hex. The archive tar bytes are NOT what `content_hash`
covers — a subscriber recomputes the hash from the materialized
`path`+body set, not from the tar framing.

**Release stream (SSE).** Output-only: emits `{"release_id",
"content_hash"}` per cut release. Fire-and-forget broadcast — the
server keeps no per-client state: no offsets, no acknowledgements, no
replay. A client that misses events converges by reading
`/api/releases/current`; only the latest release matters. Never:
carry document bodies, accept writes, or track who is listening.

**Web UI.** A client of the HTTP API with no privileged path — it
uses the same endpoints and schemas as any other client. Never: touch
the store directly.

**Subscriber.** An asynchronous convergence loop: on a release event,
on stream reconnect, and on a slow periodic poll, compare the
server's current release to the locally applied one; if they differ,
fetch, verify, apply. Idempotent — applying the same release twice is
a no-op; there is no replay and nothing to acknowledge. Its whole
output surface on the host is: versioned release dirs, the atomic
`corpus` symlink pointer, one heartbeat POST. Never: write knowledge,
merge, or mutate a delivered release. A force-resync always
re-materializes from freshly-fetched, hash-verified bytes into a new
release dir — it never reuses an on-disk dir that could have drifted.

**Adapters (existing, unchanged).** Consume files below
`$KNOWLEDGE_HOME/corpus`; produce harness-native injection. Never:
know the server exists.

## Build order (v1)

1. Store + schema + release cut (server, API only): publish a fixture
   corpus; regression tests. **Done** (path validation, serialized
   writes + optimistic lock, two-door kernel lint, release/archive/
   heartbeat/resync endpoints, loopback enforcement).
2. Subscriber: materialize + flip + heartbeat against a local server;
   fail-soft tests (server down, hash mismatch, partial fetch). Refuse
   `..` tar entries defensively; implement the one-line resync
   belief-erasure read. **Done** (fail-soft, traversal defense,
   unmanifested-entry rejection, fresh-dir force-resync after tamper,
   per-host token wiring with no `?host=` on the wire).
3. **AuthN** — per-host subscriber token + operator credential. MUST
   land before any non-loopback bind or any remote (non-colocated)
   subscriber. Until it ships, the server refuses non-loopback binds
   (a bare `:port` counts as non-loopback) unless `--insecure-no-auth`
   is passed, and v1 subscribers are colocated or the archive travels
   an already-authenticated channel. **Done** (bearer auth via
   `--operator-token-file`; per-host tokens issue/revoke;
   token-is-identity binding on `?host=` and heartbeat body host;
   auth+TLS gate for non-loopback binds; `MaxBytesReader` + server
   read/idle timeouts).
4. Curation UI: browse, edit, history, diff, publish with lints.
5. Conflict records + merge view + resolution audit.
6. Fleet page + force resync.
7. Cutover: adapters read the subscriber-materialized corpus (no
   adapter change expected); retire the git transport; update
   `architecture.md`, `REPO_MAP.md`, README.

## Out of scope (v1)

- Multi-instance HA and Postgres remain out of scope; authN landed
  with build-order step 3.
- Automated claim-conflict detection.
- Additional collections and machine-submitted streams (they need the
  additive schema changes noted in the Store section; nothing ships).
- Environment deployment, host inventory, and consumer migration
  plans — those belong to environment repositories.

### Accepted residual risks (loopback-only v1)

Step 3 (AuthN) has landed. The authentication-dependent risks
below are closed for unauthenticated and remote callers. Same-UID
local processes can still read the operator and host token files
(the single-user constraint is fixed), so local-process spoofing
remains in the threat model — 0600 file modes do not isolate same-UID
callers.

- Heartbeat spoofing: any local process can overwrite any host's
  observability row.
- Force-resync abuse: a forged `POST /api/hosts/{host}/resync` only
  triggers a redundant re-apply of authentic, hash-verified content —
  availability noise, never an integrity break.
- Forged resync ack: a spoofed `resync_applied` heartbeat can make a
  resync appear done when it was withheld; the fleet page shows
  `requested_at` vs the ack time so the anomaly is at least visible.
  The completion signal is trustworthy only post-authN.
- Stale-ack clear: because set and clear are serialized but untagged,
  an in-flight `ok && resync_applied` heartbeat acking a *previous*
  request can clear a just-set flag. The apply is idempotent (the host
  re-materialized the current release moments earlier) and a re-request
  recovers it; tagging acks with the applied release id is a post-authN
  refinement, not a v1 need.
- No request-body size limit and no `http.Server` read/idle timeouts:
  an unbounded body or a slow-loris client was a local-process DoS
  only. Both shipped with step 3: `MaxBytesReader` caps every request
  body at 1 MiB, and the server sets `ReadHeaderTimeout` 10 s,
  `ReadTimeout` 30 s, and `IdleTimeout` 2 m (`WriteTimeout` is left
  zero on purpose for future long-lived response streams).
