#!/bin/sh
# Codex CLI Tier A adapter: maintain a managed block in ~/.codex/AGENTS.md.
# Codex loads that file globally — every session on this host, any repo.
#
# Idempotent: replaces the block between the markers on every run; creates
# the file if absent; leaves everything outside the markers untouched.
# Run it after each corpus sync (append to your sync cron).
set -eu

KNOWLEDGE_HOME="${KNOWLEDGE_HOME:-$HOME/.config/agent-knowledge}"
SCRIPT_DIR=$(CDPATH= cd "$(dirname "$0")" && pwd -P)
# shellcheck source=../lib/kernel-path.sh
. "$SCRIPT_DIR/../lib/kernel-path.sh"
TARGET="${CODEX_HOME:-$HOME/.codex}/AGENTS.md"
BEGIN='<!-- agent-knowledge-kit:begin (managed block, do not edit by hand) -->'
END='<!-- agent-knowledge-kit:end -->'

if KERNEL=$(akk_resolve_kernel "$KNOWLEDGE_HOME"); then
    :
else
    resolve_rc=$?
    if [ "$resolve_rc" -eq 2 ]; then
        echo "kernel missing under $KNOWLEDGE_HOME/corpus — run sync.sh first" >&2
    fi
    exit 1
fi

if grep -F -x -q "$BEGIN" "$KERNEL" || grep -F -x -q "$END" "$KERNEL"; then
    echo "refusing kernel: content contains an agent-knowledge-kit managed marker" >&2
    exit 1
fi

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
    # Keep the delimiter structural even when the kernel has no final newline.
    echo
    echo "$END"
} > "$TARGET"
rm -f "$tmp"
echo "updated $TARGET from $KERNEL"
