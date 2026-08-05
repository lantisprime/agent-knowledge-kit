package main

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agent-knowledge-kit/knowledge-server/store"
)

// authFixture bundles an httptest server backed by the real newMux,
// the operator token it was sealed with, and the store that backs
// both lookup paths (issue/revoke, digest resolution, resync flags).
// Per-endpoint tests build one of these and use its helpers.
type authFixture struct {
	s        *store.Store
	ts       *httptest.Server
	operator string
}

func newAuthedFixture(t *testing.T) *authFixture {
	t.Helper()
	return newAuthedFixtureWith(t, "operator-secret-token-aaaaaaaaaaaa", true)
}

// newAuthedFixtureWith seals the server with the given operator token
// (passing it through a temp file so loadOperatorToken's trim/empty
// path is exercised). seedRelease builds the release-1 fixture when
// true, which the archive + GET-current tests need.
func newAuthedFixtureWith(t *testing.T, operator string, seedRelease bool) *authFixture {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "auth.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if seedRelease {
		if _, err := s.SaveDoc("kernel", "kernel", store.DocSave{Status: "active", Body: "rules"}); err != nil {
			t.Fatal(err)
		}
		// Seed a docs/runbook family so the step-4 auth-matrix entries
		// that target it (get_doc, doc_history) hit a known resource
		// and pass the existing "valid token → 2xx" assertion.
		if _, err := s.SaveDoc("docs", "runbook", store.DocSave{Status: "draft", Body: "x"}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.CutRelease(""); err != nil {
			t.Fatal(err)
		}
	}
	tf := filepath.Join(t.TempDir(), "operator-token")
	if err := os.WriteFile(tf, []byte(operator), 0o600); err != nil {
		t.Fatal(err)
	}
	auth, err := loadOperatorToken(tf)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(newMux(s, auth))
	t.Cleanup(ts.Close)
	return &authFixture{s: s, ts: ts, operator: operator}
}

func newUnauthFixture(t *testing.T, seedRelease bool) *httptest.Server {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "auth.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if seedRelease {
		if _, err := s.SaveDoc("kernel", "kernel", store.DocSave{Status: "active", Body: "rules"}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.CutRelease(""); err != nil {
			t.Fatal(err)
		}
	}
	ts := httptest.NewServer(newMux(s, nil))
	t.Cleanup(ts.Close)
	return ts
}

// do is a tiny HTTP client that always sends a fresh body and fills in
// Authorization when token is non-empty. It returns the response with
// the body already read into memory.
func (f *authFixture) do(t *testing.T, method, path, token string, body any) (*http.Response, []byte) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, f.ts.URL+path, rdr)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, buf
}

func (f *authFixture) opsToken() string { return f.operator }
func (f *authFixture) issueToken(t *testing.T, host string) string {
	t.Helper()
	resp, body := f.do(t, http.MethodPost, "/api/hosts/"+host+"/token", f.opsToken(), nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("issue token %s: status=%d body=%s", host, resp.StatusCode, body)
	}
	var got struct {
		Host  string `json:"host"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.Host != host {
		t.Fatalf("issue token: host=%q, want %q", got.Host, host)
	}
	if len(got.Token) != 64 {
		t.Fatalf("issue token: token len=%d, want 64", len(got.Token))
	}
	if _, err := hex.DecodeString(got.Token); err != nil {
		t.Fatalf("issue token: token not hex: %v", err)
	}
	return got.Token
}

// TestLoadOperatorTokenTrims — surrounded whitespace and a trailing
// newline must not defeat the comparison, and an empty file must
// surface as an error.
func TestLoadOperatorTokenTrims(t *testing.T) {
	cases := []struct {
		name     string
		contents string
		wantErr  bool
	}{
		{"plain", "secret-token-aaaaaaaaaaaaaaaaaaaaa", false},
		{"trailing newline", "secret-token-aaaaaaaaaaaaaaaaaaaaa\n", false},
		{"surrounded whitespace", "  secret-token-aaaaaaaaaaaaaaaaaaaaa  \n", false},
		{"empty", "", true},
		{"whitespace only", "   \n", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := filepath.Join(t.TempDir(), "tok")
			if err := os.WriteFile(f, []byte(tc.contents), 0o600); err != nil {
				t.Fatal(err)
			}
			auth, err := loadOperatorToken(f)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if auth == nil {
				t.Fatalf("nil auth on success")
			}
		})
	}
}

// TestLoadOperatorTokenMinLength (FIX 2, F2) — a short operator token
// is rejected at startup with a clear error; exactly 32 characters is
// the boundary that passes.
func TestLoadOperatorTokenMinLength(t *testing.T) {
	cases := []struct {
		name    string
		length  int
		wantErr bool
	}{
		{"31 chars rejected", 31, true},
		{"32 chars accepted", 32, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			token := strings.Repeat("a", tc.length)
			f := filepath.Join(t.TempDir(), "tok")
			if err := os.WriteFile(f, []byte(token), 0o600); err != nil {
				t.Fatal(err)
			}
			auth, err := loadOperatorToken(f)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got nil")
				}
				if auth != nil {
					t.Fatalf("want nil auth on error, got %#v", auth)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if auth == nil {
				t.Fatalf("nil auth on success")
			}
		})
	}
}

// TestAuthMatrix — every endpoint: no token / garbage token / host
// token (for operator-only routes) / operator token / host token (for
// any-token routes). Pins the 401/403/2xx contract end-to-end. Each
// sub-test builds its own fixture so the issue/token sub-test rotating
// the host token cannot invalidate the others' host tokens.
func TestAuthMatrix(t *testing.T) {
	type call struct {
		name   string
		method string
		path   string
		body   any
		opOnly bool
	}
	calls := []call{
		{"save doc", http.MethodPut, "/api/docs/docs/runbook", map[string]any{"status": "draft", "body": "x"}, true},
		{"cut release", http.MethodPost, "/api/releases", nil, true},
		{"request resync", http.MethodPost, "/api/hosts/h1/resync", nil, true},
		{"issue token", http.MethodPost, "/api/hosts/h1/token", nil, true},
		{"revoke token", http.MethodDelete, "/api/hosts/h1/token", nil, true},
		{"get current", http.MethodGet, "/api/releases/current", nil, false},
		{"get archive", http.MethodGet, "/api/releases/1/archive", nil, false},
		{"heartbeat", http.MethodPost, "/api/heartbeats", map[string]any{"ok": true}, false},
		// Step-4 curation surface: every list / fetch / preview /
		// cut-with-precondition route is operator-only.
		{"list docs", http.MethodGet, "/api/docs", nil, true},
		{"get doc", http.MethodGet, "/api/docs/docs/runbook", nil, true},
		{"doc history", http.MethodGet, "/api/docs/docs/runbook/versions", nil, true},
		{"release preview", http.MethodGet, "/api/releases/preview", nil, true},
	}
	for _, c := range calls {
		t.Run(c.name, func(t *testing.T) {
			f := newAuthedFixture(t)
			hostTok := f.issueToken(t, "h1")
			successTok := f.opsToken()
			if !c.opOnly {
				successTok = hostTok
			}
			// No token → 401 with WWW-Authenticate.
			r, _ := f.do(t, c.method, c.path, "", c.body)
			if r.StatusCode != http.StatusUnauthorized {
				t.Fatalf("no token: status=%d, want 401", r.StatusCode)
			}
			if got := r.Header.Get("WWW-Authenticate"); !strings.Contains(got, "Bearer") {
				t.Fatalf("no token: WWW-Authenticate=%q, want Bearer challenge", got)
			}
			// Garbage token → 401.
			r, _ = f.do(t, c.method, c.path, "definitely-not-a-token", c.body)
			if r.StatusCode != http.StatusUnauthorized {
				t.Fatalf("garbage token: status=%d, want 401", r.StatusCode)
			}
			if c.opOnly {
				// Host token on an operator-only route → 403.
				r, _ = f.do(t, c.method, c.path, hostTok, c.body)
				if r.StatusCode != http.StatusForbidden {
					t.Fatalf("host token on operator-only: status=%d, want 403", r.StatusCode)
				}
			}
			// successTok (operator or host) → 2xx.
			r, _ = f.do(t, c.method, c.path, successTok, c.body)
			if r.StatusCode < 200 || r.StatusCode >= 300 {
				t.Fatalf("valid token: status=%d, want 2xx", r.StatusCode)
			}
		})
	}
}

// TestAuthErrorBodies — 401/403/413 envelopes are the documented
// {"error","detail"} shape. The 401 body says "unauthorized", the 403
// body says "forbidden".
func TestAuthErrorBodies(t *testing.T) {
	f := newAuthedFixture(t)
	hostTok := f.issueToken(t, "h1")

	r, body := f.do(t, http.MethodGet, "/api/releases/current", "", nil)
	if r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status=%d, want 401", r.StatusCode)
	}
	if !bytes.Contains(body, []byte(`"error":"unauthorized"`)) {
		t.Fatalf("401 body should carry error=unauthorized: %s", body)
	}

	r, body = f.do(t, http.MethodPut, "/api/docs/docs/runbook", hostTok, map[string]any{"status": "draft", "body": "x"})
	if r.StatusCode != http.StatusForbidden {
		t.Fatalf("status=%d, want 403", r.StatusCode)
	}
	if !bytes.Contains(body, []byte(`"error":"forbidden"`)) {
		t.Fatalf("403 body should carry error=forbidden: %s", body)
	}
}

// TestHostBindingCurrentRelease — the token IS the host identity. A
// subscriber's ?host= must agree with the bound host, and an empty
// ?host= resolves to the bound host (so the subscriber does not need
// to coordinate its hostname out-of-band).
func TestHostBindingCurrentRelease(t *testing.T) {
	f := newAuthedFixture(t)
	hostTok := f.issueToken(t, "h1")

	// Mismatch → 403.
	r, _ := f.do(t, http.MethodGet, "/api/releases/current?host=other", hostTok, nil)
	if r.StatusCode != http.StatusForbidden {
		t.Fatalf("?host=other: status=%d, want 403", r.StatusCode)
	}
	// Self → 200.
	r, _ = f.do(t, http.MethodGet, "/api/releases/current?host=h1", hostTok, nil)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("?host=h1: status=%d, want 200", r.StatusCode)
	}
	// No param → 200, AND the resync flag reflects the TOKENS' host,
	// not the operator's (the operator has no host, which here is the
	// pre-authN loopback meaning "no per-host resync status").
	if err := f.s.RequestResync("h1"); err != nil {
		t.Fatal(err)
	}
	r, body := f.do(t, http.MethodGet, "/api/releases/current", hostTok, nil)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("no param: status=%d, want 200", r.StatusCode)
	}
	if !bytes.Contains(body, []byte(`"resync":true`)) {
		t.Fatalf("no param: body should report resync=true for bound host: %s", body)
	}
}

// TestHostBindingHeartbeat — heartbeat body host must agree with the
// token's bound host; an empty body host resolves to the bound host.
func TestHostBindingHeartbeat(t *testing.T) {
	f := newAuthedFixture(t)
	hostTok := f.issueToken(t, "h1")

	// Pre-seed the resync flag so the heartbeat can clear it.
	if err := f.s.RequestResync("h1"); err != nil {
		t.Fatal(err)
	}

	// Mismatched body host → 403.
	r, _ := f.do(t, http.MethodPost, "/api/heartbeats", hostTok, map[string]any{
		"host": "other", "ok": true, "resync_applied": true,
	})
	if r.StatusCode != http.StatusForbidden {
		t.Fatalf("body host=other: status=%d, want 403", r.StatusCode)
	}
	// Empty body host → 204; flag clears server-side.
	r, _ = f.do(t, http.MethodPost, "/api/heartbeats", hostTok, map[string]any{
		"ok": true, "resync_applied": true,
	})
	if r.StatusCode != http.StatusNoContent {
		t.Fatalf("empty body host: status=%d, want 204", r.StatusCode)
	}
	pending, err := f.s.ResyncPending("h1")
	if err != nil {
		t.Fatal(err)
	}
	if pending {
		t.Fatalf("resync flag should have been cleared by ok+resync_applied heartbeat")
	}
}

// TestIssueRevokeOverHTTP — round-trip a token via the API: issue,
// use it, reissue (old 401, new 200), delete (401), delete again (404).
func TestIssueRevokeOverHTTP(t *testing.T) {
	f := newAuthedFixture(t)

	// Issue.
	r, body := f.do(t, http.MethodPost, "/api/hosts/h1/token", f.opsToken(), nil)
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("issue: status=%d body=%s", r.StatusCode, body)
	}
	var got struct {
		Host  string `json:"host"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.Host != "h1" || len(got.Token) != 64 {
		t.Fatalf("issue: %+v", got)
	}
	first := got.Token

	// The freshly issued token works on any-token routes.
	r, _ = f.do(t, http.MethodGet, "/api/releases/current", first, nil)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("first token: status=%d, want 200", r.StatusCode)
	}

	// Reissue → new token, old stops working.
	r, body = f.do(t, http.MethodPost, "/api/hosts/h1/token", f.opsToken(), nil)
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("reissue: status=%d body=%s", r.StatusCode, body)
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	second := got.Token
	if second == first {
		t.Fatalf("reissue must yield a fresh token")
	}
	r, _ = f.do(t, http.MethodGet, "/api/releases/current", first, nil)
	if r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("old token after reissue: status=%d, want 401", r.StatusCode)
	}
	r, _ = f.do(t, http.MethodGet, "/api/releases/current", second, nil)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("new token: status=%d, want 200", r.StatusCode)
	}

	// Revoke → both tokens 401.
	r, _ = f.do(t, http.MethodDelete, "/api/hosts/h1/token", f.opsToken(), nil)
	if r.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke: status=%d, want 204", r.StatusCode)
	}
	r, _ = f.do(t, http.MethodGet, "/api/releases/current", second, nil)
	if r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("after revoke: status=%d, want 401", r.StatusCode)
	}
	// Revoke again → 404 (store.ErrNotFound).
	r, body = f.do(t, http.MethodDelete, "/api/hosts/h1/token", f.opsToken(), nil)
	if r.StatusCode != http.StatusNotFound {
		t.Fatalf("revoke absent: status=%d body=%s, want 404", r.StatusCode, body)
	}
}

// TestAuthDisabled — a mux built with auth == nil is the pre-authN
// loopback posture: every endpoint accepts every caller, no token
// header is required, and the response codes match the original
// step-2 contract.
func TestAuthDisabled(t *testing.T) {
	ts := newUnauthFixture(t, true)

	// SaveDoc.
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/docs/docs/runbook",
		strings.NewReader(`{"status":"draft","body":"x"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("save doc: status=%d, want 200", resp.StatusCode)
	}
	// CutRelease.
	resp, err = http.Post(ts.URL+"/api/releases", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("cut release: status=%d, want 201", resp.StatusCode)
	}
	// GET current.
	resp, err = http.Get(ts.URL + "/api/releases/current")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get current: status=%d, want 200", resp.StatusCode)
	}
	// GET archive.
	resp, err = http.Get(ts.URL + "/api/releases/1/archive")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get archive: status=%d, want 200", resp.StatusCode)
	}
	// Heartbeat.
	resp, err = http.Post(ts.URL+"/api/heartbeats", "application/json",
		strings.NewReader(`{"host":"h1","ok":true}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("heartbeat: status=%d, want 204", resp.StatusCode)
	}
	// Token endpoints (no auth) still work: issue returns 201, revoke 204.
	req, _ = http.NewRequest(http.MethodPost, ts.URL+"/api/hosts/h1/token", nil)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("issue token: status=%d, want 201", resp.StatusCode)
	}
	req, _ = http.NewRequest(http.MethodDelete, ts.URL+"/api/hosts/h1/token", nil)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke token: status=%d, want 204", resp.StatusCode)
	}
}

// TestOversizedBody — MaxBytesReader is the inbound body cap. The
// kernel byte cap is 24 KiB; the limit is 1 MiB. A 1 MiB+1 byte
// payload should be rejected at the gate with 413 "too_large", not
// at SaveDoc with whatever the doc refused.
func TestOversizedBody(t *testing.T) {
	f := newAuthedFixture(t)
	big := strings.Repeat("a", (1<<20)+1)
	req, _ := http.NewRequest(http.MethodPut, f.ts.URL+"/api/docs/docs/runbook",
		strings.NewReader(`{"status":"draft","body":"`+big+`"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+f.opsToken())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body: status=%d, want 413", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !bytes.Contains(body, []byte(`"error":"too_large"`)) {
		t.Fatalf("413 body should carry error=too_large: %s", body)
	}
}

// TestDecodeBodySingleJSONValue (FIX 3, F3) — decodeBody must consume
// exactly one JSON value from the request body. Trailing garbage is
// rejected as 400 invalid (the doc was never accepted); trailing
// padding that pushes the body past the 1 MiB cap is rejected as 413
// too_large because the cap fires while the second Decode drains.
func TestDecodeBodySingleJSONValue(t *testing.T) {
	f := newAuthedFixture(t)

	t.Run("trailing garbage after JSON", func(t *testing.T) {
		body := `{"status":"draft","body":"x"}garbage`
		req, _ := http.NewRequest(http.MethodPut, f.ts.URL+"/api/docs/docs/runbook",
			strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+f.opsToken())
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("trailing garbage: status=%d, want 400", resp.StatusCode)
		}
		rb, _ := io.ReadAll(resp.Body)
		if !bytes.Contains(rb, []byte(`"error":"invalid"`)) {
			t.Fatalf("trailing garbage: body should carry error=invalid: %s", rb)
		}
	})

	t.Run("trailing padding past 1 MiB cap", func(t *testing.T) {
		// Whitespace padding: json.Decoder skips leading whitespace
		// before raising a syntax error, so the second Decode keeps
		// reading until the underlying MaxBytesReader cap fires with
		// *http.MaxBytesError (413), not a premature "bad character"
		// syntax error (400).
		padding := strings.Repeat(" ", (1<<20)+1)
		body := `{"status":"draft","body":"x"}` + padding
		req, _ := http.NewRequest(http.MethodPut, f.ts.URL+"/api/docs/docs/runbook",
			strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+f.opsToken())
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusRequestEntityTooLarge {
			t.Fatalf("oversized padding: status=%d, want 413", resp.StatusCode)
		}
		rb, _ := io.ReadAll(resp.Body)
		if !bytes.Contains(rb, []byte(`"error":"too_large"`)) {
			t.Fatalf("oversized padding: body should carry error=too_large: %s", rb)
		}
	})
}

// TestInitOperatorTokenCreatesFile (FIX F2) — with -init-operator-token
// on a missing file: bootstrapOperatorToken generates a 64-hex-char
// token at the path with mode 0600, and the freshly written file is
// accepted by loadOperatorToken (length + digest path both pass).
func TestInitOperatorTokenCreatesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tok")
	auth, err := bootstrapOperatorToken(path, true)
	if err != nil {
		t.Fatalf("bootstrap with init on missing file: %v", err)
	}
	if auth == nil {
		t.Fatalf("want non-nil auth after generated init")
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat generated token file: %v", err)
	}
	if got := info.Size(); got != 64 {
		t.Fatalf("token file size = %d, want 64 hex chars", got)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("token file mode = %o, want 0600", got)
	}

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(body); got != 64 {
		t.Fatalf("token body length = %d, want 64", got)
	}
	if _, err := hex.DecodeString(string(body)); err != nil {
		t.Fatalf("token is not hex: %v", err)
	}

	// A fresh authState from the just-written file should authenticate
	// the same plaintext that was generated (digest round-trips).
	if _, err := loadOperatorToken(path); err != nil {
		t.Fatalf("loadOperatorToken on freshly initialized file: %v", err)
	}
}

// TestInitOperatorTokenRefusesExisting (FIX F2) — O_CREATE|O_EXCL
// must refuse to clobber a pre-existing token file. The file's
// contents and mode are unchanged after the failed init.
func TestInitOperatorTokenRefusesExisting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tok")
	prior := "operator-secret-token-aaaaaaaaaaaaaaaaa"
	if err := os.WriteFile(path, []byte(prior), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := initOperatorToken(path); err == nil {
		t.Fatalf("want error on existing file, got nil")
	}

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != prior {
		t.Fatalf("existing file content was changed: got %q, want %q", body, prior)
	}
	info, _ := os.Stat(path)
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("mode changed: got %o, want 0600", got)
	}
}

// TestBootstrapOperatorToken (FIX F2) — the table around the new
// generation path: empty path = auth disabled; missing file without
// init = an error that points the operator at -init-operator-token;
// missing file with init = generated; existing file without init =
// loaded; existing file with init = refused.
func TestBootstrapOperatorToken(t *testing.T) {
	t.Run("empty path returns nil auth (auth disabled)", func(t *testing.T) {
		auth, err := bootstrapOperatorToken("", false)
		if err != nil {
			t.Fatal(err)
		}
		if auth != nil {
			t.Fatalf("want nil auth for empty path, got %#v", auth)
		}
	})

	t.Run("missing file without init mentions -init-operator-token", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "tok")
		_, err := bootstrapOperatorToken(path, false)
		if err == nil {
			t.Fatalf("want error, got nil")
		}
		if !strings.Contains(err.Error(), "-init-operator-token") {
			t.Fatalf("error must mention -init-operator-token: %v", err)
		}
	})

	t.Run("missing file with init generates", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "tok")
		auth, err := bootstrapOperatorToken(path, true)
		if err != nil {
			t.Fatalf("bootstrap with init on missing file: %v", err)
		}
		if auth == nil {
			t.Fatalf("want non-nil auth on generated init")
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("token file should exist after init: %v", err)
		}
	})

	t.Run("existing file without init loads", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "tok")
		if err := os.WriteFile(path, []byte("operator-secret-token-aaaaaaaaaaaaaaaaa"), 0o600); err != nil {
			t.Fatal(err)
		}
		auth, err := bootstrapOperatorToken(path, false)
		if err != nil {
			t.Fatal(err)
		}
		if auth == nil {
			t.Fatalf("want non-nil auth on loaded token")
		}
	})

	t.Run("existing file with init refuses without touching content", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "tok")
		prior := "operator-secret-token-aaaaaaaaaaaaaaaaa"
		if err := os.WriteFile(path, []byte(prior), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := bootstrapOperatorToken(path, true); err == nil {
			t.Fatalf("want error on existing file with init, got nil")
		}
		body, _ := os.ReadFile(path)
		if string(body) != prior {
			t.Fatalf("existing file content was changed: got %q, want %q", body, prior)
		}
	})
}
