# CLAUDE.md

Read `AGENTS.md` first, then `docs/REPO_MAP.md`. Update the map in the same
approved slice when ownership, architecture, lifecycle, directory roles,
supported adapters, verification entry points, or terminology changes.

## What this repository is

`agent-knowledge-kit` is the public, generic framework for a server-backed
knowledge layer that injects durable operating knowledge into agent context.
It ships:

- a single Go knowledge server with an embedded SQLite store and web UI;
- a thin Go subscriber that materializes verified release snapshots;
- POSIX-shell adapters for Claude Code, Codex CLI, and pi;
- the kernel body template, portable metadata reference, tests, and plans.

Knowledge-server v1 build-order steps 1–7 are implemented. The step-7 cutover
retired the Git corpus sync script and two-principal fixture publisher. Forgejo
remains authoritative for this kit's source and releases; it is not the corpus
store or delivery transport.

## Ownership and public-data boundary

This repository is the sole authority for the generic kit's architecture,
schemas, adapters, tests, compatibility policy, and releases. Consumer
repositories pin and deploy a kit release. They own service supervision,
backups, host inventory, environment migration, and private server data.

This public repository must contain no real hostnames, addresses, usernames,
namespaces, topology, credentials, or private knowledge. `.episodic-memory/`
is ignored because local memory can contain environment information.

Forgejo is the only permitted push target. Never push to GitHub. A separately
authorized mirror is downstream and has no release or governance authority.

## Current architecture

The server database is the only writable knowledge state. Operators curate
through the embedded UI or HTTP API. A release cut snapshots active `kernel`
and `docs` collection versions and records their hashes. Subscribers fetch the
manifest and archive, reject unsafe or unmanifested entries, recompute the
content hash, install a fresh version directory, atomically switch
`$KNOWLEDGE_HOME/corpus`, and heartbeat.

Subscribers fail soft: server, authentication, archive, hash, or filesystem
failure retains the last-good corpus. The operator sees convergence through
heartbeats and the fleet page. Force-resync erases the subscriber's applied
belief and forces a fresh verified materialization.

Adapters consume only files below `$KNOWLEDGE_HOME/corpus`:

| Adapter | Injection point |
|---|---|
| `adapters/claude/install.sh` | installs a SessionStart hook that resolves and emits the current kernel |
| `adapters/codex/update-agents-md.sh` | rewrites a marker-delimited block in `CODEX_HOME/AGENTS.md`; rerun after each subscriber pass |
| `adapters/pi/run.sh` | validates the kernel, then launches `pi --append-system-prompt <path>` |

The shared validator rejects symlinked/non-regular kernels and canonical-path
escapes. If a corpus root happens to be a Git checkout, it additionally
requires a regular blob entry; Git is not a supported v1 delivery transport.

## Commands

```sh
# Server and subscriber
(cd knowledge-server && go run . -db knowledge.db)
(cd knowledge-server && go run ./subscriber \
  -server http://127.0.0.1:8471 -home /path/to/knowledge -once)

# Harness adapters
./adapters/claude/install.sh
./adapters/codex/update-agents-md.sh
./adapters/pi/run.sh [pi args...]

# Verification
sh tests/run.sh
(cd knowledge-server && go test -race ./...)
(cd knowledge-server && node --test ui/lib_test.mjs)

shellcheck -x adapters/lib/kernel-path.sh adapters/claude/install.sh \
  adapters/codex/update-agents-md.sh adapters/pi/run.sh tests/run.sh
```

Use `KNOWLEDGE_HOME` to select the subscriber/adapters root and `CODEX_HOME`
for Codex adapter tests. Server and subscriber flags are documented by their
`-h` output and in `README.md`.

## Security invariants

- The API is the only write door; no other component opens the database.
- Non-loopback server exposure requires authentication and TLS unless the
  operator explicitly selects the dangerous `--insecure-no-auth` override.
- Bearer tokens never belong in URLs, chat, logs, fixtures, or this repository.
- Subscriber manifests, archives, paths, redirects, hashes, and remote errors
  are untrusted input. Failures retain the previous corpus.
- Adapters treat corpus paths, kernel bytes, managed markers, and target paths
  as untrusted. Preserve containment and byte-preservation regressions.
- The fixed single-OS-user constraint means same-uid local processes can read
  token files and mutate local materialized bytes. Source containment and
  force-resync recovery are implemented; same-uid isolation is not claimed.

## Plans and status

`docs/plans/knowledge-server.md` is the accepted detailed contract. Steps 1–7
are implemented. `docs/architecture.md` is the current decision record.
`docs/plans/delivery-trust-boundary.md` is retained only as an explicitly
superseded historical plan; do not implement or cite its two-principal model as
current architecture.

Post-v1 work is not ordered: automated claim detection, additional
collections/machine-submitted streams, additive lifecycle/provenance schema,
and draft-reference/dangling-supersession lints require a newly approved
slice. Multi-instance HA and Postgres are out of scope. Environment deployment
and consumer migration remain consumer-repository work.

## Conventions

- Go code uses the standard toolchain plus the pinned SQLite dependency. Run
  `gofmt` and the race suite for Go changes.
- Adapter/test shell stays POSIX `sh` with `set -eu` where appropriate. No
  bash-only syntax, jq, or Python dependency.
- Keep the subscriber independent of the store package; it reproduces and
  verifies the wire contract from the manifest alone.
- Keep the UI a client of the public HTTP API; it never touches SQLite.
- Make surgical changes and preserve fail-soft consumer behavior.
- Policy prose is not enforcement. State exactly which behaviors have tests.
