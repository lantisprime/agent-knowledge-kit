# Architecture & event-propagation plan

Status: design accepted **adopt-with-changes** after adversarial review
(Kimi K3 via pi, 2026-07-28); reviewer signed off **CONSENSUS** on this
doc in a second round the same day, and its five non-blocking notes are
folded in. Amended 2026-07-29 with MITM hardening (§Transport & telemetry
trust; plan step 5 scope) — this amendment postdates the consensus round
and has not been re-reviewed. This doc records the architecture, the
settled decisions, the proposed event layer, the review's required
changes, and the implementation plan. Public repo — everything here is
generic; environment specifics belong in your corpus.

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
   freshness threshold (plan step 5) detects a freeze using only the
   host's own clock — no attacker-controlled network in the loop — and
   surfaces it where it matters: in the agent's context. A MITM can
   still freeze a host; it can no longer make the freeze invisible to
   the agent. Protects the *agent*.

## Known gaps (tracked, addressed deliberately — not silently in passing)

- **Tier B has no loader.** `triggers:` frontmatter is declared but
  nothing consumes it; Tier B is pull-by-convention, the retrieval mode
  the thesis rejects.
- **Staleness is invisible downstream.** No provenance (SHA + sync age)
  in the emitted kernel (promoted to plan step 5, 2026-07-29); a stale
  sync truncates the last-good SHA out of `.sync-state` (step 5);
  ff-only against a rewritten remote fails forever while exiting 0;
  nothing executes `verify:` checks or filters `status: draft` despite
  the schema saying so (both still deferred).
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
  `CLAUDE.md`/`AGENTS.md` files that harnesses auto-discover.
- **Session-lifetime gap.** Propagation ends at session start; a running
  session keeps its old kernel until restart. No transport fixes this;
  hard limits should also be enforced at the tool layer where possible.
- **`.remotes` and the hooks dir are local trust anchors** (review L3):
  document expected ownership/permissions for both; pre-signing, a
  rewritten `.remotes` is kernel forgery.

## Implementation plan

Blocking order per review; each step lands with its verify.

1. **Hook trust contract + runner (C1, H3, H4) + atomic Codex write
   (L1)** — `pull` gains the post-sync runner: mkdir-lock critical
   section, applied-SHA marker, symlink/ownership/perms refusal,
   allowlist-scrubbed env, `<old> <new>` argv, `hooks-failed:<name>`
   state recording. `update-agents-md.sh` switches to temp file + `mv`
   in the same step: the runner's first consumer is the Codex adapter,
   and automating a truncate-in-place write fleet-wide, even briefly, is
   an avoidable exposure.
   → verify: hook fires when marker ≠ HEAD even with unchanged remote;
   symlinked + group-writable hooks are skipped with a logged reason;
   kill -9 mid-batch → next pull re-runs the batch; a reader loop during
   repeated adapter runs never observes a truncated managed block.
2. **Pull mutex (H2)** — mkdir lock in `pull`; loser no-ops exit 0.
   → verify: two concurrent `pull`s on one `KNOWLEDGE_HOME`: one
   converges, one no-ops, state file never says `stale`.
3. **Signed-commit verification (H1)** — `fetch` + `verify-commit
   FETCH_HEAD` + `merge --ff-only`, falling soft to next remote; note
   shallow-clone/signature reachability (L3).
   → verify: unsigned commit on remote A + signed on remote B → host
   converges to B; all-unsigned → keeps last-good, state records why.
4. **`sync.sh listen` (M1, M2, M4)** — stdin loop, min-pull-interval,
   optional SHA-hint check with `behind` state, heartbeat file,
   `IFS= read -r`, no payload logging.
   → verify: 100-line burst → 1 pull; 1-line/sec drip → pulls at
   min-interval only; hint ≠ HEAD after all remotes → `behind` in state;
   broker-client death → clean exit, supervisor restarts.
5. **State file + `status` + host-side freshness (fail-loud half)** —
   always retain last-good SHA + timestamp; `status` exits non-zero on
   stale/behind/hooks-failed. Kernel provenance banner: adapters prepend
   `corpus <sha> · synced <age>` to the emitted kernel, escalating to a
   loud stale warning past a freshness threshold — computed from the
   host's own clock, so a network MITM cannot suppress it (§Transport &
   telemetry trust). Heartbeats carry a per-host signature (or the
   authoritative fleet check is documented as out-of-band), the broker
   dashboard is explicitly a hint.
   → verify: forced remote failure → `status` non-zero yet last-good SHA
   still printed; emitted kernel shows current SHA + age; with
   `.sync-state` aged past threshold, a fresh session's context contains
   the stale warning; a heartbeat with a bad signature is rejected by
   the dashboard consumer.
6. **Operator recipe doc (L2)** — webhook → bridge → broker cookbook
   (HMAC-validated bridge, subscribe-only topic-scoped per-host
   credentials, **mTLS both directions — mandatory, per §Transport &
   telemetry trust**; `allow_anonymous false`, no `#` grants, broker not
   shared with higher-stakes topics unreviewed), kept out of kit code. Note in the MQTT section: a
   retained message rings a spurious doorbell on every re-subscribe
   after broker restart — harmless (idempotent, rate-limited), but
   expect "event received, no change" heartbeat entries rather than
   debugging them.

Deferred (tracked in gaps): Tier B trigger loader, `verify.sh` corpus
checker, draft filtering, precedence statement in the kernel template,
mirror-first remote memory (M3). (The kernel provenance banner +
freshness threshold was deferred here until 2026-07-29, then promoted
into step 5 by the MITM hardening — host-side staleness detection is the
agent-facing defense against an invisible freeze.)
