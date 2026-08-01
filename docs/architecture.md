# Architecture & event-propagation plan

Status: design accepted **adopt-with-changes** after two independent
adversarial reviews — Kimi K3 via pi (2026-07-28, signed off
**CONSENSUS** in a second round the same day) and Codex gpt-5.6-sol
(2026-07-29, §Second adversarial review). The second review **rejects
the MITM amendment as written** (H2-b refutes host-clock freshness) and
**re-orders the implementation plan** (H3-b: the old step 1 automated
reapplication of unsigned content before signing landed). The plan below
is the re-ordered one. This doc records the architecture, the settled
decisions, the proposed event layer, both reviews' required changes, and
the implementation plan. Public repo — everything here is generic;
environment specifics belong in your corpus.

Finding IDs from the first review are bare (`C1`, `H3`); second-review
IDs carry a `-b` suffix (`C1-b`, `H3-b`) — the two sets collide
numerically and mean different things.

## Authority and repository boundary

This document is authoritative for the generic kit only because it lives and
is reviewed in `agent-knowledge-kit`. The kit owns its architecture, schemas,
adapters, tests, compatibility policy, and release lifecycle. External
environment repositories may supply requirements and pin a released version,
but they cannot define, tag, publish, or silently override the kit.

Environment-specific hosts, deployment orchestration, monitoring, access
paths, and private corpus content remain owned by the consumer. In
particular, `home-network` is a consumer and integration owner, not the
controller of this repository. Generic changes discovered there must be
proposed and accepted here before they become part of the kit.

The diagrams and ordered steps below include both current behavior and target
design. Treat only behavior backed by the checked-in implementation, plus
tests where present, as implemented; policy prose is not enforcement.

## System overview

```
        PUBLIC KIT (this repo)              PRIVATE CORPUS (per environment)
 ┌──────────────────────────────────┐  ┌──────────────────────────────────────┐
 │ schema/frontmatter.md            │  │ kernel/kernel.md    → Tier A         │
 │ kernel/kernel.template.md        │─▶│ docs/*.md           → Tier B         │
 │ adapters/sync.sh                 │  │ schema/  (copied from kit)           │
 │ adapters/{claude,codex,pi}       │  │ access paths · hosts · limits        │
 │ ZERO environment specifics       │  │ PRIVATE — never in the kit           │
 └──────────────────────────────────┘  └──────────────────┬───────────────────┘
                                                          │ git push, PR-gated,
                                                          │ supersede-in-place
                                                          ▼
                              git — baseline transport, ordered remote list
                                   (canonical first, mirrors after)
                                                          │
                                                          ▼
╔══════════════════════════════════════════════════════════════════════════════╗
║  HOST — ACCEPTED TARGET (publisher boundary not yet implemented)             ║
║                                                                              ║
║  PUBLISHER PRINCIPAL                                                         ║
║   event fast-path (doorbell) ─┐                                              ║
║   cron floor ─────────────────┼──▶ sync + corpus-release authentication      ║
║                               │        │                                      ║
║                               │        ▼                                      ║
║                               │   protected control root                      ║
║                               │   state · locks · hooks · trust               ║
║                               │        │ atomic publish                       ║
║                               │        ▼                                      ║
║  AGENT PRINCIPAL              │   protected publication root                 ║
║   protected harness integration ◀── current/corpus/kernel/kernel.md           ║
║        ├─▶ Claude Code                                                       ║
║        ├─▶ Codex CLI                                                         ║
║        └─▶ pi                                                                ║
╚═════════════════════════════════════════╤════════════════════════════════════╝
                                          ▼
              ┌────────────────────────────────────────────────────┐
              │  AGENT CONTEXT WINDOW                              │
              │  Tier A  kernel ~1–2k tok   PUSH    every session  │
              │  Tier B  corpus/docs/       trigger (loader: gap)  │
              │  Tier C  search / query     PULL                   │
              └────────────────────────────────────────────────────┘
```

One canonically contained kernel path within the selected physical corpus
release remains the content contract: everything upstream exists to publish
that file, and every adapter is a thin protected reader of it. The shipped
prototype instead reads mutable `$KNOWLEDGE_HOME/corpus` under the agent uid;
the accepted target above is not enforcement by the current code. Corpus
authority is total, which drives the security decisions below.

## Settled design decisions

- **Events trigger, git transports.** Any event fast-path (webhook,
  message bus) is a *doorbell* that runs `sync.sh pull`; git stays the
  source of truth and the event payload is never trusted as content. The
  event path must never be the only path — a cron floor always runs
  underneath, so a dead broker degrades convergence latency, never
  convergence.
- **Multi-remote decouples three concerns.** *Availability*: the ordered
  remote list (canonical first, mirrors after) — implemented.
  *Authority*: every remote can currently rewrite the kernel; the step-2
  target pins repository/ref/signer policy and authenticates a monotonic
  corpus release manifest before publication, failing soft to the next remote
  — mirrors can then withhold updates but not author accepted releases.
  *Confidentiality*: the kit mirrors publicly by design; a **corpus**
  must not be mirrored outside its trust boundary without an explicit,
  recorded decision (don't mirror / encrypt at rest / accept custody).
- **Local delivery crosses a protected publisher boundary.** The `C1-b`
  resolution is option (a): a dedicated publisher principal owns mutable
  transport state and atomically exposes immutable, versioned releases
  through paths the agent principal cannot change. Protected adapter wiring,
  hooks, keys, state, locks, and selectors are part of the same boundary.
  The decision is accepted; implementation is still pending. The frozen
  contract is `docs/plans/delivery-trust-boundary.md`.
- **Fail-soft for consumers, fail-loud for operators.** `pull` keeps
  exiting 0 on unreachable remotes; `status` is the monitoring surface
  and must exit non-zero when stale.
- **Doorbell semantics.** An event means only "pull now". Because the
  ordinary path authenticates and selects the newest acceptable corpus
  release independently of message content, ordering, dedup, replay, and
  exactly-once are non-requirements. At-least-once plus serialized,
  idempotent convergence gives correctness; events only buy latency.
- **Producer side.** Events come only from the canonical remote's
  post-receive webhook; mirrors never produce events (they lag, and
  authority stays with canonical). Preferred broker where one is wanted:
  MQTT (retained message = late-join doorbell; outbound-only host
  posture; small ACL surface). Direct per-host webhook listeners are
  rejected: the only variant that opens an inbound port on every host.
- **Transport security is load-bearing, not hygiene (MITM, 2026-07-29).**
  TLS with mutual authentication on the broker path — both directions —
  and TLS with a pinned/verified CA on the git path are architecture
  requirements, not deployment advice. Rationale: the composite MITM
  attack (withhold git updates from a host — signing cannot prevent
  withholding, and fail-soft accepts it — while forging that host's
  heartbeat to report the fleet-current SHA) turns fail-soft into
  fail-silent: agents run an arbitrarily old kernel while the operator
  view says healthy. Stale safety rules are worse than absent ones; the
  fail-loud channel must not ride a transport the attacker can own.
- **Scope (review L2).** Planned kit scope includes `listen`, the post-sync
  hook runner, and the cron floor — no network code. The webhook → bridge →
  broker chain is a documented operator recipe, not kit code: the moment the
  kit ships a bridge it owns a network-facing security surface forever.

## Delivery trust boundary — accepted target, not implemented

The current prototype conflates transport, local state, publication, and
consumption under the agent uid. The accepted target separates them:

```text
publisher principal
┌────────────────────────────────────┐
│ protected control root             │
│ checkout · quarantine · state      │
│ locks · hooks · trust policy       │
└─────────────────┬──────────────────┘
                  │ atomic publish
                  ▼
┌────────────────────────────────────┐
│ protected publication root         │
│ staging · immutable releases       │
│ atomic current selector            │
└─────────────────┬──────────────────┘
                  │ resolve once; read physical release
                  ▼
agent principal
┌────────────────────────────────────┐
│ harness cannot rewrite publication │
└────────────────────────────────────┘
```

Protection applies to the whole delivery path, not only kernel bytes. The
publisher principal must differ from the agent principal; every relevant
ancestor, selector, release, launcher, and harness configuration must resist
agent-principal replacement through modes, groups, ACLs, or symlinks. A
harness without a demonstrably protected injection point remains
prototype-only. Environment-selected paths such as `KNOWLEDGE_HOME` remain a
prototype/test interface, not hardened configuration authority.

Promotion creates and validates a same-filesystem staged release, makes it
non-agent-writable, renames it into a never-mutated version directory, and
atomically replaces `current`. Readers resolve `current` once and use that
physical version for the operation. Failure retains the last-good selector;
local-integrity failure never falls back to the mutable checkout. Production
publication remains gated on step 2 corpus release authentication.

`docs/plans/delivery-trust-boundary.md` is the accepted, falsifiable contract:
it defines identities, logical roots, trust assumptions, migration, non-goals,
and the required two-principal macOS/Linux tests. None of those protections
are implemented by the checked-in scripts yet.

## Event layer — components

### `sync.sh listen`

Reads newline doorbells on stdin and runs `pull`, coalescing bursts.
stdin is the transport boundary — any broker client pipes in:

```sh
mosquitto_sub -t corpus/updated | sync.sh listen     # MQTT
kcat -C -t corpus-updates -G knowledge-sync -u -q | sync.sh listen   # Kafka
```

Requirements (review M1/M2/M4):

- **Minimum pull interval** regardless of doorbell rate. Forged doorbells
  are otherwise a fleet-scale amplification lever against the git remote
  (N hosts × attacker-chosen rate; a slow drip defeats burst coalescing).
  Rate-limiting to ~cron cadence is what makes "forged doorbells are
  nuisance-only" true.
- **Untrusted SHA hint.** The payload may carry the pushed commit SHA as
  a *falsifiable claim*, never as content: after pull, HEAD == hint
  proves the whole chain end-to-end; mismatch after all remotes records
  `behind <sha>` in the state file. The hint is never used to select what
  to fetch or fast-forward toward — comparison happens only after
  ordinary convergence (fetching the hint SHA would reintroduce payload
  trust through the side door). Without this, event-path death is
  invisible between cron ticks — the exact silent failure the kit's
  thesis rejects.
- **Hostile stdin.** `IFS= read -r`; never eval/interpolate message
  bytes; never log raw payload (terminal-escape/log injection) — count
  doorbells, don't echo them. Treat pull's exit code as data; a failed
  pull must not kill the loop.
- **Lifecycle.** On broker-client death stdin EOFs and `listen` exits;
  the operator's supervisor (systemd/launchd) restarts it. `listen`
  touches a heartbeat file so `status` can report listener liveness. The
  cron floor is mandatory even where `listen` runs.

### Post-sync hooks — `<control-root>/hooks/post-sync/*`

Run by `pull` as the publisher principal so downstream consumers re-apply the
kernel. The historical first user was the Codex adapter, whose agent-writable
`~/.codex/AGENTS.md` snapshot currently freezes because nothing re-runs it;
that target remains prototype-only and cannot satisfy hardened step 1. A
protected Codex injection point still must be proven. `<control-root>` is the
publisher-only root defined by the accepted delivery-boundary plan.

**Trust contract (review C1 — the design's only code-execution story;
non-negotiable):**

- Hooks are installed by the operator out of band. `sync.sh` never
  copies, links, or executes anything from the corpus into the hook
  path — synced content must have no route to execution (git preserves
  exec bits; "the corpus is only markdown" is not a property git
  enforces).
- At execution time: skip symlinks; require regular files owned by the
  publisher principal; require the hook file and every parent path component
  to be protected from agent-principal writes; execute a protected copy to
  close the check/execute race.
- Hooks run with a scrubbed minimal environment, a fixed system `PATH`, and
  path/configuration values loaded only from publisher-owned configuration.
  Do not inherit caller-controlled `HOME`, `KNOWLEDGE_HOME`, or `PATH` as
  authority. Hooks receive exactly `<old-sha> <new-sha>` as argv.

**Trigger invariant (review H3):** not "SHA changed" but
"**HEAD ≠ applied-marker**". `pull` runs the hook batch whenever HEAD
differs from `<control-root>/state/hooks-applied`, and writes the marker only
after *all* hooks succeed. Otherwise a crash mid-batch on a low-churn corpus
freezes downstream consumers until the next commit — the frozen-Codex bug
resurrected in a smaller window. Hooks must therefore be idempotent.

**Failure semantics (review H4):** hook failure never fails the pull
(fail-soft for consumers), but is recorded in the state file
(`ok pull <url> <sha> hooks-failed:<name>`) so `status` can surface it
(fail-loud for operators). Fleet reporting must be a *heartbeat* —
publish HEAD + last-pull-result on every pull, changed or not, including
the stale path — never change-triggered only, or the sickest hosts
(ff-only failing forever at a frozen SHA) are exactly the ones that never
report.

### Concurrency (review H2)

With cron + `listen` + hooks, overlapping pulls are the steady state, and
a `.git/index.lock` collision currently falls through the remote loop
into the fail-soft tail — recorded as `stale` on a healthy host. All pull
entry points serialize on a mkdir-based lock (POSIX-portable; `flock` is
not) inside `pull`; the loser exits 0 as a no-op (idempotency makes this
correct). Hooks run inside the same critical section.

### Ordering constraint (review H1)

Corpus release authentication lands **before or with** `listen`. Events
collapse the forged-kernel detection window from cron cadence (minutes–
hours; an operator can catch it) to seconds, and the kernel is the most
leverage-dense injection surface in the architecture. If authentication
slips, `listen` ships disabled-by-default with that dependency documented.

### Transport & telemetry trust (MITM hardening, 2026-07-29)

In the accepted target, a network MITM cannot inject a published corpus
release (step-2 repository/ref/signer policy, manifest identity, and
anti-rollback), trigger execution (hook contract), or redirect a fetch (hint
checked, never consumed). Before those controls land, it can also make a
*freeze invisible* via the composite attack in the settled-decisions bullet
above. Three defenses protect two different parties:

1. **mTLS on the broker path, both directions.** Subscriber-side MITM is
   the doorbell-forgery/suppression attack; publisher-side MITM is the
   heartbeat-forgery attack. Mutual auth defeats the classic network MITM
   outright. Git over TLS with a pinned/verified CA, stated — not
   assumed.
2. **Broker-transported telemetry is a hint; the authoritative fleet
   view is attacker-independent.** Either hosts sign heartbeats (per-host
   key over `<sha> <timestamp> <host>` — the broker cannot forge what it
   merely carries) or the authoritative check is operator-initiated
   out-of-band (`ssh <host> sync.sh status`), with the broker topic kept
   as the cheap dashboard. Same principle as the SHA hint: transported
   claims are falsifiable inputs, never trusted state. Protects the
   *operator*.
3. **Staleness detection is host-side.** The kernel provenance banner +
   freshness threshold detects a freeze using only the host's own clock
   — no attacker-controlled network in the loop — and surfaces it where
   it matters: in the agent's context. A MITM can still freeze a host;
   it can no longer make the freeze invisible to the agent. Protects the
   *agent*.

   **Refuted 2026-07-29 (H2-b) — defense 3 does not hold as written.**
   Sync *age* measures last transport success, not corpus *currency*. A
   mirror that keeps serving the same old signed tip produces a
   successful pull every time, and `pull` stamps "now" on success
   (`adapters/sync.sh:53-54`), so the banner reads fresh indefinitely
   while the host is arbitrarily far behind. A fresh host has no prior
   version to detect rollback against, and clock rollback, a future
   timestamp, or VM-snapshot restore each make an old state look young.
   The repair is to separate *last transport success* from *verified
   release currency*: compare a monotonic signed release sequence
   against an authoritative target, detect clock regression and future
   timestamps, and fail visibly when neither currency nor sane time can
   be established (plan step 5). Until that lands, the MITM amendment's
   agent-facing defense is aspirational — defenses 1 and 2 stand.

## Second adversarial review (Codex gpt-5.6-sol, 2026-07-29)

Verdict **adopt-with-changes**; blocking before deployment or before
enabling the event layer: `C1-b`–`C3-b`, `H1-b`–`H7-b`, `M1-b`, `M2-b`,
`M9-b`. Three findings were independently reproduced in this repo before
folding (marked ✅ below); the rest are accepted on the review's evidence.
The `C2-b` and `H1-b` containment fixes landed with portable regressions on
2026-08-01; the remaining findings retain their recorded status.

**The reframing finding — `C1-b`: the verified git object is not the
delivered artifact.** Signed commits and mTLS protect bytes in transit,
but every adapter reads the *mutable checkout* at
`$KNOWLEDGE_HOME/corpus/kernel/kernel.md`, writable by the same uid the
agents run as. A local agent or build script edits the kernel in place;
HEAD stays signed, `.sync-state` still reads current, and the tampered
text enters every session. The same actor can replace the user-owned
Claude hook and gain execution at next session start. The planned
`owner == euid` hook check is no defense — attacker and updater share
that euid. This invalidates the "corpus authority is total, therefore
sign it" chain in §System overview: signing secures *transport*, and
nothing currently secures *delivery*. The required choice was binary: either
sync runs under a separate identity that atomically publishes an immutable
versioned artifact adapters read (hooks, keys, state, and markers outside
agent-writable paths), or local agents are explicitly declared fully trusted
and every local-integrity claim in this repo is withdrawn. **Decision
(2026-08-01): option (a) is accepted.** The frozen contract is
`docs/plans/delivery-trust-boundary.md`; no delivery-boundary implementation
has landed, so the vulnerability remains in the current prototype.

Remaining findings, condensed (full text and minimal fixes in the review
artifact; each is carried into the plan or the gap list below):

- **`C2-b` ✅ resolved — a signed corpus exfiltrates arbitrary files by symlink.**
  Commit `kernel/kernel.md` as a symlink to `~/.ssh/id_rsa`; git
  preserves it, `[ -f "$KERNEL" ]` follows it, and the adapters emit the
  target into model context or persist it in global `AGENTS.md`. Signing
  authenticates the malicious symlink rather than preventing it. The
  containment helper now requires a regular, canonically contained file and,
  for Git checkouts, a regular blob tree entry before an adapter reads it.
- **`H1-b` ✅ resolved — corpus content escapes the Codex managed block and
  survives retraction.** A kernel containing the literal end marker
  makes `awk` close the block early, so the following corpus text is
  preserved as user-owned content forever. Reproduced: after two adapter
  runs the injected line was duplicated outside the block, and replacing
  the corpus with a clean kernel did not remove it. Fix: refuse either
  marker as a whole line in kernel content and exit without touching the
  target. The Codex adapter now rejects either marker as a whole line
  before creating or changing its target.
- **`C3-b` — `verify-commit FETCH_HEAD` does not authenticate a
  release.** `init` clones with no verification at all
  (`adapters/sync.sh:40`), and planned verification names no signer set,
  protected ref, repository identity, release sequence, or key
  rotation/revocation. A valid signature is not proof of PR approval; a
  new host can bootstrap from an old signed mirror tip. Fix: pin
  identity/ref/authorized signer, publish a signed release manifest with
  full commit+tree hashes and a monotonic version, verify bootstrap in
  quarantine, use one verification path for init and pull alike.
- **`H2-b` — freshness ≠ currency.** Folded into §Transport & telemetry
  trust above.
- **`H3-b` — the plan accelerated untrusted content before
  authenticating it.** Old step 1 automated post-sync reapplication
  fleet-wide; signing was step 3, status/freshness step 5, mandatory
  mTLS step 6. Fix: the re-ordered plan below.
- **`H4-b` — the hook contract is exploitable under its own checks, and
  contradicts itself.** The contract requires the hooks *directory* be
  non-group/other-writable but never the hook *file*, while the step-1
  verify criterion asserts group-writable hooks are skipped. Preserving
  the caller's `PATH` lets a poisoned environment choose the commands
  hooks run; check-then-execute is a replacement race; parent
  directories above `hooks/` are unchecked. Fix: safe ownership/mode on
  the file and every path component, fixed system `PATH`, reject
  unexpected file types, hooks dir outside agent-writable
  `KNOWLEDGE_HOME`, execute a protected copy.
- **`H5-b` — "drafts never ship" is false, including for Tier A.**
  `kernel.template.md` itself carries `status: draft` and nothing parses
  frontmatter, so a copied template can be injected into every session
  while still draft. The schema, README, and CLAUDE.md all state the
  opposite. Fix: validate before promotion, or withdraw the claim from
  user-facing docs until enforcement exists.
- **`H6-b` — transport requirements are unenforceable in current code.**
  `sync.sh` accepts any git URL scheme and inherits ambient git config;
  nothing rejects plaintext HTTP, pins a CA, or sets an SSH host-key
  policy, and `listen` trusts whatever is piped to stdin. mTLS is
  therefore operator convention, not the architecture requirement
  §Transport & telemetry trust claims. Fix: validate scheme and pin
  identity per remote before clone/pull; gate listener enablement on a
  verified broker configuration.
- **`H7-b` — the "hard limits" kernel is advisory.** Undefined
  precedence against repo-level `CLAUDE.md`/`AGENTS.md` plus the
  session-lifetime gap mean a contradicting repo file or a long-running
  session silently wins. Already tracked in gaps; the review's addition
  is that Tier A must be *documented as guidance, not a security
  boundary*, wherever tool-layer enforcement is absent.
- **`M1-b` — fail-soft conflates outage, corruption, tampering, and
  local error.** Every `git pull` failure becomes "no remote reachable",
  including auth failure, divergent history, index lock, and local
  modification; `status` exits 0 even for "never synced"; state keeps
  only an abbreviated SHA.
- **`M2-b` — writes are not crash-, disk-, or filesystem-safe.** Beyond
  the known truncate-in-place: `mktemp` defaults to `/tmp`, so `mv` onto
  `~/.codex/AGENTS.md` is a cross-device rename that degrades to a
  non-atomic copy — the planned "temp + mv" fix is unsafe unless the
  temp file is created *in the target directory*. State writes are
  non-atomic and stale appends grow without bound.
- **`M3-b` — remote parsing splits, globs, and leaks.** `for url in
  $(remotes)` applies word-splitting and pathname expansion; a URL
  beginning with `-` reaches git as an option (no `--` delimiter); raw
  URLs are logged and written to state, exposing embedded credentials
  and permitting control-character log injection.
- **`M5-b` ✅ — host assumptions break off the happy path.** All three
  scripts abort with `HOME: unbound variable` under `set -u` when `HOME`
  is unset and no override is given (`sync.sh:14`, `install.sh:11`,
  `update-agents-md.sh:10`) — reachable under systemd units and some
  cron setups. The pi adapter ignores `KNOWLEDGE_HOME`, contradicting
  the shared adapter contract; planned permission checks need different
  `stat` invocations on macOS and Linux.
- **`M4-b`, `M6-b`, `M7-b`, `M8-b`, `L1-b`** — no kit/schema versioning
  or rollout story (hosts cannot be "identical" as the diagram assumes);
  heartbeat signing underspecified (canonical encoding, replay window,
  key custody, enrollment) and its verify criterion sits outside the
  kit's no-network-code scope; `verify:` is a latent command-execution
  API with no stated trust model; `install.sh` interpolates `$HOOK`
  unescaped into both JSON and a command string; the kernel token cap is
  policy with no tokenizer or enforcement. All carried to gaps.

## Known gaps (tracked, addressed deliberately — not silently in passing)

- **Tier B has no loader.** `triggers:` frontmatter is declared but
  nothing consumes it; Tier B is pull-by-convention, the retrieval mode
  the thesis rejects.
- **Staleness is invisible downstream.** No provenance (SHA + sync age)
  in the emitted kernel (promoted to plan step 5, 2026-07-29); a stale
  sync truncates the last-good SHA out of `.sync-state` (step 5);
  ff-only against a rewritten remote fails forever while exiting 0;
  nothing executes `verify:` checks or filters `status: draft` despite
  the schema saying so (both still deferred). Per `H5-b` the draft half
  is worse than deferred — it is *contradicted* by README, CLAUDE.md,
  and the schema, and `kernel.template.md` ships `status: draft` itself,
  so a copied template injects into every session while draft. Host-clock
  freshness alone does not detect a freeze (`H2-b`, §Transport &
  telemetry trust).
- **Mirror-only hosts report false freshness (review M3).** A doorbell
  makes such a host pay a canonical-timeout tax, then record
  `ok pull <mirror> <old-sha>` — "ok" while behind by exactly the commit
  that rang the bell. Mitigate: try last-successful remote first (full
  ordered list as fallback) + the SHA hint's `behind` state. Deferral
  premises: this stays deferred only while plan step 7 ships the
  min-pull-interval and SHA hint and step 5 makes `status` surface
  `behind` — descope any of those and M3 re-opens as blocking (without
  them, mirror-only hosts return to silent divergence).
- **Precedence is undefined** between the kernel and repo-level
  `CLAUDE.md`/`AGENTS.md` files that harnesses auto-discover. Until it
  is defined *and* dangerous operations are enforced at the tool layer,
  Tier A is documented guidance, not a security boundary (`H7-b`).
- **No kit or schema versioning (`M4-b`).** Only the corpus syncs;
  adapters and hooks are installed out of band with no kit version,
  schema version, `min-kit-version`, migration path, or fleet
  inventory — so the diagram's "identical on every machine" is an
  assumption, not a property, and new corpus behavior can reach hosts
  whose adapters do not support it.
- **`verify:` is a latent command-execution API (`M7-b`).** The schema
  presents it as a command and invites agents to run it without saying
  whether it is inert metadata, shell code, or sandboxed input. Declare
  it non-executable by default, or specify structured verifier types
  with argument arrays — never `eval` / `sh -c`.
- **`install.sh` interpolates `$HOOK` unescaped (`M8-b`)** into both the
  JSON fragment and a command string, so quotes, backslashes, newlines,
  or shell metacharacters in `KNOWLEDGE_HOME` corrupt the fragment or
  change what SessionStart executes.
- **Heartbeat signing is underspecified (`M6-b`)** — no canonical
  encoding, replay window, key custody, rotation, or host enrollment;
  the recipe grants subscribe-only credentials to hosts that must
  publish; and its verify criterion requires a dashboard consumer the
  kit deliberately does not ship.
- **The kernel token cap is unenforced policy (`L1-b`)** — repeatedly
  called hard, with no tokenizer, model, or release-time check behind
  it.
- **Session-lifetime gap.** Propagation ends at session start; a running
  session keeps its old kernel until restart. No transport fixes this;
  hard limits should also be enforced at the tool layer where possible.
- **`.remotes` and the hooks dir are local trust anchors** (review L3):
  document expected ownership/permissions for both; pre-signing, a
  rewritten `.remotes` is kernel forgery.

## Implementation plan

Re-ordered 2026-07-29 per `H3-b`: the previous order automated fleet-wide
reapplication of *unsigned* corpus content (old step 1) before
authenticating it (old step 3), and allowed the event layer to exist
before the observability and transport controls it leans on. The rule
now is **contain, then authenticate, then apply, then accelerate**. Each
step lands with its verify.

0. **✅ Containment patches (`C2-b`, `H1-b`; implemented 2026-08-01)** —
   independent of everything below and of the `C1-b` decision. A shared
   validator requires a regular file and canonical-path containment before
   the Claude, Codex, or checked pi adapter reads the kernel; Git checkouts
   additionally require a regular blob tree entry. `update-agents-md.sh`
   refuses to run when either marker appears as a whole line in kernel
   content, exiting non-zero without touching the target. This does not solve
   same-uid mutation or check/read races; those remain part of the step-1
   delivery trust boundary.
   → verify: `sh tests/run.sh` covers external and in-corpus kernel symlinks,
   an escaping parent path, both whole-line markers, absent-target
   preservation, a kernel without a final newline, idempotency, both corpus
   layouts, non-Git delivery, and nested-first precedence across all three
   adapters.
1. **Delivery trust boundary (`C1-b`) — accepted design; implementation
   pending (2026-08-01).** Option (a) is selected: synchronization and
   publication run as a dedicated publisher principal; protected control
   state is never agent-readable where secret and never agent-writable; and
   adapters consume an atomically selected immutable version. Protected
   harness wiring is in scope — securing only the corpus would leave the
   hook/configuration replacement attack intact. The exact contract and
   bounded implementation slices are frozen in
   `docs/plans/delivery-trust-boundary.md`. Do not interpret the accepted
   decision as enforcement by the current scripts.
   → verify: under distinct real principals, the agent cannot edit, replace,
   redirect, or unlink the delivered kernel, active selector, control state,
   or mandatory harness wiring, nor bypass injection through direct invocation
   or alternate configuration; a dedicated publisher mutex prevents
   concurrent rollback and selector/state disagreement; selector grammar and
   canonical containment are enforced; unsafe modes/groups/ACLs/ancestors
   fail; local-integrity failure never falls back to the mutable checkout; and
   captured injected bytes match one physical published version on supported
   macOS and Linux configurations.
2. **Verified bootstrap + corpus release authentication (`C3-b`, old H1)** —
   pin repository identity, ref, and authorized signer or CI attestor;
   publish a signed corpus release manifest carrying full commit and tree
   hashes plus a monotonic version; verify in quarantine *before*
   exposing content, on `init` and `pull` alike (`fetch` +
   `verify-commit FETCH_HEAD` + `merge --ff-only`, failing soft to the
   next remote). Note shallow-clone signature reachability (L3).
   → verify: fresh `init` against an unsigned or wrong-signer remote
   exposes no content; unsigned on remote A + signed on remote B →
   converges to B; all-unsigned → keeps last-good and state records why;
   a signed *rollback* to an older manifest version is refused.
3. **Pull mutex + crash-safe writes (old H2, `M2-b`)** — mkdir-based
   lock in `pull` (POSIX-portable; `flock` is not), loser no-ops exit 0.
   All target writes create their temp file **in the target directory**
   — not `/tmp`, whose cross-device rename degrades to a non-atomic
   copy — validate complete content, preserve modes, then `mv`. Same
   discipline for state and marker files, with bounded state history,
   explicit read-only / ENOSPC handling, plus an explicit power-loss
   durability disposition per supported platform. POSIX rename alone proves
   atomic visibility; step 3 must prove an allowed sync primitive, approve a
   dependency/compatibility change, or narrow the guarantee and record the
   residual risk before hardened cutover.
   → verify: two concurrent `pull`s on one protected control root — one
   converges, one no-ops, state never says `stale`; a reader loop during
   repeated adapter runs never observes a truncated managed block;
   a protected control root and adapter target on different filesystems still
   use target-local atomic renames; a full disk fails without destroying the
   target.
4. **Hook trust contract + runner (old C1/H3/H4, `H4-b`)** — `pull`
   gains the post-sync runner inside the step-3 critical section:
   applied-SHA marker (trigger on **HEAD ≠ marker**, not "SHA changed"),
   `<old> <new>` argv, `hooks-failed:<name>` state recording. Safe
   ownership and non-group/other-writable modes are required on the hook
   *file* and every parent path component — the old contract checked
   only the directory, contradicting its own verify criterion — with a
   fixed system `PATH` (not the caller's), rejection of unexpected file
   types, and hooks under the publisher-only control root. Execute a protected
   copy to avoid the check-then-execute race.
   → verify: hook fires when marker ≠ HEAD even with an unchanged
   remote; symlinked, group-writable, and wrong-owner hooks are each
   skipped with a logged reason, as is a hook under a group-writable
   parent; `kill -9` mid-batch → next pull re-runs the batch; a hook
   inheriting a poisoned `PATH` still resolves system binaries.
5. **Fail-loud status + corpus release currency (`M1-b`, `H2-b`, old M3)** —
   record *typed* failure reasons (unreachable / auth / divergent /
   locked / corrupt / policy-rejected) and full commit+tree hashes, not
   an abbreviated SHA; always retain last-good SHA and timestamp;
   `status` exits non-zero on stale, behind, never-synced, and
   hooks-failed. Separate **last transport success** from **verified
   corpus release currency**: compare the monotonic signed release sequence
   from step 2 against the authoritative target, and detect clock
   regression and future timestamps. The kernel provenance banner
   (`corpus <sha> · release <n> · synced <age>`) escalates to a loud
   warning on stale *or* behind — a repeatedly-served old signed tip
   must trip it, which host-clock age alone does not (`H2-b`).
   → verify: forced remote failure → `status` non-zero yet last-good SHA
   still printed and the reason typed; a mirror pinned at an old signed
   tip trips the banner despite fresh pull timestamps; a rolled-back
   host clock is reported, not silently trusted; emitted kernel shows
   current sha/release/age in a fresh session's context.
6. **Transport policy enforcement (`H6-b`, `M3-b`)** — validate remote
   configuration before clone/pull: allowlist schemes (reject plaintext
   HTTP and `git://`), require a pinned/verified CA or SSH host-key
   policy per remote, read `.remotes` one validated line at a time
   (`IFS= read -r`, no word-splitting or globbing), reject
   control-characters and option-like entries, pass `--` to git, and
   redact credentials from logs and state. Enforce ownership and modes
   on `.remotes` (L3).
   → verify: an `http://` remote and a `-upload-pack=…` entry are both
   refused before git runs; a URL with embedded credentials never
   appears in logs or `.sync-state`; a group-writable `.remotes` is
   refused.
7. **`sync.sh listen` (old M1/M2/M4)** — gated on steps 2 and 5, per the
   ordering constraint: stdin loop, minimum pull interval, optional
   untrusted SHA-hint check with `behind` state, heartbeat file,
   `IFS= read -r`, no payload logging. Ships disabled by default if
   either dependency has not landed.
   → verify: 100-line burst → 1 pull; 1-line/sec drip → pulls at
   min-interval only; hint ≠ HEAD after all remotes → `behind` in state;
   broker-client death → clean exit, supervisor restarts.
8. **Operator recipe doc (old L2, `M6-b`)** — webhook → bridge → broker
   cookbook (HMAC-validated bridge, topic-scoped per-host credentials
   with **separate publish and subscribe ACLs** — hosts must publish
   heartbeats, which the old recipe's subscribe-only grant forbade —
   **mTLS both directions, mandatory** per §Transport & telemetry trust;
   `allow_anonymous false`, no `#` grants, broker not shared with
   higher-stakes topics unreviewed), kept out of kit code. Specify the
   heartbeat as a versioned canonical message with a replay window,
   per-host trust store, and enrollment/rotation/revocation lifecycle,
   and state plainly that consumer-side verification lives in the recipe
   because the kit ships no network code. Note that a retained MQTT
   message rings a spurious doorbell on every re-subscribe after broker
   restart — harmless (idempotent, rate-limited), but expect "event
   received, no change" heartbeat entries rather than debugging them.
9. **Adversarial test matrix (`M9-b`)** — the kit has no automated
   suite, and the old verify criteria omitted the cases that matter
   most. Cover: unverified fresh clone, wrong-but-valid signer, changed
   remote `HEAD`, signed rollback, worktree mutation, corpus symlink,
   marker injection, key rotation, clock rollback, read-only and full
   disk, missing `HOME`, paths with spaces, and macOS/Linux parity —
   plus a fresh-host end-to-end test comparing emitted bytes against the
   signed release tree.
   → verify: the matrix runs green on macOS and Linux; `listen` stays
   disabled until it does.

Deferred (tracked in gaps): Tier B trigger loader, `verify.sh` corpus
checker, draft filtering and the `H5-b` doc-vs-reality contradiction
(withdraw the claim or enforce it), precedence statement in the kernel
template, mirror-first remote memory (M3), kit/schema versioning
(`M4-b`), `verify:` trust model (`M7-b`), `install.sh` escaping
(`M8-b`), token-cap enforcement (`L1-b`), and `M5-b`'s missing-`HOME`
and pi-`KNOWLEDGE_HOME` fixes (small, but they belong with step 9's
host-matrix work rather than scattered). (The kernel provenance banner
was deferred until 2026-07-29, promoted into the old step 5 by the MITM
hardening, then re-scoped into step 5 above once `H2-b` showed host-clock
age alone cannot detect a freeze.)
