#!/bin/sh
# sync.sh — git-baseline corpus sync. POSIX sh; git is the only dependency.
#
#   sync.sh init <remote-url>[,<mirror-url>...]   first-time clone
#   sync.sh pull                                  converge to the remote (cron this)
#   sync.sh status                                last-sync info, one line
#
# KNOWLEDGE_HOME   target dir  (default: ~/.config/agent-knowledge)
# KNOWLEDGE_REMOTES overrides the remote list for init/pull (comma-separated,
#                  tried in order — canonical first, mirrors after; useful when
#                  the canonical remote is reachable only inside one network).
set -eu

KNOWLEDGE_HOME="${KNOWLEDGE_HOME:-$HOME/.config/agent-knowledge}"
CORPUS_DIR="$KNOWLEDGE_HOME/corpus"
STATE_FILE="$KNOWLEDGE_HOME/.sync-state"

log() { printf '%s %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*"; }

remotes() {
    if [ -n "${KNOWLEDGE_REMOTES:-}" ]; then
        printf '%s' "$KNOWLEDGE_REMOTES" | tr ',' '\n'
    elif [ -f "$KNOWLEDGE_HOME/.remotes" ]; then
        cat "$KNOWLEDGE_HOME/.remotes"
    else
        echo "no remotes configured: run 'sync.sh init <url>[,<url>...]'" >&2
        exit 1
    fi
}

cmd="${1:-pull}"

case "$cmd" in
init)
    [ $# -ge 2 ] || { echo "usage: sync.sh init <url>[,<url>...]" >&2; exit 1; }
    mkdir -p "$KNOWLEDGE_HOME"
    printf '%s' "$2" | tr ',' '\n' > "$KNOWLEDGE_HOME/.remotes"
    for url in $(remotes); do
        log "trying clone: $url"
        if git clone --depth 1 "$url" "$CORPUS_DIR" 2>/dev/null; then
            log "cloned from $url"
            printf 'ok clone %s %s\n' "$url" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" > "$STATE_FILE"
            exit 0
        fi
    done
    echo "all remotes failed" >&2; exit 1
    ;;
pull)
    [ -d "$CORPUS_DIR/.git" ] || { echo "not initialized: run 'sync.sh init'" >&2; exit 1; }
    for url in $(remotes); do
        if git -C "$CORPUS_DIR" pull --ff-only "$url" HEAD >/dev/null 2>&1; then
            log "synced from $url @ $(git -C "$CORPUS_DIR" rev-parse --short HEAD)"
            printf 'ok pull %s %s %s\n' "$url" "$(git -C "$CORPUS_DIR" rev-parse --short HEAD)" \
                "$(date -u +%Y-%m-%dT%H:%M:%SZ)" > "$STATE_FILE"
            exit 0
        fi
    done
    # Fail soft: an unreachable remote must not break agents — the last-synced
    # copy stays in place, which is the whole point of local delivery.
    log "WARN: no remote reachable; keeping last-synced corpus"
    printf 'stale %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" >> "$STATE_FILE"
    exit 0
    ;;
status)
    [ -f "$STATE_FILE" ] && tail -1 "$STATE_FILE" || echo "never synced"
    ;;
*)
    echo "usage: sync.sh {init <url>[,url...] | pull | status}" >&2; exit 1
    ;;
esac
