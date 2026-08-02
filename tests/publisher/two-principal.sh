#!/bin/sh
# Privileged platform probe for the local publisher boundary.
#
# This suite never creates accounts. Run it only on a disposable macOS/Linux
# host with two pre-provisioned, distinct, non-root users. Exit 77 means the
# prerequisites were absent; it is not evidence that C1-b passed.
set -eu

TEST_DIR=$(CDPATH= cd "$(dirname "$0")" && pwd -P)
REPO_ROOT=$(CDPATH= cd "$TEST_DIR/../.." && pwd -P)
SOURCE_PUBLISHER="$REPO_ROOT/publisher/publish.sh"
PLATFORM=$(/usr/bin/uname -s)
# Principal runners inherit the caller's working directory on macOS. Use a
# universally traversable directory so a protected caller home cannot make
# otherwise-valid fixture commands fail before their access checks.
cd /

tests=0
failures=0
TEST_ROOT=

pass() {
    tests=$((tests + 1))
    printf 'ok %d - %s\n' "$tests" "$1"
}

skip_test() {
    tests=$((tests + 1))
    printf 'ok %d - %s # SKIP\n' "$tests" "$1"
}

fail() {
    tests=$((tests + 1))
    failures=$((failures + 1))
    printf 'not ok %d - %s\n' "$tests" "$1"
}

skip_suite() {
    printf '1..0 # SKIP %s\n' "$1"
    exit 77
}

cleanup() {
    [ -n "$TEST_ROOT" ] || return 0
    case "$TEST_ROOT" in
    /private/var/db/akk-publisher-platform.*|/var/lib/akk-publisher-platform.*) ;;
    *) return 0 ;;
    esac
    if [ "$PLATFORM" = Darwin ] && [ -e "$TEST_ROOT" ]; then
        /bin/chmod -RN "$TEST_ROOT" 2>/dev/null || :
    fi
    /bin/chmod -R u+w "$TEST_ROOT" 2>/dev/null || :
    /bin/rm -rf "$TEST_ROOT"
}

trap cleanup 0
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

[ "$(/usr/bin/id -u)" -eq 0 ] || skip_suite 'requires root'
[ -n "${AKK_TEST_PUBLISHER:-}" ] || skip_suite 'AKK_TEST_PUBLISHER is not configured'
[ -n "${AKK_TEST_AGENT:-}" ] || skip_suite 'AKK_TEST_AGENT is not configured'
[ -n "${AKK_TEST_SHARED_GROUP:-}" ] || skip_suite 'AKK_TEST_SHARED_GROUP is not configured'

PUBLISHER_USER=$AKK_TEST_PUBLISHER
AGENT_USER=$AKK_TEST_AGENT
SHARED_GROUP=$AKK_TEST_SHARED_GROUP
PUBLISHER_UID=$(/usr/bin/id -u "$PUBLISHER_USER" 2>/dev/null) || skip_suite 'publisher user is missing'
AGENT_UID=$(/usr/bin/id -u "$AGENT_USER" 2>/dev/null) || skip_suite 'agent user is missing'
PUBLISHER_GID=$(/usr/bin/id -g "$PUBLISHER_USER" 2>/dev/null) || skip_suite 'publisher group is missing'
[ "$PUBLISHER_UID" -ne 0 ] || skip_suite 'publisher must be non-root'
[ "$AGENT_UID" -ne 0 ] || skip_suite 'agent must be non-root'
[ "$PUBLISHER_UID" -ne "$AGENT_UID" ] || skip_suite 'publisher and agent must have different uids'

agent_group_names=$(/usr/bin/id -Gn "$AGENT_USER") || skip_suite 'cannot read agent groups'
case " $agent_group_names " in
*" $SHARED_GROUP "*) ;;
*) skip_suite 'agent is not a member of the configured shared group' ;;
esac
agent_groups=$(/usr/bin/id -G "$AGENT_USER") || skip_suite 'cannot read numeric agent groups'
case " $agent_groups " in
*" $PUBLISHER_GID "*) skip_suite 'agent belongs to the publisher primary group' ;;
esac

case "$PLATFORM" in
Darwin)
    USER_RUNNER=/usr/bin/sudo
    CHOWN=/usr/sbin/chown
    TEST_COMMAND=/bin/test
    [ -x "$USER_RUNNER" ] || skip_suite 'sudo is unavailable'
    TEST_PARENT=/private/var/db
    ;;
Linux)
    CHOWN=/usr/bin/chown
    TEST_COMMAND=/usr/bin/test
    if [ -x /usr/sbin/runuser ]; then
        USER_RUNNER=/usr/sbin/runuser
    elif [ -x /usr/bin/sudo ]; then
        USER_RUNNER=/usr/bin/sudo
    else
        skip_suite 'runuser/sudo is unavailable'
    fi
    [ -x /usr/bin/setfacl ] && [ -x /usr/bin/getfacl ] ||
        skip_suite 'Linux ACL tools are unavailable'
    TEST_PARENT=/var/lib
    ;;
*)
    skip_suite "unsupported platform: $PLATFORM"
    ;;
esac

run_as() {
    run_user=$1
    shift
    case "$USER_RUNNER" in
    /usr/sbin/runuser) "$USER_RUNNER" -u "$run_user" -- "$@" ;;
    *) "$USER_RUNNER" -n -u "$run_user" -- "$@" ;;
    esac
}

run_as "$PUBLISHER_USER" /usr/bin/id -u >/dev/null 2>&1 ||
    skip_suite 'cannot execute as publisher non-interactively'
run_as "$AGENT_USER" /usr/bin/id -u >/dev/null 2>&1 ||
    skip_suite 'cannot execute as agent non-interactively'
[ "$(run_as "$PUBLISHER_USER" /usr/bin/id -u)" = "$PUBLISHER_UID" ] ||
    skip_suite 'publisher runner resolved the wrong uid'
[ "$(run_as "$AGENT_USER" /usr/bin/id -u)" = "$AGENT_UID" ] ||
    skip_suite 'agent runner resolved the wrong uid'

TEST_ROOT=$(/usr/bin/mktemp -d "$TEST_PARENT/akk-publisher-platform.XXXXXX") || {
    printf '%s\n' 'platform setup failed: cannot allocate protected test root' >&2
    exit 1
}
/bin/chmod 0755 "$TEST_ROOT"
/bin/mkdir "$TEST_ROOT/bin"
/bin/cp "$SOURCE_PUBLISHER" "$TEST_ROOT/bin/publish.sh"
"$CHOWN" 0:0 "$TEST_ROOT/bin" "$TEST_ROOT/bin/publish.sh"
/bin/chmod 0555 "$TEST_ROOT/bin" "$TEST_ROOT/bin/publish.sh"
PROTECTED_PUBLISHER="$TEST_ROOT/bin/publish.sh"

CONTROL_ROOT="$TEST_ROOT/control"
PUBLICATION_ROOT="$TEST_ROOT/publication"
/bin/mkdir "$CONTROL_ROOT" "$PUBLICATION_ROOT"
"$CHOWN" "$PUBLISHER_UID:$PUBLISHER_GID" "$CONTROL_ROOT" "$PUBLICATION_ROOT"
/bin/chmod 0700 "$CONTROL_ROOT"
/bin/chmod 0755 "$PUBLICATION_ROOT"
run_as "$PUBLISHER_USER" "$PROTECTED_PUBLISHER" prepare \
    "$CONTROL_ROOT" "$PUBLICATION_ROOT" >/dev/null

CANDIDATE="$CONTROL_ROOT/quarantine/fixture"
/bin/mkdir -p "$CANDIDATE/corpus/kernel"
/usr/bin/printf '%s\nsequence 1\ndigest aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n' \
    'agent-knowledge-kit-authenticated-test-fixture-v1' > "$CANDIDATE/authenticated.fixture"
/usr/bin/printf 'platform fixture\n' > "$CANDIDATE/corpus/kernel/kernel.md"
"$CHOWN" -R "$PUBLISHER_UID:$PUBLISHER_GID" "$CANDIDATE"
/bin/chmod -R go-w "$CANDIDATE"
run_as "$PUBLISHER_USER" "$PROTECTED_PUBLISHER" promote-fixture \
    "$CONTROL_ROOT" "$PUBLICATION_ROOT" "$CANDIDATE" >/dev/null

RELEASE_ID=r1-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
RELEASE_ROOT="$PUBLICATION_ROOT/releases/$RELEASE_ID"
KERNEL="$RELEASE_ROOT/corpus/kernel/kernel.md"

if run_as "$AGENT_USER" "$TEST_COMMAND" -r "$KERNEL" &&
    [ "$(run_as "$AGENT_USER" /bin/cat "$KERNEL")" = 'platform fixture' ]; then
    pass 'agent can read the selected immutable kernel'
else
    fail 'agent can read the selected immutable kernel'
fi

mutation_failures=0
run_as "$AGENT_USER" /bin/sh -c 'printf tamper >> "$1"' sh "$KERNEL" 2>/dev/null ||
    mutation_failures=$((mutation_failures + 1))
run_as "$AGENT_USER" /bin/mv "$KERNEL" "$KERNEL.moved" 2>/dev/null ||
    mutation_failures=$((mutation_failures + 1))
run_as "$AGENT_USER" /bin/rm "$PUBLICATION_ROOT/current" 2>/dev/null ||
    mutation_failures=$((mutation_failures + 1))
run_as "$AGENT_USER" /bin/ln -snf "releases/$RELEASE_ID" "$PUBLICATION_ROOT/current" 2>/dev/null ||
    mutation_failures=$((mutation_failures + 1))
run_as "$AGENT_USER" /usr/bin/touch "$CONTROL_ROOT/state/agent-write" 2>/dev/null ||
    mutation_failures=$((mutation_failures + 1))
if [ "$mutation_failures" -eq 5 ] && [ -r "$KERNEL" ] && [ -L "$PUBLICATION_ROOT/current" ]; then
    pass 'agent writes, renames, unlinks, and selector replacements fail'
else
    fail 'agent writes, renames, unlinks, and selector replacements fail'
fi

/usr/bin/printf 'publisher secret\n' > "$CONTROL_ROOT/trust/probe-secret"
"$CHOWN" "$PUBLISHER_UID:$PUBLISHER_GID" "$CONTROL_ROOT/trust/probe-secret"
/bin/chmod 0600 "$CONTROL_ROOT/trust/probe-secret"
if ! run_as "$AGENT_USER" /bin/cat "$CONTROL_ROOT/trust/probe-secret" >/dev/null 2>&1 &&
    ! run_as "$AGENT_USER" /usr/bin/touch "$CONTROL_ROOT/checkout/agent-write" >/dev/null 2>&1 &&
    ! run_as "$AGENT_USER" /usr/bin/touch "$CONTROL_ROOT/hooks/agent-write" >/dev/null 2>&1; then
    pass 'agent cannot read secrets or mutate control-root classes'
else
    fail 'agent cannot read secrets or mutate control-root classes'
fi

/usr/bin/chgrp "$SHARED_GROUP" "$PUBLICATION_ROOT"
/bin/chmod 0775 "$PUBLICATION_ROOT"
if ! run_as "$PUBLISHER_USER" "$PROTECTED_PUBLISHER" check \
        "$CONTROL_ROOT" "$PUBLICATION_ROOT" >/dev/null 2>&1 &&
    run_as "$AGENT_USER" /usr/bin/touch "$PUBLICATION_ROOT/group-write-probe"; then
    pass 'unsafe shared-group write is both effective and rejected by integrity status'
else
    fail 'unsafe shared-group write is both effective and rejected by integrity status'
fi
/bin/rm -f "$PUBLICATION_ROOT/group-write-probe"
"$CHOWN" "$PUBLISHER_UID:$PUBLISHER_GID" "$PUBLICATION_ROOT"
/bin/chmod 0755 "$PUBLICATION_ROOT"

case "$PLATFORM" in
Darwin)
    /bin/chmod +a "$AGENT_USER allow add_file,delete_child" "$PUBLICATION_ROOT"
    ;;
Linux)
    /usr/bin/setfacl -m "u:$AGENT_USER:rwx" "$PUBLICATION_ROOT"
    ;;
esac
if ! run_as "$PUBLISHER_USER" "$PROTECTED_PUBLISHER" check \
        "$CONTROL_ROOT" "$PUBLICATION_ROOT" >/dev/null 2>&1 &&
    run_as "$AGENT_USER" /usr/bin/touch "$PUBLICATION_ROOT/acl-write-probe"; then
    pass 'unsafe native ACL write is both effective and rejected by integrity status'
else
    fail 'unsafe native ACL write is both effective and rejected by integrity status'
fi
case "$PLATFORM" in
Darwin) /bin/chmod -N "$PUBLICATION_ROOT" ;;
Linux) /usr/bin/setfacl -b "$PUBLICATION_ROOT" ;;
esac
/bin/rm -f "$PUBLICATION_ROOT/acl-write-probe"

if ! run_as "$AGENT_USER" "$TEST_COMMAND" -w "$PROTECTED_PUBLISHER" &&
    ! run_as "$AGENT_USER" "$TEST_COMMAND" -w "$TEST_ROOT/bin"; then
    pass 'agent cannot alter the protected publisher executable or directory'
else
    fail 'agent cannot alter the protected publisher executable or directory'
fi

if [ -x /usr/bin/sudo ]; then
    if ! run_as "$AGENT_USER" /usr/bin/sudo -n -u "$PUBLISHER_USER" \
            /usr/bin/true >/dev/null 2>&1; then
        pass 'agent cannot assume publisher through noninteractive sudo'
    else
        fail 'agent cannot assume publisher through noninteractive sudo'
    fi
else
    skip_test 'noninteractive sudo probe unavailable; broader identity proof remains pending'
fi

printf '1..%d\n' "$tests"
if [ "$failures" -ne 0 ]; then
    printf '%d platform test(s) failed\n' "$failures" >&2
    exit 1
fi
