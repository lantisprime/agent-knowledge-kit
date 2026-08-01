#!/bin/sh
# Portable regression suite for adapter source containment and managed markers.
set -u

TEST_DIR=$(CDPATH= cd "$(dirname "$0")" && pwd -P)
REPO_ROOT=$(CDPATH= cd "$TEST_DIR/.." && pwd -P)
CLAUDE_INSTALL="$REPO_ROOT/adapters/claude/install.sh"
CODEX_UPDATE="$REPO_ROOT/adapters/codex/update-agents-md.sh"
PI_RUN="$REPO_ROOT/adapters/pi/run.sh"

TEST_TMP=$(mktemp -d "${TMPDIR:-/tmp}/agent-knowledge-kit-tests.XXXXXX") || exit 1
trap 'rm -rf "$TEST_TMP"' EXIT HUP INT TERM

tests=0
failures=0

pass() {
    tests=$((tests + 1))
    printf 'ok %d - %s\n' "$tests" "$1"
}

fail() {
    tests=$((tests + 1))
    failures=$((failures + 1))
    printf 'not ok %d - %s\n' "$tests" "$1"
}

new_case() {
    case_dir="$TEST_TMP/$1"
    mkdir -p "$case_dir"
    printf '%s\n' "$case_dir"
}

write_pi_stub() {
    stub_dir=$1
    mkdir -p "$stub_dir"
    printf '%s\n' \
        '#!/bin/sh' \
        ': > "${PI_CALLED:?}"' \
        'printf '\''%s\n'\'' "$@" > "${PI_ARGS:?}"' \
        > "$stub_dir/pi"
    chmod +x "$stub_dir/pi"
}

make_git_corpus() {
    corpus_root=$1
    git -C "$corpus_root" init -q
    git -C "$corpus_root" add .
    git -C "$corpus_root" -c user.name=test -c user.email=test@example.invalid \
        commit -qm fixture
}

exercise_all_adapters_reject() {
    name=$1
    knowledge_home=$2
    case_root=$3
    target="$case_root/codex/AGENTS.md"
    target_before="$case_root/AGENTS.before"
    claude_out="$case_root/claude.out"
    claude_err="$case_root/claude.err"
    codex_out="$case_root/codex.out"
    codex_err="$case_root/codex.err"
    pi_out="$case_root/pi.out"
    pi_err="$case_root/pi.err"
    pi_called="$case_root/pi.called"
    pi_args="$case_root/pi.args"
    stub_dir="$case_root/bin"

    mkdir -p "$(dirname "$target")"
    printf 'operator prefix\noperator suffix\n' > "$target"
    cp "$target" "$target_before"

    KNOWLEDGE_HOME="$knowledge_home" "$CLAUDE_INSTALL" > /dev/null
    if KNOWLEDGE_HOME="$knowledge_home" "$knowledge_home/claude-kernel-hook.sh" \
        > "$claude_out" 2> "$claude_err" &&
        [ ! -s "$claude_out" ] &&
        grep -q 'refusing kernel' "$claude_err"; then
        pass "$name: Claude refuses without emitting kernel bytes"
    else
        fail "$name: Claude refuses without emitting kernel bytes"
    fi

    if KNOWLEDGE_HOME="$knowledge_home" CODEX_HOME="$case_root/codex" \
        "$CODEX_UPDATE" > "$codex_out" 2> "$codex_err"; then
        codex_rc=0
    else
        codex_rc=$?
    fi
    if [ "$codex_rc" -ne 0 ] && cmp -s "$target_before" "$target" &&
        grep -q 'refusing kernel' "$codex_err"; then
        pass "$name: Codex rejects and leaves AGENTS.md byte-identical"
    else
        fail "$name: Codex rejects and leaves AGENTS.md byte-identical"
    fi

    write_pi_stub "$stub_dir"
    if [ -x "$PI_RUN" ]; then
        if PATH="$stub_dir:$PATH" PI_CALLED="$pi_called" PI_ARGS="$pi_args" \
            KNOWLEDGE_HOME="$knowledge_home" "$PI_RUN" --version \
            > "$pi_out" 2> "$pi_err"; then
            pi_rc=0
        else
            pi_rc=$?
        fi
    else
        pi_rc=127
        printf 'checked pi launcher missing\n' > "$pi_err"
    fi
    if [ "$pi_rc" -ne 0 ] && [ ! -e "$pi_called" ] &&
        grep -q 'refusing kernel' "$pi_err"; then
        pass "$name: pi refuses before launching the harness"
    else
        fail "$name: pi refuses before launching the harness"
    fi
}

test_final_symlink() {
    case_root=$(new_case final-symlink)
    knowledge_home="$case_root/knowledge"
    corpus_root="$knowledge_home/corpus"
    outside="$case_root/outside-secret"
    mkdir -p "$corpus_root/kernel"
    printf 'DO NOT EXPOSE THIS FIXTURE\n' > "$outside"
    ln -s "$outside" "$corpus_root/kernel/kernel.md"
    make_git_corpus "$corpus_root"
    exercise_all_adapters_reject 'final symlink' "$knowledge_home" "$case_root"
}

test_parent_escape() {
    case_root=$(new_case parent-escape)
    knowledge_home="$case_root/knowledge"
    corpus_root="$knowledge_home/corpus"
    outside_dir="$case_root/outside"
    mkdir -p "$corpus_root" "$outside_dir"
    printf 'DO NOT EXPOSE THIS PARENT FIXTURE\n' > "$outside_dir/kernel.md"
    ln -s "$outside_dir" "$corpus_root/kernel"
    make_git_corpus "$corpus_root"
    exercise_all_adapters_reject 'parent path escape' "$knowledge_home" "$case_root"
}

test_internal_symlink() {
    case_root=$(new_case internal-symlink)
    knowledge_home="$case_root/knowledge"
    corpus_root="$knowledge_home/corpus"
    mkdir -p "$corpus_root/kernel" "$corpus_root/docs"
    printf 'IN-CORPUS SYMLINK TARGET\n' > "$corpus_root/docs/actual.md"
    ln -s ../docs/actual.md "$corpus_root/kernel/kernel.md"
    make_git_corpus "$corpus_root"
    exercise_all_adapters_reject 'in-corpus symlink' "$knowledge_home" "$case_root"
}

test_marker_rejected() {
    marker_name=$1
    marker=$2
    case_root=$(new_case "marker-$marker_name")
    knowledge_home="$case_root/knowledge"
    corpus_root="$knowledge_home/corpus"
    codex_home="$case_root/codex"
    target="$codex_home/AGENTS.md"
    before="$case_root/AGENTS.before"
    mkdir -p "$corpus_root/kernel" "$codex_home"
    printf 'safe first line\n%s\nunsafe tail\n' "$marker" > "$corpus_root/kernel/kernel.md"
    make_git_corpus "$corpus_root"
    printf 'operator-owned content\n' > "$target"
    cp "$target" "$before"

    if KNOWLEDGE_HOME="$knowledge_home" CODEX_HOME="$codex_home" \
        "$CODEX_UPDATE" > "$case_root/out" 2> "$case_root/err"; then
        rc=0
    else
        rc=$?
    fi
    if [ "$rc" -ne 0 ] && cmp -s "$before" "$target" &&
        grep -q 'managed marker' "$case_root/err"; then
        pass "$marker_name marker is rejected before touching AGENTS.md"
    else
        fail "$marker_name marker is rejected before touching AGENTS.md"
    fi
}

test_marker_absent_target() {
    case_root=$(new_case marker-absent-target)
    knowledge_home="$case_root/knowledge"
    corpus_root="$knowledge_home/corpus"
    codex_home="$case_root/codex"
    target="$codex_home/AGENTS.md"
    end='<!-- agent-knowledge-kit:end -->'
    mkdir -p "$corpus_root/kernel"
    printf '%s\n' "$end" > "$corpus_root/kernel/kernel.md"
    make_git_corpus "$corpus_root"

    if KNOWLEDGE_HOME="$knowledge_home" CODEX_HOME="$codex_home" \
        "$CODEX_UPDATE" > "$case_root/out" 2> "$case_root/err"; then
        rc=0
    else
        rc=$?
    fi
    if [ "$rc" -ne 0 ] && [ ! -e "$target" ]; then
        pass 'marker rejection does not create an absent AGENTS.md'
    else
        fail 'marker rejection does not create an absent AGENTS.md'
    fi
}

test_inline_marker_idempotency() {
    case_root=$(new_case inline-marker)
    knowledge_home="$case_root/knowledge"
    corpus_root="$knowledge_home/corpus"
    codex_home="$case_root/codex"
    target="$codex_home/AGENTS.md"
    first="$case_root/AGENTS.first"
    mkdir -p "$corpus_root/kernel" "$codex_home"
    printf '%s\n' \
        'safe <!-- agent-knowledge-kit:begin (managed block, do not edit by hand) --> inline' \
        'safe <!-- agent-knowledge-kit:end --> inline' \
        > "$corpus_root/kernel/kernel.md"
    make_git_corpus "$corpus_root"
    printf 'operator-owned content\n' > "$target"

    KNOWLEDGE_HOME="$knowledge_home" CODEX_HOME="$codex_home" \
        "$CODEX_UPDATE" > "$case_root/first.out" 2> "$case_root/first.err"
    first_rc=$?
    cp "$target" "$first"
    KNOWLEDGE_HOME="$knowledge_home" CODEX_HOME="$codex_home" \
        "$CODEX_UPDATE" > "$case_root/second.out" 2> "$case_root/second.err"
    second_rc=$?

    begin_count=$(grep -F -x -c "$begin" "$target")
    end_count=$(grep -F -x -c "$end" "$target")
    if [ "$first_rc" -eq 0 ] && [ "$second_rc" -eq 0 ] &&
        cmp -s "$first" "$target" && [ "$begin_count" -eq 1 ] &&
        [ "$end_count" -eq 1 ]; then
        pass 'inline marker text remains valid and Codex updates idempotently'
    else
        fail 'inline marker text remains valid and Codex updates idempotently'
    fi
}

test_kernel_without_final_newline() {
    case_root=$(new_case no-final-newline)
    knowledge_home="$case_root/knowledge"
    corpus_root="$knowledge_home/corpus"
    codex_home="$case_root/codex"
    target="$codex_home/AGENTS.md"
    mkdir -p "$corpus_root/kernel" "$codex_home"
    printf '%s' 'kernel without final newline' > "$corpus_root/kernel/kernel.md"
    make_git_corpus "$corpus_root"
    printf 'operator prefix\n' > "$target"

    KNOWLEDGE_HOME="$knowledge_home" CODEX_HOME="$codex_home" \
        "$CODEX_UPDATE" > "$case_root/first.out" 2> "$case_root/first.err"
    printf 'operator post-block\n' >> "$target"
    KNOWLEDGE_HOME="$knowledge_home" CODEX_HOME="$codex_home" \
        "$CODEX_UPDATE" > "$case_root/second.out" 2> "$case_root/second.err"

    end_count=$(grep -F -x -c "$end" "$target")
    post_count=$(grep -F -x -c 'operator post-block' "$target")
    if [ "$end_count" -eq 1 ] && [ "$post_count" -eq 1 ]; then
        pass 'kernel without final newline preserves a whole-line end marker'
    else
        fail 'kernel without final newline preserves a whole-line end marker'
    fi
}

test_valid_layout() {
    layout=$1
    tracking=${2:-git}
    case_root=$(new_case "valid-$layout-$tracking")
    knowledge_home="$case_root/knowledge"
    corpus_root="$knowledge_home/corpus"
    codex_home="$case_root/codex"
    target="$codex_home/AGENTS.md"
    stub_dir="$case_root/bin"
    pi_called="$case_root/pi.called"
    pi_args="$case_root/pi.args"
    expected='VALID KERNEL FIXTURE'

    if [ "$layout" = nested ]; then
        kernel="$corpus_root/corpus/kernel/kernel.md"
    else
        kernel="$corpus_root/kernel/kernel.md"
    fi
    mkdir -p "$(dirname "$kernel")"
    printf '%s\n' "$expected" > "$kernel"
    if [ "$layout" = nested ]; then
        mkdir -p "$corpus_root/kernel"
        printf 'LOWER PRECEDENCE KERNEL\n' > "$corpus_root/kernel/kernel.md"
    fi
    kernel_expected=$(CDPATH= cd "$(dirname "$kernel")" && pwd -P)/${kernel##*/}
    if [ "$tracking" = git ]; then
        make_git_corpus "$corpus_root"
    elif [ "$tracking" = parent-git ]; then
        printf 'parent repository fixture\n' > "$case_root/parent.txt"
        git -C "$case_root" init -q
        git -C "$case_root" add parent.txt
        git -C "$case_root" -c user.name=test -c user.email=test@example.invalid \
            commit -qm fixture
    fi

    KNOWLEDGE_HOME="$knowledge_home" "$CLAUDE_INSTALL" > /dev/null
    KNOWLEDGE_HOME="$knowledge_home" "$knowledge_home/claude-kernel-hook.sh" \
        > "$case_root/claude.out" 2> "$case_root/claude.err"
    claude_rc=$?

    mkdir -p "$codex_home"
    printf 'operator prefix\n' > "$target"
    if KNOWLEDGE_HOME="$knowledge_home" CODEX_HOME="$codex_home" \
        "$CODEX_UPDATE" > "$case_root/codex.out" 2> "$case_root/codex.err"; then
        codex_rc=0
    else
        codex_rc=$?
    fi

    write_pi_stub "$stub_dir"
    if [ -x "$PI_RUN" ] && PATH="$stub_dir:$PATH" PI_CALLED="$pi_called" \
        PI_ARGS="$pi_args" KNOWLEDGE_HOME="$knowledge_home" \
        "$PI_RUN" --model fixture > "$case_root/pi.out" 2> "$case_root/pi.err"; then
        pi_rc=0
    else
        pi_rc=$?
    fi

    if [ "$claude_rc" -eq 0 ] && grep -qx "$expected" "$case_root/claude.out" &&
        [ "$codex_rc" -eq 0 ] && grep -qx "$expected" "$target" &&
        [ "$pi_rc" -eq 0 ] && [ -e "$pi_called" ] &&
        grep -qx -e '--append-system-prompt' "$pi_args" &&
        grep -qx "$kernel_expected" "$pi_args"; then
        pass "$layout regular $tracking layout works across all adapters"
    else
        fail "$layout regular $tracking layout works across all adapters"
    fi
}

begin='<!-- agent-knowledge-kit:begin (managed block, do not edit by hand) -->'
end='<!-- agent-knowledge-kit:end -->'

test_final_symlink
test_parent_escape
test_internal_symlink
test_marker_rejected begin "$begin"
test_marker_rejected end "$end"
test_marker_absent_target
test_inline_marker_idempotency
test_kernel_without_final_newline
test_valid_layout flat
test_valid_layout nested
test_valid_layout flat non-git
test_valid_layout flat parent-git

printf '1..%d\n' "$tests"
if [ "$failures" -ne 0 ]; then
    printf '%d test(s) failed\n' "$failures" >&2
    exit 1
fi

# Keep the fixture-only publication transaction in the canonical portable
# verification command. It emits its own TAP document and fails this runner on
# any publication regression.
"$TEST_DIR/publisher/run.sh"
