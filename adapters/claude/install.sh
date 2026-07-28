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

mkdir -p "$KNOWLEDGE_HOME"
cat > "$HOOK" <<'EOF'
#!/bin/sh
# SessionStart hook: emit the kernel; stdout becomes session context.
KERNEL="${KNOWLEDGE_HOME:-$HOME/.config/agent-knowledge}/corpus/kernel/kernel.md"
if [ -f "$KERNEL" ]; then
    cat "$KERNEL"
else
    echo "WARNING: agent-knowledge kernel missing at $KERNEL — run sync.sh; operating without the environment contract."
fi
EOF
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
