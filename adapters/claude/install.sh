#!/bin/sh
# Claude Code Tier A adapter: register a SessionStart hook that emits the
# kernel into context at the start of every session on this host.
#
# Idempotent: safe to re-run. Writes the hook script into KNOWLEDGE_HOME and
# prints the settings.json fragment (merging JSON reliably from POSIX sh is
# not worth the risk of corrupting user settings — paste or merge the
# fragment yourself, or let your agent do it with a JSON-aware tool).
set -eu

KNOWLEDGE_HOME="${KNOWLEDGE_HOME:-$HOME/.config/agent-knowledge}"
HOOK="$KNOWLEDGE_HOME/claude-kernel-hook.sh"
SCRIPT_DIR=$(CDPATH= cd "$(dirname "$0")" && pwd -P)
KERNEL_LIB="$SCRIPT_DIR/../lib/kernel-path.sh"

[ -f "$KERNEL_LIB" ] || { echo "kernel validator missing at $KERNEL_LIB" >&2; exit 1; }

mkdir -p "$KNOWLEDGE_HOME"
{
cat <<'EOF'
#!/bin/sh
# SessionStart hook: emit the kernel; stdout becomes session context.
EOF
cat "$KERNEL_LIB"
cat <<'EOF'
KH="${KNOWLEDGE_HOME:-$HOME/.config/agent-knowledge}"
if KERNEL=$(akk_resolve_kernel "$KH"); then
    cat "$KERNEL"
    exit 0
else
    resolve_rc=$?
    if [ "$resolve_rc" -eq 2 ]; then
        echo "WARNING: agent-knowledge kernel missing under $KH/corpus — run sync.sh; operating without the environment contract."
    fi
    exit 0
fi
EOF
} > "$HOOK"
chmod +x "$HOOK"

cat <<EOF
Hook installed: $HOOK

Add to ~/.claude/settings.json (hooks.SessionStart):

  {
    "hooks": {
      "SessionStart": [
        { "hooks": [ { "type": "command", "command": "$HOOK" } ] }
      ]
    }
  }
EOF
