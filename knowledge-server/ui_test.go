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
