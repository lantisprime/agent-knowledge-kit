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
║  HOST (identical on every machine)                                           ║
║                                                                              ║
║   event fast-path (doorbell) ─┐                                              ║
║   cron floor ─────────────────┼──▶ sync.sh pull   (ff-only, fail-soft)       ║
║                               │        │ on success, HEAD ≠ applied-marker   ║
║                               │        ▼                                     ║
║                               │   hooks/post-sync/*  (operator-installed)    ║
║                               │        │                                     ║
║   $KNOWLEDGE_HOME/corpus/kernel/kernel.md ◀── the ONE path adapters resolve  ║
║        ├─▶ claude: SessionStart hook  ──▶ Claude Code                        ║
║        ├─▶ codex:  managed AGENTS.md block ──▶ Codex CLI                     ║
║        └─▶ pi:     --append-system-prompt ──▶ pi                             ║
╚═════════════════════════════════════════╤════════════════════════════════════╝
                                          ▼
              ┌────────────────────────────────────────────────────┐
              │  AGENT CONTEXT WINDOW                              │
              │  Tier A  kernel ~1–2k tok   PUSH    every session  │
              │  Tier B  corpus/docs/       trigger (loader: gap)  │
              │  Tier C  search / query     PULL                   │
              └────────────────────────────────────────────────────┘
```

The kernel path is the entire integration contract: everything upstream
exists to place one file at one path; every adapter is a thin reader of
it. That is why adding a harness is trivial — and why corpus authority is
total, which drives the security decisions below.

## Settled design decisions

- **Events trigger, git transports.** Any event fast-path (webhook,
  message bus) is a *doorbell* that runs `sync.sh pull`; git stays the
  source of truth and the event payload is never trusted as content. The
  event path must never be the only path — a cron floor always runs
  underneath, so a dead broker degrades convergence latency, never
  convergence.
- **Multi-remote decouples three concerns.** *Availability*: the ordered
  remote list (canonical first, mirrors after) — implemented.
  *Authority*: every remote can currently rewrite the kernel; the fix is
  signed commits verified before `merge --ff-only`, failing soft to the
  next remote — mirrors can then withhold updates but not author them.
  *Confidentiality*: the kit mirrors publicly by design; a **corpus**
  must not be mirrored outside its trust boundary without an explicit,
  recorded decision (don't mirror / encrypt at rest / accept custody).
- **Fail-soft for consumers, fail-loud for operators.** `pull` keeps
  exiting 0 on unreachable remotes; `status` is the monitoring surface
  and must exit non-zero when stale.
- **Doorbell semantics.** An event means only "pull now". Because the
  consumer converges to git HEAD regardless of message content, ordering,
  dedup, replay, and exactly-once are non-requirements: at-least-once +
  idempotent `git pull --ff-only` gives correctness; events only buy
  latency.
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
- **Scope (review L2).** The kit ships `listen`, the post-sync hook
  runner, and the cron floor — no network code. The webhook → bridge →
  broker chain is a documented operator recipe, not kit code: the moment
  the kit ships a bridge it owns a network-facing security surface
  forever.

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

### Post-sync hooks — `$KNOWLEDGE_HOME/hooks/post-sync/*`

Run by `pull` so downstream consumers re-apply the kernel (first user:
the Codex adapter, whose `~/.codex/AGENTS.md` snapshot currently freezes
because nothing re-runs it).

**Trust contract (review C1 — the design's only code-execution story;
non-negotiable):**

- Hooks are installed by the operator out of band. `sync.sh` never
  copies, links, or executes anything from the corpus into the hook
  path — synced content must have no route to execution (git preserves
  exec bits; "the corpus is only markdown" is not a property git
  enforces).
- At execution time: skip symlinks; require regular files owned by the
  euid running pull; require the hooks dir itself to be a real,
  non-group/other-writable directory.
- Hooks run with a scrubbed minimal environment — an explicit allowlist
  (at minimum `PATH`, `HOME`, `KNOWLEDGE_HOME`; a scrub that drops
  `KNOWLEDGE_HOME` breaks the hooks the runner exists to run) — and
  receive exactly `<old-sha> <new-sha>` as argv, nothing else.

**Trigger invariant (review H3):** not "SHA changed" but
"**HEAD ≠ applied-marker**". `pull` runs the hook batch whenever HEAD
differs from `$KNOWLEDGE_HOME/.hooks-applied`, and writes the marker only
after *all* hooks succeed. Otherwise a crash mid-batch on a low-churn
corpus freezes downstream consumers until the next commit — the
frozen-Codex bug resurrected in a smaller window. Hooks must therefore be
idempotent.

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

Signed-commit verification lands **before or with** `listen`. Events
collapse the forged-kernel detection window from cron cadence (minutes–
hours; an operator can catch it) to seconds, and the kernel is the most
leverage-dense injection surface in the architecture. If verification
slips, `listen` ships disabled-by-default with that dependency documented.

### Transport & telemetry trust (MITM hardening, 2026-07-29)

A network MITM cannot inject content (signed commits), trigger execution
(hook contract), or redirect a fetch (hint checked, never consumed) — but
unhardened, it can make a *freeze invisible* via the composite attack in
the settled-decisions bullet above. Three defenses, protecting two
different parties:

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
   be established (plan step 6). Until that lands, the MITM amendment's
   agent-facing defense is aspirational — defenses 1 and 2 stand.

## Second adversarial review (Codex gpt-5.6-sol, 2026-07-29)

Verdict **adopt-with-changes**; blocking before deployment or before
enabling the event layer: `C1-b`–`C3-b`, `H1-b`–`H7-b`, `M1-b`, `M2-b`,
`M9-b`. Three findings were independently reproduced in this repo before
folding (marked ✅ below); the rest are accepted on the review's evidence.

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
nothing currently secures *delivery*. Resolution is binary and must be
recorded: either sync runs under a separate identity that atomically
publishes an immutable versioned artifact adapters read (hooks, keys,
state, and markers outside agent-writable paths), or local agents are
explicitly declared fully trusted and every local-integrity claim in
this repo is withdrawn.

Remaining findings, condensed (full text and minimal fixes in the review
artifact; each is carried into the plan or the gap list below):

- **`C2-b` ✅ — a signed corpus exfiltrates arbitrary files by symlink.**
  Commit `kernel/kernel.md` as a symlink to `~/.ssh/id_rsa`; git
  preserves it, `[ -f "$KERNEL" ]` follows it, and the adapters emit the
  target into model context or persist it in global `AGENTS.md`. Signing
  authenticates the malicious symlink rather than preventing it. This
  doc anticipated exec bits (§hook trust contract) but not symlinked
  *content*. Fix: reject git tree mode `120000` and every non-regular
  file in an injected bundle; `lstat` + canonical-path containment
  immediately before reading.
- **`H1-b` ✅ — corpus content escapes the Codex managed block and
  survives retraction.** A kernel containing the literal end marker
  makes `awk` close the block early, so the following corpus text is
  preserved as user-owned content forever. Reproduced: after two adapter
  runs the injected line was duplicated outside the block, and replacing
  the corpus with a clean kernel did not remove it. Fix: refuse either
  marker as a whole line in kernel content and exit without touching the
  target; better, drop in-band delimiters for untrusted payloads.
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
  premises: this stays deferred only while plan step 4 ships the
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

0. **Containment patches (`C2-b`, `H1-b`)** — independent of everything
   below and of the `C1-b` decision; both holes are reproducible today
   and cost a few lines each. Every adapter `lstat`s the kernel, refuses
   non-regular files (git tree mode `120000`), and checks canonical-path
   containment before reading. `update-agents-md.sh` refuses to run when
   either marker appears as a whole line in kernel content, exiting
   non-zero without touching the target.
   → verify: a symlinked `kernel.md` pointing outside the corpus is
   refused by all three adapters with a logged reason and nothing is
   emitted; a kernel containing the end marker leaves `AGENTS.md`
   byte-identical and exits non-zero.
1. **Delivery trust boundary — decide and record (`C1-b`)** — the
   blocking architectural fork; nothing downstream is meaningful until
   it is settled. Either (a) sync runs under a separate identity and
   atomically publishes an immutable versioned artifact that adapters
   read, with hooks, signing keys, state, and applied markers outside
   agent-writable paths; or (b) local agents are declared fully trusted,
   recorded here as an accepted risk, and every local-integrity claim in
   README/CLAUDE.md/this doc is withdrawn — signing then buys transport
   integrity only.
   → verify: with the decision applied, an in-place edit of the
   delivered kernel by the agent uid is either impossible (a) or
   documented as in-scope-for-the-threat-model (b); no doc claims
   otherwise.
2. **Verified bootstrap + release authentication (`C3-b`, old H1)** —
   pin repository identity, ref, and authorized signer or CI attestor;
   publish a signed release manifest carrying full commit and tree
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
   discipline for state and marker files, with bounded state history and
   explicit read-only / ENOSPC handling.
   → verify: two concurrent `pull`s on one `KNOWLEDGE_HOME` — one
   converges, one no-ops, state never says `stale`; a reader loop during
   repeated adapter runs never observes a truncated managed block;
   `KNOWLEDGE_HOME` on a different filesystem than the target still
   renames atomically; a full disk fails without destroying the target.
4. **Hook trust contract + runner (old C1/H3/H4, `H4-b`)** — `pull`
   gains the post-sync runner inside the step-3 critical section:
   applied-SHA marker (trigger on **HEAD ≠ marker**, not "SHA changed"),
   `<old> <new>` argv, `hooks-failed:<name>` state recording. Safe
   ownership and non-group/other-writable modes are required on the hook
   *file* and every parent path component — the old contract checked
   only the directory, contradicting its own verify criterion — with a
   fixed system `PATH` (not the caller's), rejection of unexpected file
   types, and the hooks directory outside agent-writable
   `KNOWLEDGE_HOME`. Execute a protected copy to avoid the
   check-then-execute race.
   → verify: hook fires when marker ≠ HEAD even with an unchanged
   remote; symlinked, group-writable, and wrong-owner hooks are each
   skipped with a logged reason, as is a hook under a group-writable
   parent; `kill -9` mid-batch → next pull re-runs the batch; a hook
   inheriting a poisoned `PATH` still resolves system binaries.
5. **Fail-loud status + release currency (`M1-b`, `H2-b`, old M3)** —
   record *typed* failure reasons (unreachable / auth / divergent /
   locked / corrupt / policy-rejected) and full commit+tree hashes, not
   an abbreviated SHA; always retain last-good SHA and timestamp;
   `status` exits non-zero on stale, behind, never-synced, and
   hooks-failed. Separate **last transport success** from **verified
   release currency**: compare the monotonic signed release sequence
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
