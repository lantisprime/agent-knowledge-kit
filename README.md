# agent-knowledge-kit

A generic, git-native **knowledge layer for AI agent harnesses** — curated
durable knowledge plus **deterministic context injection**, so agents start
every session already knowing your environment's rules instead of
rediscovering (or guessing) them.

Works with Claude Code, OpenAI Codex CLI, and [pi](https://github.com/badlogic/pi-mono).
No server, no database, no vendor lock: a git repo, a sync script, and one
small adapter per harness.

## The thesis

> The model can only act on what reaches the context window.
> The harness decides what gets there. Storage is the easy part —
> **injection is the architecture.**

Agent memory has four tiers (working / episodic / semantic / procedural).
This kit implements delivery for the **procedural and semantic** tiers: the
standing knowledge — access paths, sandbox rules, runbooks, hard limits —
that every agent in your environment must have, whether or not it thinks to
ask.

Pull-based retrieval (search, RAG, memory queries) fails silently: the agent
that doesn't know a fact exists doesn't search for it, then states a wrong
answer with confidence. The fix is **push-first**:

| Tier | Mode | Mechanism |
|---|---|---|
| A | always injected | a hand-curated **kernel** (~1–2k tokens hard cap) delivered into every session |
| B | injected on trigger | full procedure docs loaded when the task/command warrants |
| C | queried on demand | your search/memory tooling; Tier A pointers make it discoverable |

## Kit vs. corpus

- **This repo (the kit)** is the public, generic framework: schema, kernel
  template, sync tooling, harness adapters. It contains nothing about any
  real environment.
- **Your corpus** is a separate repo (usually private!) holding your actual
  kernel and docs, written against the kit's schema. Access paths, hostnames,
  and internal topology belong there — never in a public repo.

## Governance and consumption

The kit governs and releases itself from this repository. Its architecture,
schemas, adapters, tests, compatibility policy, and release tags are not
owned by any environment repository. A consumer may propose a change, but a
generic change is reviewed and released here before the consumer adopts it.

Consumers own environment integration: they pin an immutable kit release,
deploy its adapters, validate their private corpus against the pinned schema,
and monitor convergence. The dependency is one-way: kit release → consumer
pin → private corpus. The kit never reads a consumer repository as a design
or release input.

Forgejo is the authoritative Git service and the only push target. Do not
push this repository to GitHub; any separately authorized read mirror is not
a release authority.

## Quickstart

1. Create your corpus repo from the template:

   ```sh
   ./adapters/sync.sh init <your-corpus-git-url>   # clones to $KNOWLEDGE_HOME
   ```

   `KNOWLEDGE_HOME` defaults to `~/.config/agent-knowledge`. Multiple
   remotes are supported (comma-separated) and tried in order — useful when
   the canonical remote is only reachable inside one network and a mirror
   serves the rest.

2. Write your kernel (`kernel/kernel.md` in your corpus) from
   `kernel/kernel.template.md`. Keep it under the cap; everything that
   doesn't make the cut becomes a Tier B doc.

3. Wire or launch the adapters:

   | Harness | Tier A wiring |
   |---|---|
   | Claude Code | `adapters/claude/install.sh` — SessionStart hook emits the kernel |
   | Codex CLI | `adapters/codex/update-agents-md.sh` — managed block in `~/.codex/AGENTS.md` (global, loads every session) |
   | pi | launch through `adapters/pi/run.sh`, which validates the kernel before adding `--append-system-prompt`; pi also auto-discovers repo `AGENTS.md`/`CLAUDE.md` |

   The code-backed adapters refuse symlinked, non-regular, or path-escaping
   kernels and reject non-blob kernel entries when the corpus is a Git
   checkout. The Codex adapter also refuses kernel content that contains
   either managed-block marker as a whole line. This is source containment,
   not a complete delivery-integrity boundary: the current checkout remains
   mutable by the agent uid (`C1-b` in `docs/architecture.md`).

4. Schedule `sync.sh pull` (cron / systemd timer / launchd) so every host
   converges on merge. Hosts keep their last-synced copy offline — the
   knowledge survives exactly the outages it documents.

5. Verify the loop: open a **fresh agent session** on a synced host and ask
   the agent what your environment's rules are — the answer must come from
   context, with zero prompting and zero file reads.

## Corpus layout

```
corpus/
├── kernel/kernel.md        # Tier A — the always-injected contract
├── docs/…                  # Tier B — full procedures, runbooks, guides
└── schema/                 # copied from the kit; your lint rules
```

Every doc carries frontmatter (see `schema/frontmatter.md`): `status`,
`supersedes`, `owner`, `audience`, `verify`, `generated-from`. The lifecycle
rules that keep memory from rotting:

- **Promotion ladder** — sessions → summaries → facts → procedures. When a
  lesson recurs, distill it into a doc via PR; leave a pointer behind.
- **Contradiction rule** — new truth supersedes *in place*; old truth
  survives as git history and a `superseded` doc, never as a peer that can
  outrank current state.
- **Ownership** — the kernel and anything approval-gate-shaped is manually
  curated and PR-only. Never auto-extract your safety rules.
- **Verifiability** — prefer docs whose claims carry a `verify:` command.
  Capability facts drift; a one-line check beats a stale assertion.

## Transports

Plain `git` is the baseline and the only requirement. If your environment
already has a file-distribution mechanism (a config-sync agent, rsync,
object storage), point it at the corpus repo and deliver to the same
`KNOWLEDGE_HOME` path — the adapters only care about the path, not the
transport.

## Design questions to answer for your environment

1. What is in working memory right now — what did the agent actually see?
2. Which past session matters, and can the agent find it?
3. Which facts are current, and what resolves a contradiction?
4. Which workflow applies, and where is it encoded?
5. What should be forgotten, and who owns the procedures?

If you can answer these, you have an architecture. If not, you have
scattered state — and scattered state works until the agent remembers the
wrong thing and continues with confidence.

## License

MIT — see [LICENSE](LICENSE).
