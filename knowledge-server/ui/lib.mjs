// ui/lib.mjs — PURE logic for the curation UI. No DOM access; no
// network; no globals. Every export is a data structure or a function
// over plain JS values. The Node built-in test runner loads this file
// directly; app.js imports the same exports for the DOM wiring layer.
//
// Global DOM-safety contract reminder: this module returns data
// structures only. Strings returned here may end up in the DOM, but
// only via textContent / DOM properties / createElement on the app.js
// side — NEVER via element-HTML setters, event-handler attributes, or
// style strings.

const WORD_RE = /[\p{White_Space}]+/u;
const HASH_RE = /^sha256:[0-9a-f]{64}$/;

// diffBudget caps the LCS work to keep the UI responsive. When either
// side exceeds these limits, or lines_a * lines_b exceeds the product
// cap, lineDiff returns a tooLarge marker instead of running LCS.
export const DIFF_MAX_LINES = 8000;
export const DIFF_MAX_BYTES = 512 * 1024;
export const DIFF_MAX_PRODUCT = 4_000_000;

export function encodePath(collection, family) {
  return "/api/docs/" + encodeURIComponent(collection) + "/" + encodeURIComponent(family);
}

// listDocsPath is a stable, single-source path for the browse view.
export const listDocsPath = "/api/docs";

// previewReleasePath / cutReleasePath for the publish view.
export const previewReleasePath = "/api/releases/preview";
export const cutReleasePath = "/api/releases";
export function currentReleasePath(host) {
  return "/api/releases/current" + (host ? "?host=" + encodeURIComponent(host) : "");
}

// countWords matches Go strings.Fields closely enough for the kernel
// lint advisory: splits on Unicode whitespace, drops empties.
// Verified in lib_test.mjs against pinned character classes.
export function countWords(s) {
  if (!s) return 0;
  const parts = s.split(WORD_RE);
  let n = 0;
  for (const p of parts) if (p.length > 0) n++;
  return n;
}

// countBytes matches the kernel byte-cap advisory: UTF-8 byte length
// of the body (matches Go's len([]byte(body)) for valid UTF-8).
export function countBytes(s) {
  if (!s) return 0;
  return new TextEncoder().encode(s).length;
}

// parseTriggers splits a comma-separated triggers input the same way
// the server stores it: split on ',', trim each, drop empties. The
// server additionally rejects values containing ',' up front (one
// grammar, one round-trip), but the UI splits defensively so a
// pasted "a, b" round-trips as ["a","b"].
export function parseTriggers(raw) {
  if (!raw) return [];
  const out = [];
  for (const t of raw.split(",")) {
    const v = t.trim();
    if (v.length > 0) out.push(v);
  }
  return out;
}

// expectedContentHashPattern is exported so app.js (and tests) can
// validate locally before sending.
export function isExpectedContentHashShape(s) {
  return typeof s === "string" && HASH_RE.test(s);
}

// initialSaveState — before a doc has ever been saved, the editor
// saves WITHOUT base_version (last-writer-wins opt-out, since there
// is no version to anchor against).
export function initialSaveState() {
  return { baseVersion: null, exists: false };
}

// afterSave returns the next save state from a successful server
// response: the returned version becomes the tracked base, and the
// family is marked existing. A second save with the new state will
// carry base_version and the optimistic-lock precondition is active.
export function afterSave(state, responseVersion) {
  return { baseVersion: responseVersion, exists: true };
}

// onConflict leaves the save state UNCHANGED — the editor surfaces a
// "version changed on the server — reload to see the latest" banner
// and never retries on its own. Returning the input unchanged is
// intentional: a stale base is still the correct base to display
// (and to retry with, after a manual reload).
export function onConflict(state) {
  return state;
}

// lineDiff runs LCS over two arrays of lines. Returns:
//   { tooLarge: true }   when the input exceeds the budget
//   { rows: [{ tag: "eq"|"add"|"del", text }] }  otherwise.
//
// tag values are exactly three strings; the app.js side maps them to
// CSS classes and inserts row.text via textContent.
export function lineDiff(a, b) {
  // The byte budget is measured against the joined body INCLUDING
  // newline separators, via TextEncoder. Using each line's UTF-16
  // unit length (the JS string .length) underestimates multibyte
  // UTF-8 by 2–4× for non-ASCII text and is not what the server's
  // archive byte caps measure; counting encoder-encoded bytes makes
  // the budget independent of the source charset.
  const sizeA = bodyByteLen(a);
  const sizeB = bodyByteLen(b);
  if (
    a.length > DIFF_MAX_LINES ||
    b.length > DIFF_MAX_LINES ||
    sizeA > DIFF_MAX_BYTES ||
    sizeB > DIFF_MAX_BYTES ||
    a.length * b.length > DIFF_MAX_PRODUCT
  ) {
    return { tooLarge: true };
  }
  // Standard LCS DP; rows are reconstructed from the table.
  const n = a.length;
  const m = b.length;
  const dp = new Array((n + 1) * (m + 1)).fill(0);
  for (let i = 1; i <= n; i++) {
    for (let j = 1; j <= m; j++) {
      if (a[i - 1] === b[j - 1]) {
        dp[i * (m + 1) + j] = dp[(i - 1) * (m + 1) + (j - 1)] + 1;
      } else {
        dp[i * (m + 1) + j] = Math.max(
          dp[(i - 1) * (m + 1) + j],
          dp[i * (m + 1) + (j - 1)]
        );
      }
    }
  }
  const rows = [];
  let i = n, j = m;
  while (i > 0 && j > 0) {
    if (a[i - 1] === b[j - 1]) {
      rows.push({ tag: "eq", text: a[i - 1] });
      i--; j--;
    } else if (dp[(i - 1) * (m + 1) + j] > dp[i * (m + 1) + (j - 1)]) {
      rows.push({ tag: "del", text: a[i - 1] });
      i--;
    } else {
      // Tie or up < left → take left (add). After the final
      // rows.reverse() this puts a replacement's del before its add
      // in the displayed order, matching the convention used by
      // git-style unified diffs.
      rows.push({ tag: "add", text: b[j - 1] });
      j--;
    }
  }
  while (i > 0) { rows.push({ tag: "del", text: a[i - 1] }); i--; }
  while (j > 0) { rows.push({ tag: "add", text: b[j - 1] }); j--; }
  rows.reverse();
  return { rows };
}

// bodyByteLen returns the UTF-8 byte length of `lines` joined with
// the same newline separator that app.js uses when it calls
// `.split("\n")`. The +1 per separator matters at line boundaries:
// "a\nb" → 3 bytes, but summing each line's length gives 2.
const bodyEncoder = new TextEncoder();
function bodyByteLen(lines) {
  if (lines.length === 0) return 0;
  let total = 0;
  for (let i = 0; i < lines.length; i++) {
    total += bodyEncoder.encode(lines[i]).length;
    if (i < lines.length - 1) total += 1; // joining "\n"
  }
  return total;
}

// manifestDiff classifies docs between two manifests. Match is by
// path; equality of {family_id, version, sha256} decides added /
// removed / changed. Unknown fields are ignored so the result is
// stable against the server adding more metadata later.
export function manifestDiff(oldM, newM) {
  const oldByPath = new Map();
  const newByPath = new Map();
  for (const d of (oldM && oldM.docs) || []) oldByPath.set(d.path, d);
  for (const d of (newM && newM.docs) || []) newByPath.set(d.path, d);
  const added = [], removed = [], changed = [];
  for (const [path, d] of newByPath) {
    if (!oldByPath.has(path)) {
      added.push(d);
    } else if (oldByPath.get(path).sha256 !== d.sha256) {
      changed.push({ path, old: oldByPath.get(path), next: d });
    }
  }
  for (const [path, d] of oldByPath) {
    if (!newByPath.has(path)) removed.push(d);
  }
  // Stable sort: by path.
  const byPath = (a, b) => (a.path < b.path ? -1 : a.path > b.path ? 1 : 0);
  added.sort(byPath); removed.sort(byPath); changed.sort(byPath);
  return { added, removed, changed };
}
