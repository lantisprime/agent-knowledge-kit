// Tests for the embedded web UI: serving headers, content types,
// asset allow-list, the exact CSP, and the byte-level safety
// contract (no http(s)://, no element-HTML setters, no <script>
// without src=/type="module", no on*= attributes in index.html).
// /api/* auth still rejects unauthenticated requests when auth is
// enabled — registerUI must not weaken that.
package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agent-knowledge-kit/knowledge-server/store"
)

// exactCSP is the single-line wire form of the Content-Security-Policy
// header set on every UI response. The brief pins this exact value.
const exactCSP = "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

func newUIOnlyFixture(t *testing.T) *httptest.Server {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "ui.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	ts := httptest.NewServer(newMux(s, nil))
	t.Cleanup(ts.Close)
	return ts
}

func newUIAuthedFixture(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "ui.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	tf := filepath.Join(t.TempDir(), "operator-token")
	if err := os.WriteFile(tf, []byte("operator-secret-token-aaaaaaaaaaaaaaaaa"), 0o600); err != nil {
		t.Fatal(err)
	}
	auth, err := loadOperatorToken(tf)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(newMux(s, auth))
	t.Cleanup(ts.Close)
	return ts, "operator-secret-token-aaaaaaaaaaaaaaaaa"
}

// readAllBody drains the response body so callers can inspect headers
// without leaving the connection in a bad state.
func readAllBody(t *testing.T, r *http.Response) string {
	t.Helper()
	defer r.Body.Close()
	b, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// assertUIHeaders checks the four hardening headers on a UI response.
func assertUIHeaders(t *testing.T, r *http.Response) {
	t.Helper()
	if got := r.Header.Get("Content-Security-Policy"); got != exactCSP {
		t.Fatalf("CSP header = %q, want %q", got, exactCSP)
	}
	if got := r.Header.Get("X-Frame-Options"); got != "DENY" {
		t.Fatalf("X-Frame-Options = %q, want DENY", got)
	}
	if got := r.Header.Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
	if got := r.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("X-Content-Type-Options = %q, want nosniff", got)
	}
}

func TestUIServesIndexWithExactCSP(t *testing.T) {
	ts := newUIOnlyFixture(t)
	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	body := readAllBody(t, resp)
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d, want 200", resp.StatusCode)
	}
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("Content-Type=%q, want text/html", resp.Header.Get("Content-Type"))
	}
	assertUIHeaders(t, resp)
	if !strings.Contains(body, "<title>knowledge-server curation</title>") {
		t.Fatalf("index body missing expected title")
	}
}

// TestUIDocumentGuidanceAndAsyncEditorGate pins the served HTML and JavaScript
// source contract behind the browser-reviewed UX. It deliberately checks only
// stable wiring markers; the manual browser checklist supplies runtime DOM and
// accessibility evidence without adding a browser dependency to the Go suite.
func TestUIDocumentGuidanceAndAsyncEditorGate(t *testing.T) {
	ts := newUIOnlyFixture(t)

	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	index := readAllBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("index status=%d, want 200", resp.StatusCode)
	}
	for _, want := range []string{
		`id="document-model-help"`,
		`<summary>How documents work</summary>`,
		`id="newdoc-collection-help"`,
		`id="newdoc-family-help"`,
		`id="editor-history" type="button" disabled`,
		`id="editor-reload" type="button" disabled`,
		`id="editor-fields" class="editor-fields" disabled`,
		`id="editor-save-state" class="muted" role="status" aria-live="polite"`,
		`id="history-error" class="error" role="alert" aria-live="polite"`,
	} {
		if !strings.Contains(index, want) {
			t.Errorf("index missing UX contract %q", want)
		}
	}
	historyBack := strings.Index(index, `id="history-back"`)
	historyHiddenControls := strings.Index(index, `id="history-diff-controls" class="row" hidden`)
	if historyBack < 0 || historyHiddenControls < 0 || historyBack > historyHiddenControls {
		t.Errorf("history Back must remain outside the initially hidden diff controls: back=%d controls=%d", historyBack, historyHiddenControls)
	}

	resp, err = http.Get(ts.URL + "/ui/app.js")
	if err != nil {
		t.Fatal(err)
	}
	app := readAllBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("app status=%d, want 200", resp.StatusCode)
	}
	editorStart := strings.Index(app, "async function openEditor(")
	editorEnd := strings.Index(app, "function updateKernelCounters(")
	if editorStart < 0 || editorEnd <= editorStart {
		t.Fatalf("cannot isolate openEditor source: start=%d end=%d", editorStart, editorEnd)
	}
	editorSource := app[editorStart:editorEnd]
	loadStart := strings.Index(editorSource, "setEditorLoading(true)")
	loadFetch := strings.Index(editorSource, "await apiFetch(encodePath(collection, family)")
	loadEnd := strings.Index(editorSource, "setEditorLoading(false)")
	if loadStart < 0 || loadFetch < 0 || loadEnd < 0 || !(loadStart < loadFetch && loadFetch < loadEnd) {
		t.Errorf("editor load gate ordering start=%d fetch=%d end=%d", loadStart, loadFetch, loadEnd)
	}
	if !strings.Contains(editorSource, "if (state.current !== current) return") {
		t.Error("a superseded editor load must not overwrite the current editor")
	}
	if !strings.Contains(editorSource, "setEditorLoadFailed();") {
		t.Error("a non-200/404 load must route to the failed-load gate")
	}
	// UX-2 failure path: any response other than 200/404 keeps editing
	// disabled and enables Reload so the operator can retry.
	failStart := strings.Index(app, "function setEditorLoadFailed(")
	failEnd := strings.Index(app, "async function openEditor(")
	if failStart < 0 || failEnd <= failStart {
		t.Fatalf("cannot isolate setEditorLoadFailed source: start=%d end=%d", failStart, failEnd)
	}
	failSource := app[failStart:failEnd]
	for _, want := range []string{
		`byId("editor-fields").disabled = true;`,
		`byId("editor-history").disabled = true;`,
		`byId("editor-reload").disabled = false;`,
	} {
		if !strings.Contains(failSource, want) {
			t.Errorf("load failure must leave editing disabled and enable Reload, missing %q", want)
		}
	}
	saveStart := strings.Index(app, "async function submitEditorSave(")
	saveEnd := strings.Index(app, "// ---------- history ----------")
	if saveStart < 0 || saveEnd <= saveStart {
		t.Fatalf("cannot isolate submitEditorSave source: start=%d end=%d", saveStart, saveEnd)
	}
	saveSource := app[saveStart:saveEnd]
	for _, want := range []string{
		`const current = state.current;`,
		`if (current.saving) return;`,
		`if (state.current !== current) return;`,
	} {
		if !strings.Contains(saveSource, want) {
			t.Errorf("app missing editor-save safety contract %q", want)
		}
	}
	historyStart := strings.Index(app, "async function enterHistoryView(")
	historyEnd := strings.Index(app, "function renderHistoryList(")
	if historyStart < 0 || historyEnd <= historyStart {
		t.Fatalf("cannot isolate enterHistoryView source: start=%d end=%d", historyStart, historyEnd)
	}
	historySource := app[historyStart:historyEnd]
	for _, want := range []string{
		`if (!sameHistory) clearHistoryContext();`,
		`const generation = ++state.historyGen;`,
		`if (state.current !== current || state.historyGen !== generation) return;`,
		`setText("history-error", "");`,
		`setText("history-error", ` + "`" + `History failed: HTTP ${r.status} ${r.body || ""}` + "`" + `);`,
	} {
		if !strings.Contains(historySource, want) {
			t.Errorf("app missing history safety contract %q", want)
		}
	}
	viewStart := strings.Index(app, "async function viewHistoryVersion(")
	viewEnd := strings.Index(app, "async function diffTwoVersions(")
	if viewStart < 0 || viewEnd <= viewStart {
		t.Fatalf("cannot isolate viewHistoryVersion source: start=%d end=%d", viewStart, viewEnd)
	}
	viewSource := app[viewStart:viewEnd]
	if !strings.Contains(viewSource, `const generation = ++state.historyGen;`) ||
		!strings.Contains(viewSource, `state.historyGen !== generation`) {
		t.Error("version-view requests must ignore superseded same-document responses")
	}
	if !strings.Contains(viewSource, `setText("history-error", `+"`"+`View failed: HTTP ${r.status} ${r.body || ""}`+"`"+`);`) {
		t.Error("version-view failure must use the durable history error region")
	}
	// UX-5/UX-6: diff responses must respect the monotonic history
	// request generation and report failures in the durable error region.
	diffStart := strings.Index(app, "async function diffTwoVersions(")
	diffEnd := strings.Index(app, "// renderSideBySide populates")
	if diffStart < 0 || diffEnd <= diffStart {
		t.Fatalf("cannot isolate diffTwoVersions source: start=%d end=%d", diffStart, diffEnd)
	}
	diffSource := app[diffStart:diffEnd]
	for _, want := range []string{
		`const generation = ++state.historyGen;`,
		`if (state.current !== current || state.historyGen !== generation) return;`,
	} {
		if !strings.Contains(diffSource, want) {
			t.Errorf("diff responses must respect the history request generation, missing %q", want)
		}
	}
	if !strings.Contains(diffSource, `setText("history-error", `+"`"+`Diff fetch failed: ${a.status}/${b.status}`+"`"+`);`) {
		t.Error("diff failure must use the durable history error region")
	}
}

func TestUIRootIsExactPath(t *testing.T) {
	ts := newUIOnlyFixture(t)
	// /foo must NOT fall through to index.html — registerUI uses
	// "GET /{$}" so unrelated paths 404.
	resp, err := http.Get(ts.URL + "/foo")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("GET /foo status=%d, want 404 (root must not be a catch-all)", resp.StatusCode)
	}
}

func TestUIAssetHeadersAndContentTypes(t *testing.T) {
	ts := newUIOnlyFixture(t)
	cases := []struct {
		path        string
		contentType string
	}{
		{"/ui/app.js", "text/javascript"},
		{"/ui/lib.mjs", "text/javascript"},
		{"/ui/style.css", "text/css"},
	}
	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			resp, err := http.Get(ts.URL + c.path)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != 200 {
				t.Fatalf("status=%d, want 200", resp.StatusCode)
			}
			if !strings.HasPrefix(resp.Header.Get("Content-Type"), c.contentType) {
				t.Fatalf("Content-Type=%q, want %q", resp.Header.Get("Content-Type"), c.contentType)
			}
			assertUIHeaders(t, resp)
		})
	}
}

func TestUIRoutesUnknownAsset(t *testing.T) {
	ts := newUIOnlyFixture(t)
	resp, err := http.Get(ts.URL + "/ui/not-a-file.html")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("status=%d, want 404 for unknown asset", resp.StatusCode)
	}
}

func TestUIAssetExtAllowList(t *testing.T) {
	// Even if a file with a non-allowlisted extension were present in
	// the embed, the Content-Type helper returns "" and the handler
	// 404s. We don't need to fabricate a file: verify the helper
	// directly via the only public surface (no file present at /ui/X
	// also 404s, but we want a positive check too).
	ts := newUIOnlyFixture(t)
	resp, err := http.Get(ts.URL + "/ui/whatever.png")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("non-allowlisted ext status=%d, want 404", resp.StatusCode)
	}
}

// TestUIRejectsNotEmbedded — the //go:embed directive pins exactly
// ui/index.html ui/app.js ui/lib.mjs ui/style.css. Anything else
// under /ui/ must 404, including test source files that exist on
// disk but are NOT embedded.
func TestUIRejectsNotEmbedded(t *testing.T) {
	ts := newUIOnlyFixture(t)
	for _, p := range []string{
		"/ui/lib_test.mjs",
		"/ui/index.html",
		"/ui/foo.js",
		"/ui/foo.mjs",
		"/ui/foo.css",
	} {
		t.Run(p, func(t *testing.T) {
			resp, err := http.Get(ts.URL + p)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != 404 {
				t.Fatalf("status=%d, want 404 (not in //go:embed list)", resp.StatusCode)
			}
		})
	}
}

// TestUIBinarySafety — byte-level greps across the embedded UI bytes
// that pin the no-network / no-element-HTML / no-event-handler /
// no-script-without-src-or-type-module contracts. Crude but they
// prevent regressions in the contract.
func TestUIBinarySafety(t *testing.T) {
	// Read each embedded UI byte slice via the public http handler so
	// the test exercises exactly what a browser would fetch.
	ts := newUIOnlyFixture(t)
	files := []string{"/", "/ui/app.js", "/ui/lib.mjs", "/ui/style.css"}
	for _, p := range files {
		t.Run(p, func(t *testing.T) {
			resp, err := http.Get(ts.URL + p)
			if err != nil {
				t.Fatal(err)
			}
			body := readAllBody(t, resp)
			if resp.StatusCode != 200 {
				t.Fatalf("setup fetch %s: status=%d", p, resp.StatusCode)
			}
			banned := []string{
				"http://", "https://",
				"innerHTML", "outerHTML", "insertAdjacentHTML",
				"javascript:",
			}
			for _, sub := range banned {
				if bytes.Contains([]byte(body), []byte(sub)) {
					t.Fatalf("%s contains banned substring %q", p, sub)
				}
			}
			// Only enforce script/on*= rules on the HTML page.
			if p == "/" {
				if bytes.Contains([]byte(body), []byte(" on")) {
					// Cheap check for on*= attributes: any " on" before
					// "=" would catch the prefix; tighten below.
				}
				if !bytes.Contains([]byte(body), []byte(`<script type="module" src="/ui/app.js"`)) {
					t.Fatalf("index must include <script type=\"module\" src=\"/ui/app.js\">")
				}
				// No <script> tags without a src or type="module".
				for _, line := range strings.Split(body, "\n") {
					if strings.Contains(line, "<script") &&
						!strings.Contains(line, `type="module"`) &&
						!strings.Contains(line, "src=") {
						t.Fatalf("index has <script> without src/type=module: %q", line)
					}
				}
				// No on*= attributes.
				for _, line := range strings.Split(body, "\n") {
					for _, attr := range []string{" onclick=", " onerror=", " onload=", " onmouse", " onkey", " onfocus", " onblur", " onsubmit", " onchange"} {
						if strings.Contains(line, attr) {
							t.Fatalf("index has %s attribute: %q", attr, line)
						}
					}
				}
			}
		})
	}
}

func TestUILeavesAPIAuthIntact(t *testing.T) {
	// With auth enabled, /api/* on the same mux must still 401 without
	// a token — registerUI must not relax that.
	ts, _ := newUIAuthedFixture(t)
	resp, err := http.Get(ts.URL + "/api/docs")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("/api/docs without token: status=%d, want 401", resp.StatusCode)
	}
	// The shell at / must still serve (no token) — that's by design.
	resp, err = http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("/ with auth enabled: status=%d, want 200 (shell is public)", resp.StatusCode)
	}
}

// TestUIConflictBannersAndButtons — the step-5 UI changes: the four
// legacy error banners (login-error, newdoc-error, editor-error,
// publish-error) no longer carry the `hidden` attribute (visibility
// now follows the :empty CSS rule, not the static attribute);
// conflict-resolve-save and conflict-resolve-keep DO carry `disabled`
// (read-only audit view + initial-state reset); nav-conflicts and
// editor-merge exist as buttons. style.css carries the .error:empty
// rule that backs the fix.
func TestUIConflictBannersAndButtons(t *testing.T) {
	ts := newUIOnlyFixture(t)

	// Index.
	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d, want 200", resp.StatusCode)
	}
	idxBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	idx := string(idxBody)

	// The four legacy error banners must NOT carry `hidden` anymore.
	// A banner like `<p id="login-error" class="error" hidden>` would
	// fail; the new shape is `<p id="login-error" class="error">`.
	for _, id := range []string{"login-error", "newdoc-error", "editor-error", "publish-error"} {
		// Find the <p id="…"> line and confirm it does NOT contain
		// the `hidden` attribute. A simple substring check is fine
		// here because the four ids are unique and the test is
		// pinning one specific wire change.
		open := "<p id=\"" + id + "\" class=\"error\""
		idx2 := strings.Index(idx, open)
		if idx2 < 0 {
			t.Fatalf("missing banner open tag for %q", id)
		}
		// Walk to the closing '>'.
		end := strings.Index(idx[idx2:], ">")
		if end < 0 {
			t.Fatalf("malformed banner tag for %q", id)
		}
		tag := idx[idx2 : idx2+end+1]
		if strings.Contains(tag, "hidden") {
			t.Fatalf("%s still carries the hidden attribute: %q", id, tag)
		}
	}

	// conflict-resolve-save and conflict-resolve-keep carry disabled.
	for _, id := range []string{"conflict-resolve-save", "conflict-resolve-keep"} {
		open := "<button id=\"" + id + "\" type=\"button\""
		idx2 := strings.Index(idx, open)
		if idx2 < 0 {
			t.Fatalf("missing button open tag for %q", id)
		}
		end := strings.Index(idx[idx2:], ">")
		if end < 0 {
			t.Fatalf("malformed button tag for %q", id)
		}
		tag := idx[idx2 : idx2+end+1]
		if !strings.Contains(tag, "disabled") {
			t.Fatalf("%s must carry the disabled attribute: %q", id, tag)
		}
	}

	// nav-conflicts and editor-merge exist as buttons.
	for _, id := range []string{"nav-conflicts", "editor-merge"} {
		open := "<button id=\"" + id + "\""
		if !strings.Contains(idx, open) {
			t.Fatalf("missing button #%s in index.html", id)
		}
	}

	// style.css carries the .error:empty rule.
	resp, err = http.Get(ts.URL + "/ui/style.css")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("style.css status=%d", resp.StatusCode)
	}
	cssBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(cssBody), ".error:empty") {
		t.Fatalf("style.css missing .error:empty rule")
	}
}

// TestUIFleetSurface — the step-6 UI additions: nav-fleet button in
// the topbar; view-fleet section is present; fleet-table thead
// columns are in the pinned order (host, status, release, last seen,
// error, resync pending since, token issued, actions); fleet-refresh
// and fleet-empty exist; fleet-error has no `hidden` attribute (the
// :empty CSS rule handles visibility); style.css carries the three
// status colors.
func TestUIFleetSurface(t *testing.T) {
	ts := newUIOnlyFixture(t)

	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d, want 200", resp.StatusCode)
	}
	idxBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	idx := string(idxBody)

	// nav-fleet, view-fleet, fleet-refresh, fleet-empty must exist.
	for _, id := range []string{"nav-fleet", "view-fleet", "fleet-refresh", "fleet-empty"} {
		if !strings.Contains(idx, "id=\""+id+"\"") {
			t.Fatalf("missing #%s in index.html", id)
		}
	}

	// fleet-error carries no `hidden` attribute — visibility follows
	// the .error:empty CSS rule.
	open := "<p id=\"fleet-error\" class=\"error\""
	idx2 := strings.Index(idx, open)
	if idx2 < 0 {
		t.Fatalf("missing fleet-error banner open tag")
	}
	end := strings.Index(idx[idx2:], ">")
	if end < 0 {
		t.Fatalf("malformed fleet-error tag")
	}
	tag := idx[idx2 : idx2+end+1]
	if strings.Contains(tag, "hidden") {
		t.Fatalf("fleet-error must not carry hidden: %q", tag)
	}

	// The fleet-table thead must contain exactly the 8 column headers
	// in the pinned order.
	wantHeaders := []string{
		"host", "status", "release", "last seen",
		"error", "resync pending since", "token issued", "actions",
	}
	tableStart := strings.Index(idx, "<table id=\"fleet-table\"")
	if tableStart < 0 {
		t.Fatalf("missing fleet-table")
	}
	theadStart := strings.Index(idx[tableStart:], "<thead>")
	if theadStart < 0 {
		t.Fatalf("missing fleet-table thead")
	}
	theadEnd := strings.Index(idx[tableStart+theadStart:], "</thead>")
	if theadEnd < 0 {
		t.Fatalf("missing fleet-table thead close")
	}
	thead := idx[tableStart+theadStart : tableStart+theadStart+theadEnd+len("</thead>")]
	for _, h := range wantHeaders {
		if !strings.Contains(thead, "<th>"+h+"</th>") {
			t.Fatalf("fleet-table thead missing %q: %s", h, thead)
		}
	}
	// And the order: each <th> appears in the slice order.
	prev := -1
	for _, h := range wantHeaders {
		pos := strings.Index(thead, "<th>"+h+"</th>")
		if pos <= prev {
			t.Fatalf("fleet-table thead out of order around %q: %s", h, thead)
		}
		prev = pos
	}
	// No extra <th> tags in the thead — count "<th" (which also
	// matches an attributed `<th class="…">`) and subtract the
	// "<thead" matches, so a header cell smuggled in with attributes
	// still fails the count.
	if got, want := strings.Count(thead, "<th")-strings.Count(thead, "<thead"), len(wantHeaders); got != want {
		t.Fatalf("fleet-table thead has %d th tags, want %d", got, want)
	}

	// style.css carries the three status colors.
	resp, err = http.Get(ts.URL + "/ui/style.css")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	cssBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	css := string(cssBody)
	for _, rule := range []string{".status-current", ".status-stale", ".status-dark"} {
		if !strings.Contains(css, rule) {
			t.Fatalf("style.css missing %s rule", rule)
		}
	}
}
