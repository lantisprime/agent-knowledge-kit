# shellcheck shell=sh
# Shared kernel source validation for harness adapters. POSIX sh.

akk_refuse_kernel() {
    printf 'refusing kernel: %s\n' "$1" >&2
}

# Print the selected kernel path. Return 1 for an unsafe source and 2 when no
# supported layout contains a kernel.
akk_resolve_kernel() {
    knowledge_home=$1
    corpus_root="$knowledge_home/corpus"

    if ! corpus_root_phys=$(CDPATH= cd -P "$corpus_root" 2>/dev/null && pwd -P); then
        return 2
    fi

    for kernel_rel in corpus/kernel/kernel.md kernel/kernel.md; do
        kernel_path="$corpus_root/$kernel_rel"
        if [ ! -e "$kernel_path" ] && [ ! -L "$kernel_path" ]; then
            continue
        fi

        if [ -L "$kernel_path" ]; then
            akk_refuse_kernel 'kernel path is a symbolic link'
            return 1
        fi
        if [ ! -f "$kernel_path" ]; then
            akk_refuse_kernel 'kernel path is not a regular file'
            return 1
        fi

        kernel_parent=${kernel_path%/*}
        kernel_name=${kernel_path##*/}
        if ! kernel_parent_phys=$(CDPATH= cd -P "$kernel_parent" 2>/dev/null && pwd -P); then
            akk_refuse_kernel 'kernel parent cannot be resolved'
            return 1
        fi
        case "$kernel_parent_phys/$kernel_name" in
        "$corpus_root_phys"/*) ;;
        *)
            akk_refuse_kernel 'kernel path escapes the corpus checkout'
            return 1
            ;;
        esac

        if [ -d "$corpus_root_phys/.git" ] || [ -f "$corpus_root_phys/.git" ]; then
            if ! git -C "$corpus_root_phys" rev-parse --is-inside-work-tree >/dev/null 2>&1; then
                akk_refuse_kernel 'corpus git metadata is invalid'
                return 1
            fi
            kernel_mode=$(
                git -C "$corpus_root_phys" ls-tree HEAD -- "$kernel_rel" 2>/dev/null |
                    awk 'NR == 1 { print $1; exit }'
            )
            case "$kernel_mode" in
            100644|100755) ;;
            120000)
                akk_refuse_kernel 'kernel is a symbolic link in the git tree'
                return 1
                ;;
            *)
                akk_refuse_kernel 'kernel is not a regular tracked git blob'
                return 1
                ;;
            esac
        fi

        printf '%s\n' "$kernel_parent_phys/$kernel_name"
        return 0
    done

    return 2
}
