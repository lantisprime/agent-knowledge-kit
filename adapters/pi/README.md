# pi Tier A adapter

pi has a checked launcher plus native context-file discovery:

## 1. Checked deterministic injection (recommended)

Launch pi through the kit adapter:

```sh
adapters/pi/run.sh ...
```

The launcher honors `KNOWLEDGE_HOME`, resolves the supported nested or flat
layout, refuses symbolic links and paths that escape the active corpus, and
then runs `pi --append-system-prompt <validated-kernel> ...`. Do not point pi
directly at an unvalidated path; that bypasses source containment.

`--append-system-prompt` accepts a file path, is repeatable, and works with
`--no-extensions`. If you orchestrate seats from a playbook that pins the
argv, invoke the checked launcher there — the argv is itself procedural
memory; version the change.

## 2. Repo-level auto-discovery

pi discovers and loads `AGENTS.md` and `CLAUDE.md` from the working
directory by default (disable flag exists: `--no-context-files` — don't use
it). So any repo carrying a pointer line to the kernel, or a codex-style
managed block, covers pi automatically.

## Tier B

Ship procedure docs as pi prompt templates (`--prompt-template <path|dir>`)
from the subscriber-materialized corpus, and the operator (or orchestrator)
loads them when the task matches.
