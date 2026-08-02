#!/bin/sh
# Portable regressions for the bounded Codex protected-config renderer.
set -eu
umask 077

unset CDPATH
TEST_DIR=$(cd "$(dirname "$0")" && pwd -P)
REPO_ROOT=$(cd "$TEST_DIR/../.." && pwd -P)
PUBLISHER="$REPO_ROOT/publisher/publish.sh"
RENDERER="$REPO_ROOT/adapters/codex/render-protected-config.sh"

TEST_TMP=$(mktemp -d "${TMPDIR:-/tmp}/akk-codex-protected-tests.XXXXXX") || exit 1
TEST_TMP=$(cd "$TEST_TMP" && pwd -P) || exit 1

cleanup_test_tmp() {
    chmod -R u+w "$TEST_TMP" 2>/dev/null || :
    rm -rf "$TEST_TMP"
}

trap cleanup_test_tmp EXIT HUP INT TERM

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

prepare_case() {
    case_root="$TEST_TMP/$1 with spaces"
    control_root="$case_root/control"
    publication_root="$case_root/publication"
    candidate="$control_root/quarantine/candidate"
    mkdir -p "$case_root"
    "$PUBLISHER" prepare "$control_root" "$publication_root" >/dev/null
    mkdir -p "$candidate/corpus/kernel"
    {
        printf '%s\n' 'agent-knowledge-kit-authenticated-test-fixture-v1'
        printf '%s\n' 'sequence 1'
        printf '%s\n' 'digest aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'
    } > "$candidate/authenticated.fixture"
    printf '%s\n' 'protected Codex fixture' > "$candidate/corpus/kernel/kernel.md"
    "$PUBLISHER" promote-fixture "$control_root" "$publication_root" "$candidate" >/dev/null
    release_target=$(/usr/bin/readlink "$publication_root/current")
    release_dir="$publication_root/$release_target"
}

expect_rejection() {
    output_prefix=$1
    shift
    if "$@" > "$output_prefix.out" 2> "$output_prefix.err"; then
        return 1
    fi
    [ ! -s "$output_prefix.out" ] &&
        /usr/bin/grep -q '^protected-config:' "$output_prefix.err"
}

test_exact_managed_config_ignores_environment_authority() {
    prepare_case exact-output
    expected="model_instructions_file = '$publication_root/current/corpus/kernel/kernel.md'"
    if actual=$(HOME="$case_root/hostile-home" PATH=/nonexistent \
        TMPDIR="$case_root/hostile-tmp" CODEX_HOME="$case_root/hostile-codex" \
        KNOWLEDGE_HOME="$case_root/hostile-knowledge" \
        "$RENDERER" "$publication_root") && [ "$actual" = "$expected" ]; then
        pass 'renderer emits one exact managed-config key without environment authority'
    else
        fail 'renderer emits one exact managed-config key without environment authority'
    fi
}

test_symlinked_publication_root_rejected() {
    prepare_case symlink-root
    publication_link="$case_root/publication-link"
    ln -s "$publication_root" "$publication_link"
    if expect_rejection "$case_root/symlink-root" \
        "$RENDERER" "$publication_link"; then
        pass 'symlinked publication roots are rejected'
    else
        fail 'symlinked publication roots are rejected'
    fi
}

test_malformed_selectors_rejected_without_fallback() {
    prepare_case malformed-selector
    rm "$publication_root/current"
    ln -s "$release_dir" "$publication_root/current"
    absolute_ok=0
    if expect_rejection "$case_root/absolute-selector" \
        "$RENDERER" "$publication_root"; then
        absolute_ok=1
    fi
    rm "$publication_root/current"
    ln -s '../outside' "$publication_root/current"
    parent_ok=0
    if expect_rejection "$case_root/parent-selector" \
        "$RENDERER" "$publication_root"; then
        parent_ok=1
    fi
    if [ "$absolute_ok" -eq 1 ] && [ "$parent_ok" -eq 1 ]; then
        pass 'absolute and escaping selectors fail closed without output'
    else
        fail 'absolute and escaping selectors fail closed without output'
    fi
}

test_missing_or_symlinked_kernel_rejected() {
    prepare_case unsafe-kernel
    chmod u+w "$release_dir" "$release_dir/corpus" "$release_dir/corpus/kernel"
    mv "$release_dir/corpus/kernel/kernel.md" "$case_root/kernel-outside"
    missing_ok=0
    if expect_rejection "$case_root/missing-kernel" \
        "$RENDERER" "$publication_root"; then
        missing_ok=1
    fi
    ln -s "$case_root/kernel-outside" "$release_dir/corpus/kernel/kernel.md"
    symlink_ok=0
    if expect_rejection "$case_root/symlink-kernel" \
        "$RENDERER" "$publication_root"; then
        symlink_ok=1
    fi
    if [ "$missing_ok" -eq 1 ] && [ "$symlink_ok" -eq 1 ]; then
        pass 'missing and symlinked kernels are rejected without fallback'
    else
        fail 'missing and symlinked kernels are rejected without fallback'
    fi
}

test_toml_path_injection_rejected() {
    unsafe_root="$TEST_TMP/unsafe'root"
    mkdir -p "$unsafe_root"
    if expect_rejection "$TEST_TMP/unsafe-root" \
        "$RENDERER" "$unsafe_root"; then
        pass 'paths that could escape the TOML literal are rejected'
    else
        fail 'paths that could escape the TOML literal are rejected'
    fi
}

test_exact_managed_config_ignores_environment_authority
test_symlinked_publication_root_rejected
test_malformed_selectors_rejected_without_fallback
test_missing_or_symlinked_kernel_rejected
test_toml_path_injection_rejected

printf '1..%d\n' "$tests"
if [ "$failures" -ne 0 ]; then
    printf '%d test(s) failed\n' "$failures" >&2
    exit 1
fi
