# Delivery trust boundary plan

Status: **accepted design; implementation pending** (2026-08-01)

Decision: adopt the separate-identity, immutable-publication resolution for
`C1-b`. Local agents are not fully trusted merely because they share an OS
account with today's updater. This plan freezes the contract for
architecture-plan step 1; it does not make the current scripts hardened.

In this plan, **corpus release** means one authenticated, versioned corpus
publication. A **kit release** is the separately governed version of this
repository that a consumer pins. `<corpus-release-id>` identifies the former.

## Falsifiable goal

Run synchronization and publication as a dedicated publisher principal that
the harness and agent cannot impersonate. A harness running as the agent
principal must be able to read the active corpus but must be unable to alter:

- any released corpus byte;
- the pointer selecting the active corpus release;
- the publisher's checkout, staging data, state, locks, hooks, or trust keys;
- the protected adapter launcher or harness configuration that selects the
  active publication.

A publisher promotion must expose either the complete old corpus release or
the complete new corpus release, never a mixture. A fresh harness session
must consume bytes from one immutable, versioned corpus release. For every
harness advertised as hardened, injection must be mandatory: the agent cannot
bypass the protected path by invoking another binary or configuration. These
properties require OS enforcement and tests under distinct principals;
ownership or mode checks performed by the same uid do not satisfy the goal.

## Threat model

Treat these as untrusted:

- the agent principal and every process running as it;
- agent-controlled environment variables, working directories, project
  files, and repo-local harness instructions;
- the mutable corpus checkout and all corpus bytes before validation;
- remote content until plan step 2 authenticates a corpus release.

Trust only:

- the host kernel and administrator-provisioned identity boundary;
- the distinct publisher principal and its protected executable/configuration;
- authenticated corpus release metadata after plan step 2 lands.

Root/administrator compromise, publisher-credential compromise, model
obedience, tool-level policy enforcement, availability, and live-session
refresh are outside this boundary. Harness instruction precedence remains a
separate tracked gap. A harness for which no protected injection point can be
demonstrated cannot be advertised as hardened.

## Principal and path contract

The consumer provisions two non-equivalent principals:

- **publisher principal** — may fetch, validate, and publish; owns every
  control-plane and publication path;
- **agent principal** — runs the harness; has read/traverse access only to
  published releases and no access to publisher secrets.

The kit must not bake environment usernames or platform-specific absolute
paths into this public repository. Consumer-owned installation chooses two
absolute roots with this logical layout:

```text
<control-root>/                    publisher read/write; agent no access
├── checkout/                     mutable transport checkout
├── quarantine/                   fetched candidates before publication
├── state/                        sync status and applied markers
├── locks/                        publisher serialization
├── hooks/                        protected post-sync executables
└── trust/                        signer policy and verification material

<publication-root>/                publisher-owned; agent read/traverse only
├── .staging/                     publisher-only; same filesystem as releases
├── releases/
│   └── <corpus-release-id>/      immutable after publication
│       └── corpus/…              one canonical, versioned corpus layout
└── current -> releases/<corpus-release-id>
```

Every ancestor from the filesystem root through both roots must be immune to
agent-principal replacement through mode bits, group membership, ACLs, or
symlink substitution. The consumer must verify effective access as the agent
principal; checking only numeric ownership and mode is insufficient.

The active corpus release selector and the protected harness wiring use
administrator/publisher-owned configuration. Hardened mode must not accept an
agent-controlled environment variable such as `KNOWLEDGE_HOME` as an
authority override. Existing environment overrides remain prototype/test
interfaces until replaced by a protected configuration channel.

The publisher derives `<corpus-release-id>` internally from the authenticated
step-2 manifest using the grammar `r<sequence>-<digest>`. `<sequence>` is 1–18
base-10 digits, starts with `1`–`9`, and is compared without shell arithmetic:
normalized length first, then bytewise lexical order under `LC_ALL=C`.
`<digest>` is 32–128 lowercase hexadecimal characters whose algorithm and
identity binding are defined by step 2. The whole id must fit the target
filesystem's reported `NAME_MAX`. It is never accepted as a caller-supplied
path. `current` is the only permitted symlink in the publication root; its raw
target must be exactly `releases/<corpus-release-id>`. Resolution must yield a
publisher-owned directory canonically contained immediately below
`releases/`, with a manifest whose sequence and digest match its name.
Absolute targets, separators inside either field, `.`, `..`, control bytes,
option-like values, nested symlinks, special files, and inherited/default ACLs
that grant the agent write access are rejected.

The protected publisher executable, interpreter, configuration, and update
path are part of the trusted computing base. Until authenticated kit releases
exist, administrator provisioning records a reviewed, pinned kit commit and
tree identity under the protected control root; it never installs from mutable
`main`. Later installation records the authenticated kit release identity.
The agent principal must not be able to alter those files, their dependencies,
the publisher service definition/environment, or any privilege-delegation
policy that can assume the publisher principal.

## Publication transaction

The publisher follows one serialized transaction for bootstrap and every
update:

1. Fetch into a unique publisher-only checkout or control-root quarantine
   path.
2. Authenticate the candidate according to plan step 2 and derive its corpus
   release id. Until that step lands, publication code may use authenticated
   test fixtures, but production cutover stays disabled.
3. Acquire a dedicated publisher-only publication mutex using an atomic
   operation. This mutex is part of step 1 and is distinct from the later pull
   mutex. Only one promotion may validate sequence, change `current`, or write
   selected-release state at a time. A contender never removes a lock based on
   age alone; crash recovery must prove the recorded process instance is gone
   or rely on protected supervisor serialization before reclaiming it.
4. Under the mutex, choose exactly one state transition:

   - **Fresh bootstrap:** permit the first promotion only when protected
     installation metadata identifies this root as never initialized,
     `current` and the anti-rollback watermark are both absent, and no corpus
     release is reachable. The candidate must pass step-2 authentication and
     authoritative currency checks. After selection, write the watermark and
     atomically mark installation initialized. If only one of selector or
     watermark exists, the root is non-empty, or metadata conflicts, fail loud
     as half-initialized/corrupt; never treat it as fresh.
   - **Update:** resolve and validate `current`, its manifest, and protected
     anti-rollback state. `current` is authoritative for selected bytes; state
     is the never-decreasing sequence watermark. If state is behind a valid
     `current`, advance it only as proven post-selector transaction residue.
     If state is ahead, equal-sequence/different-identity, unreadable, or
     otherwise inconsistent, fail loud and require authenticated currency
     recovery or operator intervention; never reconcile the watermark
     downward.
5. Require a candidate sequence strictly greater than the watermark. The only
   equal-sequence success is an idempotent no-op for the identical id with
   every bound identity matching. A different digest at the same sequence is
   typed `sequence-equivocation`; a lower sequence is typed rollback. Reject a
   reused id whose bytes or manifest differ.
6. Copy the candidate into `<publication-root>/.staging/` without following
   symlinks. Reject every entry except permitted directories and regular
   files; no corpus symlink or special file is published.
7. Validate the complete staged layout and manifest, then apply final
   publisher ownership and non-agent-writable permissions before making the
   version reachable.
8. Rename the staged directory into `releases/<corpus-release-id>`, then
   replace a temporary `current` selector into place atomically.
9. Derive selected-release state from the newly resolved `current` and write
   it atomically before releasing the mutex. If this state write fails,
   `current` remains authoritative for bytes, the higher of state/current
   remains the anti-rollback watermark, the publisher exits non-zero, and the
   minimal step-1 integrity check reports selector/state disagreement or
   unreadable state. The next publisher run auto-recovers only the proven
   state-behind-current residue before considering a candidate. Broader remote
   currency and fleet status remain architecture-plan step 5.

Published corpus release directories are never edited in place. Adapters
resolve `current` once to a physical versioned path and consume that path for
the operation, so a concurrent promotion cannot mix versions. Retention must
keep a resolved corpus release available to readers. Step 1 never deletes a
published corpus release; garbage collection is excluded until a future
design defines enforceable reader leases or another concrete lifetime.
Garbage collection never removes `current`.

The step-1 primitive guarantees atomic visibility across process crashes; it
does not claim power-loss durability from POSIX `rename` alone. Before
hardened cutover, architecture-plan step 3 must explicitly disposition
file/directory durability: prove an allowed platform primitive, approve a
dependency/compatibility change, or narrow the supported guarantee and record
the residual risk. No power-loss durability claim exists until then.

## Adapter and hook contract

- A hardened adapter reads only from the protected publication root. It never
  falls back to the mutable checkout after a local-integrity failure.
- The launcher, hook executable, and harness configuration that choose the
  publication root must also be outside agent-writable paths. Protecting only
  the corpus leaves the original hook-replacement attack intact.
- Publisher hooks, keys, state, locks, and applied markers live under the
  control root, not `KNOWLEDGE_HOME` in the agent's home directory.
- Each harness integration must prove a protected injection point. If the
  harness requires an agent-writable target, that integration remains
  prototype-only even when the source publication is immutable.
- Protected injection must be mandatory and non-bypassable by the agent
  principal. A wrapper is insufficient when the agent can invoke the
  underlying harness directly, select another executable or config root,
  disable injection with argv/environment, or replace precedence-affecting
  user config, repo config, plugins, or launch settings. Consumer OS/tool
  policy must close those paths, or the harness remains prototype-only.
- Network failure remains fail-soft because `current` retains the last-good
  corpus release. Missing, corrupt, or locally mutable publication state
  fails loud and never falls back to unprotected bytes.

## Migration contract

Migration is explicit and reversible until cutover:

1. Provision the publisher principal and both protected roots.
2. Install protected publisher and adapter wiring from a recorded,
   administrator-reviewed pinned kit commit/tree (or a future authenticated
   kit release) without changing the active prototype path.
3. Shadow-publish a fixture and compare emitted bytes with the staged source.
4. Land corpus release authentication and shadow-publish the first
   authenticated corpus release.
5. Land the architecture-plan step-3 durability disposition on every target
   platform; do not claim power-loss durability from atomic visibility alone.
6. Switch each harness to its protected, mandatory injection point
   independently.
7. Run the negative checks below as the real agent principal and verify a
   fresh session consumes the selected immutable corpus release.
8. Retire the mutable-checkout adapter path only after every supported harness
   passes; do not silently fall back during or after cutover.

## Required verification

The implementation is incomplete until automated fixtures or platform tests
demonstrate all of these:

1. As the agent principal, writes, renames, unlinks, and symlink replacements
   fail for a released kernel, `current`, every control-root class, and the
   protected harness wiring.
2. A safe group/ACL fixture makes agent write attempts fail. An unsafe shared-
   group fixture (`chmod g+w`) makes install/status fail non-zero. On Linux,
   `setfacl -m "u:${AKK_TEST_AGENT}:rwx" <publication-root>` must make
   verification fail non-zero; on macOS, a `chmod +a` ACE granting the test
   agent add/delete rights must do the same. The platform test records the
   exact native command and proves the agent principal would otherwise receive
   the prohibited access.
3. The agent cannot assume the publisher identity through `sudo`, `doas`,
   polkit, credentials, supplementary groups, or control of the publisher
   supervisor/service definition or environment. Different numeric uids alone
   are insufficient evidence. Installed publisher code/config/interpreter
   dependencies are non-agent-writable and match the recorded pinned kit
   commit/tree or authenticated kit release identity.
4. Invalid selectors and releases are rejected: absolute and `../` targets,
   malformed ids, zero/leading-zero/19-digit sequences, maximum-boundary and
   shell-overflow values, overlong digests or `NAME_MAX` violations, nested
   symlinks, wrong file types, mismatched manifests, reused ids with different
   bytes, and agent-writable inherited/default ACLs.
5. A protected never-initialized empty root accepts exactly one authenticated,
   currency-checked bootstrap. Selector-only, watermark-only, reachable-
   release-without-state, and conflicting-initialization fixtures all fail
   loud. A kill after first selector replacement but before watermark or
   initialized metadata completes produces a half-initialized state that
   requires authenticated operator recovery, not a second bootstrap.
6. A killed or failed publisher before selector replacement leaves `current`
   and its emitted bytes unchanged. Kill, ENOSPC, and read-only failures after
   selector replacement or during state update leave complete released bytes,
   cause the integrity check to fail on selector/state disagreement, and
   auto-reconcile only state-behind-current residue. A state-ahead-of-current
   fixture fails loud without lowering its watermark.
7. Two simultaneous promotions of sequences `n` and `n+1` finish with `n+1`
   selected and state matching its manifest; neither a late `n` nor a failed
   state write can roll the selector back. Killing a lock holder permits only
   proven-stale recovery and never produces two concurrent holders.
8. Sequential and concurrent candidates `rN-A` and `rN-B` produce a typed
   `sequence-equivocation`; only the identity already selected at sequence
   `N` may succeed as an idempotent no-op.
9. Concurrent reader loops during repeated promotions observe only complete
   old or new corpus releases, each matching a published corpus release
   identity.
10. An agent-controlled environment override cannot redirect hardened adapters
   to an arbitrary checkout or file.
11. Direct harness invocation, alternate binaries/config roots, disable flags,
   environment, user/repo configuration, plugin, and `PATH` shadow attempts
   cannot suppress, replace, or redirect protected injection for an advertised
   hardened integration. Any unclosed path keeps that integration
   prototype-only.
12. Local-integrity failure never falls back to the legacy mutable checkout and
   is visible to the operator.
13. A resolved old physical corpus-release path remains readable after any
   number of step-1 promotions because step 1 performs no release deletion.
14. A deterministic adapter test double or harness-supported context capture
   records the actual injected payload; its bytes and digest match the
   selected physical corpus release for every advertised hardened harness on
   supported macOS and Linux configurations. A fresh-session behavior check
   remains supplemental end-to-end evidence, not a byte-equality oracle over
   model output.

## Implementation slices and non-goals

This decision-record slice changes documentation and ignore rules only. The
follow-on implementation should land as bounded, independently reviewed
slices: protected publisher primitive and two-principal tests; protected
adapter/configuration paths per harness; then consumer migration guidance.
Corpus release authentication remains architecture-plan step 2 and must gate
production publication. The publication mutex and minimal selector/state
integrity check belong to step 1; the broader pull mutex, durability
disposition, post-sync execution, status/currency, transport enforcement,
listeners, and Tier B loading retain their existing ordered plan positions.
