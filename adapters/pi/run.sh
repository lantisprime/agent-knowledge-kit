#!/bin/sh
# pi Tier A adapter: validate the kernel source before launching pi.
set -eu

KNOWLEDGE_HOME="${KNOWLEDGE_HOME:-$HOME/.config/agent-knowledge}"
SCRIPT_DIR=$(CDPATH='' cd "$(dirname "$0")" && pwd -P)
# shellcheck source-path=SCRIPTDIR
# shellcheck source=../lib/kernel-path.sh
. "$SCRIPT_DIR/../lib/kernel-path.sh"

if KERNEL=$(akk_resolve_kernel "$KNOWLEDGE_HOME"); then
    :
else
    resolve_rc=$?
    if [ "$resolve_rc" -eq 2 ]; then
        echo "kernel missing under $KNOWLEDGE_HOME/corpus — run the knowledge-server subscriber first" >&2
    fi
    exit 1
fi

exec pi --append-system-prompt "$KERNEL" "$@"
