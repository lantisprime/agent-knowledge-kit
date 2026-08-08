// ui/app.js — DOM wiring layer for the curation UI. Imports pure
// logic from ./lib.mjs; does all fetch + DOM manipulation. Every
// dynamic value (API responses, URL/path state, form inputs, error
// envelopes) is treated as untrusted text and inserted only via
// textContent / DOM properties / createElement. NEVER concatenate
// untrusted values into element-HTML setters, event-handler attributes,
// or style strings.
//
// Handler registration: every handler on a PERSISTENT element is
// bound exactly once in registerPersistentHandlers() at module init;
// no addEventListener on persistent elements inside view-show or
// load functions — that pattern accumulated stale handlers across
// re-entries and broke the diff picker. Rows and per-entry buttons
// are the one exception: they are freshly created nodes that DO
// close over their entry's data, which is safe because the parent
// container is cleared with replaceChildren() on every re-render, so
// listeners live and die with their nodes.

import {
  encodePath, listDocsPath, previewReleasePath, cutReleasePath,
  currentReleasePath, countWords, countBytes, parseTriggers, parseLinksJSON, formatLinksJSON,
  isExpectedContentHashShape, initialSaveState, afterSave, onConflict,
  lineDiff, manifestDiff,
  conflictsPath, conflictPath, resolveConflictPath,
  hostsPath, hostResyncPath, parseFleetResponse, fleetRowCells, fleetSummary,
  toFormFields, metaDiffRows, resolvePayload,
} from "./lib.mjs";

const TOKEN_KEY = "ks_token";
const EDITOR_KEY = "ks_editor";

const state = {
  token: sessionStorage.getItem(TOKEN_KEY) || "",
  editor: sessionStorage.getItem(EDITOR_KEY) || "",
  docs: [],
  filter: { collection: "", status: "", tier: "" },
  // editor state
  current: null,           // { collection, family, version, saveState, conflictId }
  // history view: list of DocMeta fetched on entry + the picked set
  historyVersions: [],
  historyPicked: new Set(),
  // publish state
  publishCurrent: null,
  publishCandidate: null,
  publishConfirmed: false,
  publishConfirmArmed: false, // true after first click on "Cut release"
  // conflict surface (build-order step 5)
  conflicts: [],
  showResolved: false,
  conflict: null, // { record, current /* Doc|null */, currentStatus /* 200|404|0 */ }
  // fleet surface (build-order step 6)
  fleet: null, // { hosts, latestReleaseID, nowMs }
  fleetGen: 0, // refresh generation counter; see refreshFleet
};

// ---------- init: register ALL persistent handlers once ----------

document.addEventListener("DOMContentLoaded", () => {
  registerPersistentHandlers();
  boot();
});

function registerPersistentHandlers() {
  on("nav-browse", "click", () => switchView("browse", refreshBrowse));
  on("nav-publish", "click", () => switchView("publish", refreshPublish));
  on("logout", "click", logout);

  // login
  on("login-go", "click", submitLogin);

  // browse filters
  on("filter-collection", "change", () => {
    state.filter.collection = byId("filter-collection").value;
    renderBrowse();
  });
  on("filter-status", "change", () => {
    state.filter.status = byId("filter-status").value;
    renderBrowse();
  });
  on("filter-tier", "change", () => {
    state.filter.tier = byId("filter-tier").value;
    renderBrowse();
  });
  on("browse-new", "click", () => {
    showView("newdoc");
    byId("newdoc-family").value = "";
    setText("newdoc-error", "");
  });
  on("newdoc-cancel", "click", () => showView("browse"));
  on("newdoc-go", "click", submitNewDoc);

  // editor
  on("ed-body", "input", updateKernelCounters);
  on("editor-reload", "click", () => {
    if (!state.current) return;
    const c = state.current.collection, f = state.current.family;
    openEditor(c, f, state.current.saveState);
  });
  on("editor-save", "click", submitEditorSave);
  on("editor-history", "click", enterHistoryView);

  // history view (handlers read state.historyVersions / state.historyPicked)
  on("history-diff-go", "click", () => {
    const picked = Array.from(state.historyPicked).sort((a, b) => a - b);
    const c = state.current && state.current.collection;
    const f = state.current && state.current.family;
    if (!c || !f) return;
    diffTwoVersions(c, f, picked);
  });
  on("history-back", "click", () => showView("editor"));

  // publish
  on("publish-refresh", "click", refreshPublish);
  on("publish-cut", "click", armPublishCut);
  on("publish-cancel", "click", cancelPublishCut);
  on("publish-confirm", "click", confirmPublishCut);

  // error view retry — binding here means it works from the first
  // showView("error") onward and survives subsequent error renders.
  on("error-retry", "click", () => {
    showView("error");
    setText("error-text", "");
    boot();
  });

  // conflict surface (step 5)
  on("nav-conflicts", "click", () => switchView("conflicts", refreshConflicts));
  on("conflicts-show-resolved", "change", () => {
    state.showResolved = byId("conflicts-show-resolved").checked;
    refreshConflicts();
  });
  on("cf-go", "click", submitClaimFlag);
  on("conflict-back", "click", () => switchView("conflicts", refreshConflicts));
  on("mg-load-attempted", "click", () => {
    if (!state.conflict) return;
    populateMergeForm(toFormFields(state.conflict.record.attempted));
  });
  on("mg-load-current", "click", () => {
    if (!state.conflict) return;
    populateMergeForm(toFormFields(state.conflict.current));
  });
  on("conflict-resolve-save", "click", () => submitResolve(true));
  on("conflict-resolve-keep", "click", () => submitResolve(false));
  on("editor-merge", "click", () => {
    if (state.current && state.current.conflictId != null) {
      openConflict(state.current.conflictId);
    }
  });

  // fleet surface (step 6)
  on("nav-fleet", "click", () => switchView("fleet", refreshFleet));
  on("fleet-refresh", "click", refreshFleet);
}

// ---------- bootstrap ----------

async function boot() {
  // Probe once with whatever credentials we have so we can either
  // route to login (token rejected) or directly into browse (auth
  // disabled or token accepted).
  const probe = await apiFetch("/api/docs", { method: "GET", noAuthOk: true });
  if (probe.authFailure) {
    // apiFetch has already cleared state and called showLogin().
    return;
  }
  if (probe.status === 200) {
    showView("browse");
    await refreshBrowse();
    return;
  }
  if (probe.status === 401) {
    // No token held OR token held but the server returned 401 with
    // noAuthOk semantics (probe-only). Either way: clear and login.
    if (state.token) clearStoredCredentials();
    showLogin();
    return;
  }
  showView("error");
  setText("error-text", `Bootstrap failed: HTTP ${probe.status} ${probe.body || ""}`);
}

function showLogin() {
  showView("login");
  setText("login-msg", "Enter an operator bearer token. Stored only in this tab's sessionStorage.");
  setText("login-error", "");
  // Clear any previously typed values; stale credentials in the form
  // are a foot-gun if the operator walks away mid-session.
  clearLoginInputs();
}

async function submitLogin() {
  const tok = byId("login-token").value.trim();
  const editor = byId("login-editor").value.trim();
  if (!tok) {
    setText("login-error", "Token is required.");
    return;
  }
  state.token = tok;
  state.editor = editor;
  sessionStorage.setItem(TOKEN_KEY, tok);
  if (editor) sessionStorage.setItem(EDITOR_KEY, editor); else sessionStorage.removeItem(EDITOR_KEY);
  const probe = await apiFetch("/api/docs", { method: "GET", noAuthOk: true });
  if (probe.status === 200) {
    clearLoginInputs();
    showView("browse");
    await refreshBrowse();
    return;
  }
  if (probe.status === 401 || probe.authFailure) {
    setText("login-error", "Token rejected (401).");
    clearStoredCredentials();
    clearLoginInputs();
    return;
  }
  setText("login-error", `Login probe failed: HTTP ${probe.status}`);
}

function logout() {
  clearStoredCredentials();
  clearLoginInputs();
  showLogin();
}

function clearStoredCredentials() {
  sessionStorage.removeItem(TOKEN_KEY);
  sessionStorage.removeItem(EDITOR_KEY);
  state.token = "";
  state.editor = "";
}

function clearLoginInputs() {
  const t = byId("login-token");
  const e = byId("login-editor");
  if (t) t.value = "";
  if (e) e.value = "";
}

// ---------- view switch helper ----------

async function switchView(name, loader) {
  showView(name);
  await loader();
}

// ---------- browse ----------

async function refreshBrowse() {
  const r = await apiFetch(listDocsPath, { method: "GET" });
  if (r.authFailure) return; // apiFetch has routed to login
  if (r.status !== 200) {
    showView("error");
    setText("error-text", `Browse failed: HTTP ${r.status} ${r.body || ""}`);
    return;
  }
  state.docs = (r.json && r.json.docs) || [];
  populateFilterOptions();
  renderBrowse();
  showEl("logout");
  setText("who", state.editor ? `${state.editor}` : "(operator)");
}

function populateFilterOptions() {
  const cols = new Set(), tiers = new Set();
  for (const d of state.docs) {
    if (d.collection) cols.add(d.collection);
    if (d.tier) tiers.add(d.tier);
  }
  setSelectOptions("filter-collection", ["", ...Array.from(cols).sort()], state.filter.collection);
  setSelectOptions("filter-tier", ["", ...Array.from(tiers).sort()], state.filter.tier);
  byId("filter-status").value = state.filter.status;
}

function renderBrowse() {
  const tbody = byId("browse-table").querySelector("tbody");
  tbody.replaceChildren();
  const filtered = state.docs.filter(d => {
    if (state.filter.collection && d.collection !== state.filter.collection) return false;
    if (state.filter.status && d.status !== state.filter.status) return false;
    if (state.filter.tier && d.tier !== state.filter.tier) return false;
    return true;
  });
  if (filtered.length === 0) {
    byId("browse-empty").hidden = false;
    return;
  }
  byId("browse-empty").hidden = true;
  for (const d of filtered) {
    const tr = document.createElement("tr");
    for (const key of ["collection", "family_id", "title", "status", "tier", "version", "created_at", "editor"]) {
      const td = document.createElement("td");
      td.textContent = d[key] == null ? "" : String(d[key]);
      tr.appendChild(td);
    }
    // Per-row closure over this row's identifiers — safe because the
    // <tr> (and its listener) is discarded by replaceChildren() on
    // the next render; nothing accumulates on persistent elements.
    const collection = d.collection, family = d.family_id;
    tr.addEventListener("click", () => openEditor(collection, family, initialSaveState()));
    tbody.appendChild(tr);
  }
}

// ---------- new doc ----------

async function submitNewDoc() {
  const collection = byId("newdoc-collection").value;
  const family = byId("newdoc-family").value.trim();
  if (!family) {
    setText("newdoc-error", "Family id is required.");
    return;
  }
  await openEditor(collection, family, initialSaveState());
}

// ---------- editor ----------

async function openEditor(collection, family, saveState) {
  showView("editor");
  state.current = { collection, family, version: 0, saveState, conflictId: null };
  setText("editor-path", `${collection} / ${family}`);
  setText("editor-save-state", "");
  setText("editor-error", "");
  byId("editor-merge").hidden = true;
  // Load latest version: omit ?version entirely so the API's "absent"
  // branch (which returns 404 for an unknown family) reaches the
  // new-doc branch here, and existing docs come back at max(version).
  const r = await apiFetch(encodePath(collection, family), { method: "GET" });
  if (r.authFailure) return;
  if (r.status === 404) {
    // New doc. Leave fields blank; first save carries no base_version.
    byId("ed-title").value = "";
    byId("ed-status").value = "draft";
    byId("ed-tier").value = "";
    byId("ed-owner").value = "";
    byId("ed-audience").value = "";
    byId("ed-triggers").value = "";
    byId("ed-links").value = "";
    byId("ed-body").value = "";
    state.current.saveState = { baseVersion: null, exists: false };
    state.current.version = 0;
    setText("editor-save-state", "new (untracked)");
  } else if (r.status === 200) {
    const d = r.json;
    byId("ed-title").value = d.title || "";
    byId("ed-status").value = d.status || "draft";
    byId("ed-tier").value = d.tier || "";
    byId("ed-owner").value = d.owner || "";
    byId("ed-audience").value = d.audience || "";
    byId("ed-triggers").value = (d.triggers || []).join(", ");
    byId("ed-links").value = formatLinksJSON(d.links);
    byId("ed-body").value = d.body || "";
    state.current.saveState = { baseVersion: d.version, exists: true };
    state.current.version = d.version;
    setText("editor-save-state", `v${d.version} (base ${d.version})`);
  } else {
    setText("editor-error", `Load failed: HTTP ${r.status} ${r.body || ""}`);
  }
  updateKernelCounters();
}

function updateKernelCounters() {
  const isKernel = state.current && state.current.collection === "kernel";
  byId("kernel-counters").hidden = !isKernel;
  if (!isKernel) return;
  const body = byId("ed-body").value;
  const words = countWords(body);
  const bytes = countBytes(body);
  const wEl = byId("kernel-words"); wEl.textContent = String(words);
  wEl.classList.toggle("over", words > 2000);
  const bEl = byId("kernel-bytes"); bEl.textContent = String(bytes);
  bEl.classList.toggle("over", bytes > 24576);
}

async function submitEditorSave() {
  if (!state.current) return;
  const c = state.current.collection, f = state.current.family;
  const parsedLinks = parseLinksJSON(byId("ed-links").value);
  if (parsedLinks.error) {
    setText("editor-error", parsedLinks.error);
    return;
  }
  const body = {
    title: byId("ed-title").value,
    status: byId("ed-status").value,
    tier: byId("ed-tier").value,
    triggers: parseTriggers(byId("ed-triggers").value),
    owner: byId("ed-owner").value,
    audience: byId("ed-audience").value,
    links: parsedLinks.links,
    body: byId("ed-body").value,
  };
  if (state.current.saveState.exists) {
    body.base_version = state.current.saveState.baseVersion;
  }
  const r = await apiFetch(encodePath(c, f), {
    method: "PUT",
    body: JSON.stringify(body),
    editor: state.editor,
  });
  if (r.authFailure) return;
  if (r.status === 200) {
    state.current.saveState = afterSave(state.current.saveState, r.json.version);
    state.current.version = r.json.version;
    state.current.conflictId = null;
    byId("editor-merge").hidden = true;
    setText("editor-save-state", `v${r.json.version} (base ${r.json.version})`);
    setText("editor-error", "");
  } else if (r.status === 409) {
    setText("editor-error",
      `version changed on the server — reload to see the latest. detail: ${r.json && r.json.detail || ""}`);
    state.current.saveState = onConflict(state.current.saveState);
    // Capture the conflict id (if the server committed a record)
    // and surface the merge-view button. A 409 with no conflict_id
    // (e.g. a future error envelope) leaves the button hidden.
    const cid = r.json && typeof r.json.conflict_id === "number" ? r.json.conflict_id : null;
    state.current.conflictId = cid;
    byId("editor-merge").hidden = (cid == null);
  } else {
    setText("editor-error", `Save failed: HTTP ${r.status} ${r.body || ""}`);
  }
}

// ---------- history ----------

async function enterHistoryView() {
  if (!state.current) return;
  showView("history");
  const c = state.current.collection, f = state.current.family;
  setText("history-path", `${c} / ${f}`);
  const r = await apiFetch(encodePath(c, f) + "/versions", { method: "GET" });
  if (r.authFailure) return;
  if (r.status !== 200) {
    setText("history-path", `History failed: HTTP ${r.status} ${r.body || ""}`);
    return;
  }
  state.historyVersions = (r.json && r.json.versions) || [];
  state.historyPicked = new Set();
  renderHistoryList();
  showEl("history-diff-controls");
  byId("history-diff").hidden = true;
  byId("history-fallback").hidden = true;
}

function renderHistoryList() {
  const ul = byId("history-list");
  ul.replaceChildren();
  for (const v of state.historyVersions) {
    const li = document.createElement("li");
    li.dataset.version = String(v.version);
    const a = document.createElement("button");
    a.type = "button";
    a.textContent = `v${v.version} ${v.status} ${v.created_at}`;
    a.addEventListener("click", () => toggleHistoryPick(v.version, li));
    li.appendChild(a);
    li.appendChild(makeReadOnlyBodyLink(v.version));
    ul.appendChild(li);
  }
}

function toggleHistoryPick(version, li) {
  if (state.historyPicked.has(version)) {
    state.historyPicked.delete(version);
    li.classList.remove("picked");
    return;
  }
  if (state.historyPicked.size >= 2) {
    // Drop the smallest-version pick (oldest in sort order).
    const oldest = Array.from(state.historyPicked).sort((a, b) => a - b)[0];
    state.historyPicked.delete(oldest);
    const oldLi = byId("history-list").querySelector(`li[data-version="${oldest}"]`);
    if (oldLi) oldLi.classList.remove("picked");
  }
  state.historyPicked.add(version);
  li.classList.add("picked");
}

function makeReadOnlyBodyLink(version) {
  const btn = document.createElement("button");
  btn.type = "button";
  btn.textContent = "view";
  btn.style.marginLeft = "0.5rem";
  btn.addEventListener("click", () => viewHistoryVersion(version));
  return btn;
}

async function viewHistoryVersion(version) {
  if (!state.current) return;
  const collection = state.current.collection, family = state.current.family;
  const r = await apiFetch(encodePath(collection, family) + "?version=" + version, { method: "GET" });
  if (r.authFailure) return;
  const pre = byId("history-diff");
  pre.hidden = false;
  byId("history-fallback").hidden = true;
  if (r.status === 200) {
    pre.textContent = `v${r.json.version} (${r.json.status}) body:\n` + r.json.body;
  } else {
    pre.textContent = `view failed: HTTP ${r.status} ${r.body || ""}`;
  }
}

async function diffTwoVersions(collection, family, versions) {
  if (versions.length !== 2) {
    setText("history-path", "pick exactly two versions");
    return;
  }
  const [va, vb] = versions;
  const [a, b] = await Promise.all([
    apiFetch(encodePath(collection, family) + "?version=" + va, { method: "GET" }),
    apiFetch(encodePath(collection, family) + "?version=" + vb, { method: "GET" }),
  ]);
  if (a.authFailure || b.authFailure) return;
  if (a.status !== 200 || b.status !== 200) {
    setText("history-path", `diff fetch failed: ${a.status}/${b.status}`);
    return;
  }
  const aLines = a.json.body.split("\n");
  const bLines = b.json.body.split("\n");
  const result = lineDiff(aLines, bLines);
  const out = byId("history-diff");
  const fb = byId("history-fallback");
  out.replaceChildren();
  fb.replaceChildren();
  if (result.tooLarge) {
    out.hidden = true;
    fb.hidden = false;
    renderSideBySide(fb, `v${va}`, a.json.body, `v${vb}`, b.json.body);
    return;
  }
  fb.hidden = true;
  out.hidden = false;
  for (const row of result.rows) {
    const span = document.createElement("span");
    span.className = row.tag;
    const prefix = row.tag === "add" ? "+ " : row.tag === "del" ? "- " : "  ";
    span.textContent = prefix + row.text + "\n";
    out.appendChild(span);
  }
}

// renderSideBySide populates `container` with two read-only <pre>
// panes laid out in a two-column grid. The labels are passed in
// PRE-FORMATTED (the caller decides "v3" vs "attempted" vs "(none)")
// so the same renderer serves the history diff, the publish diff,
// and the conflict merge view. Static markup only; dynamic values
// are inserted via textContent.
function renderSideBySide(container, labelA, aBody, labelB, bBody) {
  container.classList.add("side-by-side");
  const left = document.createElement("div");
  const right = document.createElement("div");
  left.className = "side-pane";
  right.className = "side-pane";
  const leftH = document.createElement("h4");
  leftH.textContent = labelA;
  const rightH = document.createElement("h4");
  rightH.textContent = labelB;
  const leftPre = document.createElement("pre");
  leftPre.className = "diff";
  leftPre.textContent = aBody;
  const rightPre = document.createElement("pre");
  rightPre.className = "diff";
  rightPre.textContent = bBody;
  left.appendChild(leftH); left.appendChild(leftPre);
  right.appendChild(rightH); right.appendChild(rightPre);
  container.appendChild(left);
  container.appendChild(right);
}

// ---------- publish ----------

async function refreshPublish() {
  state.publishCurrent = null;
  state.publishCandidate = null;
  state.publishConfirmed = false;
  state.publishConfirmArmed = false;
  setText("publish-error", "");
  byId("publish-cut").disabled = true;
  hideEl("publish-confirm"); hideEl("publish-cancel");
  byId("publish-current").textContent = "(loading)";
  byId("publish-preview").textContent = "(loading)";
  byId("publish-lint").hidden = true;
  byId("publish-diff").replaceChildren();
  byId("publish-content-diff").replaceChildren();
  setText("publish-state", "");

  const cur = await apiFetch(currentReleasePath(""), { method: "GET" });
  if (cur.authFailure) return;
  if (cur.status === 200) {
    state.publishCurrent = cur.json;
    byId("publish-current").textContent = JSON.stringify(cur.json, null, 2);
  } else if (cur.status === 404) {
    state.publishCurrent = null;
    byId("publish-current").textContent = "(no release yet)";
  } else {
    byId("publish-current").textContent = `current failed: HTTP ${cur.status} ${cur.body || ""}`;
  }

  const prev = await apiFetch(previewReleasePath, { method: "GET" });
  if (prev.authFailure) return;
  if (prev.status === 200) {
    state.publishCandidate = prev.json;
    byId("publish-preview").textContent = JSON.stringify(prev.json, null, 2);
    byId("publish-lint").hidden = true;
  } else if (prev.status === 409) {
    state.publishCandidate = null;
    byId("publish-preview").textContent = "(lint failure)";
    const lintEl = byId("publish-lint");
    lintEl.hidden = false;
    lintEl.textContent = `Lint: ${prev.json && prev.json.detail || "(unknown)"}`;
  } else {
    byId("publish-preview").textContent = `preview failed: HTTP ${prev.status} ${prev.body || ""}`;
  }

  renderPublishDiff();
  byId("publish-cut").disabled = !state.publishCandidate;
  setText("publish-state", state.publishCandidate ? "ready (review diff before cut)" : "lint blocking");
}

function armPublishCut() {
  if (!state.publishCandidate) return;
  state.publishConfirmArmed = true;
  showEl("publish-confirm"); showEl("publish-cancel");
  setText("publish-state", "confirm cut?");
}

function cancelPublishCut() {
  state.publishConfirmArmed = false;
  hideEl("publish-confirm"); hideEl("publish-cancel");
  setText("publish-state", "ready (review diff before cut)");
}

async function confirmPublishCut() {
  if (!state.publishCandidate) return;
  const r = await apiFetch(cutReleasePath, {
    method: "POST",
    body: JSON.stringify({ expected_content_hash: state.publishCandidate.content_hash }),
    editor: state.editor,
  });
  if (r.authFailure) return;
  if (r.status === 201) {
    hideEl("publish-confirm"); hideEl("publish-cancel");
    state.publishConfirmArmed = false;
    setText("publish-state", `cut: release ${r.json.release_id}`);
    state.publishCandidate = r.json;
    await refreshPublish();
  } else if (r.status === 409) {
    // Distinguish lint (kernel cap) from stale-hash (expected_content_hash
    // mismatch). Both arrive as 409: the lint path carries a
    // numeric conflict_id when a doc-scoped lint committed a
    // policy record; the empty-release lint records nothing and
    // carries no conflict_id. The stale-hash path carries
    // error="conflict".
    const env = r.json || {};
    if (env.error === "lint") {
      const cid = typeof env.conflict_id === "number" ? env.conflict_id : null;
      setText("publish-state", cid != null
        ? "lint failed — policy conflict recorded"
        : "lint failed");
      setText("publish-error", `Lint: ${env.detail || ""}` + (cid != null ? ` (conflict #${cid})` : ""));
      hideEl("publish-confirm"); hideEl("publish-cancel");
      state.publishConfirmArmed = false;
      state.publishCandidate = null;
      byId("publish-cut").disabled = true;
    } else if (env.error === "conflict") {
      // Stale candidate: do NOT auto-refresh. Leave the banner visible;
      // the operator refreshes explicitly so they can review what
      // changed. The stale hash is useless now, so the candidate is
      // dropped and Cut stays disabled until the next refresh
      // recomputes both — re-cutting the same stale hash would only
      // clear this guidance and 409 again.
      setText("publish-state", "candidate changed — refresh preview");
      setText("publish-error", `Conflict: ${env.detail || ""}`);
      hideEl("publish-confirm"); hideEl("publish-cancel");
      state.publishConfirmArmed = false;
      state.publishCandidate = null;
      byId("publish-cut").disabled = true;
    } else {
      // 409 with unrecognized / null json — generic failure branch.
      setText("publish-state", "");
      setText("publish-error", `Cut failed: HTTP 409 ${r.body || ""}`);
    }
  } else {
    setText("publish-state", "");
    setText("publish-error", `Cut failed: HTTP ${r.status} ${r.body || ""}`);
  }
}

function renderPublishDiff() {
  const ul = byId("publish-diff");
  ul.replaceChildren();
  const { added, removed, changed } = manifestDiff(state.publishCurrent, state.publishCandidate);
  if (state.publishCurrent == null && state.publishCandidate) {
    const li = document.createElement("li");
    li.textContent = `(no current release; candidate has ${state.publishCandidate.docs.length} doc(s))`;
    ul.appendChild(li);
  } else {
    for (const d of added) {
      const li = document.createElement("li");
      li.textContent = `+ added: ${d.path} (v${d.version})`;
      ul.appendChild(li);
    }
    for (const d of removed) {
      const li = document.createElement("li");
      li.textContent = `- removed: ${d.path}`;
      ul.appendChild(li);
    }
    for (const c of changed) {
      const li = document.createElement("li");
      const btn = document.createElement("button");
      btn.type = "button";
      btn.textContent = `~ changed: ${c.path} (v${c.old.version} → v${c.next.version})`;
      // Pass family_id from the manifest entry, NOT a substring of
      // path — the path's "family.md" suffix would otherwise be
      // sent as the family and the API would 404. Collection still
      // comes from the first path segment, since manifest entries
      // carry no collection field.
      const collection = c.path.split("/")[0];
      const family = c.next.family_id;
      const oldVersion = c.old.version, newVersion = c.next.version;
      btn.addEventListener("click", () => renderChangedContentDiff(collection, family, oldVersion, newVersion));
      li.appendChild(btn);
      ul.appendChild(li);
    }
    if (added.length === 0 && removed.length === 0 && changed.length === 0 && state.publishCurrent) {
      const li = document.createElement("li");
      li.textContent = "(no changes between current and candidate)";
      ul.appendChild(li);
    }
  }
}

async function renderChangedContentDiff(collection, family, oldVersion, newVersion) {
  const a = await apiFetch(encodePath(collection, family) + "?version=" + oldVersion, { method: "GET" });
  const b = await apiFetch(encodePath(collection, family) + "?version=" + newVersion, { method: "GET" });
  // AuthFailure check FIRST: the previous order cleared the box
  // before the check, so a 401 would leave the box empty and look
  // like a successful load. This is the same authFailure-first rule
  // as everywhere else.
  if (a.authFailure || b.authFailure) return;
  const box = byId("publish-content-diff");
  box.replaceChildren();
  if (a.status !== 200 || b.status !== 200) {
    const p = document.createElement("p");
    p.textContent = `fetch failed: ${a.status}/${b.status}`;
    box.appendChild(p);
    return;
  }
  const aLines = a.json.body.split("\n");
  const bLines = b.json.body.split("\n");
  const result = lineDiff(aLines, bLines);
  if (result.tooLarge) {
    // Side-by-side for publish diff too — two read-only panes.
    const container = document.createElement("div");
    container.className = "side-by-side";
    renderSideBySide(container, `v${oldVersion}`, a.json.body, `v${newVersion}`, b.json.body);
    box.appendChild(container);
    return;
  }
  const pre = document.createElement("pre");
  pre.className = "diff";
  for (const row of result.rows) {
    const span = document.createElement("span");
    span.className = row.tag;
    const prefix = row.tag === "add" ? "+ " : row.tag === "del" ? "- " : "  ";
    span.textContent = prefix + row.text + "\n";
    pre.appendChild(span);
  }
  box.appendChild(pre);
}

// ---------- conflicts (build-order step 5) ----------

async function refreshConflicts() {
  const r = await apiFetch(conflictsPath(state.showResolved ? "" : "open"), { method: "GET" });
  if (r.authFailure) return;
  if (r.status !== 200) {
    showView("error");
    setText("error-text", `Conflicts failed: HTTP ${r.status} ${r.body || ""}`);
    return;
  }
  state.conflicts = (r.json && r.json.conflicts) || [];
  renderConflictsList();
}

function renderConflictsList() {
  const tbody = byId("conflicts-table").querySelector("tbody");
  tbody.replaceChildren();
  if (state.conflicts.length === 0) {
    byId("conflicts-empty").hidden = false;
    return;
  }
  byId("conflicts-empty").hidden = true;
  for (const c of state.conflicts) {
    const tr = document.createElement("tr");
    const fields = [
      String(c.id), c.kind, c.status,
      c.collection + "/" + c.family_id,
      c.other_collection ? c.other_collection + "/" + c.other_family_id : "",
      String(c.attempts), c.detail, c.opened_by, c.opened_at,
      c.resolved_by || "", c.resolution || "", c.winning_version != null ? String(c.winning_version) : "",
    ];
    for (const v of fields) {
      const td = document.createElement("td");
      td.textContent = v;
      tr.appendChild(td);
    }
    const id = c.id;
    tr.addEventListener("click", () => openConflict(id));
    tbody.appendChild(tr);
  }
}

async function submitClaimFlag() {
  const collection = byId("cf-collection").value.trim();
  const family = byId("cf-family").value.trim();
  const otherCollection = byId("cf-other-collection").value.trim();
  const otherFamily = byId("cf-other-family").value.trim();
  const detail = byId("cf-detail").value.trim();
  if (!collection || !family || !detail) {
    setText("cf-error", "collection, family, and detail are required");
    return;
  }
  const body = { collection, family_id: family, detail };
  if (otherCollection) body.other_collection = otherCollection;
  if (otherFamily) body.other_family_id = otherFamily;
  const r = await apiFetch(conflictsPath(""), {
    method: "POST",
    body: JSON.stringify(body),
    editor: state.editor,
  });
  if (r.authFailure) return;
  if (r.status === 201) {
    setText("cf-error", "");
    byId("cf-collection").value = "";
    byId("cf-family").value = "";
    byId("cf-other-collection").value = "";
    byId("cf-other-family").value = "";
    byId("cf-detail").value = "";
    await refreshConflicts();
  } else if (r.status === 409) {
    setText("cf-error", "already flagged (open conflict exists)");
  } else {
    setText("cf-error", `HTTP ${r.status} ${r.body || ""}`);
  }
}

function populateMergeForm(fields) {
  byId("mg-title").value = fields.title;
  byId("mg-status").value = fields.status;
  byId("mg-tier").value = fields.tier;
  byId("mg-owner").value = fields.owner;
  byId("mg-audience").value = fields.audience;
  byId("mg-triggers").value = fields.triggers;
  byId("mg-links").value = fields.links;
  byId("mg-body").value = fields.body;
}

async function openConflict(id) {
  // Entry reset: every flag touched by ANY branch is reset here
  // first so no state leaks between successive conflict views.
  state.conflict = null;
  setText("conflict-error", "");
  setText("conflict-state", "");
  setText("conflict-record", "");
  byId("conflict-diff").replaceChildren();
  byId("conflict-diff").hidden = true;
  byId("conflict-diff-fallback").replaceChildren();
  byId("conflict-diff-fallback").hidden = true;
  byId("conflict-meta-diff").querySelector("tbody").replaceChildren();
  byId("conflict-resolution").value = "";
  byId("conflict-resolution").hidden = false;
  byId("conflict-merge").hidden = true;
  byId("conflict-resolve-save").hidden = true;
  byId("conflict-resolve-save").disabled = true;
  byId("conflict-resolve-keep").hidden = false;
  byId("conflict-resolve-keep").disabled = true;
  byId("conflict-resolve-keep").textContent = "Resolve";
  showView("conflict");

  const r = await apiFetch(conflictPath(id), { method: "GET" });
  if (r.authFailure) return;
  if (r.status !== 200) {
    setText("conflict-error", `Load failed: HTTP ${r.status} ${r.body || ""}`);
    return;
  }
  const record = r.json;
  state.conflict = { record, current: null, currentStatus: 0 };

  setText("conflict-title",
    `#${record.id} ${record.kind} ${record.status} — ${record.collection}/${record.family_id}`);
  setText("conflict-record", JSON.stringify(record, null, 2));

  if (record.status === "resolved") {
    // Read-only audit view: hide both resolve buttons and the
    // resolution input.
    byId("conflict-resolution").hidden = true;
    byId("conflict-resolve-keep").hidden = true;
    byId("conflict-resolve-save").hidden = true;
    return;
  }

  const enableResolve = () => {
    byId("conflict-resolve-keep").disabled = false;
    if (record.kind === "edit") {
      byId("conflict-resolve-save").disabled = (state.conflict.currentStatus !== 200 && state.conflict.currentStatus !== 404);
    }
  };

  if (record.kind !== "edit") {
    // claim / policy: no current-doc fetch, both buttons always
    // enabled while open.
    byId("conflict-merge").hidden = true;
    byId("conflict-resolve-save").hidden = true;
    byId("conflict-resolve-keep").textContent = "Resolve";
    enableResolve();
    return;
  }

  // kind === "edit" AND status === "open" — show merge block, fetch
  // current doc.
  byId("conflict-merge").hidden = false;
  byId("conflict-resolve-save").hidden = false;
  byId("conflict-resolve-save").disabled = true; // enabled below on a good current fetch
  byId("conflict-resolve-keep").textContent = "Keep current & resolve";
  byId("conflict-resolve-keep").disabled = true; // enabled below

  // Initialize merge form from attempted (the rejected payload).
  populateMergeForm(toFormFields(record.attempted));

  // The attempted column is the LATEST rejected save. Fetch the
  // family's current state to render the diff.
  let cur;
  try {
    cur = await apiFetch(encodePath(record.collection, record.family_id), { method: "GET" });
  } catch (e) {
    cur = { status: 0, body: String(e), json: null, authFailure: false };
  }
  if (cur.authFailure) return;

  const diffPre = byId("conflict-diff");
  const fb = byId("conflict-diff-fallback");
  const metaTbody = byId("conflict-meta-diff").querySelector("tbody");

  if (cur.status === 200) {
    state.conflict.currentStatus = 200;
    state.conflict.current = cur.json;
    const attemptedBody = (record.attempted && record.attempted.body) || "";
    const aLines = attemptedBody.split("\n");
    const bLines = (cur.json.body || "").split("\n");
    const result = lineDiff(aLines, bLines);
    fb.replaceChildren();
    if (result.tooLarge) {
      diffPre.hidden = true;
      fb.hidden = false;
      renderSideBySide(fb, "attempted", attemptedBody, `v${cur.json.version}`, cur.json.body);
    } else {
      diffPre.hidden = false;
      for (const row of result.rows) {
        const span = document.createElement("span");
        span.className = row.tag;
        const prefix = row.tag === "add" ? "+ " : row.tag === "del" ? "- " : "  ";
        span.textContent = prefix + row.text + "\n";
        diffPre.appendChild(span);
      }
    }
    // Metadata diff: differing rows get class "changed" on the <tr>.
    const rows = metaDiffRows(record.attempted, cur.json);
    metaTbody.replaceChildren();
    for (const r of rows) {
      const tr = document.createElement("tr");
      if (!r.same) tr.classList.add("changed");
      for (const v of [r.field, r.a, r.b]) {
        const td = document.createElement("td");
        td.textContent = v;
        tr.appendChild(td);
      }
      metaTbody.appendChild(tr);
    }
    // Initialize merge form from attempted; the operator can hit
    // "Load current" or "Load attempted" to flip.
    populateMergeForm(toFormFields(record.attempted));
    enableResolve();
  } else if (cur.status === 404) {
    state.conflict.currentStatus = 404;
    state.conflict.current = null;
    setText("conflict-state", "family has no versions on the server");
    const attemptedBody = (record.attempted && record.attempted.body) || "";
    diffPre.hidden = true;
    fb.hidden = false;
    fb.replaceChildren();
    renderSideBySide(fb, "attempted", attemptedBody, "(none)", "");
    enableResolve();
  } else {
    // Anything else (or fetch exception) → fail closed: BOTH
    // resolve buttons disabled.
    state.conflict.currentStatus = 0;
    setText("conflict-error", `current fetch failed: HTTP ${cur.status} ${cur.body || ""}`);
  }
}

async function submitResolve(withSave) {
  if (!state.conflict) return;
  const record = state.conflict.record;
  const resolution = byId("conflict-resolution").value.trim();
  if (!resolution) {
    setText("conflict-error", "resolution text is required");
    return;
  }
  let save = null;
  if (withSave) {
    const parsedLinks = parseLinksJSON(byId("mg-links").value);
    if (parsedLinks.error) {
      setText("conflict-error", parsedLinks.error);
      return;
    }
    save = {
      title: byId("mg-title").value,
      status: byId("mg-status").value,
      tier: byId("mg-tier").value,
      triggers: parseTriggers(byId("mg-triggers").value),
      owner: byId("mg-owner").value,
      audience: byId("mg-audience").value,
      links: parsedLinks.links,
      body: byId("mg-body").value,
    };
    // base_version is the precondition the server uses: omit on
    // the 404 path (no family rows exist), include the current
    // version when one is present.
    if (state.conflict.currentStatus === 200 && state.conflict.current) {
      save.base_version = state.conflict.current.version;
    }
  }
  const r = await apiFetch(resolveConflictPath(record.id), {
    method: "POST",
    body: JSON.stringify(resolvePayload(resolution, record.attempts, save)),
    editor: state.editor,
  });
  if (r.authFailure) return;
  if (r.status === 200) {
    switchView("conflicts", refreshConflicts);
  } else if (r.status === 409) {
    setText("conflict-error", "changed on the server — reloading");
    // Re-run openConflict to refresh all panes (covers both a
    // refreshed attempt and a stale merge base).
    openConflict(record.id);
  } else {
    setText("conflict-error", `HTTP ${r.status} ${r.body || ""}`);
  }
}

// ---------- fleet (build-order step 6) ----------

async function refreshFleet() {
  // Generation guard: a concurrent refresh (nav click, Refresh, or
  // the post-resync refresh) must not let a stale response touch
  // state or the DOM. The guard is verified by code review; the
  // static+pure test posture has no async harness to unit-test it.
  const gen = ++state.fleetGen;
  const r = await apiFetch(hostsPath, { method: "GET" });
  if (gen !== state.fleetGen) return;
  if (r.authFailure) return;
  if (r.status !== 200) {
    showView("error");
    setText("error-text", `Fleet failed: HTTP ${r.status} ${r.body || ""}`);
    return;
  }
  const fleet = parseFleetResponse(r.json);
  if (!fleet) {
    setText("fleet-error", "malformed fleet response");
    return;
  }
  state.fleet = fleet;
  setText("fleet-error", "");
  renderFleet();
}

function renderFleet() {
  const tbody = byId("fleet-table").querySelector("tbody");
  tbody.replaceChildren();
  const fleet = state.fleet;
  if (!fleet || !fleet.hosts || fleet.hosts.length === 0) {
    byId("fleet-empty").hidden = false;
    setText("fleet-summary", "");
    return;
  }
  byId("fleet-empty").hidden = true;
  setText("fleet-summary", fleetSummary(fleet.hosts, fleet.latestReleaseID, fleet.nowMs));
  for (const row of fleet.hosts) {
    const { status, cells } = fleetRowCells(row, fleet.latestReleaseID, fleet.nowMs);
    const tr = document.createElement("tr");
    for (let i = 0; i < cells.length; i++) {
      const td = document.createElement("td");
      td.textContent = cells[i];
      if (i === 1) td.className = "status-" + status;
      tr.appendChild(td);
    }
    // Actions column: per-row closure over `host` is safe — the <tr>
    // is discarded by replaceChildren() on the next render.
    const host = row.host;
    const actionsTd = document.createElement("td");
    const btn = document.createElement("button");
    btn.type = "button";
    btn.textContent = "Force resync";
    btn.addEventListener("click", () => submitResync(host));
    actionsTd.appendChild(btn);
    tr.appendChild(actionsTd);
    tbody.appendChild(tr);
  }
}

async function submitResync(host) {
  const r = await apiFetch(hostResyncPath(host), {
    method: "POST",
    editor: state.editor,
  });
  if (r.authFailure) return;
  if (r.status === 204) {
    // No direct DOM mutation here: refreshFleet clears fleet-error
    // only after its response validates, preserving any current
    // diagnostic until the refreshed state is actually known-good.
    refreshFleet();
    return;
  }
  setText("fleet-error", "resync failed: HTTP " + r.status + " " + (r.body || ""));
}

// ---------- shared fetch + DOM helpers ----------

// apiFetch returns { status, body, json, authFailure }. When a 401
// arrives on a token-bearing call, apiFetch clears stored creds,
// shows the login view, and sets authFailure=true so the caller
// must return immediately rather than render an error.
async function apiFetch(path, opts) {
  const headers = {};
  if (opts.body) headers["Content-Type"] = "application/json";
  // Capture whether THIS request carries credentials: the 401 branch
  // below must not consult mutable state.token afterward, or the
  // second of two overlapping 401s (the first already cleared the
  // token) would masquerade as a non-auth failure and let a caller
  // paint the error view over the login view.
  const authed = !!state.token;
  if (authed) headers["Authorization"] = "Bearer " + state.token;
  if (opts.editor) headers["X-Editor"] = opts.editor;
  let resp;
  try {
    resp = await fetch(path, { method: opts.method, headers, body: opts.body });
  } catch (e) {
    return { status: 0, body: String(e), json: null, authFailure: false };
  }
  const text = await resp.text();
  let json = null;
  try { json = text ? JSON.parse(text) : null; } catch (_) { /* leave json null */ }
  // Late 401: drop stored credentials and bounce to login. The
  // caller decides whether to render an error or to follow us to
  // the login view; we set authFailure so they can branch.
  if (resp.status === 401 && authed && !opts.noAuthOk) {
    clearStoredCredentials();
    clearLoginInputs();
    showLogin();
    return { status: 401, body: text, json, authFailure: true };
  }
  return { status: resp.status, body: text, json, authFailure: false };
}

function showView(name) {
  for (const v of ["login", "browse", "newdoc", "editor", "history", "publish", "conflicts", "conflict", "fleet", "error"]) {
    byId("view-" + v).hidden = (v !== name);
  }
  const navForView = {
    browse: "nav-browse",
    newdoc: "nav-browse",
    editor: "nav-browse",
    history: "nav-browse",
    publish: "nav-publish",
    conflicts: "nav-conflicts",
    conflict: "nav-conflicts",
    fleet: "nav-fleet",
  };
  const activeNav = navForView[name] || "";
  for (const id of ["nav-browse", "nav-publish", "nav-conflicts", "nav-fleet"]) {
    const el = byId(id);
    const active = id === activeNav;
    el.classList.toggle("active", active);
    if (active) el.setAttribute("aria-current", "page");
    else el.removeAttribute("aria-current");
  }
}

function byId(id) { return document.getElementById(id); }

function on(id, ev, fn) {
  const el = byId(id);
  if (el) el.addEventListener(ev, fn);
}

function setText(id, s) {
  const el = byId(id);
  if (el) el.textContent = s == null ? "" : String(s);
}

function showEl(id) { const el = byId(id); if (el) el.hidden = false; }
function hideEl(id) { const el = byId(id); if (el) el.hidden = true; }

function setSelectOptions(id, values, current) {
  const sel = byId(id);
  sel.replaceChildren();
  for (const v of values) {
    const opt = document.createElement("option");
    opt.value = v;
    opt.textContent = v === "" ? "(any)" : v;
    if (v === current) opt.selected = true;
    sel.appendChild(opt);
  }
}
