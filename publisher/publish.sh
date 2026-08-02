#!/bin/sh
# Authenticated corpus publication transaction.
#
# `promote` accepts a protected bare Git repository and an SSH-signed annotated
# release tag. `promote-fixture` remains a test-only path whose descriptor is
# treated by the test harness as already authenticated and currency-approved.
#
# The portable slice validates publisher ownership/modes and rejects extended
# ACLs on managed paths. Every ancestor, effective per-agent access,
# installed-code identity, and real two-principal behavior remain platform-
# test requirements; do not treat these checks as the complete C1-b boundary.
set -eu

PATH=/usr/bin:/bin
export PATH
LC_ALL=C
export LC_ALL
umask 077

PROGRAM=${0##*/}
PLATFORM=$(/usr/bin/uname -s)
PUBLISHER_UID=$(/usr/bin/id -u)
TEMP_COUNTER=0
LOCK_HELD=0
LOCK_PATH=
STAGE_PATH=
SELECTOR_TEMP=
AUTH_TEMP=

die() {
    error_type=$1
    shift
    printf '%s: %s\n' "$error_type" "$*" >&2
    exit 1
}

usage() {
    cat >&2 <<EOF
usage:
  $PROGRAM prepare <control-root> <publication-root>
  $PROGRAM promote-fixture <control-root> <publication-root> <candidate> [hold-after-lock|before-selector|after-selector]
  $PROGRAM promote <control-root> <publication-root> <bare-candidate> <release-tag-ref>
  $PROGRAM check <control-root> <publication-root>
  $PROGRAM resolve <control-root> <publication-root>
EOF
    exit 1
}

cleanup() {
    if [ -n "$SELECTOR_TEMP" ] && { [ -e "$SELECTOR_TEMP" ] || [ -L "$SELECTOR_TEMP" ]; }; then
        /bin/rm -f "$SELECTOR_TEMP"
    fi
    if [ -n "$STAGE_PATH" ] && [ -d "$STAGE_PATH" ]; then
        /bin/chmod -R u+w "$STAGE_PATH" 2>/dev/null || :
        /bin/rm -rf "$STAGE_PATH"
    fi
    if [ -n "$AUTH_TEMP" ] && [ -d "$AUTH_TEMP" ]; then
        /bin/chmod -R u+w "$AUTH_TEMP" 2>/dev/null || :
        /bin/rm -rf "$AUTH_TEMP"
    fi
    if [ "$LOCK_HELD" -eq 1 ] && [ -n "$LOCK_PATH" ]; then
        /bin/rmdir "$LOCK_PATH" 2>/dev/null || :
    fi
}

trap cleanup 0
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

reject_control_path() {
    checked_path=$1
    path_octets=$(printf '%s' "$checked_path" | /usr/bin/od -An -tu1) || return 0
    for path_octet in $path_octets; do
        case "$path_octet" in
        1|2|3|4|5|6|7|8|9|10|11|12|13|14|15|16|17|18|19|20|21|22|23|24|25|26|27|28|29|30|31|127)
            return 0
            ;;
        esac
    done
    return 1
}

validate_path_syntax() {
    checked_path=$1
    error_type=$2
    case "$checked_path" in
    /*) ;;
    *) die "$error_type" 'path must be absolute' ;;
    esac
    reject_control_path "$checked_path" &&
        die "$error_type" 'path contains a control character'
    case "$checked_path" in
    /|*//*|*/./*|*/../*|*/.|*/..)
        die "$error_type" 'path must be normalized and must not contain dot components'
        ;;
    esac
}

canonical_existing_dir() {
    checked_path=$1
    error_type=$2
    validate_path_syntax "$checked_path" "$error_type"
    [ ! -L "$checked_path" ] || die "$error_type" "symbolic-link directory: $checked_path"
    [ -d "$checked_path" ] || die "$error_type" "directory missing: $checked_path"
    physical=$(CDPATH= cd -P "$checked_path" 2>/dev/null && pwd -P) ||
        die "$error_type" "cannot resolve directory: $checked_path"
    [ "$physical" = "$checked_path" ] ||
        die "$error_type" "path contains a symbolic or non-canonical ancestor: $checked_path"
}

validate_new_root_path() {
    new_root_path=$1
    error_type=$2
    validate_path_syntax "$new_root_path" "$error_type"
    parent=${new_root_path%/*}
    name=${new_root_path##*/}
    [ -n "$parent" ] || parent=/
    [ -n "$name" ] || die "$error_type" 'root name is empty'
    canonical_existing_dir "$parent" "$error_type"
    expected="$parent/$name"
    [ "$expected" = "$new_root_path" ] ||
        die "$error_type" "root parent is non-canonical: $new_root_path"
    if [ -e "$new_root_path" ] || [ -L "$new_root_path" ]; then
        canonical_existing_dir "$new_root_path" "$error_type"
    fi
}

stat_owner_mode() {
    stat_path=$1
    case "$PLATFORM" in
    Darwin)
        stat_result=$(/usr/bin/stat -f '%u %Lp' "$stat_path" 2>/dev/null) || return 1
        ;;
    Linux)
        stat_result=$(/usr/bin/stat -c '%u %a' "$stat_path" 2>/dev/null) || return 1
        ;;
    *)
        die unsupported-platform "publisher mode validation is unavailable on $PLATFORM"
        ;;
    esac
    set -- $stat_result
    STAT_OWNER=${1:-}
    STAT_MODE=${2:-}
    [ -n "$STAT_OWNER" ] && [ -n "$STAT_MODE" ]
}

mode_is_group_or_other_writable() {
    checked_mode=$1
    while [ "${#checked_mode}" -gt 3 ]; do
        checked_mode=${checked_mode#?}
    done
    [ "${#checked_mode}" -eq 3 ] || return 0
    group_digit=${checked_mode#?}
    group_digit=${group_digit%?}
    other_digit=${checked_mode#??}
    case "$group_digit" in 2|3|6|7) return 0 ;; esac
    case "$other_digit" in 2|3|6|7) return 0 ;; esac
    return 1
}

validate_no_extended_acl() {
    checked_path=$1
    error_type=$2
    case "$PLATFORM" in
    Darwin)
        acl_listing=$(/bin/ls -ldeb "$checked_path" 2>/dev/null) ||
            die "$error_type" "cannot inspect ACLs on $checked_path"
        acl_entries=$(printf '%s\n' "$acl_listing" | /usr/bin/sed -n '2,$p') ||
            die "$error_type" "cannot parse ACLs on $checked_path"
        [ -z "$acl_entries" ] ||
            die "$error_type" "extended ACLs are not supported on managed path: $checked_path"
        ;;
    Linux)
        listing=$(/bin/ls -ld "$checked_path" 2>/dev/null) ||
            die "$error_type" "cannot inspect ACLs on $checked_path"
        set -- $listing
        case "${1:-}" in
        *+) die "$error_type" "extended ACLs are not supported on managed path: $checked_path" ;;
        esac
        ;;
    *)
        die unsupported-platform "ACL validation is unavailable on $PLATFORM"
        ;;
    esac
}

validate_acl_tree() (
    acl_tree_root=$1
    acl_tree_error=$2
    validate_no_extended_acl "$acl_tree_root" "$acl_tree_error"
    [ -d "$acl_tree_root" ] && [ ! -L "$acl_tree_root" ] || exit 0

    for acl_tree_entry in \
        "$acl_tree_root"/* \
        "$acl_tree_root"/.[!.]* \
        "$acl_tree_root"/..?*
    do
        if [ ! -e "$acl_tree_entry" ] && [ ! -L "$acl_tree_entry" ]; then
            continue
        fi
        validate_acl_tree "$acl_tree_entry" "$acl_tree_error" || exit 1
    done
)

validate_owned_dir() {
    checked_path=$1
    error_type=$2
    canonical_existing_dir "$checked_path" "$error_type"
    stat_owner_mode "$checked_path" || die "$error_type" "cannot stat $checked_path"
    [ "$STAT_OWNER" = "$PUBLISHER_UID" ] ||
        die "$error_type" "directory is not owned by the publisher uid: $checked_path"
    if mode_is_group_or_other_writable "$STAT_MODE"; then
        die "$error_type" "directory is group/other writable: $checked_path"
    fi
    validate_no_extended_acl "$checked_path" "$error_type"
}

validate_owned_regular() {
    checked_path=$1
    error_type=$2
    [ -f "$checked_path" ] && [ ! -L "$checked_path" ] ||
        die "$error_type" "managed file is missing or non-regular: $checked_path"
    stat_owner_mode "$checked_path" || die "$error_type" "cannot stat $checked_path"
    [ "$STAT_OWNER" = "$PUBLISHER_UID" ] ||
        die "$error_type" "file is not owned by the publisher uid: $checked_path"
    if mode_is_group_or_other_writable "$STAT_MODE"; then
        die "$error_type" "file is group/other writable: $checked_path"
    fi
    validate_no_extended_acl "$checked_path" "$error_type"
}

validate_protected_tree() {
    checked_tree=$1
    error_type=$2
    wrong_owner=$(
        /usr/bin/find "$checked_tree" ! -user "$PUBLISHER_UID" -exec /bin/echo x \; 2>/dev/null
    ) || die "$error_type" "cannot inspect ownership below $checked_tree"
    [ -z "$wrong_owner" ] || die "$error_type" "tree contains an entry not owned by the publisher uid: $checked_tree"
    case "$PLATFORM" in
    Darwin)
        writable=$(
            /usr/bin/find "$checked_tree" -perm +022 -exec /bin/echo x \; 2>/dev/null
        ) || die "$error_type" "cannot inspect modes below $checked_tree"
        ;;
    Linux)
        writable=$(
            /usr/bin/find "$checked_tree" -perm /022 -exec /bin/echo x \; 2>/dev/null
        ) || die "$error_type" "cannot inspect modes below $checked_tree"
        ;;
    *)
        die unsupported-platform "tree mode validation is unavailable on $PLATFORM"
        ;;
    esac
    [ -z "$writable" ] || die "$error_type" "tree contains a group/other-writable entry: $checked_tree"
    validate_acl_tree "$checked_tree" "$error_type" || exit 1
}

roots_do_not_overlap() {
    control_root=$1
    publication_root=$2
    [ "$control_root" != "$publication_root" ] ||
        die local-integrity 'control and publication roots must differ'
    case "$control_root/" in "$publication_root/"*)
        die local-integrity 'control root must not be below publication root'
        ;;
    esac
    case "$publication_root/" in "$control_root/"*)
        die local-integrity 'publication root must not be below control root'
        ;;
    esac
}

validate_layout() {
    control_root=$1
    publication_root=$2
    validate_owned_dir "$control_root" local-integrity
    validate_owned_dir "$publication_root" local-integrity
    roots_do_not_overlap "$control_root" "$publication_root"
    for managed_dir in checkout quarantine state locks hooks trust; do
        validate_owned_dir "$control_root/$managed_dir" local-integrity
    done
    validate_owned_dir "$publication_root/.staging" local-integrity
    validate_owned_dir "$publication_root/releases" local-integrity
}

next_temp_dir() {
    temp_parent=$1
    temp_prefix=$2
    while :; do
        TEMP_COUNTER=$((TEMP_COUNTER + 1))
        CREATED_TEMP="$temp_parent/.$temp_prefix.$$.$TEMP_COUNTER"
        if /bin/mkdir "$CREATED_TEMP" 2>/dev/null; then
            return 0
        fi
        [ "$TEMP_COUNTER" -lt 100 ] || die local-integrity 'cannot allocate a target-local temporary directory'
    done
}

atomic_write() {
    target=$1
    content=$2
    target_dir=${target%/*}
    next_temp_dir "$target_dir" state
    temp_dir=$CREATED_TEMP
    temp_file="$temp_dir/value"
    printf '%s\n' "$content" > "$temp_file" || {
        /bin/rm -rf "$temp_dir"
        die state-write-failed "cannot write temporary state for $target"
    }
    /bin/chmod 600 "$temp_file"
    if ! /bin/mv -f "$temp_file" "$target"; then
        /bin/rm -rf "$temp_dir"
        die state-write-failed "cannot replace $target"
    fi
    /bin/rmdir "$temp_dir"
}

reject_nul_bytes() {
    checked_file=$1
    error_type=$2
    file_octets=$(/usr/bin/od -An -tu1 "$checked_file" 2>/dev/null) ||
        die "$error_type" "cannot inspect bytes in $checked_file"
    for file_octet in $file_octets; do
        [ "$file_octet" != 0 ] || die "$error_type" "NUL byte in structured file: $checked_file"
    done
}

read_single_line() {
    line_file=$1
    [ -f "$line_file" ] && [ ! -L "$line_file" ] || return 1
    reject_nul_bytes "$line_file" local-integrity
    {
        IFS= read -r SINGLE_LINE || return 1
        extra_line=
        if IFS= read -r extra_line || [ -n "$extra_line" ]; then
            return 1
        fi
    } < "$line_file"
}

directory_is_empty() {
    checked_dir=$1
    found=$(
        /usr/bin/find "$checked_dir" ! -path "$checked_dir" -prune -exec /bin/echo x \; 2>/dev/null
    ) ||
        return 1
    [ -z "$found" ]
}

validate_sequence() {
    sequence=$1
    case "$sequence" in ''|*[!0-9]*) return 1 ;; esac
    case "$sequence" in 0|0*) return 1 ;; esac
    [ "${#sequence}" -le 18 ]
}

validate_digest() {
    digest=$1
    digest_length=${#digest}
    [ "$digest_length" -ge 32 ] && [ "$digest_length" -le 128 ] || return 1
    case "$digest" in *[!0-9a-f]*) return 1 ;; esac
    return 0
}

validate_git_oid() {
    oid=$1
    case "${#oid}" in 40|64) ;; *) return 1 ;; esac
    case "$oid" in *[!0-9a-f]*) return 1 ;; esac
    return 0
}

validate_repository_token() {
    repository_token=$1
    case "$repository_token" in
    ''|*[!A-Za-z0-9._/:@+-]*) return 1 ;;
    esac
    return 0
}

validate_signer_token() {
    signer_token=$1
    case "$signer_token" in
    ''|*[!A-Za-z0-9._@+-]*) return 1 ;;
    esac
    return 0
}

git_clean() {
    /usr/bin/env -i \
        PATH=/usr/bin:/bin \
        LC_ALL=C \
        HOME="$control_root" \
        GIT_CONFIG_GLOBAL=/dev/null \
        GIT_CONFIG_SYSTEM=/dev/null \
        GIT_CONFIG_NOSYSTEM=1 \
        GIT_ATTR_NOSYSTEM=1 \
        GIT_NO_REPLACE_OBJECTS=1 \
        GIT_NO_LAZY_FETCH=1 \
        /usr/bin/git \
        -c gpg.format=ssh \
        -c gpg.program=/usr/bin/false \
        -c gpg.openpgp.program=/usr/bin/false \
        -c gpg.x509.program=/usr/bin/false \
        -c gpg.ssh.program=/usr/bin/ssh-keygen \
        -c gpg.ssh.allowedSignersFile="$control_root/trust/release-signers" \
        -c gpg.ssh.revocationFile="$control_root/trust/release-revocations" \
        "$@"
}

validate_full_ref() {
    checked_ref=$1
    case "$checked_ref" in refs/*) ;; *) return 1 ;; esac
    git_clean check-ref-format "$checked_ref" >/dev/null 2>&1
}

validate_release_parts() {
    sequence=$1
    digest=$2
    publication_root=$3
    validate_sequence "$sequence" || return 1
    validate_digest "$digest" || return 1
    RELEASE_ID="r$sequence-$digest"
    name_max=$(/usr/bin/getconf NAME_MAX "$publication_root/releases" 2>/dev/null) || return 1
    case "$name_max" in ''|*[!0-9]*) return 1 ;; esac
    [ "${#RELEASE_ID}" -le "$name_max" ]
}

parse_release_id() {
    parsed_id=$1
    publication_root=$2
    case "$parsed_id" in r*-*) ;;
    *) return 1 ;;
    esac
    parsed_body=${parsed_id#r}
    parsed_sequence=${parsed_body%%-*}
    parsed_digest=${parsed_body#*-}
    validate_release_parts "$parsed_sequence" "$parsed_digest" "$publication_root" || return 1
    [ "$RELEASE_ID" = "$parsed_id" ] || return 1
    ID_SEQUENCE=$parsed_sequence
    ID_DIGEST=$parsed_digest
}

compare_sequences() {
    left=$1
    right=$2
    left_length=${#left}
    right_length=${#right}
    if [ "$left_length" -lt "$right_length" ]; then
        SEQUENCE_COMPARISON=-1
    elif [ "$left_length" -gt "$right_length" ]; then
        SEQUENCE_COMPARISON=1
    else
        SEQUENCE_COMPARISON=0
        left_rest=$left
        right_rest=$right
        while [ -n "$left_rest" ]; do
            left_tail=${left_rest#?}
            left_digit=${left_rest%"$left_tail"}
            right_tail=${right_rest#?}
            right_digit=${right_rest%"$right_tail"}
            if [ "$left_digit" -lt "$right_digit" ]; then
                SEQUENCE_COMPARISON=-1
                break
            elif [ "$left_digit" -gt "$right_digit" ]; then
                SEQUENCE_COMPARISON=1
                break
            fi
            left_rest=$left_tail
            right_rest=$right_tail
        done
    fi
}

parse_fixture() {
    candidate=$1
    publication_root=$2
    fixture="$candidate/authenticated.fixture"
    [ -f "$fixture" ] && [ ! -L "$fixture" ] ||
        die candidate-invalid 'authenticated.fixture must be a regular file'
    reject_nul_bytes "$fixture" candidate-invalid
    {
        IFS= read -r fixture_format || die candidate-invalid 'fixture descriptor is truncated'
        IFS= read -r fixture_sequence_line || die candidate-invalid 'fixture descriptor is truncated'
        IFS= read -r fixture_digest_line || die candidate-invalid 'fixture descriptor is truncated'
        fixture_extra=
        if IFS= read -r fixture_extra || [ -n "$fixture_extra" ]; then
            die candidate-invalid 'fixture descriptor has extra lines'
        fi
    } < "$fixture"
    [ "$fixture_format" = 'agent-knowledge-kit-authenticated-test-fixture-v1' ] ||
        die candidate-invalid 'fixture descriptor format is invalid'
    case "$fixture_sequence_line" in 'sequence '*) ;;
    *) die candidate-invalid 'fixture sequence field is invalid' ;;
    esac
    case "$fixture_digest_line" in 'digest '*) ;;
    *) die candidate-invalid 'fixture digest field is invalid' ;;
    esac
    candidate_sequence=${fixture_sequence_line#sequence }
    candidate_digest=${fixture_digest_line#digest }
    validate_release_parts "$candidate_sequence" "$candidate_digest" "$publication_root" ||
        die candidate-invalid 'fixture release identity is invalid'
    candidate_id=$RELEASE_ID
}

validate_tree_entries() {
    tree_root=$1
    error_type=$2
    [ -d "$tree_root" ] && [ ! -L "$tree_root" ] ||
        die "$error_type" "corpus directory is missing or symbolic: $tree_root"
    bad=$(/usr/bin/find "$tree_root" ! -type d ! -type f -exec /bin/echo x \; 2>/dev/null) ||
        die "$error_type" "cannot inspect corpus tree: $tree_root"
    [ -z "$bad" ] || die "$error_type" 'corpus contains a symbolic link or special file'
}

fixture_meta_content() {
    meta_id=$1
    parse_release_id "$meta_id" "$publication_root" || die local-integrity 'invalid release id'
    printf '%s\nrelease %s\nsequence %s\ndigest %s' \
        'agent-knowledge-kit-local-test-publication-v1' \
        "$meta_id" "$ID_SEQUENCE" "$ID_DIGEST"
}

production_meta_content() {
    meta_release=$1
    meta_repository=$2
    meta_ref=$3
    meta_tag=$4
    meta_sequence=$5
    meta_commit=$6
    meta_tree=$7
    meta_corpus_tree=$8
    meta_archive=$9
    printf '%s\nrelease %s\nrepository %s\nref %s\ntag %s\nsequence %s\ncommit %s\ntree %s\ncorpus-tree %s\narchive %s' \
        'agent-knowledge-kit-production-publication-v1' \
        "$meta_release" "$meta_repository" "$meta_ref" "$meta_tag" \
        "$meta_sequence" "$meta_commit" "$meta_tree" \
        "$meta_corpus_tree" "$meta_archive"
}

parse_production_meta() {
    production_meta=$1
    expected_id=$2
    reject_nul_bytes "$production_meta" local-integrity
    {
        IFS= read -r meta_format || die local-integrity 'production release metadata is truncated'
        IFS= read -r meta_release_line || die local-integrity 'production release metadata is truncated'
        IFS= read -r meta_repository_line || die local-integrity 'production release metadata is truncated'
        IFS= read -r meta_ref_line || die local-integrity 'production release metadata is truncated'
        IFS= read -r meta_tag_line || die local-integrity 'production release metadata is truncated'
        IFS= read -r meta_sequence_line || die local-integrity 'production release metadata is truncated'
        IFS= read -r meta_commit_line || die local-integrity 'production release metadata is truncated'
        IFS= read -r meta_tree_line || die local-integrity 'production release metadata is truncated'
        IFS= read -r meta_corpus_tree_line || die local-integrity 'production release metadata is truncated'
        IFS= read -r meta_archive_line || die local-integrity 'production release metadata is truncated'
        meta_extra=
        if IFS= read -r meta_extra || [ -n "$meta_extra" ]; then
            die local-integrity 'production release metadata has extra lines'
        fi
    } < "$production_meta"
    [ "$meta_format" = 'agent-knowledge-kit-production-publication-v1' ] ||
        die local-integrity 'production release metadata format is invalid'
    case "$meta_release_line" in 'release '*) ;; *) die local-integrity 'production release field is invalid' ;; esac
    case "$meta_repository_line" in 'repository '*) ;; *) die local-integrity 'production repository field is invalid' ;; esac
    case "$meta_ref_line" in 'ref '*) ;; *) die local-integrity 'production ref field is invalid' ;; esac
    case "$meta_tag_line" in 'tag '*) ;; *) die local-integrity 'production tag field is invalid' ;; esac
    case "$meta_sequence_line" in 'sequence '*) ;; *) die local-integrity 'production sequence field is invalid' ;; esac
    case "$meta_commit_line" in 'commit '*) ;; *) die local-integrity 'production commit field is invalid' ;; esac
    case "$meta_tree_line" in 'tree '*) ;; *) die local-integrity 'production tree field is invalid' ;; esac
    case "$meta_corpus_tree_line" in 'corpus-tree '*) ;; *) die local-integrity 'production corpus-tree field is invalid' ;; esac
    case "$meta_archive_line" in 'archive '*) ;; *) die local-integrity 'production archive field is invalid' ;; esac

    META_RELEASE=${meta_release_line#release }
    META_REPOSITORY=${meta_repository_line#repository }
    META_REF=${meta_ref_line#ref }
    META_TAG=${meta_tag_line#tag }
    META_SEQUENCE=${meta_sequence_line#sequence }
    META_COMMIT=${meta_commit_line#commit }
    META_TREE=${meta_tree_line#tree }
    META_CORPUS_TREE=${meta_corpus_tree_line#corpus-tree }
    META_ARCHIVE=${meta_archive_line#archive }

    [ "$META_RELEASE" = "$expected_id" ] || die local-integrity 'production release metadata identity is invalid'
    parse_release_id "$META_RELEASE" "$publication_root" || die local-integrity 'production release metadata identity is invalid'
    [ "$META_SEQUENCE" = "$ID_SEQUENCE" ] && [ "$META_ARCHIVE" = "$ID_DIGEST" ] ||
        die local-integrity 'production release metadata does not bind its release id'
    validate_repository_token "$META_REPOSITORY" || die local-integrity 'production repository field is invalid'
    validate_full_ref "$META_REF" || die local-integrity 'production ref field is invalid'
    validate_full_ref "$META_TAG" || die local-integrity 'production tag field is invalid'
    validate_sequence "$META_SEQUENCE" || die local-integrity 'production sequence field is invalid'
    validate_git_oid "$META_COMMIT" || die local-integrity 'production commit field is invalid'
    validate_git_oid "$META_TREE" || die local-integrity 'production tree field is invalid'
    validate_git_oid "$META_CORPUS_TREE" || die local-integrity 'production corpus-tree field is invalid'
    validate_git_oid "$META_ARCHIVE" || die local-integrity 'production archive field is invalid'

    expected_meta_with_sentinel=$(
        production_meta_content "$META_RELEASE" "$META_REPOSITORY" \
            "$META_REF" "$META_TAG" "$META_SEQUENCE" "$META_COMMIT" \
            "$META_TREE" "$META_CORPUS_TREE" "$META_ARCHIVE"
        printf '\n.akk-meta-sentinel'
    )
    actual_meta_with_sentinel=$(
        /bin/cat "$production_meta" && printf '.akk-meta-sentinel'
    ) || die local-integrity 'cannot read production release metadata'
    [ "$actual_meta_with_sentinel" = "$expected_meta_with_sentinel" ] ||
        die local-integrity 'production release metadata is not byte-exact'
}

validate_release_dir() {
    release_dir=$1
    expected_id=$2
    [ -d "$release_dir" ] && [ ! -L "$release_dir" ] ||
        die local-integrity "selected release is not a physical directory: $expected_id"
    physical=$(CDPATH= cd -P "$release_dir" 2>/dev/null && pwd -P) ||
        die local-integrity "cannot resolve selected release: $expected_id"
    [ "$physical" = "$publication_root/releases/$expected_id" ] ||
        die local-integrity 'selected release is not an immediate physical child of releases'
    validate_owned_dir "$release_dir" local-integrity
    validate_owned_dir "$release_dir/corpus" local-integrity
    validate_tree_entries "$release_dir" local-integrity
    unexpected_root_entry=$(
        /usr/bin/find "$release_dir" \( ! -path "$release_dir" -prune \) \
            ! -path "$release_dir/corpus" ! -path "$release_dir/release.meta" \
            -exec /bin/echo x \; 2>/dev/null
    ) || die local-integrity 'cannot validate selected release layout'
    [ -z "$unexpected_root_entry" ] ||
        die local-integrity 'selected release contains an unexpected root entry'
    meta_file="$release_dir/release.meta"
    validate_owned_regular "$meta_file" local-integrity
    validate_protected_tree "$release_dir" local-integrity
    reject_nul_bytes "$meta_file" local-integrity
    IFS= read -r selected_meta_format < "$meta_file" ||
        die local-integrity 'selected release metadata is empty'
    case "$selected_meta_format" in
    agent-knowledge-kit-local-test-publication-v1)
        expected_meta_with_sentinel=$(
            fixture_meta_content "$expected_id"
            printf '\n.akk-meta-sentinel'
        )
        actual_meta_with_sentinel=$(
            /bin/cat "$meta_file" && printf '.akk-meta-sentinel'
        ) || die local-integrity 'cannot read selected release metadata'
        [ "$actual_meta_with_sentinel" = "$expected_meta_with_sentinel" ] ||
            die local-integrity 'selected release metadata does not match its directory byte-for-byte'
        ;;
    agent-knowledge-kit-production-publication-v1)
        parse_production_meta "$meta_file" "$expected_id"
        ;;
    *)
        die local-integrity 'selected release metadata format is invalid'
        ;;
    esac
}

read_selector() {
    current="$publication_root/current"
    [ -L "$current" ] || die local-integrity 'current selector is missing or is not a symbolic link'
    raw_with_sentinel=$(
        /usr/bin/readlink -n "$current" && printf '.akk-selector-sentinel'
    ) || die local-integrity 'current selector output is malformed'
    case "$raw_with_sentinel" in
    *.akk-selector-sentinel) ;;
    *) die local-integrity 'current selector output is malformed' ;;
    esac
    raw_target=${raw_with_sentinel%.akk-selector-sentinel}
    case "$raw_target" in releases/*) ;;
    *) die local-integrity 'current selector target must be releases/<release-id>' ;;
    esac
    selector_id=${raw_target#releases/}
    case "$selector_id" in */*|'') die local-integrity 'current selector is nested or empty' ;; esac
    parse_release_id "$selector_id" "$publication_root" || die local-integrity 'current selector release id is invalid'
    [ "$raw_target" = "releases/$selector_id" ] || die local-integrity 'current selector target is not canonical'
    validate_release_dir "$publication_root/releases/$selector_id" "$selector_id"
    ACTIVE_ID=$selector_id
    ACTIVE_SEQUENCE=$ID_SEQUENCE
}

state_content() {
    state_id=$1
    printf '%s\nselected %s\nwatermark %s' \
        'agent-knowledge-kit-local-publication-state-v1' "$state_id" "$state_id"
}

read_publication_state() {
    state_file="$control_root/state/publication"
    validate_owned_regular "$state_file" local-integrity
    reject_nul_bytes "$state_file" local-integrity
    {
        IFS= read -r state_format || die local-integrity 'publication state is truncated'
        IFS= read -r selected_line || die local-integrity 'publication state is truncated'
        IFS= read -r watermark_line || die local-integrity 'publication state is truncated'
        state_extra=
        if IFS= read -r state_extra || [ -n "$state_extra" ]; then
            die local-integrity 'publication state has extra lines'
        fi
    } < "$state_file"
    [ "$state_format" = 'agent-knowledge-kit-local-publication-state-v1' ] ||
        die local-integrity 'publication state format is invalid'
    case "$selected_line" in 'selected '*) ;;
    *) die local-integrity 'selected-release state is invalid' ;;
    esac
    case "$watermark_line" in 'watermark '*) ;;
    *) die local-integrity 'watermark state is invalid' ;;
    esac
    STATE_SELECTED=${selected_line#selected }
    STATE_WATERMARK=${watermark_line#watermark }
    parse_release_id "$STATE_SELECTED" "$publication_root" || die local-integrity 'selected-release id is invalid'
    STATE_SEQUENCE=$ID_SEQUENCE
    parse_release_id "$STATE_WATERMARK" "$publication_root" || die local-integrity 'watermark id is invalid'
    [ "$STATE_SELECTED" = "$STATE_WATERMARK" ] || die local-integrity 'selected release and watermark disagree'
}

read_installation() {
    installation_file="$control_root/state/installation"
    validate_owned_regular "$installation_file" local-integrity
    read_single_line "$installation_file" || die local-integrity 'installation state is missing or malformed'
    INSTALLATION_STATE=$SINGLE_LINE
    case "$INSTALLATION_STATE" in never-initialized|initialized) ;;
    *) die local-integrity 'installation state value is invalid' ;;
    esac
}

check_clean_bootstrap() {
    if [ -e "$publication_root/current" ] || [ -L "$publication_root/current" ]; then
        die local-integrity 'never-initialized root has a selector'
    fi
    [ ! -e "$control_root/state/publication" ] && [ ! -L "$control_root/state/publication" ] ||
        die local-integrity 'never-initialized root has publication state'
    directory_is_empty "$publication_root/releases" ||
        die local-integrity 'never-initialized root has release residue'
}

check_integrity() {
    validate_layout "$control_root" "$publication_root"
    read_installation
    if [ "$INSTALLATION_STATE" = never-initialized ]; then
        check_clean_bootstrap
        CHECK_RESULT=never-initialized
        return 0
    fi
    read_selector
    read_publication_state
    [ "$STATE_SELECTED" = "$ACTIVE_ID" ] || die local-integrity 'selector and publication state disagree'
    CHECK_RESULT=$ACTIVE_ID
}

acquire_lock() {
    LOCK_PATH="$control_root/locks/publication"
    attempt=0
    while ! /bin/mkdir "$LOCK_PATH" 2>/dev/null; do
        attempt=$((attempt + 1))
        [ "$attempt" -lt 10 ] ||
            die publication-locked 'publication mutex is busy; orphaned locks are never reclaimed by age or PID'
        /bin/sleep 1
    done
    LOCK_HELD=1
}

release_lock() {
    [ "$LOCK_HELD" -eq 1 ] || return 0
    /bin/rmdir "$LOCK_PATH" || die local-integrity 'cannot release publication mutex'
    LOCK_HELD=0
}

transaction_state() {
    read_installation
    if [ "$INSTALLATION_STATE" = never-initialized ]; then
        check_clean_bootstrap
        TRANSACTION_BOOTSTRAP=1
        ACTIVE_ID=
        ACTIVE_SEQUENCE=
        return 0
    fi

    TRANSACTION_BOOTSTRAP=0
    read_selector
    read_publication_state
    parse_release_id "$ACTIVE_ID" "$publication_root" || die local-integrity 'active release id is invalid'
    active_sequence=$ID_SEQUENCE
    parse_release_id "$STATE_WATERMARK" "$publication_root" || die local-integrity 'watermark id is invalid'
    state_sequence=$ID_SEQUENCE
    compare_sequences "$state_sequence" "$active_sequence"
    if [ "$SEQUENCE_COMPARISON" -gt 0 ]; then
        die local-integrity 'watermark is ahead of current; refusing to lower it'
    elif [ "$SEQUENCE_COMPARISON" -eq 0 ] && [ "$STATE_WATERMARK" != "$ACTIVE_ID" ]; then
        die local-integrity 'equal-sequence selector/state identity conflict'
    elif [ "$SEQUENCE_COMPARISON" -lt 0 ]; then
        # The only automatic repair: a valid selector proves a completed
        # post-selector transaction whose atomic state write did not land.
        atomic_write "$control_root/state/publication" "$(state_content "$ACTIVE_ID")"
        STATE_SELECTED=$ACTIVE_ID
        STATE_WATERMARK=$ACTIVE_ID
    fi
    ACTIVE_SEQUENCE=$active_sequence
}

candidate_matches_release() {
    candidate=$1
    release_dir=$2
    expected_id=$3
    validate_release_dir "$release_dir" "$expected_id"
    /usr/bin/diff -r "$candidate/corpus" "$release_dir/corpus" >/dev/null 2>&1
}

freeze_stage() {
    stage=$1
    /usr/bin/find "$stage" -type f -exec /bin/chmod 0444 {} + ||
        die candidate-invalid 'cannot freeze staged files'
    /usr/bin/find "$stage" -type d -exec /bin/chmod 0555 {} + ||
        die candidate-invalid 'cannot freeze staged directories'
    # Darwin refuses to rename a directory whose owner-write bit is clear.
    # The release root remains 0755: writable by the distinct publisher only,
    # while the agent principal has read/traverse access and cannot mutate it.
    /bin/chmod 0755 "$stage" || die candidate-invalid 'cannot finalize staged release root mode'
}

replace_selector() {
    release_id=$1
    next_temp_dir "$publication_root" current
    selector_dir=$CREATED_TEMP
    /bin/rmdir "$selector_dir"
    SELECTOR_TEMP=$selector_dir
    /bin/ln -s "releases/$release_id" "$SELECTOR_TEMP" ||
        die local-integrity 'cannot create temporary selector'
    case "$PLATFORM" in
    Darwin)
        /bin/mv -fh "$SELECTOR_TEMP" "$publication_root/current" ||
            die local-integrity 'cannot atomically replace current selector'
        ;;
    Linux)
        /bin/mv -Tf "$SELECTOR_TEMP" "$publication_root/current" ||
            die local-integrity 'cannot atomically replace current selector'
        ;;
    *)
        die unsupported-platform "atomic selector replacement is unavailable on $PLATFORM"
        ;;
    esac
    SELECTOR_TEMP=
}

prepare_roots() {
    control_root=$1
    publication_root=$2
    validate_new_root_path "$control_root" local-integrity
    validate_new_root_path "$publication_root" local-integrity
    roots_do_not_overlap "$control_root" "$publication_root"

    if [ ! -d "$control_root" ]; then
        /bin/mkdir "$control_root"
        /bin/chmod 0700 "$control_root"
    fi
    if [ ! -d "$publication_root" ]; then
        /bin/mkdir "$publication_root"
        /bin/chmod 0755 "$publication_root"
    fi
    validate_owned_dir "$control_root" local-integrity
    validate_owned_dir "$publication_root" local-integrity

    for managed_dir in checkout quarantine state locks hooks trust; do
        if [ ! -d "$control_root/$managed_dir" ]; then
            /bin/mkdir "$control_root/$managed_dir"
            /bin/chmod 0700 "$control_root/$managed_dir"
        fi
    done
    if [ ! -d "$publication_root/.staging" ]; then
        /bin/mkdir "$publication_root/.staging"
        /bin/chmod 0700 "$publication_root/.staging"
    fi
    if [ ! -d "$publication_root/releases" ]; then
        /bin/mkdir "$publication_root/releases"
        /bin/chmod 0755 "$publication_root/releases"
    fi
    validate_layout "$control_root" "$publication_root"

    installation="$control_root/state/installation"
    if [ ! -e "$installation" ] && [ ! -L "$installation" ]; then
        [ ! -e "$control_root/state/publication" ] ||
            die local-integrity 'unprepared root has publication state residue'
        [ ! -e "$publication_root/current" ] && [ ! -L "$publication_root/current" ] ||
            die local-integrity 'unprepared root has selector residue'
        directory_is_empty "$publication_root/releases" ||
            die local-integrity 'unprepared root has release residue'
        atomic_write "$installation" never-initialized
    fi
    check_integrity
    printf '%s\n' "$CHECK_RESULT"
}

validate_trust_file() {
    trust_file=$1
    validate_owned_regular "$trust_file" local-integrity
    stat_owner_mode "$trust_file" || die local-integrity "cannot stat $trust_file"
    [ "$STAT_MODE" = 600 ] || die local-integrity "trust file mode must be 0600: $trust_file"
    reject_nul_bytes "$trust_file" local-integrity
}

read_release_policy() {
    policy_file="$control_root/trust/release-policy"
    signers_file="$control_root/trust/release-signers"
    revocations_file="$control_root/trust/release-revocations"
    validate_trust_file "$policy_file"
    validate_trust_file "$signers_file"
    validate_trust_file "$revocations_file"

    {
        IFS= read -r policy_format || die local-integrity 'release policy is truncated'
        IFS= read -r policy_repository_line || die local-integrity 'release policy is truncated'
        IFS= read -r policy_ref_line || die local-integrity 'release policy is truncated'
        IFS= read -r policy_tag_prefix_line || die local-integrity 'release policy is truncated'
        IFS= read -r policy_signer_line || die local-integrity 'release policy is truncated'
        policy_extra=
        if IFS= read -r policy_extra || [ -n "$policy_extra" ]; then
            die local-integrity 'release policy has extra lines'
        fi
    } < "$policy_file"
    [ "$policy_format" = 'agent-knowledge-kit-corpus-release-policy-v1' ] ||
        die local-integrity 'release policy format is invalid'
    case "$policy_repository_line" in 'repository '*) ;; *) die local-integrity 'release repository policy is invalid' ;; esac
    case "$policy_ref_line" in 'ref '*) ;; *) die local-integrity 'release ref policy is invalid' ;; esac
    case "$policy_tag_prefix_line" in 'tag-prefix '*) ;; *) die local-integrity 'release tag-prefix policy is invalid' ;; esac
    case "$policy_signer_line" in 'signer '*) ;; *) die local-integrity 'release signer policy is invalid' ;; esac

    POLICY_REPOSITORY=${policy_repository_line#repository }
    POLICY_REF=${policy_ref_line#ref }
    POLICY_TAG_PREFIX=${policy_tag_prefix_line#tag-prefix }
    POLICY_SIGNER=${policy_signer_line#signer }
    validate_repository_token "$POLICY_REPOSITORY" || die local-integrity 'release repository policy is invalid'
    case "$POLICY_REF" in refs/heads/*) ;; *) die local-integrity 'release ref policy must name a branch' ;; esac
    validate_full_ref "$POLICY_REF" || die local-integrity 'release ref policy is invalid'
    case "$POLICY_TAG_PREFIX" in refs/tags/*/) ;; *) die local-integrity 'release tag-prefix policy is invalid' ;; esac
    tag_prefix_probe="${POLICY_TAG_PREFIX}r1"
    validate_full_ref "$tag_prefix_probe" || die local-integrity 'release tag-prefix policy is invalid'
    validate_signer_token "$POLICY_SIGNER" || die local-integrity 'release signer policy is invalid'

    {
        IFS= read -r signer_line || die local-integrity 'release signer file is empty'
        signer_extra=
        if IFS= read -r signer_extra || [ -n "$signer_extra" ]; then
            die local-integrity 'release signer file must contain exactly one signer'
        fi
    } < "$signers_file"
    signer_principal=
    signer_algorithm=
    signer_key=
    signer_trailing=
    IFS=' 	' read -r signer_principal signer_algorithm signer_key signer_trailing <<EOF
$signer_line
EOF
    [ -z "$signer_trailing" ] && [ "$signer_principal" = "$POLICY_SIGNER" ] &&
        [ "$signer_algorithm" = ssh-ed25519 ] ||
        die local-integrity 'release signer entry does not match policy'
    case "$signer_key" in ''|*[!A-Za-z0-9+/=]*) die local-integrity 'release signer key is invalid' ;; esac

    while IFS= read -r revocation_line || [ -n "$revocation_line" ]; do
        [ -n "$revocation_line" ] || die local-integrity 'release revocation file contains a blank line'
        revocation_algorithm=
        revocation_key=
        revocation_trailing=
        IFS=' 	' read -r revocation_algorithm revocation_key revocation_trailing <<EOF
$revocation_line
EOF
        [ -z "$revocation_trailing" ] && [ "$revocation_algorithm" = ssh-ed25519 ] ||
            die local-integrity 'release revocation entry is invalid'
        case "$revocation_key" in ''|*[!A-Za-z0-9+/=]*) die local-integrity 'release revocation key is invalid' ;; esac
    done < "$revocations_file"
}

validate_release_tag_ref() {
    checked_tag_ref=$1
    validate_full_ref "$checked_tag_ref" || die authentication-failed 'release tag ref is invalid'
    case "$checked_tag_ref" in
    "$POLICY_TAG_PREFIX"r*) ;;
    *) die authentication-failed 'release tag does not match the protected prefix' ;;
    esac
    tag_sequence=${checked_tag_ref#"$POLICY_TAG_PREFIX"r}
    validate_sequence "$tag_sequence" || die authentication-failed 'release tag sequence is invalid'
    case "$tag_sequence" in */*) die authentication-failed 'release tag sequence is nested' ;; esac
    release_tag_short=${checked_tag_ref#refs/tags/}
}

validate_bare_candidate() {
    bare_candidate=$1
    canonical_existing_dir "$bare_candidate" candidate-invalid
    [ "${bare_candidate%/*}" = "$control_root/quarantine" ] ||
        die candidate-invalid 'production candidate must be an immediate child of quarantine'
    validate_owned_dir "$bare_candidate" candidate-invalid
    validate_tree_entries "$bare_candidate" candidate-invalid
    validate_protected_tree "$bare_candidate" candidate-invalid
    [ ! -e "$bare_candidate/objects/info/alternates" ] &&
        [ ! -L "$bare_candidate/objects/info/alternates" ] ||
        die candidate-invalid 'production candidate may not use alternate object storage'
    [ ! -e "$bare_candidate/info/grafts" ] && [ ! -L "$bare_candidate/info/grafts" ] ||
        die candidate-invalid 'production candidate may not use grafted history'
    [ ! -e "$bare_candidate/shallow" ] && [ ! -L "$bare_candidate/shallow" ] ||
        die candidate-invalid 'production candidate may not use shallow history'
    bare_result=$(git_clean --git-dir="$bare_candidate" rev-parse --is-bare-repository 2>/dev/null) ||
        die candidate-invalid 'production candidate is not a readable Git repository'
    [ "$bare_result" = true ] || die candidate-invalid 'production candidate must be a bare Git repository'
    partial_clone_rc=0
    partial_clone_config=$(git_clean --git-dir="$bare_candidate" config --local --get-regexp \
        '^(extensions\.partialclone|remote\..*\.(promisor|partialclonefilter))$' 2>/dev/null) ||
        partial_clone_rc=$?
    case "$partial_clone_rc" in
    0) die candidate-invalid 'production candidate may not use partial-clone object retrieval' ;;
    1) ;;
    *) die candidate-invalid 'production candidate configuration is unreadable' ;;
    esac
    [ -z "$partial_clone_config" ] ||
        die candidate-invalid 'production candidate may not use partial-clone object retrieval'
}

authenticate_candidate() {
    candidate=$1
    release_tag=$2
    validate_bare_candidate "$candidate"
    read_release_policy
    validate_release_tag_ref "$release_tag"

    tag_oid=$(git_clean --git-dir="$candidate" show-ref --verify --hash "$release_tag" 2>/dev/null) ||
        die authentication-failed 'release tag is missing from the candidate repository'
    validate_git_oid "$tag_oid" || die authentication-failed 'release tag object id is invalid'
    tag_type=$(git_clean --git-dir="$candidate" cat-file -t "$tag_oid" 2>/dev/null) ||
        die authentication-failed 'release tag object is unreadable'
    [ "$tag_type" = tag ] || die authentication-failed 'release ref must point to an annotated tag'

    next_temp_dir "$control_root/quarantine" authentication
    AUTH_TEMP=$CREATED_TEMP
    tag_object="$AUTH_TEMP/tag-object"
    git_clean --git-dir="$candidate" cat-file tag "$tag_oid" > "$tag_object" 2>/dev/null ||
        die authentication-failed 'release tag object is unreadable'
    reject_nul_bytes "$tag_object" authentication-failed
    tag_object_line_1=$(/usr/bin/sed -n '1p' "$tag_object")
    tag_object_line_2=$(/usr/bin/sed -n '2p' "$tag_object")
    tag_object_line_3=$(/usr/bin/sed -n '3p' "$tag_object")
    tag_object_line_4=$(/usr/bin/sed -n '4p' "$tag_object")
    tag_object_line_5=$(/usr/bin/sed -n '5p' "$tag_object")
    tag_object_line_14=$(/usr/bin/sed -n '14p' "$tag_object")
    case "$tag_object_line_1" in 'object '*) ;; *) die authentication-failed 'release tag object header is invalid' ;; esac
    release_commit=${tag_object_line_1#object }
    validate_git_oid "$release_commit" || die authentication-failed 'release commit object id is invalid'
    [ "$tag_object_line_2" = 'type commit' ] || die authentication-failed 'release tag must target a commit'
    [ "$tag_object_line_3" = "tag $release_tag_short" ] || die authentication-failed 'release tag object name is invalid'
    case "$tag_object_line_4" in 'tagger '*) ;; *) die authentication-failed 'release tagger header is invalid' ;; esac
    [ -z "$tag_object_line_5" ] || die authentication-failed 'release tag object header is not strict'
    [ "$tag_object_line_14" = '-----BEGIN SSH SIGNATURE-----' ] ||
        die authentication-failed 'release manifest does not have the exact v1 shape'
    [ "$(/usr/bin/tail -n 1 "$tag_object")" = '-----END SSH SIGNATURE-----' ] ||
        die authentication-failed 'release tag SSH signature block is malformed'

    git_clean --git-dir="$candidate" verify-tag --raw "$tag_oid" >/dev/null 2>&1 ||
        die authentication-failed 'release tag signature is missing, unauthorized, or revoked'
    commit_type=$(git_clean --git-dir="$candidate" cat-file -t "$release_commit" 2>/dev/null) ||
        die authentication-failed 'release commit object is unreadable'
    [ "$commit_type" = commit ] || die authentication-failed 'release tag target is not a commit'
    commit_object="$AUTH_TEMP/commit-object"
    git_clean --git-dir="$candidate" cat-file commit "$release_commit" > "$commit_object" 2>/dev/null ||
        die authentication-failed 'release commit object is unreadable'
    reject_nul_bytes "$commit_object" authentication-failed
    commit_signature_header_count=$(
        /usr/bin/sed -n '/^gpgsig /p' "$commit_object" | /usr/bin/wc -l | /usr/bin/tr -d ' '
    ) || die authentication-failed 'cannot inspect release commit signature'
    if [ "$commit_signature_header_count" != 1 ] ||
        ! /usr/bin/grep -q '^gpgsig -----BEGIN SSH SIGNATURE-----$' "$commit_object"; then
        die authentication-failed 'release commit must contain exactly one SSH signature'
    fi
    git_clean --git-dir="$candidate" verify-commit --raw "$release_commit" >/dev/null 2>&1 ||
        die authentication-failed 'release commit signature is missing, unauthorized, or revoked'

    policy_ref_oid=$(git_clean --git-dir="$candidate" show-ref --verify --hash "$POLICY_REF" 2>/dev/null) ||
        die authentication-failed 'protected release branch is missing'
    validate_git_oid "$policy_ref_oid" || die authentication-failed 'protected release branch object id is invalid'
    [ "$(git_clean --git-dir="$candidate" cat-file -t "$policy_ref_oid" 2>/dev/null)" = commit ] ||
        die authentication-failed 'protected release branch does not point to a commit'
    git_clean --git-dir="$candidate" merge-base --is-ancestor "$release_commit" "$policy_ref_oid" >/dev/null 2>&1 ||
        die authentication-failed 'release commit is not reachable from the protected branch'

    release_tree=$(git_clean --git-dir="$candidate" rev-parse "$release_commit^{tree}" 2>/dev/null) ||
        die authentication-failed 'release root tree is unreadable'
    release_corpus_tree=$(git_clean --git-dir="$candidate" rev-parse "$release_commit:corpus" 2>/dev/null) ||
        die authentication-failed 'release corpus tree is missing'
    if ! validate_git_oid "$release_tree" || ! validate_git_oid "$release_corpus_tree"; then
        die authentication-failed 'release tree object id is invalid'
    fi
    [ "$(git_clean --git-dir="$candidate" cat-file -t "$release_corpus_tree" 2>/dev/null)" = tree ] ||
        die authentication-failed 'release corpus object is not a tree'
    unsafe_modes=$(git_clean --git-dir="$candidate" ls-tree -r --format='%(objectmode)' "$release_corpus_tree" 2>/dev/null |
        /usr/bin/sed '/^100644$/d; /^100755$/d') ||
        die candidate-invalid 'cannot inspect release corpus modes'
    [ -z "$unsafe_modes" ] || die candidate-invalid 'release corpus contains a symbolic link, gitlink, or special mode'
    kernel_entry=$(git_clean --git-dir="$candidate" ls-tree "$release_commit" -- corpus/kernel/kernel.md 2>/dev/null) ||
        die candidate-invalid 'cannot inspect production kernel entry'
    kernel_mode=
    kernel_type=
    kernel_oid=
    kernel_path=
    IFS=' 	' read -r kernel_mode kernel_type kernel_oid kernel_path <<EOF
$kernel_entry
EOF
    { [ "$kernel_mode" = 100644 ] || [ "$kernel_mode" = 100755 ]; } &&
        [ "$kernel_type" = blob ] && [ -n "$kernel_oid" ] &&
        [ "$kernel_path" = corpus/kernel/kernel.md ] ||
        die candidate-invalid 'production corpus kernel must be a regular Git blob'

    AUTH_ARCHIVE="$AUTH_TEMP/corpus.tar"
    git_clean --git-dir="$candidate" archive --format=tar --output="$AUTH_ARCHIVE" \
        "$release_commit" corpus 2>/dev/null ||
        die authentication-failed 'cannot materialize the authenticated corpus archive'
    release_archive=$(git_clean --git-dir="$candidate" hash-object "$AUTH_ARCHIVE" 2>/dev/null) ||
        die authentication-failed 'cannot identify the authenticated corpus archive'
    validate_git_oid "$release_archive" || die authentication-failed 'release archive object id is invalid'

    expected_manifest="$AUTH_TEMP/expected-manifest"
    actual_manifest="$AUTH_TEMP/actual-manifest"
    {
        printf '%s\n' 'agent-knowledge-kit-corpus-release-v1'
        printf 'repository %s\n' "$POLICY_REPOSITORY"
        printf 'ref %s\n' "$POLICY_REF"
        printf 'sequence %s\n' "$tag_sequence"
        printf 'commit %s\n' "$release_commit"
        printf 'tree %s\n' "$release_tree"
        printf 'corpus-tree %s\n' "$release_corpus_tree"
        printf 'archive %s\n' "$release_archive"
    } > "$expected_manifest"
    /usr/bin/sed '1,/^$/d; /^-----BEGIN SSH SIGNATURE-----$/,$d' \
        "$tag_object" > "$actual_manifest" ||
        die authentication-failed 'cannot parse the release manifest'
    /usr/bin/diff "$expected_manifest" "$actual_manifest" >/dev/null 2>&1 ||
        die authentication-failed 'release manifest does not match protected policy and Git objects'

    validate_release_parts "$tag_sequence" "$release_archive" "$publication_root" ||
        die authentication-failed 'authenticated release identity is invalid'
    promotion_id=$RELEASE_ID
    promotion_sequence=$tag_sequence
    promotion_meta=$(production_meta_content "$promotion_id" "$POLICY_REPOSITORY" \
        "$POLICY_REF" "$release_tag" "$promotion_sequence" "$release_commit" \
        "$release_tree" "$release_corpus_tree" "$release_archive")
}

promote_fixture() {
    control_root=$1
    publication_root=$2
    candidate=$3
    failpoint=${4:-}
    case "$failpoint" in ''|hold-after-lock|before-selector|after-selector) ;;
    *) die candidate-invalid 'unknown fixture failpoint' ;;
    esac

    validate_layout "$control_root" "$publication_root"
    canonical_existing_dir "$candidate" candidate-invalid
    case "$candidate/" in "$control_root/quarantine/"*) ;;
    *) die candidate-invalid 'candidate must be below the protected quarantine directory' ;;
    esac
    [ "$candidate" != "$control_root/quarantine" ] ||
        die candidate-invalid 'candidate must be a child below quarantine, not quarantine itself'
    validate_owned_dir "$candidate" candidate-invalid
    validate_tree_entries "$candidate/corpus" candidate-invalid
    parse_fixture "$candidate" "$publication_root"
    promotion_id=$candidate_id
    promotion_sequence=$candidate_sequence

    acquire_lock
    if [ "$failpoint" = hold-after-lock ]; then
        # Deterministic test-only signal/lock handshake. This is an explicit
        # fixture verb argument, never an environment-controlled failpoint.
        while :; do :; done
    fi
    # Re-resolve the candidate and final selector/state only after the mutex.
    canonical_existing_dir "$candidate" candidate-invalid
    validate_tree_entries "$candidate/corpus" candidate-invalid
    parse_fixture "$candidate" "$publication_root"
    [ "$candidate_id" = "$promotion_id" ] || die candidate-invalid 'candidate identity changed before publication'
    transaction_state

    if [ "$TRANSACTION_BOOTSTRAP" -eq 0 ]; then
        compare_sequences "$promotion_sequence" "$ACTIVE_SEQUENCE"
        if [ "$SEQUENCE_COMPARISON" -lt 0 ]; then
            die rollback "candidate $promotion_id is older than $ACTIVE_ID"
        elif [ "$SEQUENCE_COMPARISON" -eq 0 ] && [ "$promotion_id" != "$ACTIVE_ID" ]; then
            die sequence-equivocation "candidate $promotion_id conflicts with $ACTIVE_ID"
        fi
    fi

    release_dir="$publication_root/releases/$promotion_id"
    if [ -e "$release_dir" ] || [ -L "$release_dir" ]; then
        [ -d "$release_dir" ] && [ ! -L "$release_dir" ] ||
            die candidate-invalid 'reused release id has the wrong file type'
        candidate_matches_release "$candidate" "$release_dir" "$promotion_id" ||
            die candidate-invalid 'reused release id has different bytes or metadata'
    else
        next_temp_dir "$publication_root/.staging" release
        STAGE_PATH=$CREATED_TEMP
        if ! /bin/cp -RP "$candidate/corpus" "$STAGE_PATH/corpus"; then
            die candidate-invalid 'cannot copy candidate without following links'
        fi
        printf '%s\n' "$(fixture_meta_content "$promotion_id")" > "$STAGE_PATH/release.meta"
        validate_tree_entries "$STAGE_PATH/corpus" candidate-invalid
        freeze_stage "$STAGE_PATH"
        if ! /bin/mv "$STAGE_PATH" "$release_dir"; then
            die local-integrity 'cannot make staged release reachable'
        fi
        STAGE_PATH=
    fi

    if [ "$failpoint" = before-selector ]; then
        die test-fixture-failure 'stopped before selector replacement'
    fi

    if [ "$TRANSACTION_BOOTSTRAP" -eq 0 ] && [ "$promotion_id" = "$ACTIVE_ID" ]; then
        # Equal identity is the only idempotent equal-sequence success.
        release_lock
        printf '%s\n' "$promotion_id"
        return 0
    fi

    replace_selector "$promotion_id"
    if [ "$failpoint" = after-selector ]; then
        die test-fixture-failure 'stopped after selector replacement and before state update'
    fi

    atomic_write "$control_root/state/publication" "$(state_content "$promotion_id")"
    if [ "$TRANSACTION_BOOTSTRAP" -eq 1 ]; then
        atomic_write "$control_root/state/installation" initialized
    fi
    release_lock
    printf '%s\n' "$promotion_id"
}

promote_production() {
    control_root=$1
    publication_root=$2
    candidate=$3
    release_tag=$4

    validate_layout "$control_root" "$publication_root"
    acquire_lock
    # Candidate paths, trust policy, Git identities, reachability, signatures,
    # and exact archive bytes are all re-resolved while selection is locked.
    authenticate_candidate "$candidate" "$release_tag"
    transaction_state

    if [ "$TRANSACTION_BOOTSTRAP" -eq 0 ]; then
        compare_sequences "$promotion_sequence" "$ACTIVE_SEQUENCE"
        if [ "$SEQUENCE_COMPARISON" -lt 0 ]; then
            die rollback "candidate $promotion_id is older than $ACTIVE_ID"
        elif [ "$SEQUENCE_COMPARISON" -eq 0 ] && [ "$promotion_id" != "$ACTIVE_ID" ]; then
            die sequence-equivocation "candidate $promotion_id conflicts with $ACTIVE_ID"
        fi
    fi

    next_temp_dir "$publication_root/.staging" release
    STAGE_PATH=$CREATED_TEMP
    /usr/bin/tar -xf "$AUTH_ARCHIVE" -C "$STAGE_PATH" ||
        die candidate-invalid 'cannot extract authenticated corpus archive'
    validate_tree_entries "$STAGE_PATH/corpus" candidate-invalid
    [ -f "$STAGE_PATH/corpus/kernel/kernel.md" ] &&
        [ ! -L "$STAGE_PATH/corpus/kernel/kernel.md" ] ||
        die candidate-invalid 'authenticated corpus kernel is missing or non-regular'
    unexpected_stage_entry=$(
        /usr/bin/find "$STAGE_PATH" \( ! -path "$STAGE_PATH" -prune \) \
            ! -path "$STAGE_PATH/corpus" -exec /bin/echo x \; 2>/dev/null
    ) || die candidate-invalid 'cannot validate authenticated archive layout'
    [ -z "$unexpected_stage_entry" ] ||
        die candidate-invalid 'authenticated archive contains an unexpected root entry'
    printf '%s\n' "$promotion_meta" > "$STAGE_PATH/release.meta"

    release_dir="$publication_root/releases/$promotion_id"
    if [ -e "$release_dir" ] || [ -L "$release_dir" ]; then
        validate_release_dir "$release_dir" "$promotion_id"
        /usr/bin/diff -r "$STAGE_PATH/corpus" "$release_dir/corpus" >/dev/null 2>&1 ||
            die sequence-equivocation 'reused production release id has different corpus bytes'
        /usr/bin/diff "$STAGE_PATH/release.meta" "$release_dir/release.meta" >/dev/null 2>&1 ||
            die sequence-equivocation 'reused production release id has different authenticated metadata'
        /bin/chmod -R u+w "$STAGE_PATH" 2>/dev/null || :
        /bin/rm -rf "$STAGE_PATH"
        STAGE_PATH=
    else
        freeze_stage "$STAGE_PATH"
        if ! /bin/mv "$STAGE_PATH" "$release_dir"; then
            die local-integrity 'cannot make authenticated staged release reachable'
        fi
        STAGE_PATH=
        validate_release_dir "$release_dir" "$promotion_id"
    fi

    if [ "$TRANSACTION_BOOTSTRAP" -eq 0 ] && [ "$promotion_id" = "$ACTIVE_ID" ]; then
        release_lock
        printf '%s\n' "$promotion_id"
        return 0
    fi

    replace_selector "$promotion_id"
    atomic_write "$control_root/state/publication" "$(state_content "$promotion_id")"
    if [ "$TRANSACTION_BOOTSTRAP" -eq 1 ]; then
        atomic_write "$control_root/state/installation" initialized
    fi
    release_lock
    printf '%s\n' "$promotion_id"
}

cmd=${1:-}
case "$cmd" in
prepare)
    [ "$#" -eq 3 ] || usage
    prepare_roots "$2" "$3"
    ;;
promote-fixture)
    [ "$#" -ge 4 ] && [ "$#" -le 5 ] || usage
    promote_fixture "$2" "$3" "$4" "${5:-}"
    ;;
promote)
    [ "$#" -eq 5 ] || usage
    promote_production "$2" "$3" "$4" "$5"
    ;;
check)
    [ "$#" -eq 3 ] || usage
    control_root=$2
    publication_root=$3
    check_integrity
    printf '%s\n' "$CHECK_RESULT"
    ;;
resolve)
    [ "$#" -eq 3 ] || usage
    control_root=$2
    publication_root=$3
    check_integrity
    [ "$CHECK_RESULT" != never-initialized ] || die local-integrity 'no release is selected'
    printf '%s\n' "$publication_root/releases/$CHECK_RESULT/corpus"
    ;;
*)
    usage
    ;;
esac
