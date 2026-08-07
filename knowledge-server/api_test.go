// Step-4 API tests for the new routes: list, get, history, preview,
// and the conditional cut. Builds on the authFixture from auth_test.go.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"agent-knowledge-kit/knowledge-server/store"
)

// TestListDocs — GET /api/docs returns {"docs": [...]} and the
// empty-store form is the literal `{"docs":[]}\n` (json.Encoder
// trailing newline), not `{"docs":null}` and not a Contains match.
func TestListDocs(t *testing.T) {
	f := newAuthedFixtureWith(t, "operator-secret-token-aaaaaaaaaaaaaaaaa", false)

	// Empty store.
	r, body := f.do(t, http.MethodGet, "/api/docs", f.opsToken(), nil)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("empty list: status=%d, want 200", r.StatusCode)
	}
	// Exact body — assert equality, not Contains. The trailing
	// newline comes from json.Encoder.Encode; the wire form is
	// pinned.
	wantEmpty := `{"docs":[]}` + "\n"
	if string(body) != wantEmpty {
		t.Fatalf("empty list body = %q, want %q", body, wantEmpty)
	}

	// One active doc.
	if _, err := f.s.SaveDoc("docs", "r", store.DocSave{Status: "active", Body: "x"}); err != nil {
		t.Fatal(err)
	}
	r, body = f.do(t, http.MethodGet, "/api/docs", f.opsToken(), nil)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200", r.StatusCode)
	}
	var got struct {
		Docs []store.DocMeta `json:"docs"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Docs) != 1 || got.Docs[0].FamilyID != "r" || got.Docs[0].Version != 1 {
		t.Fatalf("list: %#v", got)
	}
}

// TestGetDocVersionTable — absent → latest; explicit version parses;
// empty, non-numeric, < 1, or overflow → 400 invalid. The success
// rows decode the response and assert the version field PLUS the
// body field, so the test pins ROUTING (which version the API
// chose) not just the HTTP status.
func TestGetDocVersionTable(t *testing.T) {
	f := newAuthedFixture(t)
	// Seed two versions.
	if _, err := f.s.SaveDoc("docs", "v", store.DocSave{Status: "draft", Body: "v1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.SaveDoc("docs", "v", store.DocSave{Status: "active", Body: "v2"}); err != nil {
		t.Fatal(err)
	}

	type expect struct {
		name     string
		path     string
		wantCode int
		wantVer  int    // expected Version field on 200; 0 = no version check
		wantBody string // expected Body field on 200; "" = no body check
	}
	cases := []expect{
		{"absent → latest", "/api/docs/docs/v", 200, 2, "v2"},
		{"explicit valid", "/api/docs/docs/v?version=1", 200, 1, "v1"},
		{"explicit latest version", "/api/docs/docs/v?version=2", 200, 2, "v2"},
		{"empty ?version=", "/api/docs/docs/v?version=", 400, 0, ""},
		{"non-numeric", "/api/docs/docs/v?version=abc", 400, 0, ""},
		{"zero", "/api/docs/docs/v?version=0", 400, 0, ""},
		{"negative", "/api/docs/docs/v?version=-1", 400, 0, ""},
		{"overflow", "/api/docs/docs/v?version=99999999999999999999", 400, 0, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, body := f.do(t, http.MethodGet, c.path, f.opsToken(), nil)
			if r.StatusCode != c.wantCode {
				t.Fatalf("status=%d body=%s, want %d", r.StatusCode, body, c.wantCode)
			}
			if c.wantCode == 400 && !bytes.Contains(body, []byte(`"error":"invalid"`)) {
				t.Fatalf("400 body should carry error=invalid: %s", body)
			}
			if c.wantCode == 200 {
				var got store.Doc
				if err := json.Unmarshal(body, &got); err != nil {
					t.Fatalf("decode 200: %v body=%s", err, body)
				}
				if got.Version != c.wantVer {
					t.Fatalf("version = %d, want %d", got.Version, c.wantVer)
				}
				if got.Body != c.wantBody {
					t.Fatalf("body = %q, want %q", got.Body, c.wantBody)
				}
			}
		})
	}
}

// TestGetDocUnknownFamily — 404 envelope, not 200 with empty body.
func TestGetDocUnknownFamily(t *testing.T) {
	f := newAuthedFixture(t)
	r, body := f.do(t, http.MethodGet, "/api/docs/docs/absent", f.opsToken(), nil)
	if r.StatusCode != http.StatusNotFound {
		t.Fatalf("status=%d, want 404", r.StatusCode)
	}
	if !bytes.Contains(body, []byte(`"error":"not_found"`)) {
		t.Fatalf("body should carry error=not_found: %s", body)
	}
}

// TestDocHistoryNewestFirst — ordering and shape.
func TestDocHistoryNewestFirst(t *testing.T) {
	f := newAuthedFixture(t)
	for i := 0; i < 3; i++ {
		if _, err := f.s.SaveDoc("docs", "h", store.DocSave{Status: "draft", Body: "v"}); err != nil {
			t.Fatal(err)
		}
	}
	r, body := f.do(t, http.MethodGet, "/api/docs/docs/h/versions", f.opsToken(), nil)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", r.StatusCode, body)
	}
	var got struct {
		Versions []store.DocMeta `json:"versions"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Versions) != 3 {
		t.Fatalf("len=%d, want 3", len(got.Versions))
	}
	for i := 0; i < len(got.Versions)-1; i++ {
		if got.Versions[i].Version <= got.Versions[i+1].Version {
			t.Fatalf("not newest-first: %v then %v", got.Versions[i].Version, got.Versions[i+1].Version)
		}
	}
}

// TestReleasePreviewNoActiveDocs — 409 lint envelope.
func TestReleasePreviewLintEnvelope(t *testing.T) {
	// Use a non-seeded fixture: the default newAuthedFixture seeds
	// an active kernel doc and a release, which would make preview
	// return 200, not the 409 lint envelope we're testing here.
	f := newAuthedFixtureWith(t, "operator-secret-token-aaaaaaaaaaaaaaaaa", false)
	r, body := f.do(t, http.MethodGet, "/api/releases/preview", f.opsToken(), nil)
	if r.StatusCode != http.StatusConflict {
		t.Fatalf("status=%d body=%s, want 409", r.StatusCode, body)
	}
	if !bytes.Contains(body, []byte(`"error":"lint"`)) {
		t.Fatalf("body should carry error=lint: %s", body)
	}
}

// TestCutReleaseBodyTable — exhaustive pin of the new body contract:
// empty / whitespace → unconditional (201), stale hash → 409 conflict,
// matching hash → 201, malformed/empty/missing/null hash → 400 invalid
// with NO release row.
func TestCutReleaseBodyTable(t *testing.T) {
	// newAuthedFixture seeds a release; this suite needs a clean
	// per-subtest store so "no release row was inserted" is a real
	// assertion, not a false positive against the seeded row.
	freshAuthed := func(t *testing.T) *authFixture {
		t.Helper()
		return newAuthedFixtureWith(t, "operator-secret-token-aaaaaaaaaaaaaaaaa", false)
	}

	t.Run("empty body unconditional", func(t *testing.T) {
		f := freshAuthed(t)
		if _, err := f.s.SaveDoc("kernel", "kernel", store.DocSave{Status: "active", Body: "k"}); err != nil {
			t.Fatal(err)
		}
		// 0-byte body.
		req, _ := http.NewRequest(http.MethodPost, f.ts.URL+"/api/releases", nil)
		req.Header.Set("Authorization", "Bearer "+f.opsToken())
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("empty body: status=%d, want 201", resp.StatusCode)
		}
	})
	t.Run("whitespace-only body unconditional", func(t *testing.T) {
		f := freshAuthed(t)
		if _, err := f.s.SaveDoc("kernel", "kernel", store.DocSave{Status: "active", Body: "k"}); err != nil {
			t.Fatal(err)
		}
		req, _ := http.NewRequest(http.MethodPost, f.ts.URL+"/api/releases", strings.NewReader("   \n  "))
		req.Header.Set("Authorization", "Bearer "+f.opsToken())
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("whitespace body: status=%d, want 201", resp.StatusCode)
		}
	})
	t.Run("matching hash cuts", func(t *testing.T) {
		f := freshAuthed(t)
		if _, err := f.s.SaveDoc("kernel", "kernel", store.DocSave{Status: "active", Body: "k"}); err != nil {
			t.Fatal(err)
		}
		prev, err := f.s.PreviewRelease()
		if err != nil {
			t.Fatal(err)
		}
		var prevWire struct {
			ReleaseID   int64               `json:"release_id"`
			ContentHash string              `json:"content_hash"`
			CreatedAt   string              `json:"created_at"`
			Docs        []store.ManifestDoc `json:"docs"`
		}
		if err := json.Unmarshal([]byte(jsonFor(t, prev)), &prevWire); err != nil {
			t.Fatal(err)
		}
		r, body := f.do(t, http.MethodPost, "/api/releases", f.opsToken(),
			map[string]any{"expected_content_hash": prev.ContentHash})
		if r.StatusCode != http.StatusCreated {
			t.Fatalf("matching hash: status=%d body=%s, want 201", r.StatusCode, body)
		}
		var cutWire struct {
			ReleaseID   int64               `json:"release_id"`
			ContentHash string              `json:"content_hash"`
			CreatedAt   string              `json:"created_at"`
			Docs        []store.ManifestDoc `json:"docs"`
		}
		if err := json.Unmarshal(body, &cutWire); err != nil {
			t.Fatalf("decode cut wire: %v body=%s", err, body)
		}
		// The cut response and the preview are the same manifest
		// (release_docs rows equal preview docs, content hash
		// matches). ReleaseID on the cut must be > 0 and the docs
		// list must be in the same order.
		if cutWire.ContentHash != prevWire.ContentHash {
			t.Fatalf("cut content_hash = %q, preview = %q", cutWire.ContentHash, prevWire.ContentHash)
		}
		if cutWire.ReleaseID == 0 {
			t.Fatalf("cut ReleaseID = 0, want > 0")
		}
		if !reflect.DeepEqual(cutWire.Docs, prevWire.Docs) {
			t.Fatalf("cut docs != preview docs:\n cut=%#v\n prev=%#v", cutWire.Docs, prevWire.Docs)
		}
		// And CurrentRelease — what an HTTP client would see after
		// the cut — must report the same docs in the same order.
		cur, err := f.s.CurrentRelease()
		if err != nil {
			t.Fatal(err)
		}
		if cur.ContentHash != cutWire.ContentHash {
			t.Fatalf("CurrentRelease hash = %q, cut = %q", cur.ContentHash, cutWire.ContentHash)
		}
		if !manifestDocsEqual(cur.Docs, cutWire.Docs) {
			t.Fatalf("CurrentRelease docs != cut docs:\n cur=%#v\n cut=%#v", cur.Docs, cutWire.Docs)
		}
	})
	t.Run("stale hash conflicts and does not cut", func(t *testing.T) {
		f := freshAuthed(t)
		if _, err := f.s.SaveDoc("kernel", "kernel", store.DocSave{Status: "active", Body: "v1"}); err != nil {
			t.Fatal(err)
		}
		prev, err := f.s.PreviewRelease()
		if err != nil {
			t.Fatal(err)
		}
		// Change the doc so the candidate diverges.
		if _, err := f.s.SaveDoc("kernel", "kernel", store.DocSave{Status: "active", Body: "v2"}); err != nil {
			t.Fatal(err)
		}
		r, body := f.do(t, http.MethodPost, "/api/releases", f.opsToken(),
			map[string]any{"expected_content_hash": prev.ContentHash})
		if r.StatusCode != http.StatusConflict {
			t.Fatalf("stale hash: status=%d body=%s, want 409", r.StatusCode, body)
		}
		if !bytes.Contains(body, []byte(`"error":"conflict"`)) {
			t.Fatalf("body should carry error=conflict: %s", body)
		}
		// No release row was inserted.
		if _, err := f.s.CurrentRelease(); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("stale cut must not insert a release row: %v", err)
		}
	})

	bad := []struct {
		name string
		body string
	}{
		{"empty object", `{}`},
		{"unrelated field", `{"unrelated":true}`},
		{"empty hash", `{"expected_content_hash":""}`},
		{"null hash", `{"expected_content_hash":null}`},
		{"malformed hash", `{"expected_content_hash":"sha256:XYZ"}`},
	}
	for _, b := range bad {
		t.Run("400 "+b.name, func(t *testing.T) {
			f := freshAuthed(t)
			if _, err := f.s.SaveDoc("kernel", "kernel", store.DocSave{Status: "active", Body: "k"}); err != nil {
				t.Fatal(err)
			}
			// Before the request: no release row exists.
			if _, err := f.s.CurrentRelease(); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("setup: want no release yet, got %v", err)
			}
			req, _ := http.NewRequest(http.MethodPost, f.ts.URL+"/api/releases",
				strings.NewReader(b.body))
			req.Header.Set("Authorization", "Bearer "+f.opsToken())
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				rb, _ := io.ReadAll(resp.Body)
				t.Fatalf("%s: status=%d body=%s, want 400", b.name, resp.StatusCode, rb)
			}
			// NO release row was created (the 400 path is a no-op).
			if _, err := f.s.CurrentRelease(); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("%s: a 400 must not insert a release row, but CurrentRelease is %v", b.name, err)
			}
		})
	}

	t.Run("trailing JSON after object is 400", func(t *testing.T) {
		f := freshAuthed(t)
		if _, err := f.s.SaveDoc("kernel", "kernel", store.DocSave{Status: "active", Body: "k"}); err != nil {
			t.Fatal(err)
		}
		body := `{"expected_content_hash":"sha256:` + strings.Repeat("a", 64) + `"}garbage`
		req, _ := http.NewRequest(http.MethodPost, f.ts.URL+"/api/releases", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+f.opsToken())
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			rb, _ := io.ReadAll(resp.Body)
			t.Fatalf("trailing data: status=%d body=%s, want 400", resp.StatusCode, rb)
		}
	})
}

// TestJSONResponsesCarryHardeningHeaders — writeJSON applies
// Cache-Control: no-store and X-Content-Type-Options: nosniff on
// EVERY JSON response, success and error.
func TestJSONResponsesCarryHardeningHeaders(t *testing.T) {
	f := newAuthedFixture(t)

	// Seed something so /api/docs has body.
	if _, err := f.s.SaveDoc("docs", "h", store.DocSave{Status: "draft", Body: "x"}); err != nil {
		t.Fatal(err)
	}

	checkSuccess := func(t *testing.T, r *http.Response) {
		t.Helper()
		if r.Header.Get("Cache-Control") != "no-store" {
			t.Fatalf("Cache-Control=%q, want no-store", r.Header.Get("Cache-Control"))
		}
		if r.Header.Get("X-Content-Type-Options") != "nosniff" {
			t.Fatalf("X-Content-Type-Options=%q, want nosniff", r.Header.Get("X-Content-Type-Options"))
		}
	}
	checkError := checkSuccess

	// Success responses.
	r, _ := f.do(t, http.MethodGet, "/api/docs", f.opsToken(), nil)
	checkSuccess(t, r)
	r, _ = f.do(t, http.MethodGet, "/api/docs/docs/h", f.opsToken(), nil)
	checkSuccess(t, r)
	r, _ = f.do(t, http.MethodGet, "/api/docs/docs/h/versions", f.opsToken(), nil)
	checkSuccess(t, r)
	r, _ = f.do(t, http.MethodGet, "/api/releases/current", f.opsToken(), nil)
	// 404 here (no release yet) is a JSON error envelope too.
	checkError(t, r)

	// Error responses.
	r, _ = f.do(t, http.MethodGet, "/api/docs", "", nil)
	checkError(t, r) // 401
	r, _ = f.do(t, http.MethodGet, "/api/docs/docs/absent", f.opsToken(), nil)
	checkError(t, r) // 404
	r, _ = f.do(t, http.MethodGet, "/api/docs/docs/h?version=abc", f.opsToken(), nil)
	checkError(t, r) // 400
}

// jsonFor marshals v with json.Encoder so test assertions exercise
// the SAME wire form (including the trailing newline) the server
// sends. Store types have json tags; this is the round-trip-safe
// path.
func jsonFor(t *testing.T, v any) string {
	t.Helper()
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(v); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

// manifestDocsEqual compares two doc lists path/family_id/version/
// sha256, in order. Used by the matching-cut regression to pin the
// full manifest equality, not just content_hash.
func manifestDocsEqual(a, b []store.ManifestDoc) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// --- conflict API tests (build-order step 5) ----------------------------

// TestConflictsAuthMatrix — every conflict route is operator-only:
// host token → 403, missing token → 401 (auth-enabled server), for
// GET list, GET one, POST flag, POST resolve.
func TestConflictsAuthMatrix(t *testing.T) {
	f := newAuthedFixture(t)
	hostTok := f.issueToken(t, "h1")
	calls := []struct {
		name   string
		method string
		path   string
		body   any
	}{
		{"list", http.MethodGet, "/api/conflicts", nil},
		{"get", http.MethodGet, "/api/conflicts/1", nil},
		{"flag", http.MethodPost, "/api/conflicts", map[string]any{"collection": "docs", "family_id": "f", "detail": "d"}},
		{"resolve", http.MethodPost, "/api/conflicts/1/resolve", map[string]any{"resolution": "r", "expected_attempts": 1}},
	}
	for _, c := range calls {
		t.Run(c.name, func(t *testing.T) {
			// No token → 401.
			r, _ := f.do(t, c.method, c.path, "", c.body)
			if r.StatusCode != http.StatusUnauthorized {
				t.Fatalf("no token: status=%d, want 401", r.StatusCode)
			}
			if got := r.Header.Get("WWW-Authenticate"); !strings.Contains(got, "Bearer") {
				t.Fatalf("no token: WWW-Authenticate=%q, want Bearer challenge", got)
			}
			// Host token → 403.
			r, _ = f.do(t, c.method, c.path, hostTok, c.body)
			if r.StatusCode != http.StatusForbidden {
				t.Fatalf("host token: status=%d, want 403", r.StatusCode)
			}
			// Operator token → NOT 401/403/5xx. GET /api/conflicts/1
			// returns 404 when no such row exists; that is the
			// correct non-error shape — the auth matrix asserts the
			// route EXISTS and ACCEPTS the operator token, not that
			// a specific id resolves.
			r, _ = f.do(t, c.method, c.path, f.opsToken(), c.body)
			if r.StatusCode == http.StatusUnauthorized || r.StatusCode == http.StatusForbidden || r.StatusCode >= 500 {
				t.Fatalf("operator: status=%d, want non-401/403/5xx", r.StatusCode)
			}
		})
	}
}

// TestPUTStaleSaveReturnsConflictID — the 409 envelope on a stale-base
// save carries a numeric conflict_id; GET /api/conflicts/{id} returns
// the record with the attempted payload.
func TestPUTStaleSaveReturnsConflictID(t *testing.T) {
	f := newAuthedFixture(t)
	if _, err := f.s.SaveDoc("docs", "f", store.DocSave{Status: "active", Body: "v1"}); err != nil {
		t.Fatal(err)
	}
	stale := 0
	r, body := f.do(t, http.MethodPut, "/api/docs/docs/f", f.opsToken(),
		map[string]any{"status": "active", "body": "stale", "base_version": stale})
	if r.StatusCode != http.StatusConflict {
		t.Fatalf("stale save: status=%d body=%s, want 409", r.StatusCode, body)
	}
	if !bytes.Contains(body, []byte(`"error":"conflict"`)) {
		t.Fatalf("body should carry error=conflict: %s", body)
	}
	var env struct {
		Error      string `json:"error"`
		Detail     string `json:"detail"`
		ConflictID *int64 `json:"conflict_id"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatal(err)
	}
	if env.ConflictID == nil {
		t.Fatalf("409 body must carry numeric conflict_id: %s", body)
	}
	// GET the conflict: full record with attempted payload.
	r, body = f.do(t, http.MethodGet, fmt.Sprintf("/api/conflicts/%d", *env.ConflictID), f.opsToken(), nil)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("get conflict: status=%d body=%s", r.StatusCode, body)
	}
	var got store.Conflict
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.Kind != "edit" || got.Status != "open" {
		t.Fatalf("kind/status: %q/%q", got.Kind, got.Status)
	}
	if got.Attempted == nil {
		t.Fatalf("attempted must round-trip")
	}
	if got.Attempted.Body != "stale" {
		t.Fatalf("attempted.body: %q", got.Attempted.Body)
	}
}

// TestCutReleaseLintReturnsConflictID — over-cap kernel cut returns
// 409 "lint" envelope with a numeric conflict_id (policy record was
// committed alongside the rejection). Uses an authFixture so the
// store is directly accessible for seeding.
func TestCutReleaseLintReturnsConflictID(t *testing.T) {
	f := newAuthedFixture(t)
	if _, err := f.s.SaveDoc("kernel", "kernel", store.DocSave{Status: "active", Body: strings.Repeat("x", 24577)}); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPost, f.ts.URL+"/api/releases", nil)
	req.Header.Set("Authorization", "Bearer "+f.opsToken())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		rb, _ := io.ReadAll(resp.Body)
		t.Fatalf("lint cut: status=%d body=%s, want 409", resp.StatusCode, rb)
	}
	rb, _ := io.ReadAll(resp.Body)
	if !bytes.Contains(rb, []byte(`"error":"lint"`)) {
		t.Fatalf("lint body: %s", rb)
	}
	var env struct {
		ConflictID *int64 `json:"conflict_id"`
	}
	_ = json.Unmarshal(rb, &env)
	if env.ConflictID == nil {
		t.Fatalf("lint body must carry numeric conflict_id: %s", rb)
	}
}

// TestResolveConflictOverHTTP — keep-current and with-save paths,
// 404 unknown id, 400 malformed id, 400 empty resolution, 400
// missing/null/0/negative expected_attempts, 409 wrong expected_attempts
// WITHOUT a conflict_id field (no record committed on that path), 400
// save-on-claim, X-Editor propagates to resolved_by and, on the
// with-save path, to the new doc version's editor.
func TestResolveConflictOverHTTP(t *testing.T) {
	f := newAuthedFixture(t)
	// Seed: open an edit conflict via a stale save.
	if _, err := f.s.SaveDoc("docs", "f", store.DocSave{Status: "active", Body: "v1"}); err != nil {
		t.Fatal(err)
	}
	stale := 0
	if _, err := f.s.SaveDoc("docs", "f", store.DocSave{Status: "draft", Body: "stale", BaseVersion: &stale}); err == nil {
		t.Fatal("stale: want error")
	}
	conflicts, err := f.s.ListConflicts("")
	if err != nil {
		t.Fatal(err)
	}
	if len(conflicts) != 1 {
		t.Fatalf("want 1 conflict, got %d", len(conflicts))
	}
	id := conflicts[0].ID

	t.Run("keep-current resolves", func(t *testing.T) {
		r, body := f.do(t, http.MethodPost, fmt.Sprintf("/api/conflicts/%d/resolve", id), f.opsToken(),
			map[string]any{"resolution": "by hand", "expected_attempts": 1})
		if r.StatusCode != http.StatusOK {
			t.Fatalf("status=%d body=%s", r.StatusCode, body)
		}
		var got store.Conflict
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatal(err)
		}
		if got.Status != "resolved" {
			t.Fatalf("status: %q", got.Status)
		}
		if got.WinningVersion != 1 {
			t.Fatalf("winning_version: %d, want 1", got.WinningVersion)
		}
	})
	t.Run("with-save resolves and inserts new doc version", func(t *testing.T) {
		// Open a fresh edit conflict.
		if _, err := f.s.SaveDoc("docs", "g", store.DocSave{Status: "active", Body: "v1"}); err != nil {
			t.Fatal(err)
		}
		if _, err := f.s.SaveDoc("docs", "g", store.DocSave{Status: "draft", Body: "stale", BaseVersion: &stale}); err == nil {
			t.Fatal("stale: want error")
		}
		conflicts2, _ := f.s.ListConflicts("")
		var gid int64
		for _, c := range conflicts2 {
			if c.FamilyID == "g" {
				gid = c.ID
			}
		}
		r, body := f.do(t, http.MethodPost, fmt.Sprintf("/api/conflicts/%d/resolve", gid), f.opsToken(),
			map[string]any{
				"resolution": "merged", "expected_attempts": 1,
				"save": map[string]any{
					"status": "active", "body": "merged-body",
					"base_version": 1,
				},
			})
		if r.StatusCode != http.StatusOK {
			t.Fatalf("status=%d body=%s", r.StatusCode, body)
		}
		var got store.Conflict
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatal(err)
		}
		if got.WinningVersion != 2 {
			t.Fatalf("winning_version: %d, want 2", got.WinningVersion)
		}
		// New doc version is in the store, edited by operator.
		d, err := f.s.GetDoc("docs", "g", 2)
		if err != nil {
			t.Fatal(err)
		}
		if d.Editor != "operator" {
			t.Fatalf("editor: %q, want operator", d.Editor)
		}
	})
	t.Run("X-Editor propagates to resolved_by", func(t *testing.T) {
		if _, err := f.s.SaveDoc("docs", "h", store.DocSave{Status: "active", Body: "v1"}); err != nil {
			t.Fatal(err)
		}
		if _, err := f.s.SaveDoc("docs", "h", store.DocSave{Status: "draft", Body: "stale", BaseVersion: &stale}); err == nil {
			t.Fatal("stale: want error")
		}
		conflicts3, _ := f.s.ListConflicts("")
		var hid int64
		for _, c := range conflicts3 {
			if c.FamilyID == "h" {
				hid = c.ID
			}
		}
		req, _ := http.NewRequest(http.MethodPost, f.ts.URL+fmt.Sprintf("/api/conflicts/%d/resolve", hid),
			strings.NewReader(`{"resolution":"r","expected_attempts":1}`))
		req.Header.Set("Authorization", "Bearer "+f.opsToken())
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Editor", "alice")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			rb, _ := io.ReadAll(resp.Body)
			t.Fatalf("status=%d body=%s", resp.StatusCode, rb)
		}
		var got store.Conflict
		rb, _ := io.ReadAll(resp.Body)
		json.Unmarshal(rb, &got)
		if got.ResolvedBy != "alice" {
			t.Fatalf("resolved_by: %q, want alice", got.ResolvedBy)
		}
	})
	t.Run("X-Editor propagates to doc editor on with-save path", func(t *testing.T) {
		if _, err := f.s.SaveDoc("docs", "i", store.DocSave{Status: "active", Body: "v1"}); err != nil {
			t.Fatal(err)
		}
		if _, err := f.s.SaveDoc("docs", "i", store.DocSave{Status: "draft", Body: "stale", BaseVersion: &stale}); err == nil {
			t.Fatal("stale: want error")
		}
		conflicts4, _ := f.s.ListConflicts("")
		var iid int64
		for _, c := range conflicts4 {
			if c.FamilyID == "i" {
				iid = c.ID
			}
		}
		body := `{"resolution":"r","expected_attempts":1,"save":{"status":"active","body":"merged","base_version":1}}`
		req, _ := http.NewRequest(http.MethodPost, f.ts.URL+fmt.Sprintf("/api/conflicts/%d/resolve", iid),
			strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+f.opsToken())
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Editor", "bob")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			rb, _ := io.ReadAll(resp.Body)
			t.Fatalf("status=%d body=%s", resp.StatusCode, rb)
		}
		d, _ := f.s.GetDoc("docs", "i", 2)
		if d.Editor != "bob" {
			t.Fatalf("editor: %q, want bob", d.Editor)
		}
	})
	t.Run("unknown id → 404", func(t *testing.T) {
		r, body := f.do(t, http.MethodPost, "/api/conflicts/99999/resolve", f.opsToken(),
			map[string]any{"resolution": "r", "expected_attempts": 1})
		if r.StatusCode != http.StatusNotFound {
			t.Fatalf("status=%d body=%s", r.StatusCode, body)
		}
	})
	t.Run("malformed id → 400", func(t *testing.T) {
		r, body := f.do(t, http.MethodPost, "/api/conflicts/abc/resolve", f.opsToken(),
			map[string]any{"resolution": "r", "expected_attempts": 1})
		if r.StatusCode != http.StatusBadRequest {
			t.Fatalf("status=%d body=%s", r.StatusCode, body)
		}
		if !bytes.Contains(body, []byte(`"error":"invalid"`)) {
			t.Fatalf("body: %s", body)
		}
	})
	t.Run("empty resolution → 400", func(t *testing.T) {
		// Open a fresh conflict first.
		if _, err := f.s.SaveDoc("docs", "j", store.DocSave{Status: "active", Body: "v1"}); err != nil {
			t.Fatal(err)
		}
		if _, err := f.s.SaveDoc("docs", "j", store.DocSave{Status: "draft", Body: "stale", BaseVersion: &stale}); err == nil {
			t.Fatal("stale: want error")
		}
		conflicts5, _ := f.s.ListConflicts("")
		var jid int64
		for _, c := range conflicts5 {
			if c.FamilyID == "j" {
				jid = c.ID
			}
		}
		r, body := f.do(t, http.MethodPost, fmt.Sprintf("/api/conflicts/%d/resolve", jid), f.opsToken(),
			map[string]any{"resolution": "", "expected_attempts": 1})
		if r.StatusCode != http.StatusBadRequest {
			t.Fatalf("status=%d body=%s", r.StatusCode, body)
		}
	})
	t.Run("missing/null/0/negative expected_attempts → 400", func(t *testing.T) {
		cases := []struct {
			name string
			body any
		}{
			{"missing", map[string]any{"resolution": "r"}},
			{"null", map[string]any{"resolution": "r", "expected_attempts": nil}},
			{"zero", map[string]any{"resolution": "r", "expected_attempts": 0}},
			{"negative", map[string]any{"resolution": "r", "expected_attempts": -1}},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				r, body := f.do(t, http.MethodPost, fmt.Sprintf("/api/conflicts/%d/resolve", id), f.opsToken(), c.body)
				if r.StatusCode != http.StatusBadRequest {
					t.Fatalf("status=%d body=%s", r.StatusCode, body)
				}
			})
		}
	})
	t.Run("wrong expected_attempts → 409 without conflict_id", func(t *testing.T) {
		// Open a fresh conflict.
		if _, err := f.s.SaveDoc("docs", "k", store.DocSave{Status: "active", Body: "v1"}); err != nil {
			t.Fatal(err)
		}
		if _, err := f.s.SaveDoc("docs", "k", store.DocSave{Status: "draft", Body: "stale", BaseVersion: &stale}); err == nil {
			t.Fatal("stale: want error")
		}
		conflicts6, _ := f.s.ListConflicts("")
		var kid int64
		for _, c := range conflicts6 {
			if c.FamilyID == "k" {
				kid = c.ID
			}
		}
		r, body := f.do(t, http.MethodPost, fmt.Sprintf("/api/conflicts/%d/resolve", kid), f.opsToken(),
			map[string]any{"resolution": "r", "expected_attempts": 99})
		if r.StatusCode != http.StatusConflict {
			t.Fatalf("status=%d body=%s", r.StatusCode, body)
		}
		if !bytes.Contains(body, []byte(`"error":"conflict"`)) {
			t.Fatalf("body: %s", body)
		}
		if bytes.Contains(body, []byte(`conflict_id`)) {
			t.Fatalf("wrong-attempts path must NOT commit a record: %s", body)
		}
		// Conflict still open at attempts=1 (unchanged).
		after, _ := f.s.GetConflict(kid)
		if after.Status != "open" || after.Attempts != 1 {
			t.Fatalf("conflict must be unchanged: %+v", after)
		}
	})
	t.Run("save on claim conflict → 400", func(t *testing.T) {
		if _, err := f.s.FlagClaimConflict("docs", "claim", "", "", "d", "a"); err != nil {
			t.Fatal(err)
		}
		conflicts7, _ := f.s.ListConflicts("")
		var cid int64
		for _, c := range conflicts7 {
			if c.Kind == "claim" {
				cid = c.ID
			}
		}
		r, body := f.do(t, http.MethodPost, fmt.Sprintf("/api/conflicts/%d/resolve", cid), f.opsToken(),
			map[string]any{
				"resolution": "r", "expected_attempts": 1,
				"save": map[string]any{"status": "active", "body": "x"},
			})
		if r.StatusCode != http.StatusBadRequest {
			t.Fatalf("status=%d body=%s", r.StatusCode, body)
		}
	})
}

// TestListConflictsQueryTable — GET /api/conflicts?status=nonsense →
// 400; empty list wire form is literally {"conflicts":[]}.
func TestListConflictsQueryTable(t *testing.T) {
	f := newAuthedFixture(t)
	r, body := f.do(t, http.MethodGet, "/api/conflicts?status=nonsense", f.opsToken(), nil)
	if r.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", r.StatusCode, body)
	}
	if !bytes.Contains(body, []byte(`"error":"invalid"`)) {
		t.Fatalf("body: %s", body)
	}
	// Empty list wire form.
	r, body = f.do(t, http.MethodGet, "/api/conflicts", f.opsToken(), nil)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", r.StatusCode, body)
	}
	want := `{"conflicts":[]}` + "\n"
	if string(body) != want {
		t.Fatalf("empty body = %q, want %q", body, want)
	}
}

// TestPostFlagConflict — 201 on valid (X-Editor → opened_by; default
// "operator"); 400 missing detail; 409 duplicate.
func TestPostFlagConflict(t *testing.T) {
	f := newAuthedFixture(t)
	t.Run("valid with default editor", func(t *testing.T) {
		r, body := f.do(t, http.MethodPost, "/api/conflicts", f.opsToken(),
			map[string]any{"collection": "docs", "family_id": "f", "detail": "d"})
		if r.StatusCode != http.StatusCreated {
			t.Fatalf("status=%d body=%s", r.StatusCode, body)
		}
		var got store.Conflict
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatal(err)
		}
		if got.OpenedBy != "operator" {
			t.Fatalf("opened_by: %q, want operator (default)", got.OpenedBy)
		}
	})
	t.Run("X-Editor propagates", func(t *testing.T) {
		body := `{"collection":"docs","family_id":"q","detail":"d"}`
		req, _ := http.NewRequest(http.MethodPost, f.ts.URL+"/api/conflicts", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+f.opsToken())
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Editor", "alice")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			rb, _ := io.ReadAll(resp.Body)
			t.Fatalf("status=%d body=%s", resp.StatusCode, rb)
		}
		var got store.Conflict
		rb, _ := io.ReadAll(resp.Body)
		json.Unmarshal(rb, &got)
		if got.OpenedBy != "alice" {
			t.Fatalf("opened_by: %q, want alice", got.OpenedBy)
		}
	})
	t.Run("missing detail → 400", func(t *testing.T) {
		r, body := f.do(t, http.MethodPost, "/api/conflicts", f.opsToken(),
			map[string]any{"collection": "docs", "family_id": "x"})
		if r.StatusCode != http.StatusBadRequest {
			t.Fatalf("status=%d body=%s", r.StatusCode, body)
		}
	})
	t.Run("duplicate → 409", func(t *testing.T) {
		// The default-editor subtest already flagged docs/f.
		r, body := f.do(t, http.MethodPost, "/api/conflicts", f.opsToken(),
			map[string]any{"collection": "docs", "family_id": "f", "detail": "d"})
		if r.StatusCode != http.StatusConflict {
			t.Fatalf("status=%d body=%s", r.StatusCode, body)
		}
	})
}
