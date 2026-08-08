// ui/lib_test.mjs — Node built-in runner tests for the pure logic in
// ui/lib.mjs. Zero deps. Run with: node --test ui/lib_test.mjs

import { test } from "node:test";
import assert from "node:assert/strict";

import {
  DIFF_MAX_LINES, DIFF_MAX_BYTES, DIFF_MAX_PRODUCT,
  encodePath, countWords, countBytes, parseTriggers, parseLinksJSON, formatLinksJSON,
  isExpectedContentHashShape, initialSaveState, afterSave, onConflict,
  lineDiff, manifestDiff,
  conflictsPath, conflictPath, resolveConflictPath,
  hostsPath, hostResyncPath, DARK_AFTER_MS, SKEW_TOLERANCE_MS,
  fleetStatus, formatAge, fleetRowCells, fleetSummary, parseFleetResponse,
  toFormFields, metaDiffRows, resolvePayload,
} from "./lib.mjs";

// ---------- encodePath ----------

test("encodePath escapes collection and family independently", () => {
  assert.equal(encodePath("docs", "ops?#% name"), "/api/docs/docs/ops%3F%23%25%20name");
  assert.equal(encodePath("kernel", "kernel"), "/api/docs/kernel/kernel");
  assert.equal(encodePath("docs", "with/slash"), "/api/docs/docs/with%2Fslash");
});

test("encodePath encodes known valid idents (no special chars)", () => {
  // The store's validateIdent accepts ASCII letter/digit/_/- so the
  // URL form is unchanged.
  assert.equal(encodePath("docs", "normal-name_1"), "/api/docs/docs/normal-name_1");
});

// ---------- countWords: pin to Go strings.Fields semantics ----------
// Critical: inputs are JS escape sequences (no literal invisibles),
// verified per char below.

test("countWords basic splits", () => {
  assert.equal(countWords(""), 0);
  assert.equal(countWords("a"), 1);
  assert.equal(countWords("a b"), 2);
  assert.equal(countWords("  a   b  "), 2);
  assert.equal(countWords("a\tb\nc"), 3);
});

test("countWords U+0085 (NEL) and U+00A0 (NBSP) split", () => {
  // U+0085 Next Line — White_Space property yes
  assert.equal(countWords("a\u0085b"), 2);
  // U+00A0 No-Break Space — White_Space property yes
  assert.equal(countWords("a\u00A0b"), 2);
});

test("countWords U+200B (ZWSP) and U+FEFF (BOM) do NOT split", () => {
  // U+200B Zero Width Space — NOT White_Space; Go strings.Fields
  // splits on unicode.IsSpace, which excludes U+200B. JS /\s/ has
  // historically excluded U+200B as well; our /\p{White_Space}/u
  // also excludes it. Pin that.
  assert.equal(countWords("a\u200Bb"), 1);
  assert.equal(countWords("a\uFEFFb"), 1);
});

// ---------- countBytes ----------

test("countBytes UTF-8 byte length matches multibyte input", () => {
  // "é" is 0xC3 0xA9 = 2 bytes
  assert.equal(countBytes("é"), 2);
  // "☃" is 0xE2 0x98 0x83 = 3 bytes
  assert.equal(countBytes("☃"), 3);
  // empty / whitespace
  assert.equal(countBytes(""), 0);
  assert.equal(countBytes("a"), 1);
});

// ---------- parseTriggers ----------

test("parseTriggers splits, trims, drops empties", () => {
  assert.deepEqual(parseTriggers(""), []);
  assert.deepEqual(parseTriggers("a"), ["a"]);
  assert.deepEqual(parseTriggers("a,b,c"), ["a", "b", "c"]);
  assert.deepEqual(parseTriggers(" a , b ,  "), ["a", "b"]);
});

// ---------- document links ----------

test("parseLinksJSON accepts empty and exact link objects", () => {
  assert.deepEqual(parseLinksJSON(""), { links: [], error: "" });
  const links = [
    { relation: "reference", collection: "docs", family_id: "runbook", version: 3 },
    { relation: "supersedes", collection: "docs", family_id: "old", version: 1 },
  ];
  assert.deepEqual(parseLinksJSON(JSON.stringify(links)), { links, error: "" });
});

test("parseLinksJSON rejects malformed shapes before save", () => {
  for (const raw of [
    "{",
    `{}`,
    `[{"relation":"mentions","collection":"docs","family_id":"x","version":1}]`,
    `[{"relation":"reference","collection":"","family_id":"x","version":1}]`,
    `[{"relation":"reference","collection":"docs","family_id":"","version":1}]`,
    `[{"relation":"reference","collection":"docs","family_id":"x","version":0}]`,
  ]) {
    const got = parseLinksJSON(raw);
    assert.equal(got.links, null, raw);
    assert.notEqual(got.error, "", raw);
  }
});

test("formatLinksJSON emits empty text or stable pretty JSON", () => {
  assert.equal(formatLinksJSON([]), "");
  assert.equal(formatLinksJSON(null), "");
  const links = [{ relation: "reference", collection: "docs", family_id: "x", version: 2 }];
  assert.equal(formatLinksJSON(links), JSON.stringify(links, null, 2));
});

// ---------- isExpectedContentHashShape ----------

test("isExpectedContentHashShape accepts sha256:<64 lowercase hex>", () => {
  assert.equal(isExpectedContentHashShape("sha256:" + "a".repeat(64)), true);
  assert.equal(isExpectedContentHashShape("sha256:" + "0".repeat(64)), true);
  assert.equal(isExpectedContentHashShape("sha256:" + "f".repeat(64)), true);
});

test("isExpectedContentHashShape rejects bad shapes", () => {
  assert.equal(isExpectedContentHashShape(""), false);
  assert.equal(isExpectedContentHashShape(null), false);
  assert.equal(isExpectedContentHashShape(undefined), false);
  assert.equal(isExpectedContentHashShape(123), false);
  // Uppercase hex rejected.
  assert.equal(isExpectedContentHashShape("sha256:" + "A".repeat(64)), false);
  // Wrong prefix.
  assert.equal(isExpectedContentHashShape("sha512:" + "a".repeat(64)), false);
  // Truncated.
  assert.equal(isExpectedContentHashShape("sha256:" + "a".repeat(63)), false);
});

// ---------- save-state transitions ----------

test("initialSaveState: no base, not exists", () => {
  const s = initialSaveState();
  assert.equal(s.baseVersion, null);
  assert.equal(s.exists, false);
});

test("afterSave advances base to response version and marks exists", () => {
  const created = afterSave(initialSaveState(), 1);
  assert.equal(created.baseVersion, 1);
  assert.equal(created.exists, true);
  const next = afterSave(created, 2);
  assert.equal(next.baseVersion, 2);
  assert.equal(next.exists, true);
});

test("onConflict leaves state unchanged", () => {
  const s = { baseVersion: 5, exists: true };
  const out = onConflict(s);
  assert.equal(out, s);
});

test("existing→save→save advances base twice", () => {
  // An existing family's first save moves base from 5 → 7, the
  // second save moves base from 7 → 11. exists stays true through
  // both transitions. This is the once-created case from the
  // brief: a second save must never fall back to omitting
  // base_version (the last-writer-wins opt-out), and a once-saved
  // family must carry base_version on every subsequent save.
  let s = { baseVersion: 5, exists: true };
  s = afterSave(s, 7);
  assert.equal(s.baseVersion, 7);
  assert.equal(s.exists, true);
  s = afterSave(s, 11);
  assert.equal(s.baseVersion, 11);
  assert.equal(s.exists, true);
});

// ---------- lineDiff: LCS correctness ----------

test("lineDiff equal inputs return all eq rows", () => {
  const r = lineDiff(["a", "b"], ["a", "b"]);
  assert.equal(r.tooLarge, undefined);
  assert.deepEqual(r.rows, [
    { tag: "eq", text: "a" },
    { tag: "eq", text: "b" },
  ]);
});

test("lineDiff pure insert", () => {
  const r = lineDiff(["a"], ["a", "b"]);
  assert.deepEqual(r.rows, [
    { tag: "eq", text: "a" },
    { tag: "add", text: "b" },
  ]);
});

test("lineDiff pure delete", () => {
  const r = lineDiff(["a", "b"], ["a"]);
  assert.deepEqual(r.rows, [
    { tag: "eq", text: "a" },
    { tag: "del", text: "b" },
  ]);
});

test("lineDiff replace", () => {
  const r = lineDiff(["a", "x", "c"], ["a", "y", "c"]);
  assert.deepEqual(r.rows, [
    { tag: "eq", text: "a" },
    { tag: "del", text: "x" },
    { tag: "add", text: "y" },
    { tag: "eq", text: "c" },
  ]);
});

test("lineDiff interleaved", () => {
  const r = lineDiff(["a", "b", "c"], ["c", "b", "a"]);
  // LCS length is 1 here: the arrays share all three elements but
  // share no order-preserving subsequence of length > 1. The walk
  // finds "c" as the common element.
  const adds = r.rows.filter(x => x.tag === "add").map(x => x.text);
  const dels = r.rows.filter(x => x.tag === "del").map(x => x.text);
  const eqs = r.rows.filter(x => x.tag === "eq").map(x => x.text);
  assert.deepEqual(eqs, ["c"]);
  assert.deepEqual(new Set(adds), new Set(["a", "b"]));
  assert.deepEqual(new Set(dels), new Set(["a", "b"]));
});

// ---------- lineDiff: budget fallback ----------

test("lineDiff tooLarge on lines_a > DIFF_MAX_LINES", () => {
  const a = new Array(DIFF_MAX_LINES + 1).fill("x");
  const b = ["x"];
  const r = lineDiff(a, b);
  assert.equal(r.tooLarge, true);
});

test("lineDiff tooLarge on lines_b > DIFF_MAX_LINES", () => {
  const a = ["x"];
  const b = new Array(DIFF_MAX_LINES + 1).fill("x");
  const r = lineDiff(a, b);
  assert.equal(r.tooLarge, true);
});

test("lineDiff tooLarge on byte cap", () => {
  // Use a line of 600 bytes repeated so the total exceeds 512 KiB
  // but stays under the 8000-line cap.
  const big = "x".repeat(600);
  const a = new Array(1000).fill(big); // 600_000 bytes > 512 KiB
  const r = lineDiff(a, a);
  assert.equal(r.tooLarge, true);
});

test("lineDiff byte budget uses UTF-8 not UTF-16 units", () => {
  // Each "é" is 1 UTF-16 unit (2 bytes UTF-8). A line of 600 "é"s is
  // 1200 UTF-8 bytes. With 500 such lines + 499 newlines we get
  // 500*1200 + 499 = 600_499 UTF-8 bytes — over the 512 KiB cap.
  // The old per-line .length sum would report only 500*600 = 300_000
  // UTF-16 units, under the cap, and the test would fail.
  const emojiLine = "é".repeat(600); // 1200 UTF-8 bytes per line
  const lines = new Array(500).fill(emojiLine);
  const r = lineDiff(lines, lines);
  assert.equal(r.tooLarge, true);
});

test("lineDiff byte budget counts joining newlines", () => {
  // Two lines totalling EXACTLY the 512 KiB cap; only the single
  // joining "\n" pushes the body to cap+1. An implementation that
  // sums line bytes without separators measures exactly the cap
  // (not over, budget check is strict >) and would run the diff.
  const half = "x".repeat(256 * 1024); // 262144 bytes; ×2 = the cap
  const lines = [half, half];
  const r = lineDiff(lines, lines);
  assert.equal(r.tooLarge, true);
});

test("lineDiff tooLarge on product cap", () => {
  // 2000 * 2000 = 4_000_000 (at cap, allowed). 2000 * 2001 over.
  const a = new Array(2000).fill("a");
  const b = new Array(2001).fill("a");
  const r = lineDiff(a, b);
  assert.equal(r.tooLarge, true);
});

test("lineDiff budget constants exported", () => {
  assert.equal(DIFF_MAX_LINES, 8000);
  assert.equal(DIFF_MAX_BYTES, 512 * 1024);
  assert.equal(DIFF_MAX_PRODUCT, 4_000_000);
});

// ---------- manifestDiff ----------

test("manifestDiff classifies added/removed/changed", () => {
  const oldM = { docs: [
    { path: "a/x.md", family_id: "x", version: 1, sha256: "1" },
    { path: "a/y.md", family_id: "y", version: 2, sha256: "2" },
    { path: "a/z.md", family_id: "z", version: 3, sha256: "3" },
  ]};
  const newM = { docs: [
    { path: "a/x.md", family_id: "x", version: 1, sha256: "1" },          // unchanged
    { path: "a/y.md", family_id: "y", version: 2, sha256: "DIFF" },       // changed
    { path: "a/w.md", family_id: "w", version: 1, sha256: "w1" },          // added
    // a/z.md removed
  ]};
  const { added, removed, changed } = manifestDiff(oldM, newM);
  assert.equal(added.length, 1);
  assert.equal(added[0].path, "a/w.md");
  assert.equal(removed.length, 1);
  assert.equal(removed[0].path, "a/z.md");
  assert.equal(changed.length, 1);
  assert.equal(changed[0].path, "a/y.md");
  assert.equal(changed[0].old.sha256, "2");
  assert.equal(changed[0].next.sha256, "DIFF");
});

test("manifestDiff empty old + populated new yields all added", () => {
  const newM = { docs: [{ path: "p", family_id: "f", version: 1, sha256: "h" }] };
  const { added, removed, changed } = manifestDiff(null, newM);
  assert.equal(added.length, 1);
  assert.equal(removed.length, 0);
  assert.equal(changed.length, 0);
});

test("manifestDiff empty new yields all removed", () => {
  const oldM = { docs: [{ path: "p", family_id: "f", version: 1, sha256: "h" }] };
  const { added, removed, changed } = manifestDiff(oldM, null);
  assert.equal(added.length, 0);
  assert.equal(removed.length, 1);
  assert.equal(changed.length, 0);
});

// ---------- conflictsPath / conflictPath / resolveConflictPath ----------

test("conflictsPath no status is literal /api/conflicts", () => {
  // The wire form is pinned: empty / undefined / null / "" all yield
  // the unfiltered path with no query string. The handler treats
  // absent status as "all".
  assert.equal(conflictsPath(""), "/api/conflicts");
  assert.equal(conflictsPath(undefined), "/api/conflicts");
  assert.equal(conflictsPath(null), "/api/conflicts");
});

test("conflictsPath encodes status values", () => {
  assert.equal(conflictsPath("open"), "/api/conflicts?status=open");
  assert.equal(conflictsPath("resolved"), "/api/conflicts?status=resolved");
  // Defensive: a status with characters the URL grammar would
  // mangle must still produce a valid query value.
  assert.equal(conflictsPath("with space"), "/api/conflicts?status=with%20space");
  assert.equal(conflictsPath("a&b"), "/api/conflicts?status=a%26b");
});

test("conflictPath / resolveConflictPath use encodeURIComponent on id", () => {
  assert.equal(conflictPath(1), "/api/conflicts/1");
  assert.equal(conflictPath("42"), "/api/conflicts/42");
  // A numeric-looking id with characters that need encoding still
  // encodes — the server validates the parsed int, but the wire
  // form must never leave an unescaped byte.
  assert.equal(resolveConflictPath(7), "/api/conflicts/7/resolve");
  // resolveConflictPath composes on conflictPath, not by string
  // concatenation in the caller.
  assert.equal(resolveConflictPath(2), conflictPath(2) + "/resolve");
});

// ---------- toFormFields ----------

test("toFormFields full doc", () => {
  const doc = {
    title: "T", status: "active", tier: "B", owner: "alice",
    audience: "ops", triggers: ["a", "b"],
    links: [{ relation: "reference", collection: "docs", family_id: "x", version: 1 }],
    body: "body",
  };
  assert.deepEqual(toFormFields(doc), {
    title: "T", status: "active", tier: "B", owner: "alice",
    audience: "ops", triggers: "a, b", links: JSON.stringify(doc.links, null, 2), body: "body",
  });
});

test("toFormFields nullish fields become empty strings; status defaults to draft", () => {
  const doc = { title: null, status: undefined, triggers: null };
  const out = toFormFields(doc);
  assert.equal(out.title, "");
  assert.equal(out.status, "draft");
  assert.equal(out.triggers, "");
  assert.equal(out.body, "");
  assert.equal(out.tier, "");
  assert.equal(out.owner, "");
  assert.equal(out.audience, "");
  assert.equal(out.links, "");
});

test("toFormFields null docLike → all-empty shape with status draft", () => {
  const out = toFormFields(null);
  assert.deepEqual(out, {
    title: "", status: "draft", tier: "", owner: "", audience: "",
    triggers: "", links: "", body: "",
  });
  // undefined is the same shape.
  assert.deepEqual(toFormFields(undefined), out);
});

test("toFormFields triggers array joins with ', '", () => {
  assert.equal(toFormFields({ triggers: ["deploy", "rollback"] }).triggers, "deploy, rollback");
  assert.equal(toFormFields({ triggers: ["only"] }).triggers, "only");
  // Empty array is the empty string, not the string "[]".
  assert.equal(toFormFields({ triggers: [] }).triggers, "");
});

// ---------- metaDiffRows ----------

test("metaDiffRows identical → all same:true", () => {
  const a = { title: "T", status: "active", tier: "B", owner: "x", audience: "ops", triggers: ["a", "b"], body: "ignored" };
  const rows = metaDiffRows(a, { ...a });
  assert.equal(rows.length, 7);
  for (const r of rows) {
    assert.equal(r.same, true, `field ${r.field}`);
    assert.equal(r.a, r.b, `field ${r.field}`);
  }
});

test("metaDiffRows EXACT field order and differing title/triggers/links flagged", () => {
  const a = { title: "old", status: "active", tier: "B", owner: "alice", audience: "ops", triggers: ["a", "b"], body: "x" };
  const b = {
    title: "new", status: "draft", tier: "B", owner: "alice", audience: "ops",
    triggers: ["a", "c"], links: [{ relation: "reference", collection: "docs", family_id: "x", version: 1 }], body: "y",
  };
  const rows = metaDiffRows(a, b);
  // Pinned order: title, status, tier, owner, audience, triggers, links.
  assert.deepEqual(rows.map((r) => r.field), ["title", "status", "tier", "owner", "audience", "triggers", "links"]);
  // title differs.
  assert.deepEqual(rows[0], { field: "title", a: "old", b: "new", same: false });
  // status differs.
  assert.deepEqual(rows[1], { field: "status", a: "active", b: "draft", same: false });
  // tier / owner / audience equal.
  assert.equal(rows[2].same, true);
  assert.equal(rows[3].same, true);
  assert.equal(rows[4].same, true);
  // triggers differ in the ", "-joined form.
  assert.deepEqual(rows[5], { field: "triggers", a: "a, b", b: "a, c", same: false });
  assert.equal(rows[6].same, false);
});

test("metaDiffRows attempted=null tolerated: all a=\"\" (except status=draft default)", () => {
  const rows = metaDiffRows(null, { title: "x", status: "active", tier: "B", owner: "y", audience: "ops", triggers: [], body: "z" });
  assert.equal(rows.length, 7);
  // status defaults to "draft" on the attempted side per toFormFields;
  // every other a-value is the empty string.
  for (const r of rows) {
    if (r.field === "status") {
      assert.equal(r.a, "draft");
    } else {
      assert.equal(r.a, "", `field ${r.field} should be "" on the null side`);
    }
  }
  // And the rows still have b values from current.
  assert.equal(rows[0].b, "x");
  assert.equal(rows[1].b, "active");
});

// ---------- resolvePayload ----------

test("resolvePayload without save omits the save key", () => {
  const out = resolvePayload("by hand", 1, null);
  assert.deepEqual(out, { resolution: "by hand", expected_attempts: 1 });
  // The save key is OMITTED — not present-as-null. JSON.stringify
  // makes this distinction visible.
  assert.equal(JSON.stringify(out), `{"resolution":"by hand","expected_attempts":1}`);
});

test("resolvePayload with undefined save also omits the save key", () => {
  const out = resolvePayload("by hand", 1, undefined);
  assert.equal("save" in out, false);
});

test("resolvePayload with save includes the save object", () => {
  const save = { status: "active", body: "merged" };
  const out = resolvePayload("merged", 2, save);
  assert.deepEqual(out, {
    resolution: "merged", expected_attempts: 2, save,
  });
});

test("resolvePayload expected_attempts always present", () => {
  for (const v of [0, 1, 5, 99]) {
    const out = resolvePayload("r", v, null);
    assert.equal(out.expected_attempts, v);
  }
});

// ---------- hostsPath / hostResyncPath ----------

test("hostsPath is the literal /api/hosts", () => {
  assert.equal(hostsPath, "/api/hosts");
});

test("hostResyncPath encodes host (plain and percent-encoding)", () => {
  assert.equal(hostResyncPath("h1"), "/api/hosts/h1/resync");
  // Name with characters that need percent-encoding: the URL grammar
  // would otherwise mangle them; the server's {host} pattern matches
  // the decoded value.
  assert.equal(hostResyncPath("a b/c"), "/api/hosts/a%20b%2Fc/resync");
  assert.equal(hostResyncPath("with space"), "/api/hosts/with%20space/resync");
});

// ---------- DARK_AFTER_MS / SKEW_TOLERANCE_MS pinned constants ----------

test("DARK_AFTER_MS is 5 minutes", () => {
  assert.equal(DARK_AFTER_MS, 300000);
});

test("SKEW_TOLERANCE_MS is 30 seconds", () => {
  assert.equal(SKEW_TOLERANCE_MS, 30000);
});

// ---------- fleetStatus: every branch ----------

const NOW = Date.parse("2026-08-07T12:00:00Z");
const freshSeen = "2026-08-07T11:59:30Z"; // 30s before NOW
const exactlyBoundary = "2026-08-07T11:55:00Z"; // exactly 5 min before NOW
const justInsideBoundary = "2026-08-07T11:55:01Z"; // 4m59s before NOW
const justOutsideBoundary = "2026-08-07T11:54:59Z"; // 5m1s before NOW

test("fleetStatus: no seen_at → dark", () => {
  assert.equal(fleetStatus({ host: "h", ok: true, release_id: 1 }, 1, NOW), "dark");
  assert.equal(fleetStatus({ host: "h", release_id: 1 }, 1, NOW), "dark");
  assert.equal(fleetStatus({ host: "h", seen_at: "" }, 1, NOW), "dark");
});

test("fleetStatus: unparseable seen_at → dark", () => {
  assert.equal(fleetStatus({ host: "h", seen_at: "not-a-date", ok: true, release_id: 1 }, 1, NOW), "dark");
  // Empty string from the server is "absent" per the brief; the
  // field presence with a non-empty but unparseable value is the
  // branch the test pins here.
});

test("fleetStatus: age exactly DARK_AFTER_MS is NOT dark (strict boundary)", () => {
  // exactly 5 min old → age === DARK_AFTER_MS, NOT > DARK_AFTER_MS.
  assert.equal(fleetStatus({ host: "h", seen_at: exactlyBoundary, ok: true, release_id: 1 }, 1, NOW), "current");
});

test("fleetStatus: age DARK_AFTER_MS + 1 → dark", () => {
  // Exact millisecond boundary: the same seen_at that is NOT dark at
  // NOW must flip dark at NOW + 1 (age === DARK_AFTER_MS + 1ms). A
  // threshold that is off by even 1ms fails here; the 1s-out case
  // below is kept as the coarser regression.
  assert.equal(fleetStatus({ host: "h", seen_at: exactlyBoundary, ok: true, release_id: 1 }, 1, NOW + 1), "dark");
  assert.equal(fleetStatus({ host: "h", seen_at: justOutsideBoundary, ok: true, release_id: 1 }, 1, NOW), "dark");
});

test("fleetStatus: age just inside boundary → not dark; ok=false → stale", () => {
  assert.equal(fleetStatus({ host: "h", seen_at: justInsideBoundary, ok: false, release_id: 1 }, 1, NOW), "stale");
});

test("fleetStatus: ok=true but release behind latest → stale", () => {
  assert.equal(fleetStatus({ host: "h", seen_at: freshSeen, ok: true, release_id: 0 }, 1, NOW), "stale");
});

test("fleetStatus: ok=true + current release + fresh → current", () => {
  assert.equal(fleetStatus({ host: "h", seen_at: freshSeen, ok: true, release_id: 1 }, 1, NOW), "current");
});

test("fleetStatus: latestReleaseID 0 and release_id 0 → current", () => {
  // No release cut yet; a host that heartbeated ok against that
  // server reports release_id 0 and is "current" (converged with an
  // empty server).
  assert.equal(fleetStatus({ host: "h", seen_at: freshSeen, ok: true, release_id: 0 }, 0, NOW), "current");
});

test("fleetStatus: seen_at 10s ahead of now (within tolerance) clamps, classifies by ok/release", () => {
  // Within SKEW_TOLERANCE_MS the in-flight heartbeat is treated as
  // benign; the host is classified by ok/release rules, not by the
  // far-future stamp.
  const future = "2026-08-07T12:00:10Z"; // 10s ahead of NOW; within 30s tolerance
  assert.equal(fleetStatus({ host: "h", seen_at: future, ok: true, release_id: 1 }, 1, NOW), "current");
  assert.equal(fleetStatus({ host: "h", seen_at: future, ok: false, release_id: 1 }, 1, NOW), "stale");
});

test("fleetStatus: seen_at more than SKEW_TOLERANCE_MS ahead → dark", () => {
  // 10 min ahead of NOW — far beyond the 30s tolerance. The host's
  // freshness is unknowable; unknowable must read as dark, never
  // current.
  const farFuture = "2026-08-07T12:10:00Z";
  assert.equal(fleetStatus({ host: "h", seen_at: farFuture, ok: true, release_id: 1 }, 1, NOW), "dark");
});

// ---------- formatAge ----------

test("formatAge: negative and < 1000 are 0s", () => {
  assert.equal(formatAge(-1000), "0s");
  assert.equal(formatAge(0), "0s");
  assert.equal(formatAge(999), "0s");
});

test("formatAge: seconds, minutes, hours, days", () => {
  assert.equal(formatAge(1000), "1s");
  assert.equal(formatAge(59 * 1000), "59s");
  assert.equal(formatAge(60 * 1000), "1m");
  assert.equal(formatAge(59 * 60 * 1000), "59m");
  assert.equal(formatAge(60 * 60 * 1000), "1h");
  assert.equal(formatAge(23 * 60 * 60 * 1000), "23h");
  assert.equal(formatAge(24 * 60 * 60 * 1000), "1d");
  assert.equal(formatAge(48 * 60 * 60 * 1000), "2d");
});

test("formatAge: floor behavior (90s → 1m)", () => {
  assert.equal(formatAge(90 * 1000), "1m");
});

// ---------- fleetRowCells ----------

test("fleetRowCells: heartbeated host → 7 cells with release, age, error/resync/token passthrough", () => {
  const row = {
    host: "h1",
    release_id: 7,
    ok: true,
    seen_at: freshSeen,
    error: "boom",
    resync_requested_at: "2026-08-07T11:00:00Z",
    token_created_at: "2026-08-07T10:00:00Z",
  };
  const { status, cells } = fleetRowCells(row, 7, NOW);
  assert.equal(status, "current");
  assert.equal(cells.length, 7);
  assert.equal(cells[0], "h1");
  assert.equal(cells[1], "current");
  assert.equal(cells[2], "7");
  assert.equal(cells[3], freshSeen + " (30s ago)");
  assert.equal(cells[4], "boom");
  assert.equal(cells[5], "2026-08-07T11:00:00Z");
  assert.equal(cells[6], "2026-08-07T10:00:00Z");
});

test("fleetRowCells: never-seen token-only host → release \"\" and last seen \"(never)\"", () => {
  const row = {
    host: "tok",
    release_id: 0,
    ok: false,
    token_created_at: "2026-08-07T10:00:00Z",
  };
  const { status, cells } = fleetRowCells(row, 0, NOW);
  assert.equal(status, "dark");
  assert.equal(cells[0], "tok");
  // release cell is "" because seen_at is absent.
  assert.equal(cells[2], "");
  // last seen is the literal "(never)".
  assert.equal(cells[3], "(never)");
  // error/resync passthrough as empty.
  assert.equal(cells[4], "");
  assert.equal(cells[5], "");
  assert.equal(cells[6], "2026-08-07T10:00:00Z");
});

test("fleetRowCells: unparseable seen_at → rendered verbatim, no age suffix", () => {
  const row = {
    host: "h1",
    release_id: 1,
    ok: true,
    seen_at: "garbage",
  };
  const { status, cells } = fleetRowCells(row, 1, NOW);
  assert.equal(status, "dark");
  assert.equal(cells[3], "garbage");
});

test("fleetRowCells: status in cells[1] equals the returned status", () => {
  const row = { host: "h", seen_at: freshSeen, ok: true, release_id: 1 };
  const { status, cells } = fleetRowCells(row, 1, NOW);
  assert.equal(cells[1], status);
});

// ---------- fleetSummary ----------

test("fleetSummary: empty array → \"\"", () => {
  assert.equal(fleetSummary([], 0, NOW), "");
  assert.equal(fleetSummary(null, 0, NOW), "");
});

test("fleetSummary: mixed statuses → correct counts in pinned format", () => {
  const hosts = [
    { host: "a", seen_at: freshSeen, ok: true, release_id: 1 }, // current
    { host: "b", seen_at: freshSeen, ok: false, release_id: 1 }, // stale
    { host: "c", seen_at: "2026-08-07T08:00:00Z", ok: true, release_id: 1 }, // dark (older than 5 min)
    { host: "d", release_id: 0, ok: true }, // dark, no seen_at
  ];
  const summary = fleetSummary(hosts, 1, NOW);
  assert.equal(summary, "4 host(s) — 1 current, 1 stale, 2 dark · latest release 1");
});

test("fleetSummary: latestReleaseID 0 → \"latest release (none)\"", () => {
  const hosts = [{ host: "a", seen_at: freshSeen, ok: true, release_id: 0 }];
  const summary = fleetSummary(hosts, 0, NOW);
  assert.equal(summary, "1 host(s) — 1 current, 0 stale, 0 dark · latest release (none)");
});

// ---------- parseFleetResponse ----------

test("parseFleetResponse: valid response round-trips, nowMs === Date.parse(now)", () => {
  const json = {
    hosts: [
      { host: "h1", release_id: 1, ok: true, seen_at: "2026-08-07T11:59:00Z" },
    ],
    latest_release_id: 1,
    now: "2026-08-07T12:00:00Z",
  };
  const out = parseFleetResponse(json);
  assert.notEqual(out, null);
  assert.equal(out.hosts, json.hosts); // passed through unchanged
  assert.equal(out.latestReleaseID, 1);
  assert.equal(out.nowMs, Date.parse("2026-08-07T12:00:00Z"));
});

test("parseFleetResponse: null json → null", () => {
  assert.equal(parseFleetResponse(null), null);
});

test("parseFleetResponse: hosts not an array → null", () => {
  assert.equal(parseFleetResponse({ hosts: "nope", latest_release_id: 0, now: "2026-08-07T12:00:00Z" }), null);
});

test("parseFleetResponse: hosts: [null] → null", () => {
  assert.equal(parseFleetResponse({ hosts: [null], latest_release_id: 0, now: "2026-08-07T12:00:00Z" }), null);
});

test("parseFleetResponse: row missing host or empty-string host → null", () => {
  assert.equal(parseFleetResponse({
    hosts: [{ release_id: 0, ok: true }],
    latest_release_id: 0,
    now: "2026-08-07T12:00:00Z",
  }), null);
  assert.equal(parseFleetResponse({
    hosts: [{ host: "", release_id: 0, ok: true }],
    latest_release_id: 0,
    now: "2026-08-07T12:00:00Z",
  }), null);
});

test("parseFleetResponse: row non-boolean ok → null", () => {
  assert.equal(parseFleetResponse({
    hosts: [{ host: "h", release_id: 0, ok: "yes" }],
    latest_release_id: 0,
    now: "2026-08-07T12:00:00Z",
  }), null);
});

test("parseFleetResponse: row release_id non-number / fractional / negative → null", () => {
  for (const bad of ["1", 1.5, -1]) {
    assert.equal(parseFleetResponse({
      hosts: [{ host: "h", release_id: bad, ok: true }],
      latest_release_id: 0,
      now: "2026-08-07T12:00:00Z",
    }), null, `release_id=${bad}`);
  }
});

test("parseFleetResponse: optional field explicitly undefined is accepted (same as absent)", () => {
  // The contract says optional fields are "absent (undefined) or a
  // string" — a property present but set to undefined must validate,
  // so the check is on the VALUE, not key presence.
  const out = parseFleetResponse({
    hosts: [{ host: "h", release_id: 0, ok: true, seen_at: undefined, error: undefined }],
    latest_release_id: 0,
    now: "2026-08-07T12:00:00Z",
  });
  assert.notEqual(out, null);
  assert.equal(out.hosts[0].host, "h");
});

test("parseFleetResponse: row non-string optional field → null", () => {
  assert.equal(parseFleetResponse({
    hosts: [{ host: "h", release_id: 0, ok: true, seen_at: 123 }],
    latest_release_id: 0,
    now: "2026-08-07T12:00:00Z",
  }), null);
  assert.equal(parseFleetResponse({
    hosts: [{ host: "h", release_id: 0, ok: true, error: 123 }],
    latest_release_id: 0,
    now: "2026-08-07T12:00:00Z",
  }), null);
  assert.equal(parseFleetResponse({
    hosts: [{ host: "h", release_id: 0, ok: true, resync_requested_at: [1] }],
    latest_release_id: 0,
    now: "2026-08-07T12:00:00Z",
  }), null);
  assert.equal(parseFleetResponse({
    hosts: [{ host: "h", release_id: 0, ok: true, token_created_at: { x: 1 } }],
    latest_release_id: 0,
    now: "2026-08-07T12:00:00Z",
  }), null);
});

test("parseFleetResponse: negative or non-integer latest_release_id → null", () => {
  assert.equal(parseFleetResponse({ hosts: [], latest_release_id: -1, now: "2026-08-07T12:00:00Z" }), null);
  assert.equal(parseFleetResponse({ hosts: [], latest_release_id: 1.5, now: "2026-08-07T12:00:00Z" }), null);
  assert.equal(parseFleetResponse({ hosts: [], latest_release_id: "1", now: "2026-08-07T12:00:00Z" }), null);
  assert.equal(parseFleetResponse({ hosts: [], latest_release_id: null, now: "2026-08-07T12:00:00Z" }), null);
});

test("parseFleetResponse: missing or unparseable now → null", () => {
  assert.equal(parseFleetResponse({ hosts: [], latest_release_id: 0 }), null);
  // Unparseable.
  assert.equal(parseFleetResponse({
    hosts: [], latest_release_id: 0, now: "garbage",
  }), null);
  // Date-only (no time component) is not an RFC3339 timestamp.
  assert.equal(parseFleetResponse({
    hosts: [], latest_release_id: 0, now: "2026-08-07",
  }), null);
});

test("parseFleetResponse: parseable now WITHOUT trailing Z → null", () => {
  // The trailing-Z check pins the RFC3339 UTC wire shape (a date-
  // only or offset timestamp is rejected, not silently reinterpreted).
  assert.equal(parseFleetResponse({
    hosts: [], latest_release_id: 0, now: "2026-08-07T12:00:00+00:00",
  }), null);
  // Hour-minute offset without Z.
  assert.equal(parseFleetResponse({
    hosts: [], latest_release_id: 0, now: "2026-08-07T12:00:00-05:00",
  }), null);
});
