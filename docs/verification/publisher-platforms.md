# Publisher platform verification

Status: **portable and two-principal suites pass on disposable macOS and Linux
hosts** (2026-08-02).

This record contains no consumer host or account identifiers. Each run used
two generic, non-root fixture principals with distinct numeric identities and
a third shared-group fixture. The runner created its protected test root on
the native system volume and removed it at exit.

## Evidence

| Platform | Filesystem and security mechanism | Portable result | Privileged result |
|---|---|---:|---:|
| macOS 26.5 (25F71), arm64; Apple Git 2.50.1; OpenSSH 10.2p1 | APFS; native `chmod +a`/`chmod -N` ACLs | 18 adapter + 17 transaction + 11 authentication tests pass | 7/7 pass |
| Ubuntu 26.04, kernel 7.0.0-28, arm64; Git 2.53.0; OpenSSH 10.2p1; ACL tools 2.3.2 | ext4; `setfacl`/`getfacl` 2.3.2 | 18 adapter + 17 transaction + 11 authentication tests pass | 7/7 pass |

Verified script identities:

```text
publisher/publish.sh
sha256 9d369bc3269b7302098f06d092a33032e982058217a843bec7084d0f28e08e51

tests/publisher/two-principal.sh
sha256 8c57a21919044e4d11fca4e326413e513e7375607abcf228c64811e4f948785d
```

The exact privileged assertions were:

1. the agent fixture principal reads the selected immutable kernel;
2. writes, renames, unlinks, and selector replacement fail;
3. publisher secrets and control-root mutations remain inaccessible;
4. a shared-group write grant is effective for the agent and makes publisher
   integrity status fail;
5. a native ACL write grant is effective for the agent and makes integrity
   status fail;
6. the installed publisher executable and directory remain non-writable; and
7. noninteractive `sudo` cannot assume the publisher fixture principal.

The native unsafe-ACL fixtures were exactly:

```sh
# macOS
chmod +a '<agent-fixture> allow add_file,delete_child' <publication-root>

# Linux
setfacl -m 'u:<agent-fixture>:rwx' <publication-root>
```

Both commands first proved that the agent could create a probe below the
publication root, while `publish.sh check` failed. Removing the ACL restored
the protected fixture.

## Scope

This is evidence for the runner's effective-access, native ACL, installed-code
mode, and noninteractive-sudo assertions on APFS and ext4. It is not complete
proof for every real consumer ancestor, supervisor, privilege-delegation rule,
alternate harness binary, or protected configuration path. Consumers must run
the same probe against their provisioned identities and separately close the
mandatory harness-injection checks before claiming a hardened integration.
