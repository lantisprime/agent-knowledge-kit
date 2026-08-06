// ui/lib_test.mjs — Node built-in runner tests for the pure logic in
// ui/lib.mjs. Zero deps. Run with: node --test ui/lib_test.mjs

import { test } from "node:test";
import assert from "node:assert/strict";

import {
  DIFF_MAX_LINES, DIFF_MAX_BYTES, DIFF_MAX_PRODUCT,
  encodePath, countWords, countBytes, parseTriggers,
  isExpectedContentHashShape, initialSaveState, afterSave, onConflict,
  lineDiff, manifestDiff,
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
