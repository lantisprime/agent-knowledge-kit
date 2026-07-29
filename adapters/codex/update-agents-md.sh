#!/bin/sh
# Codex CLI Tier A adapter: maintain a managed block in ~/.codex/AGENTS.md.
# Codex loads that file globally — every session on this host, any repo.
#
# Idempotent: replaces the block between the markers on every run; creates
# the file if absent; leaves everything outside the markers untouched.
# Run it after each corpus sync (append to your sync cron).
set -eu

KNOWLEDGE_HOME="${KNOWLEDGE_HOME:-$HOME/.config/agent-knowledge}"
# nested (corpus repo keeps content under corpus/) or flat layout — try both
KERNEL="$KNOWLEDGE_HOME/corpus/corpus/kernel/kernel.md"
[ -f "$KERNEL" ] || KERNEL="$KNOWLEDGE_HOME/corpus/kernel/kernel.md"
TARGET="${CODEX_HOME:-$HOME/.codex}/AGENTS.md"
BEGIN='<!-- agent-knowledge-kit:begin (managed block, do not edit by hand) -->'
END='<!-- agent-knowledge-kit:end -->'

[ -f "$KERNEL" ] || { echo "kernel missing at $KERNEL — run sync.sh first" >&2; exit 1; }
mkdir -p "$(dirname "$TARGET")"
touch "$TARGET"

tmp="$(mktemp)"
# keep everything outside the previous managed block
awk -v b="$BEGIN" -v e="$END" '
    $0 == b {inblock=1; next}
    $0 == e {inblock=0; next}
    !inblock {print}
' "$TARGET" > "$tmp"
{
    cat "$tmp"
    echo "$BEGIN"
    cat "$KERNEL"
    echo "$END"
} > "$TARGET"
rm -f "$tmp"
echo "updated $TARGET from $KERNEL"
