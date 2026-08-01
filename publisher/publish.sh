#!/bin/sh
# Fixture-only corpus publication transaction.
#
# This is not a production authentication path. `promote-fixture` accepts a
# descriptor that the test harness treats as already authenticated and
# currency-approved. The production `promote` verb remains disabled until
# architecture-plan step 2 defines and enforces the real release manifest.
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
  $PROGRAM check <control-root> <publication-root>
  $PROGRAM resolve <control-root> <publication-root>

Production promotion is unavailable until corpus release authentication lands.
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
    expected_meta_with_sentinel=$(
        fixture_meta_content "$expected_id"
        printf '\n.akk-meta-sentinel'
    )
    actual_meta_with_sentinel=$(
        /bin/cat "$meta_file" && printf '.akk-meta-sentinel'
    ) || die local-integrity 'cannot read selected release metadata'
    [ "$actual_meta_with_sentinel" = "$expected_meta_with_sentinel" ] ||
        die local-integrity 'selected release metadata does not match its directory byte-for-byte'
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
        [ "$attempt" -lt 3 ] ||
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
    die authentication-unavailable 'production corpus release authentication has not landed'
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
