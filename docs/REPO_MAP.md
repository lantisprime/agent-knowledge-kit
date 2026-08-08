# Agent Knowledge Kit Repository Map

> **AI orientation artifact**
> - **Purpose:** locate authority, code, schemas, workflows, and verification
>   entry points without importing consumer-repository assumptions.
> - **Authority:** navigation aid; `AGENTS.md`, `CLAUDE.md`, the accepted plan,
>   tests, schema, and implementation remain governing evidence.
> - **Verified:** 2026-08-08 against the step-7 cutover working tree based on
>   Forgejo `main` at `033d7ff`.
> - **Update trigger:** ownership, architecture, lifecycle, directory roles,
>   supported adapters, verification entry points, or terminology changes.

## 1. Executive summary

`agent-knowledge-kit` is a public, generic framework for centrally curated
knowledge and deterministic agent-context injection. The v1 server owns the
only writable knowledge database and cuts immutable releases. Thin subscribers
verify and materialize releases; harness adapters consume the active local
corpus pointer.

Knowledge-server build-order steps 1–7 are implemented. The step-7 cutover
retired Git as a private-corpus transport and removed the two-principal fixture
publisher. Forgejo remains the sole authority for this kit's source and
releases.

## 2. Authority and dependency direction

| Concern | Authoritative home |
|---|---|
| Generic architecture and trust model | `docs/architecture.md` |
| Detailed v1 contract and API | `docs/plans/knowledge-server.md` |
| Agent/contributor contract | `AGENTS.md`, `CLAUDE.md` |
| Public interface | `README.md` |
| Store/API/subscriber implementation | `knowledge-server/` |
| Harness injection | `adapters/` |
| Environment deployment and monitoring | Consumer repository |
| Environment knowledge and credentials | Consumer-operated server/database |

```text
kit change -> tests/review in this repo -> Forgejo release
    -> consumer pins release -> consumer deploys server/subscribers/adapters
    -> operator curates private knowledge through server UI/API
```

The kit never reaches into a consumer repository for design or release input.
A consumer cannot tag, publish, or rewrite the kit.

## 3. Repository shape

| Path | Role | Status |
|---|---|---|
| `AGENTS.md` | Repository-wide authority and workflow | Authoritative |
| `CLAUDE.md` | Implementation constraints, commands, and known risks | Authoritative |
| `README.md` | User-facing contract and quickstart | Shipped interface |
| `docs/architecture.md` | Current system and trust decisions | Implemented v1 record |
| `docs/plans/knowledge-server.md` | Detailed store/API/subscriber/UI contract | Accepted; steps 1–7 implemented |
| `docs/plans/delivery-trust-boundary.md` | Previous two-principal design | Superseded historical record |
| `knowledge-server/main.go` | Server flags, exposure gate, HTTP lifecycle | Implemented |
| `knowledge-server/api.go` | Only HTTP/API write door and endpoint schemas | Implemented |
| `knowledge-server/auth.go` | Operator/host bearer authentication | Implemented |
| `knowledge-server/store/store.go` | SQLite schema, versions, releases, conflicts, hosts | Implemented |
| `knowledge-server/ui.*`, `knowledge-server/ui/` | Embedded curation/conflict/fleet UI | Implemented |
| `knowledge-server/subscriber/main.go` | Poll, verify, materialize, flip, heartbeat | Implemented |
| `adapters/lib/kernel-path.sh` | Shared kernel source containment | Implemented |
| `adapters/claude/install.sh` | Claude SessionStart hook installer | Implemented |
| `adapters/codex/update-agents-md.sh` | Codex managed-block updater | Implemented |
| `adapters/pi/run.sh` | Checked pi launcher | Implemented |
| `tests/run.sh` | Portable adapter/cutover regression suite | Implemented |
| `kernel/kernel.template.md` | Tier A body shape | Template |
| `schema/frontmatter.md` | Portable metadata reference; not a v1 import format | Reference |

Removed in step 7: `adapters/sync.sh`, `publisher/publish.sh`, and
`tests/publisher/`. Do not route private knowledge through Forgejo or restore
those paths without a new accepted design.

## 4. Runtime data flow

```text
operator UI/API
    -> SQLite document versions
    -> transactional immutable release
    -> manifest + archive
    -> per-host authenticated subscriber
    -> releases/<id>[.<n>]
    -> atomic corpus symlink
    -> harness adapter
    -> Tier A kernel in fresh context
```

The subscriber uses polling in v1. Server failure retains the last-good corpus;
bad manifests, archives, hashes, redirects, credentials, or filesystem writes
never replace it. The fleet page is the fail-loud operator surface.

## 5. Implemented v1

- SQLite collections, immutable document versions, one-active-per-family, and
  transactional release cuts.
- Draft exclusion plus 2,000-word and 24-KiB kernel release lints.
- Deterministic manifest/content hashes and tar archives.
- Embedded UI for browse/edit/history/diff, guarded publish, conflicts, and
  fleet state.
- Edit/claim/kernel-policy conflicts and append-only resolution audit.
- Operator and host bearer credentials, token-bound host identity, TLS gate,
  bounded request bodies, and server read/idle timeouts.
- Subscriber fail-soft convergence, path/tar containment, exact manifest-set
  materialization, hash verification, fresh-directory force-resync, atomic
  corpus selection, and heartbeat.
- Fleet current/stale/dark classification and persistent force-resync flags.
- Claude, Codex, and pi kernel adapters with shared containment and managed
  marker regressions.

Not implemented: Tier B trigger loading, automated claim detection,
draft-reference/dangling-supersession lints, additional collections,
machine-submitted streams, SSE notifications, multi-instance HA, Postgres,
kit/schema compatibility negotiation, or authenticated binary distribution.

## 6. Security boundaries

- API input, archive bytes, manifests, identifiers, paths, redirects, kernel
  bytes, and adapter targets are untrusted.
- The server database is the only writable knowledge authority.
- Non-loopback serving requires bearer authentication plus TLS unless the
  explicit dangerous override is used.
- A subscriber token is one host identity; administrative writes require the
  operator token.
- The fixed single-user model does not isolate same-uid local processes. Do not
  present mode-0600 token files or local releases as a principal boundary.
- Subscriber failure keeps the last-good corpus; operator observability must
  surface stale/error/dark hosts.

## 7. Change and release workflow

Until authenticated binary releases and compatibility negotiation land:

```text
isolated branch/worktree -> reproduce -> minimum implementation
-> narrow tests -> broad tests -> diff review -> Forgejo PR -> reviewed merge
```

Forgejo is the only push and release authority. Consumer repositories pin and
deploy releases; they never write back into this repository.

## 8. Verification entry points

```sh
# Portable adapter and cutover regressions
sh -n tests/run.sh
sh tests/run.sh

# Go implementation
cd knowledge-server
go test -race ./...

# Pure UI helpers
node --test ui/lib_test.mjs

# Shell lint
shellcheck -x ../adapters/lib/kernel-path.sh \
  ../adapters/claude/install.sh \
  ../adapters/codex/update-agents-md.sh \
  ../adapters/pi/run.sh \
  ../tests/run.sh
```

The full Go suite requires loopback socket permission because API and
subscriber tests use `httptest`. A fresh-session injection check and production
TLS/service checks run in the consumer environment.

Documentation changes also run:

```sh
git diff --check
rg -n 'home-network' AGENTS.md CLAUDE.md README.md docs
rg -n 'sync\.sh|publisher/publish\.sh|two-principal' \
  AGENTS.md CLAUDE.md README.md adapters docs schema tests
```

Expected `home-network` hits describe it only as a consumer boundary. Expected
legacy-delivery hits occur only in explicit supersession/history text and the
negative cutover regression.

## 9. Canonical terminology

| Term | Meaning |
|---|---|
| kit | This public generic framework and its Forgejo release lifecycle |
| knowledge server | Single writable service containing store, API, and UI |
| collection | Server policy grouping such as `kernel` or `docs` |
| document version | Immutable saved revision in the server database |
| release | Immutable manifest/archive snapshot of active release-bearing docs |
| subscriber | Thin host client that verifies and materializes releases |
| corpus | Subscriber-selected local release exposed at `$KNOWLEDGE_HOME/corpus` |
| Tier A / kernel | Small operating contract injected into every fresh session |
| Tier B | Materialized procedure docs intended for trigger-based loading |
| adapter | Harness-specific Tier A injection mechanism |
| consumer | Environment repository/operator that pins and deploys the kit |
