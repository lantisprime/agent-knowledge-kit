#!/bin/sh
# Portable production corpus release authentication regressions.
set -u
umask 077

unset CDPATH
TEST_DIR=$(cd "$(dirname "$0")" && pwd -P)
REPO_ROOT=$(cd "$TEST_DIR/../.." && pwd -P)
PUBLISHER="$REPO_ROOT/publisher/publish.sh"

TEST_TMP=$(mktemp -d "${TMPDIR:-/tmp}/akk-authentication-tests.XXXXXX") || exit 1

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

prepare_case() {
    case_root="$TEST_TMP/$1 with spaces"
    mkdir -p "$case_root"
    case_root=$(cd "$case_root" && pwd -P)
    control_root="$case_root/control root"
    publication_root="$case_root/publication root"
    source_repo="$case_root/source repo"
    signing_key="$case_root/release signing key"
    candidate="$control_root/quarantine/candidate.git"

    "$PUBLISHER" prepare "$control_root" "$publication_root" >/dev/null
    ssh-keygen -q -t ed25519 -N '' -f "$signing_key"
    wrong_signing_key="$case_root/wrong release signing key"
    ssh-keygen -q -t ed25519 -N '' -f "$wrong_signing_key"
    git init -q "$source_repo"
    git -C "$source_repo" symbolic-ref HEAD refs/heads/main
    git -C "$source_repo" config user.name 'Release Test'
    git -C "$source_repo" config user.email 'release@example.invalid'
    git -C "$source_repo" config gpg.format ssh
    git -C "$source_repo" config user.signingkey "$signing_key"
    printf '%s\n' 'release history root' > "$source_repo/.release-root"
    git -C "$source_repo" add .release-root
    git -C "$source_repo" commit -q -S -m 'release history root'

    IFS=' ' read -r signing_algorithm signing_public_key _ \
        < "$signing_key.pub"
    {
        printf '%s\n' 'agent-knowledge-kit-corpus-release-policy-v1'
        printf '%s\n' 'repository example.invalid/corpus'
        printf '%s\n' 'ref refs/heads/main'
        printf '%s\n' 'tag-prefix refs/tags/corpus-release/'
        printf '%s\n' 'signer corpus-release'
    } > "$control_root/trust/release-policy"
    printf 'corpus-release %s %s\n' "$signing_algorithm" "$signing_public_key" \
        > "$control_root/trust/release-signers"
    : > "$control_root/trust/release-revocations"
    chmod 0600 \
        "$control_root/trust/release-policy" \
        "$control_root/trust/release-signers" \
        "$control_root/trust/release-revocations"
}

write_release_commit() {
    release_sequence=$1
    release_payload=$2
    commit_signature=${3:-signed}
    mkdir -p "$source_repo/corpus/kernel" "$source_repo/corpus/docs"
    printf '%s\n' "$release_payload" > "$source_repo/corpus/kernel/kernel.md"
    printf 'sequence=%s\n' "$release_sequence" > "$source_repo/corpus/docs/generation"
    git -C "$source_repo" add corpus
    case "$commit_signature" in
    signed) git -C "$source_repo" commit -q -S -m "release $release_sequence" ;;
    unsigned) git -C "$source_repo" commit -q --no-gpg-sign -m "release $release_sequence" ;;
    *) return 1 ;;
    esac
}

create_release_tag() {
    release_sequence=$1
    tag_signature=${2:-signed}
    release_tag="refs/tags/corpus-release/r$release_sequence"
    release_commit=$(git -C "$source_repo" rev-parse HEAD)
    release_tree=$(git -C "$source_repo" rev-parse 'HEAD^{tree}')
    release_corpus_tree=$(git -C "$source_repo" rev-parse 'HEAD:corpus')
    release_archive="$case_root/release-$release_sequence.tar"
    git -C "$source_repo" archive --format=tar \
        --output="$release_archive" "$release_commit" corpus
    release_archive_oid=$(git -C "$source_repo" hash-object "$release_archive")
    manifest_repository=${TEST_MANIFEST_REPOSITORY:-example.invalid/corpus}
    manifest_ref=${TEST_MANIFEST_REF:-refs/heads/main}
    manifest_commit=${TEST_MANIFEST_COMMIT:-$release_commit}
    manifest_tree=${TEST_MANIFEST_TREE:-$release_tree}
    manifest_corpus_tree=${TEST_MANIFEST_CORPUS_TREE:-$release_corpus_tree}
    manifest_archive=${TEST_MANIFEST_ARCHIVE:-$release_archive_oid}
    release_manifest="agent-knowledge-kit-corpus-release-v1
repository $manifest_repository
ref $manifest_ref
sequence $release_sequence
commit $manifest_commit
tree $manifest_tree
corpus-tree $manifest_corpus_tree
archive $manifest_archive"
    if [ -n "${TEST_MANIFEST_EXTRA:-}" ]; then
        release_manifest="$release_manifest
$TEST_MANIFEST_EXTRA"
    fi
    git -C "$source_repo" tag -d "corpus-release/r$release_sequence" \
        >/dev/null 2>&1 || :
    case "$tag_signature" in
    signed)
        git -C "$source_repo" tag -s "corpus-release/r$release_sequence" \
            -m "$release_manifest" "$release_commit"
        ;;
    unsigned)
        git -C "$source_repo" tag -a "corpus-release/r$release_sequence" \
            -m "$release_manifest" "$release_commit"
        ;;
    *) return 1 ;;
    esac
}

make_release() {
    release_sequence=$1
    release_payload=$2
    commit_signature=${3:-signed}
    tag_signature=${4:-signed}
    write_release_commit "$release_sequence" "$release_payload" "$commit_signature"
    create_release_tag "$release_sequence" "$tag_signature"
}

clone_candidate() {
    git clone -q --bare "$source_repo" "$candidate"
}

update_candidate() {
    git -C "$candidate" fetch -q --force "$source_repo" \
        '+refs/heads/main:refs/heads/main' \
        '+refs/tags/corpus-release/*:refs/tags/corpus-release/*'
}

promote_release() {
    "$PUBLISHER" promote "$control_root" "$publication_root" \
        "$candidate" "$release_tag"
}

test_authenticated_bootstrap() {
    prepare_case authenticated-bootstrap
    make_release 1 'authenticated kernel generation one'
    clone_candidate

    if GIT_CONFIG_COUNT=1 \
        GIT_CONFIG_KEY_0=gpg.ssh.allowedSignersFile \
        GIT_CONFIG_VALUE_0=/dev/null \
        promote_release > "$case_root/promote.out" 2> "$case_root/promote.err" &&
        resolved=$("$PUBLISHER" resolve "$control_root" "$publication_root") &&
        grep -qx 'authenticated kernel generation one' \
            "$resolved/kernel/kernel.md"; then
        pass 'signed policy-matching Git objects bootstrap despite hostile ambient Git config'
    else
        fail 'signed policy-matching Git objects bootstrap despite hostile ambient Git config'
    fi
}

test_unsigned_commit_rejected() {
    prepare_case unsigned-commit
    make_release 1 unsigned-commit unsigned signed
    clone_candidate
    if expect_failure authentication-failed "$case_root/promote" \
        promote_release && [ ! -e "$publication_root/current" ]; then
        pass 'an unsigned release commit is rejected before publication'
    else
        fail 'an unsigned release commit is rejected before publication'
    fi
}

test_unsigned_tag_rejected() {
    prepare_case unsigned-tag
    make_release 1 unsigned-tag signed unsigned
    clone_candidate
    if expect_failure authentication-failed "$case_root/promote" \
        promote_release && [ ! -e "$publication_root/current" ]; then
        pass 'an unsigned release tag is rejected before publication'
    else
        fail 'an unsigned release tag is rejected before publication'
    fi
}

test_wrong_and_revoked_signers_rejected() {
    prepare_case wrong-signer
    git -C "$source_repo" config user.signingkey "$wrong_signing_key"
    make_release 1 wrong-signer
    clone_candidate
    wrong_ok=0
    if expect_failure authentication-failed "$case_root/wrong" \
        promote_release; then
        wrong_ok=1
    fi

    prepare_case revoked-signer
    IFS=' ' read -r signing_algorithm signing_public_key _ \
        < "$signing_key.pub"
    printf '%s %s\n' "$signing_algorithm" "$signing_public_key" \
        > "$control_root/trust/release-revocations"
    make_release 1 revoked-signer
    clone_candidate
    revoked_ok=0
    if expect_failure authentication-failed "$case_root/revoked" \
        promote_release; then
        revoked_ok=1
    fi

    if [ "$wrong_ok" -eq 1 ] && [ "$revoked_ok" -eq 1 ]; then
        pass 'wrong and revoked release signers are rejected'
    else
        fail 'wrong and revoked release signers are rejected'
    fi
}

test_repository_ssh_program_cannot_bypass_verification() {
    prepare_case repository-ssh-program
    git -C "$source_repo" config user.signingkey "$wrong_signing_key"
    make_release 1 repository-program-bypass
    clone_candidate
    repository_program="$candidate/fake-ssh-keygen"
    program_sentinel="$case_root/repository-program-ran"
    {
        printf '%s\n' '#!/bin/sh'
        printf ': > "%s"\n' "$program_sentinel"
        printf '%s\n' 'exec /usr/bin/ssh-keygen "$@"'
    } > "$repository_program"
    chmod 0700 "$repository_program"
    git -C "$candidate" config gpg.ssh.program "$repository_program"

    if expect_failure authentication-failed "$case_root/repository-program" \
        promote_release && [ ! -e "$program_sentinel" ] &&
        [ ! -e "$publication_root/current" ]; then
        pass 'repository-local SSH programs cannot execute or bypass signature policy'
    else
        fail 'repository-local SSH programs cannot execute or bypass signature policy'
    fi
}

test_repository_non_ssh_program_cannot_execute() {
    prepare_case repository-non-ssh-program
    mkdir -p "$source_repo/corpus/kernel" "$source_repo/corpus/docs"
    printf '%s\n' repository-non-ssh-program > "$source_repo/corpus/kernel/kernel.md"
    printf '%s\n' sequence=1 > "$source_repo/corpus/docs/generation"
    git -C "$source_repo" add corpus
    release_tree=$(git -C "$source_repo" write-tree)
    release_parent=$(git -C "$source_repo" rev-parse HEAD)
    malicious_commit="$case_root/non-ssh-commit"
    {
        printf 'tree %s\n' "$release_tree"
        printf 'parent %s\n' "$release_parent"
        printf '%s\n' 'author Release Test <release@example.invalid> 0 +0000'
        printf '%s\n' 'committer Release Test <release@example.invalid> 0 +0000'
        printf '%s\n' 'gpgsig -----BEGIN PGP SIGNATURE-----'
        printf '%s\n' ' forged'
        printf '%s\n' ' -----END PGP SIGNATURE-----'
        printf '%s\n' '' 'release 1'
    } > "$malicious_commit"
    malicious_commit_oid=$(git -C "$source_repo" hash-object -t commit -w "$malicious_commit")
    git -C "$source_repo" update-ref refs/heads/main "$malicious_commit_oid"
    create_release_tag 1 signed
    clone_candidate

    repository_program="$candidate/fake-openpgp-verifier"
    program_sentinel="$case_root/repository-openpgp-program-ran"
    {
        printf '%s\n' '#!/bin/sh'
        printf ': > "%s"\n' "$program_sentinel"
        printf '%s\n' 'exit 1'
    } > "$repository_program"
    chmod 0700 "$repository_program"
    git -C "$candidate" config gpg.program "$repository_program"
    git -C "$candidate" config gpg.openpgp.program "$repository_program"

    if expect_failure authentication-failed "$case_root/repository-openpgp-program" \
        promote_release && [ ! -e "$program_sentinel" ] &&
        [ ! -e "$publication_root/current" ]; then
        pass 'repository-local non-SSH verifiers cannot execute during authentication'
    else
        fail 'repository-local non-SSH verifiers cannot execute during authentication'
    fi
}

test_repository_archive_program_cannot_execute() {
    prepare_case repository-archive-program
    make_release 1 repository-archive-program
    clone_candidate
    repository_program="$candidate/fake-archive-filter"
    program_sentinel="$case_root/repository-archive-program-ran"
    {
        printf '%s\n' '#!/bin/sh'
        printf ': > "%s"\n' "$program_sentinel"
        printf '%s\n' 'exit 1'
    } > "$repository_program"
    chmod 0700 "$repository_program"
    git -C "$candidate" config tar.tar.command "$repository_program"

    if promote_release > "$case_root/promote.out" 2> "$case_root/promote.err" &&
        [ ! -e "$program_sentinel" ] &&
        resolved=$("$PUBLISHER" resolve "$control_root" "$publication_root") &&
        grep -qx repository-archive-program "$resolved/kernel/kernel.md"; then
        pass 'repository-local archive programs cannot execute during materialization'
    else
        fail 'repository-local archive programs cannot execute during materialization'
    fi
}

test_partial_clone_cannot_execute_lazy_fetch() {
    prepare_case partial-clone-fetch
    make_release 1 partial-clone-fetch
    clone_candidate
    blob_oid=$(git -C "$source_repo" rev-parse HEAD:corpus/kernel/kernel.md)
    blob_prefix=${blob_oid%"${blob_oid#??}"}
    blob_suffix=${blob_oid#??}
    blob_path="$candidate/objects/$blob_prefix/$blob_suffix"
    [ -f "$blob_path" ] || {
        fail 'partial-clone configuration is rejected without lazy fetch execution'
        return
    }
    mv "$blob_path" "$candidate/missing-object"
    lazy_helper="$TEST_TMP/lazy-fetch-helper"
    lazy_sentinel="$TEST_TMP/lazy-fetch-ran"
    {
        printf '%s\n' '#!/bin/sh'
        printf ': > "%s"\n' "$lazy_sentinel"
        printf '%s\n' 'exit 1'
    } > "$lazy_helper"
    chmod 0700 "$lazy_helper"
    git -C "$candidate" config core.repositoryFormatVersion 1
    git -C "$candidate" config extensions.partialClone origin
    git -C "$candidate" config remote.origin.promisor true
    git -C "$candidate" config remote.origin.partialCloneFilter blob:none
    git -C "$candidate" config remote.origin.url "ext::$lazy_helper"
    git -C "$candidate" config protocol.ext.allow always

    if expect_failure candidate-invalid "$case_root/partial-clone" \
        promote_release && [ ! -e "$lazy_sentinel" ] &&
        [ ! -e "$publication_root/current" ]; then
        pass 'partial-clone configuration is rejected without lazy fetch execution'
    else
        fail 'partial-clone configuration is rejected without lazy fetch execution'
    fi
}

test_policy_and_manifest_mismatch_rejected() {
    prepare_case policy-mismatch
    TEST_MANIFEST_REPOSITORY=other.invalid/corpus
    export TEST_MANIFEST_REPOSITORY
    make_release 1 wrong-repository
    unset TEST_MANIFEST_REPOSITORY
    clone_candidate
    repository_ok=0
    if expect_failure authentication-failed "$case_root/repository" \
        promote_release; then
        repository_ok=1
    fi

    prepare_case extra-manifest
    TEST_MANIFEST_EXTRA='unexpected field'
    export TEST_MANIFEST_EXTRA
    make_release 1 extra-manifest
    unset TEST_MANIFEST_EXTRA
    clone_candidate
    manifest_ok=0
    if expect_failure authentication-failed "$case_root/manifest" \
        promote_release; then
        manifest_ok=1
    fi

    prepare_case archive-mismatch
    TEST_MANIFEST_ARCHIVE=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
    export TEST_MANIFEST_ARCHIVE
    make_release 1 archive-mismatch
    unset TEST_MANIFEST_ARCHIVE
    clone_candidate
    archive_ok=0
    if expect_failure authentication-failed "$case_root/archive" \
        promote_release; then
        archive_ok=1
    fi

    if [ "$repository_ok" -eq 1 ] && [ "$manifest_ok" -eq 1 ] &&
        [ "$archive_ok" -eq 1 ]; then
        pass 'policy mismatch, extra manifest bytes, and archive mismatch are rejected'
    else
        fail 'policy mismatch, extra manifest bytes, and archive mismatch are rejected'
    fi
}

test_unreachable_release_and_unsafe_tree_rejected() {
    prepare_case unreachable-ref
    make_release 1 unreachable
    git -C "$source_repo" reset -q --hard HEAD^
    clone_candidate
    unreachable_ok=0
    if expect_failure authentication-failed "$case_root/unreachable" \
        promote_release; then
        unreachable_ok=1
    fi

    prepare_case symlink-tree
    mkdir -p "$source_repo/corpus/docs"
    ln -s /etc/passwd "$source_repo/corpus/docs/escape"
    make_release 1 symlink-tree
    clone_candidate
    symlink_ok=0
    if expect_failure candidate-invalid "$case_root/symlink" \
        promote_release; then
        symlink_ok=1
    fi

    if [ "$unreachable_ok" -eq 1 ] && [ "$symlink_ok" -eq 1 ]; then
        pass 'unreachable commits and unsafe Git tree modes are rejected'
    else
        fail 'unreachable commits and unsafe Git tree modes are rejected'
    fi
}

test_worktree_tampering_cannot_change_published_bytes() {
    prepare_case worktree-tamper
    make_release 1 'signed object bytes'
    printf '%s\n' 'mutable worktree tamper' > "$source_repo/corpus/kernel/kernel.md"
    clone_candidate
    if promote_release >/dev/null 2>&1 &&
        resolved=$("$PUBLISHER" resolve "$control_root" "$publication_root") &&
        grep -qx 'signed object bytes' "$resolved/kernel/kernel.md" &&
        ! grep -q 'mutable worktree tamper' "$resolved/kernel/kernel.md"; then
        pass 'mutable worktree bytes cannot reach production publication'
    else
        fail 'mutable worktree bytes cannot reach production publication'
    fi
}

test_reused_release_id_with_different_bytes_rejected() {
    prepare_case reused-production-id
    make_release 1 original-bytes
    clone_candidate
    if ! promote_release >/dev/null 2>&1; then
        fail 'a reused authenticated release id rejects different published bytes'
        return
    fi
    resolved=$("$PUBLISHER" resolve "$control_root" "$publication_root")
    chmod u+w "$resolved/kernel/kernel.md"
    printf '%s\n' corrupted-bytes > "$resolved/kernel/kernel.md"
    chmod 0444 "$resolved/kernel/kernel.md"
    if expect_failure sequence-equivocation "$case_root/reused" \
        promote_release; then
        pass 'a reused authenticated release id rejects different published bytes'
    else
        fail 'a reused authenticated release id rejects different published bytes'
    fi
}

test_authenticated_update_rollback_and_equivocation() {
    prepare_case monotonic-production
    make_release 2 generation-two
    clone_candidate
    bootstrap_ok=0
    if promote_release >/dev/null 2>&1; then
        bootstrap_ok=1
    fi

    make_release 3 generation-three
    generation_two_commit=$(git -C "$source_repo" rev-parse \
        'refs/tags/corpus-release/r2^{}')
    update_candidate
    update_ok=0
    if promote_release >/dev/null 2>&1; then
        update_ok=1
    fi

    git -C "$source_repo" reset -q --hard "$generation_two_commit"
    make_release 1 generation-one
    update_candidate
    rollback_ok=0
    if expect_failure rollback "$case_root/rollback" promote_release; then
        rollback_ok=1
    fi

    git -C "$source_repo" reset -q --hard "$generation_two_commit"
    make_release 3 different-generation-three
    update_candidate
    equivocation_ok=0
    if expect_failure sequence-equivocation "$case_root/equivocation" \
        promote_release; then
        equivocation_ok=1
    fi

    if [ "$bootstrap_ok" -eq 1 ] && [ "$update_ok" -eq 1 ] &&
        [ "$rollback_ok" -eq 1 ] &&
        [ "$equivocation_ok" -eq 1 ]; then
        pass 'authenticated updates refuse rollback and sequence equivocation'
    else
        fail 'authenticated updates refuse rollback and sequence equivocation'
    fi
}

test_authenticated_bootstrap
test_unsigned_commit_rejected
test_unsigned_tag_rejected
test_wrong_and_revoked_signers_rejected
test_repository_ssh_program_cannot_bypass_verification
test_repository_non_ssh_program_cannot_execute
test_repository_archive_program_cannot_execute
test_partial_clone_cannot_execute_lazy_fetch
test_policy_and_manifest_mismatch_rejected
test_unreachable_release_and_unsafe_tree_rejected
test_worktree_tampering_cannot_change_published_bytes
test_reused_release_id_with_different_bytes_rejected
test_authenticated_update_rollback_and_equivocation

printf '1..%d\n' "$tests"
if [ "$failures" -ne 0 ]; then
    printf '%d test(s) failed\n' "$failures" >&2
    exit 1
fi
