# Agent Knowledge Kit Repository Map

> **AI orientation artifact**
> - **Purpose:** locate authority, code, schemas, workflows, and verification
>   entry points without importing assumptions from a consumer repository.
> - **Authority:** navigation aid; `AGENTS.md`, `CLAUDE.md`, accepted plans,
>   tests, schemas, and implementation remain the governing evidence.
> - **Verified:** 2026-08-01 against Forgejo `main` at `b183b59`.
> - **Update trigger:** ownership, architecture, lifecycle, directory roles,
>   supported adapters, verification entry points, or terminology changes.

## 1. Executive summary

`agent-knowledge-kit` is a public, generic framework for delivering durable
knowledge into AI agent context. It defines the corpus contract, a Tier A
kernel template, synchronization behavior, and harness adapters. It contains
no environment-specific knowledge and operates independently of every
consumer repository.

The checked-in shell scripts are currently a happy-path prototype, not a
hardened release system. `docs/architecture.md` records the accepted target
and its ordered hardening plan. Claims such as draft exclusion, token-cap
enforcement, authenticated releases, immutable publication, trigger-based
Tier B loading, and event-driven synchronization are not implemented merely
because they appear in a schema or design document.

## 2. Authority and dependency direction

| Concern | Authoritative home |
|---|---|
| Generic architecture and threat model | This repo: `docs/architecture.md` |
| Agent/contributor contract | This repo: `AGENTS.md`, `CLAUDE.md` |
| Public interface | This repo: `README.md` |
| Schema and compatibility policy | This repo: `schema/` |
| Adapters, tests, and releases | This repo |
| Environment deployment and monitoring | Consumer repository |
| Environment facts and procedures | Private corpus |

The required ownership flow, once release authentication exists, is:

```text
kit change -> tests/review in this repo -> Forgejo release
    -> consumer pins release -> consumer deploys adapters
    -> private corpus validates against pinned schema
```

An external repository may provide requirements or open a proposed change,
but it cannot be the kit's source of truth. `home-network` is one consumer:
it owns its hosts, environment integration, and operational policy, not the
kit's schemas, code, roadmap, tags, or releases.

## 3. Repository shape

| Path | Role | Current status |
|---|---|---|
| `AGENTS.md` | Repository-wide agent contract and ownership boundary | Authoritative guidance |
| `CLAUDE.md` | Implementation constraints, commands, and known gaps | Authoritative guidance |
| `README.md` | Public concept, quickstart, and corpus contract | Shipped interface plus policy prose |
| `docs/architecture.md` | Threat model, accepted decisions, gaps, and ordered plan | Target design; not all implemented |
| `adapters/sync.sh` | Clone, pull, and status prototype | Implemented, unhardened |
| `adapters/lib/kernel-path.sh` | Shared kernel source validation | Implemented containment helper |
| `adapters/claude/install.sh` | Claude SessionStart hook installer | Implemented, unhardened |
| `adapters/codex/update-agents-md.sh` | Codex global managed-block updater | Implemented, unhardened |
| `adapters/pi/run.sh` | Checked pi launcher | Implemented containment adapter |
| `adapters/pi/README.md` | pi launch and discovery guidance | Public adapter instructions |
| `tests/run.sh` | Portable C2-b/H1-b adapter regressions | Implemented narrow suite |
| `kernel/kernel.template.md` | Shape of an environment Tier A kernel | Template; currently draft |
| `schema/frontmatter.md` | Minimal corpus document schema | Prose contract; enforcement absent |
| `LICENSE` | MIT license | Release/legal boundary |

There is currently no automated cross-platform test pipeline, authenticated
release pipeline, schema version, or machine-enforced compatibility check.
Adding those belongs here, not in a consumer repository.

## 4. Runtime data flow

Current prototype:

```text
private corpus Git remote(s)
    -> sync adapter on a consumer host
    -> mutable local checkout
    -> harness adapter
    -> Tier A kernel in a fresh agent context
```

Target after delivery containment and authentication:

```text
verified corpus release
    -> immutable versioned publication
    -> harness adapter
    -> Tier A kernel in a fresh agent context
```

The kit supplies generic tooling. The consumer chooses the OS principal,
absolute installation paths, scheduler, secret transport, monitoring, and
deployment mechanism. The private corpus supplies all environment content.
Those layers may depend on a pinned kit release; the kit does not depend on
their repositories.

## 5. Current implementation versus target

### Implemented prototype

- Ordered git remotes for clone and pull.
- Last-known corpus retained when all pulls fail.
- Claude SessionStart hook generation.
- Codex managed-block generation.
- Checked pi command-line launcher.
- Regular-file, git-tree-mode, and canonical-path validation before adapters
  read a kernel.
- Whole-line managed-marker rejection before Codex touches its target.
- Portable regression fixtures for the containment behaviors above.
- Markdown frontmatter and kernel templates.

### Required before hardened use

- Tests for the remaining URL, state, target-path, and draft failure modes.
- Canonical-path and file-type containment for adapter targets.
- Atomic, locked writes that preserve user-owned content or fail safely.
- Structured remote parsing, redacted logs, and restrictive state files.
- One versioned corpus layout plus versioned frontmatter and routing schemas.
- Verified bootstrap, authenticated immutable releases, anti-rollback, and
  fail-loud operator status.
- Enforced draft exclusion and size limits; defined Tier B routing/loading.
- Platform verification on supported macOS and Linux shells.

Do not add fleet-wide hooks, listeners, or deployment automation ahead of
the containment and release-authentication sequence.

## 6. Change and release workflow

Until this repository gains an authenticated release pipeline, development
stops at a reviewed Forgejo merge:

```text
isolated branch/worktree -> reproduce with tests -> minimal fix
-> local verification -> diff review -> Forgejo PR -> reviewed merge
```

A tag must not be presented as a hardened release before release
authentication lands. After that, Forgejo produces the release/tag and the
consumer updates an explicit pin.

Forgejo is the only push authority. Never push to GitHub. A separately
authorized server-side mirror is downstream and read-only from the kit's
governance perspective; it must not trigger or define a release.

Consumer repositories must not:

- tag or publish the kit;
- keep a competing canonical copy of kit schemas or architecture;
- run a workflow that writes directly into the kit repository;
- silently follow mutable `main` for production deployment.

## 7. Verification entry points

Current syntax checks:

```sh
sh -n adapters/sync.sh
sh -n adapters/lib/kernel-path.sh
sh -n adapters/claude/install.sh
sh -n adapters/codex/update-agents-md.sh
sh -n adapters/pi/run.sh
sh -n tests/run.sh
sh tests/run.sh
```

Current manual happy-path checks are documented in `README.md`.
`tests/run.sh` covers the `C2-b` source-containment and `H1-b`
marker-injection gaps on the local platform. The full macOS/Linux adversarial
matrix remains plan step 9.

Documentation-only changes should at minimum run:

```sh
git diff --check
rg -n 'home-network' AGENTS.md CLAUDE.md README.md docs
```

The substantive prose hits name `home-network` as a consumer boundary, never
an authority; the verification command and this explanation also match.

## 8. Canonical terminology

| Term | Meaning |
|---|---|
| kit | This public generic framework, including schemas, adapters, tests, and releases |
| corpus | Separate, usually private repository containing environment knowledge |
| consumer | Repository or environment that pins and deploys a kit release |
| Tier A / kernel | Small contract injected into every fresh agent session |
| Tier B | Procedures selected for task-triggered context loading |
| Tier C | Knowledge queried on demand |
| publication | Validated local content exposed to adapters |
| adapter | Harness-specific mechanism that delivers Tier A knowledge into context |
| release | Immutable, authenticated kit version produced from this repository |
| pin | Consumer's explicit reference to a kit release/schema version |
