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

// conflictsPath: list endpoint. Empty (or falsy) status → unfiltered
// list; otherwise the query is "?status=" + encodeURIComponent.
// No-status form is the literal "/api/conflicts", NOT
// "/api/conflicts?status=" — the wire form is pinned.
export function conflictsPath(status) {
  if (!status) return "/api/conflicts";
  return "/api/conflicts?status=" + encodeURIComponent(status);
}

// conflictPath: single record by id. Numeric id is the only path
// shape the server accepts (other forms 400 invalid); the
// String(id) makes the type explicit and avoids a number-vs-string
// split in callers.
export function conflictPath(id) {
  return "/api/conflicts/" + encodeURIComponent(String(id));
}

// resolveConflictPath: append "/resolve" to the single-record path.
export function resolveConflictPath(id) {
  return conflictPath(id) + "/resolve";
}

// hostsPath: fleet-page read. Operator-only.
export const hostsPath = "/api/hosts";

// hostResyncPath: per-host force-resync POST. The host segment is
// percent-encoded so a name with characters the URL grammar would
// otherwise mangle still produces a valid URL; the server's {host}
// pattern matches the decoded value.
export function hostResyncPath(host) {
  return "/api/hosts/" + encodeURIComponent(host) + "/resync";
}

// DARK_AFTER_MS: a host is dark after 5 minutes without a heartbeat
// — 5× the subscriber's default 60s poll interval, so one or two
// missed polls (server restart, transient network) do not flap the
// status.
export const DARK_AFTER_MS = 5 * 60 * 1000;

// SKEW_TOLERANCE_MS: how far into the future of the server's `now`
// a seen_at may sit and still be trusted. Both stamps come from the
// same server clock, but a heartbeat can land between the DB read
// and the `now` stamp, so a slightly-future seen_at is normal. A
// seen_at MORE than this ahead means clock rollback or a restored
// snapshot — the host's freshness is unknowable, and unknowable must
// read as dark, never current.
export const SKEW_TOLERANCE_MS = 30 * 1000;

// fleetStatus classifies one HostStatus row as exactly one of
// "dark" | "stale" | "current". Decision order is pinned:
//   1. !row.seen_at (absent/empty)            → "dark"  (never heartbeated)
//   2. Date.parse(row.seen_at) is NaN         → "dark"  (unparseable = unknown)
//   3. age = nowMs - seenMs;
//      age < -SKEW_TOLERANCE_MS               → "dark"  (far-future stamp:
//      clock rollback / snapshot restore; fail dark, not current)
//   4. -SKEW_TOLERANCE_MS <= age < 0 → clamp age to 0 (benign in-flight
//      heartbeat between the DB read and the `now` stamp)
//   5. age > DARK_AFTER_MS                    → "dark"  (STRICTLY greater:
//      age exactly at the boundary is not dark)
//   6. !row.ok                                → "stale" (alive but failing)
//   7. row.release_id !== latestReleaseID     → "stale" (alive, behind)
//   8. otherwise                              → "current"
// Note rule 7 uses plain !== including latestReleaseID === 0: a host
// that heartbeated ok against a server with no releases reports
// release_id 0 and is "current" (converged with an empty server).
export function fleetStatus(row, latestReleaseID, nowMs) {
  if (!row || typeof row.seen_at !== "string" || row.seen_at === "") {
    return "dark";
  }
  const seenMs = Date.parse(row.seen_at);
  if (!Number.isFinite(seenMs)) {
    return "dark";
  }
  const rawAge = nowMs - seenMs;
  if (rawAge < -SKEW_TOLERANCE_MS) {
    return "dark";
  }
  const age = rawAge < 0 ? 0 : rawAge;
  if (age > DARK_AFTER_MS) {
    return "dark";
  }
  if (!row.ok) {
    return "stale";
  }
  if (row.release_id !== latestReleaseID) {
    return "stale";
  }
  return "current";
}

// formatAge returns a human age label, largest unit only, floored:
// negative or < 1000 → "0s"; then "Ns" (< 60s), "Nm" (< 60m), "Nh"
// (< 24h), "Nd" (no upper cutoff — 48h is "2d"). Pure arithmetic,
// no Date access.
export function formatAge(ms) {
  const n = Number(ms);
  if (!Number.isFinite(n) || n < 1000) return "0s";
  const sec = Math.floor(n / 1000);
  if (sec < 60) return sec + "s";
  const min = Math.floor(sec / 60);
  if (min < 60) return min + "m";
  const hr = Math.floor(min / 60);
  if (hr < 24) return hr + "h";
  const day = Math.floor(hr / 24);
  return day + "d";
}

// parseFleetResponse validates and normalizes the GET /api/hosts
// response body. Returns { hosts, latestReleaseID, nowMs } on
// success, null on ANY failure — the caller treats null as
// "malformed fleet response" and keeps its previous state.
export function parseFleetResponse(json) {
  if (!json || typeof json !== "object" || Array.isArray(json)) return null;
  if (!Array.isArray(json.hosts)) return null;
  if (!Number.isSafeInteger(json.latest_release_id) || json.latest_release_id < 0) return null;
  if (typeof json.now !== "string" || !json.now.endsWith("Z")) return null;
  if (!Number.isFinite(Date.parse(json.now))) return null;
  for (const host of json.hosts) {
    if (!host || typeof host !== "object" || Array.isArray(host)) return null;
    if (typeof host.host !== "string" || host.host === "") return null;
    if (!Number.isSafeInteger(host.release_id) || host.release_id < 0) return null;
    if (typeof host.ok !== "boolean") return null;
    for (const k of ["error", "seen_at", "resync_requested_at", "token_created_at"]) {
      // Optional means "absent (undefined) or a string": a property
      // explicitly set to undefined is equivalent to absent, so test
      // the VALUE, not key presence (`in` would reject {seen_at:
      // undefined}, which the contract permits).
      if (host[k] !== undefined && typeof host[k] !== "string") return null;
    }
  }
  return { hosts: json.hosts, latestReleaseID: json.latest_release_id, nowMs: Date.parse(json.now) };
}

// fleetRowCells returns { status, cells } for one fleet table row.
// status is the fleetStatus(...) value; cells is an array of
// EXACTLY 7 strings in the table's column order (all columns except
// the actions column).
export function fleetRowCells(row, latestReleaseID, nowMs) {
  const status = fleetStatus(row, latestReleaseID, nowMs);
  const release = row.seen_at ? String(row.release_id) : "";
  const seenMs = row.seen_at ? Date.parse(row.seen_at) : NaN;
  let lastSeen;
  if (!row.seen_at) {
    lastSeen = "(never)";
  } else if (!Number.isFinite(seenMs)) {
    lastSeen = row.seen_at;
  } else {
    lastSeen = row.seen_at + " (" + formatAge(nowMs - seenMs) + " ago)";
  }
  const cells = [
    row.host,
    status,
    release,
    lastSeen,
    row.error || "",
    row.resync_requested_at || "",
    row.token_created_at || "",
  ];
  return { status, cells };
}

// fleetSummary returns the fleet-summary line. Empty hosts array →
// "" (the app shows the fleet-empty element instead). Otherwise:
//
//   "<n> host(s) — <c> current, <s> stale, <d> dark · latest release <id>"
//
// with "latest release (none)" when latestReleaseID is 0. Counts
// come from fleetStatus over each row. Pluralize with the bare
// "host(s)" literal (no grammar logic).
export function fleetSummary(hosts, latestReleaseID, nowMs) {
  if (!Array.isArray(hosts) || hosts.length === 0) return "";
  let current = 0, stale = 0, dark = 0;
  for (const row of hosts) {
    const s = fleetStatus(row, latestReleaseID, nowMs);
    if (s === "current") current++;
    else if (s === "stale") stale++;
    else dark++;
  }
  const releaseLabel = latestReleaseID === 0 ? "latest release (none)" : "latest release " + latestReleaseID;
  return hosts.length + " host(s) — " + current + " current, " + stale + " stale, " + dark + " dark · " + releaseLabel;
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

// parseLinksJSON validates the editor's JSON-array representation before a
// save is sent. The server repeats the same semantic checks and remains the
// trust boundary; this function exists to give the operator immediate,
// fail-closed feedback instead of sending malformed metadata.
export function parseLinksJSON(raw) {
  if (!raw || raw.trim() === "") return { links: [], error: "" };
  let links;
  try {
    links = JSON.parse(raw);
  } catch (_err) {
    return { links: null, error: "Links must be valid JSON." };
  }
  if (!Array.isArray(links)) {
    return { links: null, error: "Links must be a JSON array." };
  }
  for (let i = 0; i < links.length; i++) {
    const link = links[i];
    if (!link || typeof link !== "object" || Array.isArray(link)) {
      return { links: null, error: `Link ${i + 1} must be an object.` };
    }
    if (link.relation !== "reference" && link.relation !== "supersedes") {
      return { links: null, error: `Link ${i + 1} relation must be reference or supersedes.` };
    }
    if (typeof link.collection !== "string" || link.collection === "") {
      return { links: null, error: `Link ${i + 1} collection is required.` };
    }
    if (typeof link.family_id !== "string" || link.family_id === "") {
      return { links: null, error: `Link ${i + 1} family_id is required.` };
    }
    if (!Number.isSafeInteger(link.version) || link.version <= 0) {
      return { links: null, error: `Link ${i + 1} version must be a positive integer.` };
    }
  }
  return { links, error: "" };
}

export function formatLinksJSON(links) {
  if (!Array.isArray(links) || links.length === 0) return "";
  return JSON.stringify(links, null, 2);
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

// toFormFields converts a server Doc (or null) into the plain-string
// shape the merge form uses. The order matches the form's row order;
// the merge form's 8 input fields are filled positionally. triggers
// becomes ", "-joined and links becomes pretty JSON; null/undefined fields
// become "". Nullish
// docLike returns the all-empty shape with status "draft" (a fresh
// family has no rows to populate from).
export function toFormFields(docLike) {
  if (!docLike) {
    return { title: "", status: "draft", tier: "", owner: "", audience: "", triggers: "", links: "", body: "" };
  }
  return {
    title: docLike.title == null ? "" : String(docLike.title),
    status: docLike.status == null ? "draft" : String(docLike.status),
    tier: docLike.tier == null ? "" : String(docLike.tier),
    owner: docLike.owner == null ? "" : String(docLike.owner),
    audience: docLike.audience == null ? "" : String(docLike.audience),
    triggers: Array.isArray(docLike.triggers) ? docLike.triggers.join(", ") : "",
    links: formatLinksJSON(docLike.links),
    body: docLike.body == null ? "" : String(docLike.body),
  };
}

// metaDiffRows compares the metadata fields of two doc-like values
// over EXACTLY these 7 fields, in this order: title, status, tier,
// owner, audience, triggers, links. (body is diffed separately, never
// here.) triggers is compared/displayed in the ", "-joined string
// form and links in stable pretty JSON. Each row is
// {field, a, b, same}. Both inputs are passed through toFormFields
// so null and missing fields are normalized to "". The order is
// pinned — the UI renders rows into a table tbody in sequence and
// the unit test asserts the exact 7 names in this order.
export function metaDiffRows(attempted, current) {
  const a = toFormFields(attempted);
  const b = toFormFields(current);
  const fields = ["title", "status", "tier", "owner", "audience", "triggers", "links"];
  return fields.map((f) => ({
    field: f,
    a: a[f],
    b: b[f],
    same: a[f] === b[f],
  }));
}

// resolvePayload shapes the body sent to POST /api/conflicts/{id}/resolve.
// {resolution, expected_attempts} is always present; the save key is
// included only when save is non-null/non-undefined, matching the
// brief's "save key omitted when null/undefined" wire form. The
// server decodes the same way: present-and-non-null populates the
// field; absent-or-null leaves it nil.
export function resolvePayload(resolution, expectedAttempts, save) {
  if (save == null) {
    return { resolution, expected_attempts: expectedAttempts };
  }
  return { resolution, expected_attempts: expectedAttempts, save };
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
