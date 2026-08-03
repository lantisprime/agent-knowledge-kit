#!/bin/sh
# Render the Codex managed-config key for one authenticated publication root.
set -eu

PATH=/usr/bin:/bin
LC_ALL=C
TMPDIR=/tmp
export PATH LC_ALL TMPDIR
unset CDPATH

usage() {
    printf 'usage: %s <absolute-publication-root>\n' "$0" >&2
    exit 64
}

die() {
    printf 'protected-config: %s\n' "$1" >&2
    exit 1
}

[ "$#" -eq 1 ] || usage
publication_root=$1

case "$publication_root" in
/*) ;;
*) die 'publication root must be absolute' ;;
esac
case "$publication_root" in
*/*/../*|*/../*|*/..|*/*/./*|*/./*|*/.|*//*|*/)
    die 'publication root must be lexically normalized'
    ;;
*[!A-Za-z0-9_./+\ -]*)
    die 'publication root contains an unsupported character'
    ;;
esac

[ -d "$publication_root" ] && [ ! -L "$publication_root" ] ||
    die 'publication root must be a physical directory'
physical_publication_root=$(cd "$publication_root" && pwd -P) ||
    die 'publication root is not accessible'
[ "$physical_publication_root" = "$publication_root" ] ||
    die 'publication root must not traverse a symbolic link'

selector="$publication_root/current"
[ -L "$selector" ] || die 'current selector must be a symbolic link'
selector_capture=$(/usr/bin/mktemp "${TMPDIR:-/tmp}/akk-codex-selector.XXXXXX") ||
    die 'cannot allocate selector validation state'
trap '/bin/rm -f "$selector_capture"' EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM
/usr/bin/readlink "$selector" > "$selector_capture" ||
    die 'current selector is unreadable'
selector_lines=$(/usr/bin/wc -l < "$selector_capture" | /usr/bin/tr -d ' ')
[ "$selector_lines" = 1 ] || die 'current selector target is malformed'
selector_target=$(/usr/bin/sed -n '1p' "$selector_capture")
case "$selector_target" in
releases/r*-*) ;;
*) die 'current selector target is malformed' ;;
esac
release_id=${selector_target#releases/}
case "$release_id" in */*|*-|*--*) die 'current selector target is malformed' ;; esac
[ "$selector_target" = "releases/$release_id" ] ||
    die 'current selector target is malformed'
release_identity=${release_id#r}
release_sequence=${release_identity%%-*}
release_digest=${release_identity#*-}
case "$release_sequence" in
''|0*|*[!0-9]*) die 'current selector target is malformed' ;;
esac
[ "${#release_sequence}" -le 18 ] ||
    die 'current selector target is malformed'
case "$release_digest" in
''|*[!0-9a-f]*) die 'current selector target is malformed' ;;
esac
digest_length=${#release_digest}
[ "$digest_length" -ge 32 ] && [ "$digest_length" -le 128 ] ||
    die 'current selector target is malformed'

release_dir="$publication_root/releases/$release_id"
[ -d "$release_dir" ] && [ ! -L "$release_dir" ] ||
    die 'selected release must be a physical directory'
physical_release_dir=$(cd "$release_dir" && pwd -P) ||
    die 'selected release is not accessible'
[ "$physical_release_dir" = "$release_dir" ] ||
    die 'selected release escapes the publication root'

corpus_dir="$release_dir/corpus"
kernel_dir="$corpus_dir/kernel"
kernel="$kernel_dir/kernel.md"
[ -d "$corpus_dir" ] && [ ! -L "$corpus_dir" ] ||
    die 'selected corpus must be a physical directory'
[ -d "$kernel_dir" ] && [ ! -L "$kernel_dir" ] ||
    die 'selected kernel directory must be physical'
[ -f "$kernel" ] && [ ! -L "$kernel" ] ||
    die 'selected kernel must be a regular file'
physical_kernel_dir=$(cd "$kernel_dir" && pwd -P) ||
    die 'selected kernel directory is not accessible'
[ "$physical_kernel_dir" = "$kernel_dir" ] ||
    die 'selected kernel escapes the authenticated release'

printf "model_instructions_file = '%s'\n" "$publication_root/current/corpus/kernel/kernel.md"
