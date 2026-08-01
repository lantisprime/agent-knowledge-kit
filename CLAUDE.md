# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

Read `AGENTS.md` first for repository ownership and workflow, then use
`docs/REPO_MAP.md` for orientation. Update the map in the same approved slice
when ownership, architecture, lifecycle, directory roles, supported adapters,
verification entry points, or canonical terminology changes.

## What this repo is

`agent-knowledge-kit` is the **public, generic framework** for a git-native
knowledge layer that pushes durable environment knowledge into AI agent
context deterministically. It has no build system, no dependencies beyond
`git` and POSIX `sh`, and no runtime of its own — the deliverables are a
schema, a kernel template, sync/publication primitives, and one thin adapter
per harness.

## Self-governance boundary

This repository is the sole authority for the generic kit's architecture,
schemas, adapters, tests, compatibility policy, and releases. Environment
repositories and private corpora consume pinned kit releases; they do not
define or publish the kit. `home-network` is an environment consumer, not an
upstream specification or controller.

When a consumer exposes a generic gap, make the change and review it here.
Do not copy a competing canonical design or implementation into the consumer,
and do not import environment-specific policy into this public repository.

Forgejo is the authoritative Git service and the only permitted push target.
Never push to GitHub. Any separately authorized server-side read mirror is
downstream of Forgejo and has no governance or release authority.

## Hard boundary: kit vs. corpus

**This repo is public and must contain zero environment specifics** — no
hostnames, IPs, usernames, namespaces, topology, or credential locations.
Those belong in a separate *private corpus repo* per environment, written
against `schema/frontmatter.md`. Treat any change that would introduce a real
system name here as a security regression, not a style issue.

**Current prototype:** the corpus is cloned to `$KNOWLEDGE_HOME/corpus`
(default `~/.config/agent-knowledge`) and is the only thing the shipped
adapters read. `.episodic-memory/` is gitignored for the same reason: local
memory references the operating environment. `publisher/publish.sh` is a
separate fixture-only transaction primitive; no adapter consumes its
publications yet, and its production promotion verb is deliberately disabled.

## Architecture

The thesis: pull-based retrieval fails *silently* (an agent that doesn't know
a fact exists never searches for it), so critical knowledge must be pushed.
Three delivery tiers, which every file here serves:

- **Tier A** — `kernel/kernel.md` in the corpus, injected into *every*
  session by an adapter. Hard cap ~1–2k tokens; `kernel/kernel.template.md`
  defines the section shape (access paths, hard limits, sandbox contract,
  change control, stop conditions, Tier B/C pointers).
- **Tier B** — full procedure docs, loaded on trigger (`triggers:` frontmatter).
- **Tier C** — query-on-demand tooling; Tier A pointers make it discoverable.

**The current prototype adapter contract** (`adapters/`): every code-backed
adapter validates and resolves a regular, canonically contained kernel below
`${KNOWLEDGE_HOME:-$HOME/.config/agent-knowledge}/corpus`, supporting the
current nested and flat corpus layouts. The Claude and Codex adapters are
re-runnable after each sync; the pi adapter validates at launch. They differ
only in the harness-native injection point. Git checkouts additionally
require the kernel's tree entry to be a regular blob:

| Adapter | Injection point |
|---|---|
| `claude/install.sh` | writes a SessionStart hook script; prints (does not merge) the `settings.json` fragment — merging JSON from POSIX sh risks corrupting user settings |
| `codex/update-agents-md.sh` | rewrites a marker-delimited managed block in `~/.codex/AGENTS.md`, preserving everything outside the markers |
| `pi/run.sh` | validates the kernel, then launches `pi --append-system-prompt <kernel path>`; pi also has native `AGENTS.md`/`CLAUDE.md` discovery |

**Fail-soft is deliberate.** `sync.sh pull` tries each remote in order and,
if none is reachable, logs a warning, marks state `stale`, and exits 0 —
keeping the last-synced corpus in place. The Claude hook likewise prints a
warning instead of failing when the kernel is missing. An unreachable remote
must never break an agent session; the knowledge is meant to survive exactly
the outages it documents. Preserve this behavior when editing.

## Commands

```sh
./adapters/sync.sh init <url>[,<mirror-url>...]   # first clone into $KNOWLEDGE_HOME
./adapters/sync.sh pull                           # converge (cron/timer this)
./adapters/sync.sh status                         # last-sync line

./adapters/claude/install.sh                      # writes hook, prints settings fragment
./adapters/codex/update-agents-md.sh              # requires a synced kernel; run after each pull
./adapters/pi/run.sh [pi args...]                 # validates the kernel, then launches pi

./publisher/publish.sh prepare <control-root> <publication-root>
./publisher/publish.sh promote-fixture <control-root> <publication-root> <candidate>
./publisher/publish.sh check <control-root> <publication-root>

sh tests/run.sh                                   # all portable regressions
sh tests/publisher/two-principal.sh               # privileged; exit 77 without prerequisites
shellcheck adapters/sync.sh adapters/lib/kernel-path.sh \
  adapters/claude/install.sh adapters/codex/update-agents-md.sh \
  adapters/pi/run.sh publisher/publish.sh tests/run.sh \
  tests/publisher/run.sh tests/publisher/two-principal.sh
```

Env overrides for testing: `KNOWLEDGE_HOME` (target dir), `KNOWLEDGE_REMOTES`
(comma-separated remote list, overrides `$KNOWLEDGE_HOME/.remotes`),
`CODEX_HOME`.

`tests/run.sh` runs the kernel-source/managed-marker regressions and the
fixture-only publication transaction suite. The opt-in two-principal runner
requires root plus pre-provisioned publisher, agent, and shared-group fixtures;
exit 77 is a prerequisite skip, not C1-b evidence. Automated macOS/Linux
execution and the broader adversarial matrix remain open. The kit's end-to-end
verification loop is still README step 5: open a **fresh agent session** on a
synced host and ask what the environment's rules are — the answer must come
from context with zero prompting and zero file reads. Capability claims about
harnesses drift; prove them with the CLI, and record the check in a doc's
`verify:` field.

## Design decisions & plan

`docs/architecture.md` is the authoritative design record: system diagram,
settled decisions (including the separate-identity delivery trust boundary),
the event-propagation layer (`sync.sh listen`, post-sync hook trust contract),
known gaps, and the ordered implementation plan from two adversarial reviews
(2026-07-28 and 2026-07-29). The accepted `C1-b` contract is frozen in
`docs/plans/delivery-trust-boundary.md`; only its fixture-only publication
transaction sub-slice is implemented. Read both before extending
the sync or adapter layer; the gaps listed there are tracked deliberately —
don't "fix" them silently in passing.

**Do not treat the shipped code as hardened.** The containment slice now
rejects corpus symlinks and Codex marker injection (`C2-b`, `H1-b`), with
portable regressions. The fixture-only publisher exercises immutable staging,
atomic selection, anti-rollback state, strict local integrity, and a
publication mutex, but production authentication, proven ancestor/effective
access, orphan-lock recovery, and protected harness wiring have not landed.
Adapters still read a *mutable, agent-writable* checkout, so commit signing
secures transport and nothing yet secures end-to-end delivery. `init` also
clones with no verification (`C3-b`), and host-clock sync age does not detect
a freeze (`H2-b`). The plan order is **contain, then authenticate, then apply,
then accelerate** — do not automate fleet-wide reapplication ahead of corpus
release authentication.

For delivery-boundary work, same-uid ownership checks, agent-controlled path
overrides, or protecting corpus bytes while leaving launcher/harness
configuration writable do not satisfy step 1. Follow the two-principal
negative tests in the accepted plan and keep prototype fallbacks out of
hardened mode.

Its authority is scoped to this repository. External designs, including
environment integration ADRs, are requirements or historical input only. A
generic decision becomes authoritative only when it lands here. Conversely,
this repository does not own consumer deployment, host inventory,
orchestration, monitoring, or private corpus content.

## Conventions

- **POSIX `sh`, `set -eu`, git-only dependency.** No bash-isms, no jq, no
  Python. The publisher explicitly dispatches incompatible native macOS/Linux
  `stat`, `find -perm`, `readlink -n`, and atomic symlink-replacement flags;
  never replace that selector operation with plain `mv` onto `current`. If a
  task needs a real parser, print the fragment and let the operator (or their
  agent) apply it — that is what `claude/install.sh` does.
- **Schema rules are load-bearing** (`schema/frontmatter.md`): one claim one
  home; supersede in place rather than publishing peers; kernel edits are
  PR-only and bounded by the token cap; `status: draft` never ships.
  These are **policy, not enforcement** — nothing parses frontmatter or
  counts tokens, and `kernel/kernel.template.md` itself carries
  `status: draft` (`H5-b`, `L1-b`). Don't cite them as guarantees.
- Docs are written for two audiences at once (operators and the agents
  reading them as context) — keep them terse and imperative.
