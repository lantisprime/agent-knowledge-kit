# pi Tier A adapter

pi needs no install script — it has two native paths:

## 1. Deterministic per-session injection (recommended for driven seats)

Add to every launch argv:

```sh
pi --append-system-prompt "$HOME/.config/agent-knowledge/corpus/kernel/kernel.md" ...
```

`--append-system-prompt` accepts a file path, is repeatable, and works with
`--no-extensions`. If you orchestrate seats from a playbook that pins the
argv, add the flag there — the argv is itself procedural memory; version the
change.

## 2. Repo-level auto-discovery

pi discovers and loads `AGENTS.md` and `CLAUDE.md` from the working
directory by default (disable flag exists: `--no-context-files` — don't use
it). So any repo carrying a pointer line to the kernel, or a codex-style
managed block, covers pi automatically.

## Tier B

Ship procedure docs as pi prompt templates (`--prompt-template <path|dir>`)
from the synced corpus, and the operator (or orchestrator) loads them when
the task matches.
