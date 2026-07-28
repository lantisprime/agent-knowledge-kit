# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this repo is

`agent-knowledge-kit` is the **public, generic framework** for a git-native
knowledge layer that pushes durable environment knowledge into AI agent
context deterministically. It has no build system, no dependencies beyond
`git` and POSIX `sh`, and no runtime of its own — the deliverables are a
schema, a kernel template, one sync script, and one thin adapter per harness.

## Hard boundary: kit vs. corpus

**This repo is public and must contain zero environment specifics** — no
hostnames, IPs, usernames, namespaces, topology, or credential locations.
Those belong in a separate *private corpus repo* per environment, written
against `schema/frontmatter.md`. Treat any change that would introduce a real
system name here as a security regression, not a style issue.

The corpus is cloned to `$KNOWLEDGE_HOME/corpus` (default
`~/.config/agent-knowledge`) and is the only thing the adapters read.
`.episodic-memory/` is gitignored for the same reason: local memory
references the operating environment.

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

**The adapter contract** (`adapters/`): every adapter resolves
`${KNOWLEDGE_HOME:-$HOME/.config/agent-knowledge}/corpus/kernel/kernel.md`,
is idempotent, and re-runnable after each sync. They differ only in the
harness-native injection point:

| Adapter | Injection point |
|---|---|
| `claude/install.sh` | writes a SessionStart hook script; prints (does not merge) the `settings.json` fragment — merging JSON from POSIX sh risks corrupting user settings |
| `codex/update-agents-md.sh` | rewrites a marker-delimited managed block in `~/.codex/AGENTS.md`, preserving everything outside the markers |
| `pi/README.md` | no script — `--append-system-prompt <kernel path>` in launch argv, plus native `AGENTS.md`/`CLAUDE.md` discovery |

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

shellcheck adapters/sync.sh adapters/claude/install.sh adapters/codex/update-agents-md.sh
```

Env overrides for testing: `KNOWLEDGE_HOME` (target dir), `KNOWLEDGE_REMOTES`
(comma-separated remote list, overrides `$KNOWLEDGE_HOME/.remotes`),
`CODEX_HOME`.

There is no test suite. The kit's own verification loop is the one in the
README step 5: open a **fresh agent session** on a synced host and ask what
the environment's rules are — the answer must come from context with zero
prompting and zero file reads. Capability claims about harnesses drift; prove
them with the CLI, and record the check in a doc's `verify:` field.

## Design decisions & plan

`docs/architecture.md` is the authoritative design record: system diagram,
settled decisions (events-trigger/git-transports, multi-remote
availability/authority/confidentiality split, fail-soft-for-consumers /
fail-loud-for-operators), the event-propagation layer (`sync.sh listen`,
post-sync hook trust contract), known gaps, and the ordered implementation
plan from two adversarial reviews (2026-07-28 and 2026-07-29). Read it
before extending the sync or adapter layer; the gaps listed there are
tracked deliberately — don't "fix" them silently in passing.

**Do not treat the shipped code as hardened.** The second review's
blocking findings are open: adapters read a *mutable, agent-writable*
checkout, so commit signing secures transport and nothing secures
delivery (`C1-b`); a corpus symlink exfiltrates arbitrary files through
every adapter (`C2-b`); corpus text containing the Codex end marker
escapes the managed block permanently (`H1-b`); `init` clones with no
verification (`C3-b`); and host-clock sync age does not detect a freeze
(`H2-b`). The plan order is **contain, then authenticate, then apply,
then accelerate** — do not automate fleet-wide reapplication ahead of
release authentication.

## Conventions

- **POSIX `sh`, `set -eu`, git-only dependency.** No bash-isms, no jq, no
  Python. If a task needs a real parser, print the fragment and let the
  operator (or their agent) apply it — that is what `claude/install.sh` does.
- **Schema rules are load-bearing** (`schema/frontmatter.md`): one claim one
  home; supersede in place rather than publishing peers; kernel edits are
  PR-only and bounded by the token cap; `status: draft` never ships.
  These are **policy, not enforcement** — nothing parses frontmatter or
  counts tokens, and `kernel/kernel.template.md` itself carries
  `status: draft` (`H5-b`, `L1-b`). Don't cite them as guarantees.
- Docs are written for two audiences at once (operators and the agents
  reading them as context) — keep them terse and imperative.
