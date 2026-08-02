#!/bin/sh
# Portable regressions for local publication integrity and fixture transactions.
set -u
umask 077

TEST_DIR=$(CDPATH= cd "$(dirname "$0")" && pwd -P)
REPO_ROOT=$(CDPATH= cd "$TEST_DIR/../.." && pwd -P)
PUBLISHER="$REPO_ROOT/publisher/publish.sh"
SYSTEM_PATH=/usr/bin:/bin

TEST_TMP=$(mktemp -d "${TMPDIR:-/tmp}/akk-publisher-tests.XXXXXX") || exit 1

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

new_case() {
    case_root="$TEST_TMP/$1 with spaces"
    mkdir -p "$case_root"
    case_root=$(CDPATH= cd "$case_root" && pwd -P)
    control_root="$case_root/control root"
    publication_root="$case_root/publication root"
}

prepare_case() {
    new_case "$1"
    "$PUBLISHER" prepare "$control_root" "$publication_root" \
        > "$case_root/prepare.out" 2> "$case_root/prepare.err"
}

make_candidate() {
    candidate_name=$1
    candidate_sequence=$2
    candidate_digest=$3
    candidate_payload=$4
    candidate="$control_root/quarantine/$candidate_name"
    mkdir -p "$candidate/corpus/kernel" "$candidate/corpus/docs"
    {
        printf '%s\n' 'agent-knowledge-kit-authenticated-test-fixture-v1'
        printf 'sequence %s\n' "$candidate_sequence"
        printf 'digest %s\n' "$candidate_digest"
    } > "$candidate/authenticated.fixture"
    printf '%s\n' "$candidate_payload" > "$candidate/corpus/kernel/kernel.md"
    printf 'generation=%s\n' "$candidate_sequence" > "$candidate/corpus/docs/generation"
}

promote() {
    "$PUBLISHER" promote-fixture "$control_root" "$publication_root" "$@"
}

expect_failure() {
    expected_type=$1
    output_file=$2
    shift 2
    if "$@" > "$output_file.out" 2> "$output_file.err"; then
        return 1
    fi
    first_error_line=$(sed -n '1p' "$output_file.err")
    case "$first_error_line" in
    "$expected_type":*) return 0 ;;
    *) return 1 ;;
    esac
}

has_error_type() {
    error_file=$1
    expected_type=$2
    first_error_line=$(sed -n '1p' "$error_file")
    case "$first_error_line" in
    "$expected_type":*) return 0 ;;
    *) return 1 ;;
    esac
}

A32=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
B32=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
C32=cccccccccccccccccccccccccccccccc
D32=dddddddddddddddddddddddddddddddd

test_production_promotion_requires_complete_arguments() {
    new_case production-arguments
    if expect_failure 'usage' "$case_root/promote" \
        "$PUBLISHER" promote "$control_root" "$publication_root"; then
        pass 'production promotion requires the complete authenticated interface'
    else
        fail 'production promotion requires the complete authenticated interface'
    fi
}

test_bootstrap_resolve_and_hostile_environment() {
    prepare_case bootstrap
    make_candidate r1 1 "$A32" 'kernel generation one'
    fake_bin="$case_root/fake-bin"
    sentinel="$case_root/hostile-path-used"
    mkdir -p "$fake_bin"
    for command_name in mv readlink stat; do
        {
            printf '%s\n' '#!/bin/sh'
            printf 'touch "%s"\n' "$sentinel"
            printf 'exit 99\n'
        } > "$fake_bin/$command_name"
        chmod +x "$fake_bin/$command_name"
    done

    if HOME="$case_root/hostile-home" \
        KNOWLEDGE_HOME="$case_root/hostile-knowledge" \
        PATH="$fake_bin:$SYSTEM_PATH" \
        promote "$candidate" > "$case_root/promote.out" 2> "$case_root/promote.err" &&
        resolved=$(
            HOME="$case_root/other-home" KNOWLEDGE_HOME="$case_root/other-knowledge" \
                PATH="$fake_bin:$SYSTEM_PATH" \
                "$PUBLISHER" resolve "$control_root" "$publication_root"
        ) &&
        [ "$resolved" = "$publication_root/releases/r1-$A32/corpus" ] &&
        grep -qx 'kernel generation one' "$resolved/kernel/kernel.md" &&
        "$PUBLISHER" check "$control_root" "$publication_root" >/dev/null &&
        [ ! -e "$sentinel" ]; then
        pass 'bootstrap resolves one physical release and ignores hostile environment authority'
    else
        fail 'bootstrap resolves one physical release and ignores hostile environment authority'
    fi
}

test_invalid_identity_rejected() {
    prepare_case invalid-identity
    make_candidate leading-zero 01 "$A32" invalid
    bad_sequence=$candidate
    make_candidate uppercase 2 AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA invalid
    bad_digest=$candidate

    if expect_failure 'candidate-invalid' "$case_root/sequence" \
        promote "$bad_sequence" &&
        expect_failure 'candidate-invalid' "$case_root/digest" \
            promote "$bad_digest" &&
        [ ! -e "$publication_root/current" ]; then
        pass 'invalid release sequence and digest never become reachable'
    else
        fail 'invalid release sequence and digest never become reachable'
    fi
}

test_strict_records_reject_unterminated_trailing_data() {
    prepare_case strict-fixture
    make_candidate trailing 1 "$A32" trailing
    printf 'ignored-junk' >> "$candidate/authenticated.fixture"
    fixture_candidate=$candidate
    fixture_ok=0
    if expect_failure 'candidate-invalid' "$case_root/fixture" \
        promote "$fixture_candidate"; then
        fixture_ok=1
    fi

    prepare_case strict-fixture-nul
    make_candidate trailing-nul 1 "$A32" trailing
    printf '\000' >> "$candidate/authenticated.fixture"
    fixture_nul_ok=0
    if expect_failure 'candidate-invalid' "$case_root/fixture-nul" \
        promote "$candidate"; then
        fixture_nul_ok=1
    fi

    prepare_case strict-installation
    printf 'ignored-junk' >> "$control_root/state/installation"
    installation_ok=0
    if expect_failure 'local-integrity' "$case_root/installation" \
        "$PUBLISHER" check "$control_root" "$publication_root"; then
        installation_ok=1
    fi

    prepare_case strict-installation-nul
    printf '\000' >> "$control_root/state/installation"
    installation_nul_ok=0
    if expect_failure 'local-integrity' "$case_root/installation-nul" \
        "$PUBLISHER" check "$control_root" "$publication_root"; then
        installation_nul_ok=1
    fi

    prepare_case strict-publication-state
    make_candidate selected 1 "$A32" selected
    promote "$candidate" >/dev/null
    printf 'ignored-junk' >> "$control_root/state/publication"
    state_ok=0
    if expect_failure 'local-integrity' "$case_root/state" \
        "$PUBLISHER" check "$control_root" "$publication_root"; then
        state_ok=1
    fi

    if [ "$fixture_ok" -eq 1 ] && [ "$fixture_nul_ok" -eq 1 ] &&
        [ "$installation_ok" -eq 1 ] && [ "$installation_nul_ok" -eq 1 ] &&
        [ "$state_ok" -eq 1 ]; then
        pass 'fixture, installation, and publication parsers reject trailing unterminated data'
    else
        fail 'fixture, installation, and publication parsers reject trailing unterminated data'
    fi
}

test_unsafe_candidate_entries_rejected() {
    prepare_case unsafe-entry
    make_candidate symlink 1 "$A32" safe
    ln -s /etc/passwd "$candidate/corpus/docs/escape"
    symlink_candidate=$candidate
    make_candidate fifo 2 "$B32" safe
    mkfifo "$candidate/corpus/docs/pipe"
    fifo_candidate=$candidate

    if expect_failure 'candidate-invalid' "$case_root/symlink" \
        promote "$symlink_candidate" &&
        expect_failure 'candidate-invalid' "$case_root/fifo" \
            promote "$fifo_candidate" &&
        [ ! -e "$publication_root/current" ]; then
        pass 'symlinks and special files are rejected before publication'
    else
        fail 'symlinks and special files are rejected before publication'
    fi
}

test_monotonicity_equivocation_and_idempotence() {
    prepare_case monotonicity
    make_candidate selected 2 "$B32" 'kernel generation two'
    selected=$candidate
    make_candidate equivocation 2 "$C32" 'equivocating generation two'
    equivocation=$candidate
    make_candidate rollback 1 "$A32" 'kernel generation one'
    rollback=$candidate

    if promote "$selected" >/dev/null &&
        promote "$selected" >/dev/null &&
        expect_failure 'sequence-equivocation' "$case_root/equivocation" \
            promote "$equivocation" &&
        expect_failure 'rollback' "$case_root/rollback" \
            promote "$rollback" &&
        [ "$("$PUBLISHER" resolve "$control_root" "$publication_root")" = \
            "$publication_root/releases/r2-$B32/corpus" ]; then
        pass 'promotion is idempotent and refuses rollback or sequence equivocation'
    else
        fail 'promotion is idempotent and refuses rollback or sequence equivocation'
    fi
}

test_large_sequences_compare_without_shell_overflow() {
    prepare_case large-sequence
    make_candidate seventeen 99999999999999999 "$A32" seventeen
    seventeen=$candidate
    make_candidate eighteen 100000000000000000 "$B32" eighteen
    eighteen=$candidate

    if promote "$seventeen" >/dev/null && promote "$eighteen" >/dev/null &&
        [ "$("$PUBLISHER" resolve "$control_root" "$publication_root")" = \
            "$publication_root/releases/r100000000000000000-$B32/corpus" ]; then
        pass '17/18-digit release sequences compare without shell arithmetic overflow'
    else
        fail '17/18-digit release sequences compare without shell arithmetic overflow'
    fi
}

test_half_initialized_bootstrap_never_restarts() {
    prepare_case half-bootstrap-selector
    make_candidate selected 1 "$A32" selected
    selected=$candidate
    if promote "$selected" after-selector \
        > "$case_root/after.out" 2> "$case_root/after.err"; then
        after_rc=0
    else
        after_rc=$?
    fi
    selector_half_ok=0
    if [ "$after_rc" -ne 0 ] &&
        expect_failure 'local-integrity' "$case_root/check-after" \
            "$PUBLISHER" check "$control_root" "$publication_root" &&
        expect_failure 'local-integrity' "$case_root/retry-after" \
            promote "$selected"; then
        selector_half_ok=1
    fi

    prepare_case half-bootstrap-release
    make_candidate staged 1 "$A32" staged
    staged=$candidate
    if promote "$staged" before-selector \
        > "$case_root/before.out" 2> "$case_root/before.err"; then
        before_rc=0
    else
        before_rc=$?
    fi
    release_half_ok=0
    if [ "$before_rc" -ne 0 ] &&
        expect_failure 'local-integrity' "$case_root/check-before" \
            "$PUBLISHER" check "$control_root" "$publication_root" &&
        expect_failure 'local-integrity' "$case_root/retry-before" \
            promote "$staged"; then
        release_half_ok=1
    fi

    if [ "$selector_half_ok" -eq 1 ] && [ "$release_half_ok" -eq 1 ]; then
        pass 'selector-only and release-only bootstrap residue require operator recovery'
    else
        fail 'selector-only and release-only bootstrap residue require operator recovery'
    fi
}

test_state_ahead_of_selector_never_lowers() {
    prepare_case state-ahead
    make_candidate selected 1 "$A32" selected
    promote "$candidate" >/dev/null
    state_file="$control_root/state/publication"
    chmod u+w "$state_file"
    {
        printf '%s\n' 'agent-knowledge-kit-local-publication-state-v1'
        printf 'selected r2-%s\n' "$B32"
        printf 'watermark r2-%s\n' "$B32"
    } > "$state_file"
    chmod 0600 "$state_file"
    make_candidate future 3 "$C32" future
    future=$candidate

    if expect_failure 'local-integrity' "$case_root/check" \
        "$PUBLISHER" check "$control_root" "$publication_root" &&
        expect_failure 'local-integrity' "$case_root/promote" \
            promote "$future" &&
        grep -qx "watermark r2-$B32" "$state_file"; then
        pass 'state ahead of current fails loud and is never reconciled downward'
    else
        fail 'state ahead of current fails loud and is never reconciled downward'
    fi
}

test_invalid_selector_fails_loud() {
    prepare_case invalid-selector
    make_candidate selected 1 "$A32" selected
    promote "$candidate" >/dev/null
    rm "$publication_root/current"
    ln -s "$publication_root/releases/r1-$A32" "$publication_root/current"

    if expect_failure 'local-integrity' "$case_root/check" \
        "$PUBLISHER" check "$control_root" "$publication_root" &&
        expect_failure 'local-integrity' "$case_root/resolve" \
            "$PUBLISHER" resolve "$control_root" "$publication_root"; then
        pass 'an absolute or malformed active selector fails loud'
    else
        fail 'an absolute or malformed active selector fails loud'
    fi
}

test_selector_preserves_raw_newline_for_validation() {
    prepare_case selector-newline
    make_candidate selected 1 "$A32" selected
    promote "$candidate" >/dev/null
    rm "$publication_root/current"
    newline='
'
    ln -s "releases/r1-$A32$newline" "$publication_root/current"

    if expect_failure 'local-integrity' "$case_root/check" \
        "$PUBLISHER" check "$control_root" "$publication_root"; then
        pass 'selector validation rejects a raw target with a trailing newline'
    else
        fail 'selector validation rejects a raw target with a trailing newline'
    fi
}

test_mode_tampering_fails_integrity_check() {
    prepare_case mode-tamper
    make_candidate selected 1 "$A32" selected
    promote "$candidate" >/dev/null
    selected_path=$("$PUBLISHER" resolve "$control_root" "$publication_root")
    chmod g+w "$selected_path/kernel/kernel.md"
    kernel_ok=0
    if expect_failure 'local-integrity' "$case_root/kernel-mode" \
        "$PUBLISHER" check "$control_root" "$publication_root"; then
        kernel_ok=1
    fi
    chmod g-w "$selected_path/kernel/kernel.md"
    chmod g+w "$control_root/state/publication"
    state_ok=0
    if expect_failure 'local-integrity' "$case_root/state-mode" \
        "$PUBLISHER" check "$control_root" "$publication_root"; then
        state_ok=1
    fi

    if [ "$kernel_ok" -eq 1 ] && [ "$state_ok" -eq 1 ]; then
        pass 'group-writable released bytes and protected state fail integrity checks'
    else
        fail 'group-writable released bytes and protected state fail integrity checks'
    fi
}

test_release_layout_and_metadata_are_exact() {
    prepare_case exact-release
    make_candidate selected 1 "$A32" selected
    mkdir "$candidate/corpus/.hidden"
    printf '%s\n' hidden > "$candidate/corpus/.hidden/value"
    newline_name='line
break'
    printf '%s\n' newline > "$candidate/corpus/$newline_name"
    promote "$candidate" >/dev/null
    release_root="$publication_root/releases/r1-$A32"
    path_walk_ok=0
    if "$PUBLISHER" check "$control_root" "$publication_root" >/dev/null &&
        [ -f "$release_root/corpus/.hidden/value" ] &&
        [ -f "$release_root/corpus/$newline_name" ]; then
        path_walk_ok=1
    fi
    chmod u+w "$release_root/release.meta"
    printf '\n' >> "$release_root/release.meta"
    chmod 0444 "$release_root/release.meta"
    metadata_ok=0
    if expect_failure 'local-integrity' "$case_root/metadata" \
        "$PUBLISHER" check "$control_root" "$publication_root"; then
        metadata_ok=1
    fi

    prepare_case exact-metadata-nul
    make_candidate selected 1 "$A32" selected
    promote "$candidate" >/dev/null
    release_root="$publication_root/releases/r1-$A32"
    chmod u+w "$release_root/release.meta"
    printf '\000' >> "$release_root/release.meta"
    chmod 0444 "$release_root/release.meta"
    metadata_nul_ok=0
    if expect_failure 'local-integrity' "$case_root/metadata-nul" \
        "$PUBLISHER" check "$control_root" "$publication_root"; then
        metadata_nul_ok=1
    fi

    prepare_case exact-layout
    make_candidate selected 1 "$A32" selected
    promote "$candidate" >/dev/null
    release_root="$publication_root/releases/r1-$A32"
    ln -s /etc/passwd "$release_root/extra-link"
    layout_ok=0
    if expect_failure 'local-integrity' "$case_root/layout" \
        "$PUBLISHER" check "$control_root" "$publication_root"; then
        layout_ok=1
    fi

    if [ "$path_walk_ok" -eq 1 ] && [ "$metadata_ok" -eq 1 ] &&
        [ "$metadata_nul_ok" -eq 1 ] &&
        [ "$layout_ok" -eq 1 ]; then
        pass 'selected release paths, metadata, and root layout are exact'
    else
        fail 'selected release paths, metadata, and root layout are exact'
    fi
}

test_failure_boundaries_and_state_repair() {
    prepare_case failure-boundaries
    make_candidate selected 1 "$A32" 'kernel generation one'
    selected=$candidate
    promote "$selected" >/dev/null
    old_path=$("$PUBLISHER" resolve "$control_root" "$publication_root")

    make_candidate before 2 "$B32" 'kernel generation two'
    before=$candidate
    if promote "$before" before-selector \
        > "$case_root/before.out" 2> "$case_root/before.err"; then
        before_rc=0
    else
        before_rc=$?
    fi
    after_before=$("$PUBLISHER" resolve "$control_root" "$publication_root")

    make_candidate after 3 "$C32" 'kernel generation three'
    after=$candidate
    if promote "$after" after-selector \
        > "$case_root/after.out" 2> "$case_root/after.err"; then
        after_rc=0
    else
        after_rc=$?
    fi

    if [ "$before_rc" -ne 0 ] && [ "$after_before" = "$old_path" ] &&
        grep -qx 'kernel generation one' "$old_path/kernel/kernel.md" &&
        [ "$after_rc" -ne 0 ] &&
        expect_failure 'local-integrity' "$case_root/disagreement" \
            "$PUBLISHER" check "$control_root" "$publication_root" &&
        promote "$after" >/dev/null &&
        "$PUBLISHER" check "$control_root" "$publication_root" >/dev/null &&
        [ -r "$old_path/kernel/kernel.md" ] &&
        [ "$("$PUBLISHER" resolve "$control_root" "$publication_root")" = \
            "$publication_root/releases/r3-$C32/corpus" ]; then
        pass 'pre-selector failure preserves old bytes and post-selector residue repairs upward'
    else
        fail 'pre-selector failure preserves old bytes and post-selector residue repairs upward'
    fi
}

test_concurrent_promotions_end_at_highest_sequence() {
    prepare_case concurrency
    make_candidate initial 1 "$A32" initial
    promote "$candidate" >/dev/null
    make_candidate second 2 "$B32" second
    second=$candidate
    make_candidate third 3 "$C32" third
    third=$candidate

    promote "$second" > "$case_root/second.out" 2> "$case_root/second.err" &
    second_pid=$!
    promote "$third" > "$case_root/third.out" 2> "$case_root/third.err" &
    third_pid=$!
    if wait "$second_pid"; then second_rc=0; else second_rc=$?; fi
    if wait "$third_pid"; then third_rc=0; else third_rc=$?; fi

    resolved=$("$PUBLISHER" resolve "$control_root" "$publication_root")
    if [ "$third_rc" -eq 0 ] &&
        { [ "$second_rc" -eq 0 ] || has_error_type "$case_root/second.err" rollback; } &&
        [ "$resolved" = "$publication_root/releases/r3-$C32/corpus" ] &&
        grep -qx third "$resolved/kernel/kernel.md" &&
        "$PUBLISHER" check "$control_root" "$publication_root" >/dev/null; then
        pass 'concurrent promotions serialize at the highest sequence'
    else
        fail 'concurrent promotions serialize at the highest sequence'
    fi
}


test_signal_stops_holder_before_lock_cleanup() {
    prepare_case signal-cleanup
    make_candidate initial 1 "$A32" initial
    promote "$candidate" >/dev/null
    old_path=$("$PUBLISHER" resolve "$control_root" "$publication_root")
    make_candidate held 2 "$B32" held
    held=$candidate

    "$PUBLISHER" promote-fixture "$control_root" "$publication_root" \
        "$held" hold-after-lock > "$case_root/held.out" 2> "$case_root/held.err" &
    held_pid=$!
    attempts=0
    while [ ! -d "$control_root/locks/publication" ] && [ "$attempts" -lt 50 ]; do
        attempts=$((attempts + 1))
        sleep 0.1
    done
    kill -TERM "$held_pid"
    if wait "$held_pid"; then held_rc=0; else held_rc=$?; fi

    if [ "$held_rc" -ne 0 ] && [ ! -e "$control_root/locks/publication" ] &&
        [ "$("$PUBLISHER" resolve "$control_root" "$publication_root")" = "$old_path" ]; then
        pass 'a signaled lock holder exits before cleanup releases the mutex'
    else
        fail 'a signaled lock holder exits before cleanup releases the mutex'
    fi
}

test_orphaned_lock_fails_closed() {
    prepare_case orphan-lock
    make_candidate selected 1 "$A32" selected
    mkdir "$control_root/locks/publication"
    if expect_failure 'publication-locked' "$case_root/locked" \
        promote "$candidate" && [ ! -e "$publication_root/current" ]; then
        pass 'orphaned publication mutex fails closed without PID/age reclamation'
    else
        fail 'orphaned publication mutex fails closed without PID/age reclamation'
    fi
    rmdir "$control_root/locks/publication"
}

if [ ! -x "$PUBLISHER" ]; then
    printf 'publisher executable missing: %s\n' "$PUBLISHER" >&2
    exit 1
fi

test_production_promotion_requires_complete_arguments
test_bootstrap_resolve_and_hostile_environment
test_invalid_identity_rejected
test_strict_records_reject_unterminated_trailing_data
test_unsafe_candidate_entries_rejected
test_monotonicity_equivocation_and_idempotence
test_large_sequences_compare_without_shell_overflow
test_half_initialized_bootstrap_never_restarts
test_state_ahead_of_selector_never_lowers
test_invalid_selector_fails_loud
test_selector_preserves_raw_newline_for_validation
test_mode_tampering_fails_integrity_check
test_release_layout_and_metadata_are_exact
test_failure_boundaries_and_state_repair
test_concurrent_promotions_end_at_highest_sequence
test_signal_stops_holder_before_lock_cleanup
test_orphaned_lock_fails_closed

printf '1..%d\n' "$tests"
if [ "$failures" -ne 0 ]; then
    printf '%d test(s) failed\n' "$failures" >&2
    exit 1
fi
