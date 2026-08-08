# agent-knowledge-kit

A server-backed knowledge layer for AI agent harnesses: curated durable
knowledge plus deterministic context injection, so agents start each session
with the operating rules they need.

The kit works with Claude Code, OpenAI Codex CLI, and
[pi](https://github.com/badlogic/pi-mono). A single Go server owns the writable
knowledge store and release history. Thin subscribers materialize verified
release snapshots on consumer hosts; small POSIX-shell adapters inject the
Tier A kernel through each harness's native context mechanism.

## Status

Knowledge-server v1 is implemented. The legacy Git corpus transport and the
two-principal fixture publisher were retired in the step-7 cutover. Forgejo is
the authoritative Git service for this kit's source and releases, but it is
not a knowledge transport or store.

The server is intentionally single-instance SQLite in v1. Environment
deployment, host inventory, backups, and service supervision belong to the
consumer repository.

## Why push knowledge

Pull-only retrieval fails silently: an agent that does not know a fact exists
will not search for it. The kit uses three delivery tiers:

| Tier | Mode | v1 mechanism |
|---|---|---|
| A | Always injected | A capped kernel materialized and passed to every fresh session |
| B | Loaded on trigger | Procedure documents materialized on each host; trigger loader remains consumer/harness work |
| C | Queried on demand | Future query collections and external search/memory tools |

## Components

```text
human operator -> web UI / HTTP API -> SQLite store
                                      -> immutable release snapshot
                                      -> authenticated subscriber
                                      -> $KNOWLEDGE_HOME/releases/<id>
                                      -> atomic $KNOWLEDGE_HOME/corpus pointer
                                      -> Claude / Codex / pi adapter
                                      -> fresh agent context
```

- `knowledge-server/` contains the Go server, embedded curation UI, SQLite
  store, API, fleet view, conflicts, release history, and subscriber.
- `adapters/` contains harness-specific Tier A injection. Adapters know only
  `$KNOWLEDGE_HOME/corpus`; they do not know the server exists.
- `kernel/kernel.template.md` is a body template for the Tier A kernel.
- `schema/frontmatter.md` documents the equivalent portable metadata shape;
  the v1 server stores metadata in SQLite and does not import frontmatter.

## Local quickstart

The loopback configuration below is for evaluation on one machine. Do not
expose it on a network.

1. Start the server:

   ```sh
   cd knowledge-server
   go run . -db knowledge.db
   ```

2. Open `http://127.0.0.1:8471/`. Create an active document in the `kernel`
   collection, preview the release, and publish it. The server blocks a cut if
   the kernel exceeds 2,000 words or 24 KiB.

3. Materialize the current release:

   ```sh
   cd knowledge-server
   go run ./subscriber \
     -server http://127.0.0.1:8471 \
     -home "${KNOWLEDGE_HOME:-$HOME/.config/agent-knowledge}" \
     -once
   ```

4. Wire the harness adapter:

   | Harness | Tier A wiring |
   |---|---|
   | Claude Code | `adapters/claude/install.sh`; its SessionStart hook resolves the active kernel on every new session |
   | Codex CLI | `adapters/codex/update-agents-md.sh`; run it after each subscriber convergence pass |
   | pi | launch through `adapters/pi/run.sh` |

   The Codex adapter copies the current kernel into its managed `AGENTS.md`
   block. A typical scheduled Codex convergence therefore runs the subscriber
   with `-once`, then runs the Codex updater. Both commands are idempotent and
   preserve the previous release when the server is unavailable.

5. Open a fresh harness session and ask for the environment's operating
   rules. The answer should come from injected context without a file read or
   retrieval prompt.

## Network deployment

Every endpoint is authenticated when `-operator-token-file` is configured.
The server refuses a non-loopback bind unless both authentication and TLS are
enabled, except for the explicit dangerous `--insecure-no-auth` override.

Generate an operator token without placing it in a command URL or chat:

```sh
cd knowledge-server
go run . \
  -operator-token-file /protected/path/operator.token \
  -init-operator-token
```

Restart without `-init-operator-token`, supplying `-tls-cert`, `-tls-key`, and
the desired `-listen` address. Mint one host token per subscriber through the
operator-authenticated API, store it in a mode-0600 file, and pass that file
with subscriber `-token-file`. Use `-ca-file` for a private CA.

The subscriber verifies the release manifest, every archive entry, and the
content hash before installing a fresh version directory and atomically
switching `corpus`. Any failure retains the last-good release and reports a
best-effort heartbeat.

## Curation and lifecycle

- The Documents view includes an inline guide to collections, family ids,
  immutable versions, statuses, metadata, and exact-version links. Editor
  controls remain disabled until the latest version has loaded, preventing a
  slow response from overwriting operator input.
- Every save creates an immutable version; one active version is allowed per
  collection/family.
- Saves may carry ordered, version-specific `reference` and `supersedes`
  links. The editor exposes them as a JSON array and document reads return
  them with the selected immutable version. The exact wire contract is in
  `docs/plans/document-link-lints.md`.
- Draft versions never enter a release.
- Publish preview and cut use the same candidate computation. The UI submits
  the preview hash as a cut precondition to detect intervening corpus-byte
  changes; cut recomputes link lints transactionally even when only metadata
  changed.
- Stale optimistic-lock saves open edit-conflict records. Claim conflicts can
  be flagged manually. Kernel-cap, invalid active-reference, and invalid
  supersession failures open policy conflicts. Active references must resolve
  to an exact active version in the release; supersession targets must exist
  and already be superseded.
- Force-resync causes a host to re-fetch, verify, and install a fresh directory
  even when its release id already matches.

The database is the only writable knowledge state. Consumer hosts do not
merge, reconcile, or edit materialized releases.

## Development and verification

```sh
sh tests/run.sh

cd knowledge-server
go test -race ./...
node --test ui/lib_test.mjs
```

`tests/run.sh` covers adapter containment, managed-marker safety, and the
subscriber-materialized cutover shape. The Go suite covers store/API/auth/UI
and subscriber convergence, including fail-soft behavior, archive traversal,
hash mismatch, TLS pinning, force-resync, and fleet/conflict behavior.

## Governance

This public repository owns the generic architecture, schemas, adapters,
tests, compatibility policy, and releases. It must contain no real environment
names, addresses, topology, credentials, or private knowledge.

Forgejo is the only permitted push and release authority. Consumers pin a kit
release and own deployment and private server data; they do not define or
publish the kit.

## License

MIT — see [LICENSE](LICENSE).
