# Repository Guidance for AI Agents

This file applies to the entire `agent-knowledge-kit` repository. Read it
together with `CLAUDE.md`; the stricter instruction wins.

## Ownership and authority

1. This repository is the sole source of truth for the generic kit: its
   architecture, schemas, kernel template, adapters, tests, compatibility
   policy, and releases.
2. Environment repositories and private corpora are consumers. They may pin a
   released kit version and own environment-specific integration, but they do
   not define or publish the kit. In particular, `home-network` is not a
   controller or upstream specification for this repository.
3. Proposed generic changes discovered in a consumer belong in this
   repository and must be reviewed here. Do not maintain a canonical copy of
   generic kit design or code in a consumer repository.
4. This public repository must contain no real hostnames, IP addresses,
   usernames, namespaces, topology, credentials, or private corpus content.
5. Forgejo is the authoritative Git service and the only permitted push
   target. Never push to GitHub. A read mirror, if the operator separately
   authorizes one, is not a release or governance authority.

## Start-of-task orientation

Read these in order:

1. `CLAUDE.md` -- implementation constraints and known security gaps.
2. `docs/REPO_MAP.md` -- ownership, repository shape, workflows, and terms.
3. `README.md` -- user-facing contract and quickstart.
4. `docs/architecture.md` -- current architecture decisions and target plan.
5. The nearest accepted plan and relevant tests for the requested slice.

Then run read-only checks:

```sh
git status --short --branch
git remote -v
git log -1 --oneline --decorate
git diff --stat
```

Do not assume a checkout is current or clean. Do not fetch, pull, switch,
rebase, or repair an existing worktree unless the task authorizes it.

## Truth and status labels

Use this order when evidence conflicts:

1. Explicit operator instruction for the current task.
2. `AGENTS.md` and `CLAUDE.md` in this repository.
3. An accepted, frozen plan for the active slice.
4. Tests and machine-readable schemas.
5. Current implementation.
6. `docs/architecture.md` for accepted design and ordered future work.
7. `README.md` for the public interface.
8. External designs, integration ADRs, reviews, and historical artifacts.

Distinguish `implemented`, `prototype`, `planned`, and `deferred`. The current
scripts demonstrate the happy path but are not hardened. Never present a
policy stated only in prose as an enforced guarantee.

## Change workflow

For non-trivial changes:

1. `[orient] -> verify:` authority, clean baseline, and applicable plan.
2. `[reproduce] -> verify:` a failing test or fixture demonstrates the gap.
3. `[freeze scope] -> verify:` writable files, behavior contracts,
   exclusions, and exact checks are explicit.
4. `[implement] -> verify:` minimum complete change with no environment data.
5. `[review] -> verify:` inspect the actual diff and classify every finding as
   `ACCEPT`, `ACCEPT-WITH-MOD`, `REJECT`, `DEFER`, or `NEEDS-EVIDENCE`.
6. `[test] -> verify:` narrow tests, then the relevant broader suite.
7. `[handoff] -> verify:` decisions, files, repository state, exact results,
   limitations, and next step are recorded.

Security regressions require a negative test first. Treat corpus input,
remote metadata, filesystem paths, managed-block markers, and adapter targets
as untrusted. Do not expand deployment or fleet automation until the delivery
trust boundary and release authentication are implemented and tested.

## Session handoff persistence

- When producing a session handoff, store the same substantive handoff in the
  project-local episodic-memory store under `.episodic-memory/episodes/` before
  declaring wrap-up complete. Use project `agent-knowledge-kit`, category
  `context`, and tag `handoff`; do not rely on a Codex transcript as the only
  durable copy.
- Include the decisions, files changed, repository state, exact verification,
  limitations, and next concrete step. Do not store credentials, private corpus
  content, or environment-specific infrastructure details.
- When the operator asks to `load handoff`, search active project-local
  `handoff` episodes first and load the newest one. If none exists, check the
  canonical session handoff and then prior Codex session transcripts, and state
  which source was used.
- Treat a handoff as historical context rather than current-state proof.
  Reconcile it with the required start-of-task Git checks before acting.

## Repository boundaries

- `agent-knowledge-kit`: generic, public framework and its release lifecycle.
- consumer repository: environment integration, version pin, installation,
  deployment, and operational monitoring.
- private corpus: environment kernel, routing, procedures, and sensitive facts.

Dependency direction is one-way:

```text
agent-knowledge-kit release -> consumer pin -> private corpus validation
```

The kit must not reach into a consumer to obtain its design, tests, schemas,
or release inputs. A consumer must not push, tag, release, or rewrite this
repository. Cross-repository automation may open a change request, but the
change must be reviewed and released from the kit repository.

## Documentation discipline

Update `docs/REPO_MAP.md` in the same approved slice when ownership,
architecture, directory roles, lifecycle, supported adapters, verification
entry points, or canonical terminology changes. Do not churn it for a routine
bug fix that leaves those contracts unchanged.

Update `README.md` when the supported public interface changes. Update
`docs/architecture.md` when a design decision or implementation ordering
changes. Keep current behavior separate from target design in every document.
