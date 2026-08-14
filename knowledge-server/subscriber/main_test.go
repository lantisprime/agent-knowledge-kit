package main

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// --- test fixtures: independent of the store package, per the hard
// boundary that the subscriber (and its tests) must not import it. ---

type testDoc struct {
	path string
	body []byte
}

func buildTar(docs []testDoc) []byte {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, d := range docs {
		if err := tw.WriteHeader(&tar.Header{
			Name: d.path, Mode: 0o644, Size: int64(len(d.body)), ModTime: time.Now(),
		}); err != nil {
			panic(err)
		}
		if _, err := tw.Write(d.body); err != nil {
			panic(err)
		}
	}
	if err := tw.Close(); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

// computeContentHash independently reproduces the pinned content-hash
// construction (docs/plans/knowledge-server.md) using only stdlib
// crypto/sha256, so the test does not need to import the store package.
func computeContentHash(docs []testDoc) string {
	h := sha256.New()
	for _, d := range docs {
		sum := sha256.Sum256(d.body)
		io.WriteString(h, d.path)
		h.Write([]byte{0})
		h.Write(sum[:])
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

func docsToManifest(docs []testDoc) []manifestDoc {
	out := make([]manifestDoc, len(docs))
	for i, d := range docs {
		sum := sha256.Sum256(d.body)
		out[i] = manifestDoc{Path: d.path, SHA256: hex.EncodeToString(sum[:])}
	}
	return out
}

type heartbeatCapture struct {
	Host          string  `json:"host"`
	ReleaseID     int64   `json:"release_id"`
	OK            bool    `json:"ok"`
	Error         *string `json:"error"`
	ResyncApplied bool    `json:"resync_applied"`
}

// fakeServer is a minimal stand-in for the real knowledge-server,
// serving canned /api/releases/current, /api/releases/{id}/archive, and
// /api/heartbeats responses so these tests need no real server.
type fakeServer struct {
	mu                sync.Mutex
	current           currentRelease
	archives          map[int64][]byte
	archiveHits       map[int64]int
	heartbeats        []heartbeatCapture
	requireToken      string         // when non-empty, every handler 401s unless Authorization matches
	hostQueries       []string       // ?host= values seen on /current, in arrival order ("" when absent)
	authorizedHits    map[string]int // endpoint key ("current","archive","heartbeat") -> count of authorized requests
	protocolResponses map[string]string
	protocolRequests  map[string][]string
	redirectOnCurrent bool // when true, /current returns 302 to /sentinel (FIX 1 redirect-refusal test)
	sentinelHits      int  // hits on /sentinel, used to assert the redirect is refused client-side
}

func newFakeServer() *fakeServer {
	return &fakeServer{
		archives:          map[int64][]byte{},
		archiveHits:       map[int64]int{},
		authorizedHits:    map[string]int{},
		protocolResponses: map[string]string{},
		protocolRequests:  map[string][]string{},
	}
}

func (f *fakeServer) setProtocolResponse(endpoint, version string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.protocolResponses[endpoint] = version
}

func (f *fakeServer) recordProtocol(w http.ResponseWriter, r *http.Request, endpoint string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.protocolRequests[endpoint] = append(f.protocolRequests[endpoint],
		r.Header.Values("Agent-Knowledge-Protocol-Version")...)
	if version := f.protocolResponses[endpoint]; version != "" {
		w.Header().Set("Agent-Knowledge-Protocol-Version", version)
	}
}

func (f *fakeServer) protocolRequestsSeen(endpoint string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.protocolRequests[endpoint]))
	copy(out, f.protocolRequests[endpoint])
	return out
}

func (f *fakeServer) setRequireToken(tok string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requireToken = tok
}

func (f *fakeServer) hostQueriesSeen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.hostQueries))
	copy(out, f.hostQueries)
	return out
}

func (f *fakeServer) authorizedHitsByEndpoint() map[string]int {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]int, len(f.authorizedHits))
	for k, v := range f.authorizedHits {
		out[k] = v
	}
	return out
}

// authorize checks the Authorization header against requireToken (when
// set) and, on match, records the hit under the given endpoint key.
// Returns true when the caller is authorized.
func (f *fakeServer) authorize(r *http.Request, endpoint string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.requireToken == "" {
		f.authorizedHits[endpoint]++
		return true
	}
	if r.Header.Get("Authorization") == "Bearer "+f.requireToken {
		f.authorizedHits[endpoint]++
		return true
	}
	return false
}

func (f *fakeServer) setCurrent(cur currentRelease) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.current = cur
}

func (f *fakeServer) setArchive(id int64, body []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.archives[id] = body
}

func (f *fakeServer) archiveHitCount(id int64) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.archiveHits[id]
}

func (f *fakeServer) lastHeartbeat() heartbeatCapture {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.heartbeats[len(f.heartbeats)-1]
}

func (f *fakeServer) heartbeatCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.heartbeats)
}

func (f *fakeServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/releases/current", func(w http.ResponseWriter, r *http.Request) {
		f.recordProtocol(w, r, "current")
		if !f.authorize(r, "current") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		f.mu.Lock()
		redirect := f.redirectOnCurrent
		f.hostQueries = append(f.hostQueries, r.URL.Query().Get("host"))
		f.mu.Unlock()
		if redirect {
			http.Redirect(w, r, "/sentinel", http.StatusFound)
			return
		}
		f.mu.Lock()
		cur := f.current
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(cur)
	})
	mux.HandleFunc("GET /api/releases/{id}/archive", func(w http.ResponseWriter, r *http.Request) {
		f.recordProtocol(w, r, "archive")
		if !f.authorize(r, "archive") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		body, ok := f.archives[id]
		f.archiveHits[id]++
		f.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/x-tar")
		w.Write(body)
	})
	mux.HandleFunc("POST /api/heartbeats", func(w http.ResponseWriter, r *http.Request) {
		f.recordProtocol(w, r, "heartbeat")
		if !f.authorize(r, "heartbeat") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var hb heartbeatCapture
		json.NewDecoder(r.Body).Decode(&hb)
		f.mu.Lock()
		f.heartbeats = append(f.heartbeats, hb)
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	// /sentinel is registered on every fake so the FIX 1 redirect-refusal
	// test can prove the client never re-issues a request to the redirect
	// target (the subscriber's CheckRedirect returns an error instead).
	mux.HandleFunc("GET /sentinel", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.sentinelHits++
		f.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
	return mux
}

func (f *fakeServer) setRedirectOnCurrent(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.redirectOnCurrent = v
}

func (f *fakeServer) sentinelHitCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sentinelHits
}

func testClient() *http.Client {
	return &http.Client{Timeout: 5 * time.Second}
}

func readCorpusFile(t *testing.T, home, rel string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(home, "corpus", rel))
	if err != nil {
		t.Fatalf("read corpus file %q: %v", rel, err)
	}
	return b
}

func corpusLink(t *testing.T, home string) string {
	t.Helper()
	link, err := os.Readlink(filepath.Join(home, "corpus"))
	if err != nil {
		t.Fatalf("readlink corpus: %v", err)
	}
	return link
}

// --- tests ---

func TestFlipCorpusSwitchesRelease(t *testing.T) {
	home := t.TempDir()
	for _, id := range []string{"1", "2"} {
		if err := os.MkdirAll(filepath.Join(home, "releases", id), 0o755); err != nil {
			t.Fatalf("create release %s: %v", id, err)
		}
	}

	if err := flipCorpus(home, "1"); err != nil {
		t.Fatalf("flip to release 1: %v", err)
	}
	if link := corpusLink(t, home); link != filepath.Join("releases", "1") {
		t.Fatalf("corpus link after first flip = %q, want releases/1", link)
	}

	if err := flipCorpus(home, "2"); err != nil {
		t.Fatalf("flip to release 2: %v", err)
	}
	if link := corpusLink(t, home); link != filepath.Join("releases", "2") {
		t.Fatalf("corpus link after second flip = %q, want releases/2", link)
	}
	for _, id := range []string{"1", "2"} {
		if fi, err := os.Stat(filepath.Join(home, "releases", id)); err != nil || !fi.IsDir() {
			t.Fatalf("release %s not retained as a directory: info=%v err=%v", id, fi, err)
		}
	}
}

func TestConvergeHappyPath(t *testing.T) {
	home := t.TempDir()
	docs := []testDoc{{"kernel/kernel.md", []byte("# Kernel\nhello\n")}}
	hash := computeContentHash(docs)

	srv := newFakeServer()
	srv.setCurrent(currentRelease{ReleaseID: 1, ContentHash: hash, Docs: docsToManifest(docs)})
	srv.setArchive(1, buildTar(docs))
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	converge(ts.URL, home, "t1", "", testClient())

	if link := corpusLink(t, home); link != filepath.Join("releases", "1") {
		t.Fatalf("corpus link = %q, want releases/1", link)
	}
	if got := readCorpusFile(t, home, "kernel/kernel.md"); string(got) != string(docs[0].body) {
		t.Fatalf("kernel.md content = %q, want %q", got, docs[0].body)
	}
	applied, err := os.ReadFile(filepath.Join(home, ".applied"))
	if err != nil {
		t.Fatalf("read .applied: %v", err)
	}
	if want := "1 " + hash + "\n"; string(applied) != want {
		t.Fatalf(".applied = %q, want %q", applied, want)
	}
	if fi, err := os.Lstat(filepath.Join(home, "corpus")); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("corpus is not a symlink: %v", err)
	}
	hb := srv.lastHeartbeat()
	if !hb.OK || hb.ReleaseID != 1 || hb.ResyncApplied || hb.Error != nil {
		t.Fatalf("heartbeat = %+v, want ok=true release_id=1 resync_applied=false error=nil", hb)
	}
}

func TestConvergeIdempotentSecondPass(t *testing.T) {
	home := t.TempDir()
	docs := []testDoc{{"kernel/kernel.md", []byte("# Kernel\nhello\n")}}
	hash := computeContentHash(docs)

	srv := newFakeServer()
	srv.setCurrent(currentRelease{ReleaseID: 1, ContentHash: hash, Docs: docsToManifest(docs)})
	srv.setArchive(1, buildTar(docs))
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	converge(ts.URL, home, "t1", "", testClient())
	if hits := srv.archiveHitCount(1); hits != 1 {
		t.Fatalf("archive hits after first pass = %d, want 1", hits)
	}
	appliedBefore, err := os.ReadFile(filepath.Join(home, ".applied"))
	if err != nil {
		t.Fatalf("read .applied: %v", err)
	}

	converge(ts.URL, home, "t1", "", testClient())
	if hits := srv.archiveHitCount(1); hits != 1 {
		t.Fatalf("archive hits after second (idempotent) pass = %d, want 1 (no re-materialize)", hits)
	}
	appliedAfter, err := os.ReadFile(filepath.Join(home, ".applied"))
	if err != nil {
		t.Fatalf("read .applied: %v", err)
	}
	if string(appliedBefore) != string(appliedAfter) {
		t.Fatalf(".applied changed across idempotent pass: %q -> %q", appliedBefore, appliedAfter)
	}
	if hb := srv.lastHeartbeat(); !hb.OK || hb.ResyncApplied {
		t.Fatalf("second-pass heartbeat = %+v, want ok=true resync_applied=false", hb)
	}
}

func TestConvergeNewReleaseFlipsCorpus(t *testing.T) {
	home := t.TempDir()
	docs1 := []testDoc{{"kernel/kernel.md", []byte("# Kernel\nrelease one\n")}}
	hash1 := computeContentHash(docs1)
	docs2 := []testDoc{{"kernel/kernel.md", []byte("# Kernel\nrelease two\n")}}
	hash2 := computeContentHash(docs2)

	srv := newFakeServer()
	srv.setCurrent(currentRelease{ReleaseID: 1, ContentHash: hash1, Docs: docsToManifest(docs1)})
	srv.setArchive(1, buildTar(docs1))
	srv.setArchive(2, buildTar(docs2))
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	converge(ts.URL, home, "t1", "", testClient())
	if link := corpusLink(t, home); link != filepath.Join("releases", "1") {
		t.Fatalf("corpus link after release 1 = %q, want releases/1", link)
	}

	srv.setCurrent(currentRelease{ReleaseID: 2, ContentHash: hash2, Docs: docsToManifest(docs2)})
	converge(ts.URL, home, "t1", "", testClient())

	if link := corpusLink(t, home); link != filepath.Join("releases", "2") {
		t.Fatalf("corpus link after release 2 = %q, want releases/2", link)
	}
	if got := readCorpusFile(t, home, "kernel/kernel.md"); string(got) != string(docs2[0].body) {
		t.Fatalf("active kernel after release 2 = %q, want %q", got, docs2[0].body)
	}
	oldBody, err := os.ReadFile(filepath.Join(home, "releases", "1", "kernel", "kernel.md"))
	if err != nil {
		t.Fatalf("read retained release 1: %v", err)
	}
	if string(oldBody) != string(docs1[0].body) {
		t.Fatalf("retained release 1 changed: got %q want %q", oldBody, docs1[0].body)
	}
	applied, err := os.ReadFile(filepath.Join(home, ".applied"))
	if err != nil {
		t.Fatalf("read .applied: %v", err)
	}
	if want := "2 " + hash2 + "\n"; string(applied) != want {
		t.Fatalf(".applied after release 2 = %q, want %q", applied, want)
	}
	if srv.archiveHitCount(1) != 1 || srv.archiveHitCount(2) != 1 {
		t.Fatalf("archive hits = release1:%d release2:%d, want 1 each",
			srv.archiveHitCount(1), srv.archiveHitCount(2))
	}
	if hb := srv.lastHeartbeat(); !hb.OK || hb.ReleaseID != 2 || hb.ResyncApplied || hb.Error != nil {
		t.Fatalf("release-2 heartbeat = %+v, want ok release_id=2", hb)
	}
}

func TestConvergeRejectsIncompatibleCurrentProtocol(t *testing.T) {
	home := t.TempDir()
	docs1 := []testDoc{{"kernel/kernel.md", []byte("# Kernel\nrelease one\n")}}
	hash1 := computeContentHash(docs1)
	docs2 := []testDoc{{"kernel/kernel.md", []byte("# Kernel\nrelease two\n")}}
	hash2 := computeContentHash(docs2)

	srv := newFakeServer()
	srv.setCurrent(currentRelease{ReleaseID: 1, ContentHash: hash1, Docs: docsToManifest(docs1)})
	srv.setArchive(1, buildTar(docs1))
	srv.setArchive(2, buildTar(docs2))
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	converge(ts.URL, home, "t1", "", testClient())
	wantLink := corpusLink(t, home)
	wantApplied, err := os.ReadFile(filepath.Join(home, ".applied"))
	if err != nil {
		t.Fatal(err)
	}

	srv.setCurrent(currentRelease{ReleaseID: 2, ContentHash: hash2, Docs: docsToManifest(docs2)})
	srv.setProtocolResponse("current", "2")
	converge(ts.URL, home, "t1", "", testClient())

	if link := corpusLink(t, home); link != wantLink {
		t.Fatalf("corpus link after incompatible current protocol = %q, want unchanged %q", link, wantLink)
	}
	if got, err := os.ReadFile(filepath.Join(home, ".applied")); err != nil || !bytes.Equal(got, wantApplied) {
		t.Fatalf(".applied after incompatible current protocol = %q, %v; want unchanged %q", got, err, wantApplied)
	}
	if hits := srv.archiveHitCount(2); hits != 0 {
		t.Fatalf("release 2 archive hits = %d, want 0 when current protocol is incompatible", hits)
	}
	if got := srv.protocolRequestsSeen("current"); len(got) != 2 || got[0] != "1" || got[1] != "1" {
		t.Fatalf("current protocol request headers = %v, want [1 1]", got)
	}
	hb := srv.lastHeartbeat()
	if hb.OK || hb.Error == nil {
		t.Fatalf("heartbeat after incompatible current protocol = %+v, want ok=false and error", hb)
	}
}

func TestConvergeRejectsIncompatibleArchiveProtocol(t *testing.T) {
	home := t.TempDir()
	docs1 := []testDoc{{"kernel/kernel.md", []byte("# Kernel\nrelease one\n")}}
	hash1 := computeContentHash(docs1)
	docs2 := []testDoc{{"kernel/kernel.md", []byte("# Kernel\nrelease two\n")}}
	hash2 := computeContentHash(docs2)

	srv := newFakeServer()
	srv.setCurrent(currentRelease{ReleaseID: 1, ContentHash: hash1, Docs: docsToManifest(docs1)})
	srv.setArchive(1, buildTar(docs1))
	srv.setArchive(2, buildTar(docs2))
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	converge(ts.URL, home, "t1", "", testClient())
	wantLink := corpusLink(t, home)
	srv.setCurrent(currentRelease{ReleaseID: 2, ContentHash: hash2, Docs: docsToManifest(docs2)})
	srv.setProtocolResponse("archive", "2")
	converge(ts.URL, home, "t1", "", testClient())

	if link := corpusLink(t, home); link != wantLink {
		t.Fatalf("corpus link after incompatible archive protocol = %q, want unchanged %q", link, wantLink)
	}
	if hits := srv.archiveHitCount(2); hits != 1 {
		t.Fatalf("release 2 archive hits = %d, want 1 rejected response", hits)
	}
	if got := srv.protocolRequestsSeen("archive"); len(got) != 2 || got[0] != "1" || got[1] != "1" {
		t.Fatalf("archive protocol request headers = %v, want [1 1]", got)
	}
	hb := srv.lastHeartbeat()
	if hb.OK || hb.Error == nil {
		t.Fatalf("heartbeat after incompatible archive protocol = %+v, want ok=false and error", hb)
	}
}

func TestHeartbeatReportsIncompatibleProtocolResponse(t *testing.T) {
	var requestVersions []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestVersions = append(requestVersions, r.Header.Values("Agent-Knowledge-Protocol-Version")...)
		w.Header().Set("Agent-Knowledge-Protocol-Version", "2")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer ts.Close()

	var logs bytes.Buffer
	previousWriter := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(previousWriter)

	heartbeat(testClient(), ts.URL, "t1", "", 1, true, "", false)
	if len(requestVersions) != 1 || requestVersions[0] != "1" {
		t.Fatalf("heartbeat protocol request headers = %v, want [1]", requestVersions)
	}
	if !strings.Contains(logs.String(), "incompatible protocol") {
		t.Fatalf("heartbeat log = %q, want incompatible protocol error", logs.String())
	}
}

func TestConvergeServerDown(t *testing.T) {
	home := t.TempDir()
	docs := []testDoc{{"kernel/kernel.md", []byte("# Kernel\nhello\n")}}
	hash := computeContentHash(docs)

	srv := newFakeServer()
	srv.setCurrent(currentRelease{ReleaseID: 1, ContentHash: hash, Docs: docsToManifest(docs)})
	srv.setArchive(1, buildTar(docs))
	ts := httptest.NewServer(srv.handler())

	converge(ts.URL, home, "t1", "", testClient())
	wantLink := corpusLink(t, home)
	wantBody := readCorpusFile(t, home, "kernel/kernel.md")

	ts.Close() // server now refuses connections

	converge(ts.URL, home, "t1", "", testClient()) // must not panic, must exit cleanly

	if link := corpusLink(t, home); link != wantLink {
		t.Fatalf("corpus link after server-down pass = %q, want unchanged %q", link, wantLink)
	}
	if got := readCorpusFile(t, home, "kernel/kernel.md"); string(got) != string(wantBody) {
		t.Fatalf("kernel.md changed after server-down pass: %q, want %q", got, wantBody)
	}
}

func TestConvergeServerNeverReachableFirstPass(t *testing.T) {
	home := t.TempDir()
	ts := httptest.NewServer(newFakeServer().handler())
	ts.Close() // never reachable

	converge(ts.URL, home, "t1", "", testClient()) // must not panic

	if _, err := os.Lstat(filepath.Join(home, "corpus")); !os.IsNotExist(err) {
		t.Fatalf("corpus should not exist when the server was never reachable, lstat err = %v", err)
	}
}

func TestConvergeHashMismatch(t *testing.T) {
	home := t.TempDir()
	docs1 := []testDoc{{"kernel/kernel.md", []byte("# Kernel\nv1\n")}}
	hash1 := computeContentHash(docs1)

	srv := newFakeServer()
	srv.setCurrent(currentRelease{ReleaseID: 1, ContentHash: hash1, Docs: docsToManifest(docs1)})
	srv.setArchive(1, buildTar(docs1))
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	converge(ts.URL, home, "t1", "", testClient())

	// Release 2: archive content does not match the advertised hash.
	docs2 := []testDoc{{"kernel/kernel.md", []byte("# Kernel\nv2\n")}}
	srv.setCurrent(currentRelease{ReleaseID: 2, ContentHash: "sha256:" + hex.EncodeToString(make([]byte, 32)), Docs: docsToManifest(docs2)})
	srv.setArchive(2, buildTar(docs2))

	converge(ts.URL, home, "t1", "", testClient())

	if link := corpusLink(t, home); link != filepath.Join("releases", "1") {
		t.Fatalf("corpus link after mismatch = %q, want unchanged releases/1", link)
	}
	if got := readCorpusFile(t, home, "kernel/kernel.md"); string(got) != string(docs1[0].body) {
		t.Fatalf("kernel.md changed after mismatch: %q, want %q", got, docs1[0].body)
	}
	applied, err := os.ReadFile(filepath.Join(home, ".applied"))
	if err != nil {
		t.Fatalf("read .applied: %v", err)
	}
	if want := "1 " + hash1 + "\n"; string(applied) != want {
		t.Fatalf(".applied changed after mismatch: %q, want %q", applied, want)
	}
	if _, err := os.Stat(filepath.Join(home, "releases", "2")); !os.IsNotExist(err) {
		t.Fatalf("releases/2 should not exist after a hash mismatch, stat err = %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(home, "releases"))
	if err != nil {
		t.Fatalf("read releases dir: %v", err)
	}
	for _, e := range entries {
		if e.Name() != "1" {
			t.Fatalf("unexpected leftover in releases dir: %q (temp dir not discarded?)", e.Name())
		}
	}
	if hb := srv.lastHeartbeat(); hb.OK || hb.ReleaseID != 2 || hb.Error == nil {
		t.Fatalf("heartbeat after mismatch = %+v, want ok=false release_id=2 error!=nil", hb)
	}
}

func TestConvergeTruncatedArchive(t *testing.T) {
	home := t.TempDir()
	docs1 := []testDoc{{"kernel/kernel.md", []byte("# Kernel\nv1\n")}}
	hash1 := computeContentHash(docs1)

	srv := newFakeServer()
	srv.setCurrent(currentRelease{ReleaseID: 1, ContentHash: hash1, Docs: docsToManifest(docs1)})
	srv.setArchive(1, buildTar(docs1))
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	converge(ts.URL, home, "t1", "", testClient())

	// The body must be large enough that cutting the archive well past
	// the 512-byte tar header still lands inside the file's data
	// blocks (not just past the harmless trailing zero end-of-archive
	// markers, which archive/tar tolerates being absent).
	docs2 := []testDoc{{"kernel/kernel.md", bytes.Repeat([]byte("v2 truncation filler\n"), 100)}}
	hash2 := computeContentHash(docs2)
	full := buildTar(docs2)
	truncated := full[:600] // past the header, mid-body
	srv.setCurrent(currentRelease{ReleaseID: 2, ContentHash: hash2, Docs: docsToManifest(docs2)})
	srv.setArchive(2, truncated)

	converge(ts.URL, home, "t1", "", testClient())

	if link := corpusLink(t, home); link != filepath.Join("releases", "1") {
		t.Fatalf("corpus link after truncated archive = %q, want unchanged releases/1", link)
	}
	if got := readCorpusFile(t, home, "kernel/kernel.md"); string(got) != string(docs1[0].body) {
		t.Fatalf("kernel.md changed after truncated archive: %q, want %q", got, docs1[0].body)
	}
	if _, err := os.Stat(filepath.Join(home, "releases", "2")); !os.IsNotExist(err) {
		t.Fatalf("releases/2 should not exist after a truncated archive, stat err = %v", err)
	}
	if hb := srv.lastHeartbeat(); hb.OK || hb.Error == nil {
		t.Fatalf("heartbeat after truncated archive = %+v, want ok=false error!=nil", hb)
	}
}

func TestConvergeTraversalDefense(t *testing.T) {
	home := t.TempDir()
	docs1 := []testDoc{{"kernel/kernel.md", []byte("# Kernel\nv1\n")}}
	hash1 := computeContentHash(docs1)

	srv := newFakeServer()
	srv.setCurrent(currentRelease{ReleaseID: 1, ContentHash: hash1, Docs: docsToManifest(docs1)})
	srv.setArchive(1, buildTar(docs1))
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	converge(ts.URL, home, "t1", "", testClient())

	evil := []testDoc{{"../escape", []byte("pwned")}}
	evilHash := computeContentHash(evil)
	srv.setCurrent(currentRelease{ReleaseID: 2, ContentHash: evilHash, Docs: docsToManifest(evil)})
	srv.setArchive(2, buildTar(evil))

	converge(ts.URL, home, "t1", "", testClient())

	if link := corpusLink(t, home); link != filepath.Join("releases", "1") {
		t.Fatalf("corpus link after traversal attempt = %q, want unchanged releases/1", link)
	}
	if got := readCorpusFile(t, home, "kernel/kernel.md"); string(got) != string(docs1[0].body) {
		t.Fatalf("kernel.md changed after traversal attempt: %q, want %q", got, docs1[0].body)
	}
	if _, err := os.Lstat(filepath.Join(home, "escape")); !os.IsNotExist(err) {
		t.Fatalf("traversal entry escaped: %s/escape should not exist, lstat err = %v", home, err)
	}
	if hb := srv.lastHeartbeat(); hb.OK || hb.Error == nil {
		t.Fatalf("heartbeat after traversal attempt = %+v, want ok=false error!=nil", hb)
	}
}

func TestConvergeResyncBeliefErasure(t *testing.T) {
	home := t.TempDir()
	docs := []testDoc{{"kernel/kernel.md", []byte("# Kernel\nhello\n")}}
	hash := computeContentHash(docs)

	srv := newFakeServer()
	srv.setCurrent(currentRelease{ReleaseID: 1, ContentHash: hash, Docs: docsToManifest(docs)})
	srv.setArchive(1, buildTar(docs))
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	converge(ts.URL, home, "t1", "", testClient())
	if hits := srv.archiveHitCount(1); hits != 1 {
		t.Fatalf("archive hits after first pass = %d, want 1", hits)
	}

	// Same release, but the server now reports a pending force-resync.
	srv.setCurrent(currentRelease{ReleaseID: 1, ContentHash: hash, Docs: docsToManifest(docs), Resync: true})

	converge(ts.URL, home, "t1", "", testClient())

	if hits := srv.archiveHitCount(1); hits != 2 {
		t.Fatalf("archive hits after resync pass = %d, want 2 (must re-materialize)", hits)
	}
	// F1: a same-id resync must land in a FRESH dir (releases/1.1), not
	// reuse the pre-existing releases/1 — see TestConvergeResyncAfterTamper
	// for why (a locally-tampered releases/1 must not be trusted).
	if link := corpusLink(t, home); link != filepath.Join("releases", "1.1") {
		t.Fatalf("corpus link after resync = %q, want releases/1.1 (fresh dir, not reused)", link)
	}
	if got := readCorpusFile(t, home, "kernel/kernel.md"); string(got) != string(docs[0].body) {
		t.Fatalf("kernel.md content after resync = %q, want %q", got, docs[0].body)
	}
	hb := srv.lastHeartbeat()
	if !hb.OK || !hb.ResyncApplied {
		t.Fatalf("heartbeat after resync = %+v, want ok=true resync_applied=true", hb)
	}
}

// TestConvergeResyncAfterTamper is the F1 regression: the materialized corpus
// is agent-writable (the kit's threat model), so a
// same-release-id force-resync must NOT reuse whatever is already on
// disk at releases/<id> — it could have been tampered with locally.
// The subscriber must always flip corpus onto the freshly downloaded,
// hash-verified tree, never a pre-existing dir, and must only claim
// resync_applied:true after a real re-materialize.
func TestConvergeResyncAfterTamper(t *testing.T) {
	home := t.TempDir()
	docs := []testDoc{{"kernel/kernel.md", []byte("# Kernel\ncorrect server content\n")}}
	hash := computeContentHash(docs)

	srv := newFakeServer()
	srv.setCurrent(currentRelease{ReleaseID: 1, ContentHash: hash, Docs: docsToManifest(docs)})
	srv.setArchive(1, buildTar(docs))
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	converge(ts.URL, home, "t1", "", testClient())
	if got := readCorpusFile(t, home, "kernel/kernel.md"); string(got) != string(docs[0].body) {
		t.Fatalf("kernel.md after first pass = %q, want %q", got, docs[0].body)
	}

	// Simulate local tampering of the materialized release dir (the
	// materialized corpus is agent-writable per the kit's threat model).
	kernelOnDisk := filepath.Join(home, "releases", "1", "kernel", "kernel.md")
	if err := os.WriteFile(kernelOnDisk, []byte("TAMPERED"), 0o644); err != nil {
		t.Fatalf("simulate tamper: %v", err)
	}
	if got := readCorpusFile(t, home, "kernel/kernel.md"); string(got) != "TAMPERED" {
		t.Fatalf("tamper setup failed, corpus reads = %q", got)
	}

	// Server offers the SAME release id and content hash, but flags a
	// pending force-resync (e.g. an operator suspected drift).
	srv.setCurrent(currentRelease{ReleaseID: 1, ContentHash: hash, Docs: docsToManifest(docs), Resync: true})

	converge(ts.URL, home, "t1", "", testClient())

	if hits := srv.archiveHitCount(1); hits != 2 {
		t.Fatalf("archive hits after tamper+resync = %d, want 2 (must actually re-fetch, not fake success)", hits)
	}
	if got := readCorpusFile(t, home, "kernel/kernel.md"); string(got) != string(docs[0].body) {
		t.Fatalf("corpus after tamper+resync = %q, want the correct server content %q, not the tampered copy",
			got, docs[0].body)
	}
	if link := corpusLink(t, home); link == filepath.Join("releases", "1") {
		t.Fatalf("corpus link after tamper+resync still points at the possibly-tampered releases/1 dir")
	}
	hb := srv.lastHeartbeat()
	if !hb.OK || !hb.ResyncApplied {
		t.Fatalf("heartbeat after tamper+resync = %+v, want ok=true resync_applied=true (a REAL re-materialize happened)", hb)
	}
	applied, err := os.ReadFile(filepath.Join(home, ".applied"))
	if err != nil {
		t.Fatalf("read .applied: %v", err)
	}
	if want := "1 " + hash + "\n"; string(applied) != want {
		t.Fatalf(".applied after tamper+resync = %q, want %q", applied, want)
	}
}

// TestConvergeUnmanifestedTarEntryRejected is the F3 regression: an
// archive entry that is not listed in the manifest must never be
// materialized, even if every manifest doc it also carries is correct.
func TestConvergeUnmanifestedTarEntryRejected(t *testing.T) {
	home := t.TempDir()
	docs1 := []testDoc{{"kernel/kernel.md", []byte("# Kernel\nv1\n")}}
	hash1 := computeContentHash(docs1)

	srv := newFakeServer()
	srv.setCurrent(currentRelease{ReleaseID: 1, ContentHash: hash1, Docs: docsToManifest(docs1)})
	srv.setArchive(1, buildTar(docs1))
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	converge(ts.URL, home, "t1", "", testClient())

	// Release 2's archive smuggles in an extra file not present in the
	// manifest; content_hash is computed over the manifest set only
	// (matching what a real store.CutRelease would produce), so this
	// can only be caught by the materialized-set-equals-manifest-set
	// check, not by the hash.
	manifestDocs2 := []testDoc{{"kernel/kernel.md", []byte("# Kernel\nv2\n")}}
	hash2 := computeContentHash(manifestDocs2)
	archiveDocs2 := []testDoc{
		{"kernel/kernel.md", []byte("# Kernel\nv2\n")},
		{"kernel/EXTRA.md", []byte("smuggled\n")},
	}
	srv.setCurrent(currentRelease{ReleaseID: 2, ContentHash: hash2, Docs: docsToManifest(manifestDocs2)})
	srv.setArchive(2, buildTar(archiveDocs2))

	converge(ts.URL, home, "t1", "", testClient())

	if link := corpusLink(t, home); link != filepath.Join("releases", "1") {
		t.Fatalf("corpus link after unmanifested entry = %q, want unchanged releases/1", link)
	}
	if _, err := os.Stat(filepath.Join(home, "releases", "2", "kernel", "EXTRA.md")); !os.IsNotExist(err) {
		t.Fatalf("unmanifested file should not have been left on disk, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, "releases", "2")); !os.IsNotExist(err) {
		t.Fatalf("releases/2 should not exist after an unmanifested entry, stat err = %v", err)
	}
	if hb := srv.lastHeartbeat(); hb.OK || hb.Error == nil {
		t.Fatalf("heartbeat after unmanifested entry = %+v, want ok=false error!=nil", hb)
	}
}

// TestConvergeTokened is the subscriber-side slice of step 3: with a
// bearer token, the subscriber must send Authorization on every
// request and must NOT send ?host= (the server resolves the host
// from the token, so a hostname in the URL would either mismatch or
// leak the wrong identity).
func TestConvergeTokened(t *testing.T) {
	home := t.TempDir()
	docs := []testDoc{{"kernel/kernel.md", []byte("# Kernel\nhello\n")}}
	hash := computeContentHash(docs)

	srv := newFakeServer()
	srv.setRequireToken("host-secret-token")
	srv.setCurrent(currentRelease{ReleaseID: 1, ContentHash: hash, Docs: docsToManifest(docs)})
	srv.setArchive(1, buildTar(docs))
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	// wireHost = "" because a tokened subscriber hands identity off to
	// the server (see subscriber.main).
	converge(ts.URL, home, "", "host-secret-token", testClient())

	if link := corpusLink(t, home); link != filepath.Join("releases", "1") {
		t.Fatalf("corpus link = %q, want releases/1", link)
	}
	if got := readCorpusFile(t, home, "kernel/kernel.md"); string(got) != string(docs[0].body) {
		t.Fatalf("kernel.md content = %q, want %q", got, docs[0].body)
	}
	hb := srv.lastHeartbeat()
	if !hb.OK || hb.Host != "" || hb.ReleaseID != 1 || hb.ResyncApplied || hb.Error != nil {
		t.Fatalf("heartbeat = %+v, want ok=true host=\"\" release_id=1 resync_applied=false error=nil", hb)
	}
	hits := srv.authorizedHitsByEndpoint()
	if hits["current"] != 1 || hits["archive"] != 1 || hits["heartbeat"] != 1 {
		t.Fatalf("authorized hits = %+v, want current=1 archive=1 heartbeat=1", hits)
	}
	for _, hq := range srv.hostQueriesSeen() {
		if hq != "" {
			t.Fatalf("tokened subscriber must not send ?host=, saw %q", hq)
		}
	}
}

// TestConverge401FailSoft pins down the failure mode: every endpoint
// returning 401 is the same shape as "server unreachable" from the
// subscriber's view (an HTTP error before any payload). The previous
// corpus must be left exactly as-is, the subscriber must not panic,
// and on a never-authorized first pass no corpus should ever be
// created. Mirrors the TestConvergeServerDown /
// TestConvergeServerNeverReachableFirstPass assertions.
func TestConverge401FailSoft(t *testing.T) {
	t.Run("second pass 401 leaves corpus unchanged", func(t *testing.T) {
		home := t.TempDir()
		docs := []testDoc{{"kernel/kernel.md", []byte("# Kernel\nhello\n")}}
		hash := computeContentHash(docs)

		srv := newFakeServer()
		srv.setCurrent(currentRelease{ReleaseID: 1, ContentHash: hash, Docs: docsToManifest(docs)})
		srv.setArchive(1, buildTar(docs))
		ts := httptest.NewServer(srv.handler())
		defer ts.Close()

		// Tokenless first pass — succeeds.
		converge(ts.URL, home, "t1", "", testClient())
		wantLink := corpusLink(t, home)
		wantBody := readCorpusFile(t, home, "kernel/kernel.md")
		appliedBefore, err := os.ReadFile(filepath.Join(home, ".applied"))
		if err != nil {
			t.Fatalf("read .applied: %v", err)
		}

		// Server now requires a token the client never sends.
		srv.setRequireToken("required-but-not-sent")

		converge(ts.URL, home, "t1", "", testClient()) // must not panic

		if link := corpusLink(t, home); link != wantLink {
			t.Fatalf("corpus link after 401 pass = %q, want unchanged %q", link, wantLink)
		}
		if got := readCorpusFile(t, home, "kernel/kernel.md"); string(got) != string(wantBody) {
			t.Fatalf("kernel.md changed after 401 pass: %q, want %q", got, wantBody)
		}
		appliedAfter, err := os.ReadFile(filepath.Join(home, ".applied"))
		if err != nil {
			t.Fatalf("read .applied: %v", err)
		}
		if string(appliedBefore) != string(appliedAfter) {
			t.Fatalf(".applied changed after 401 pass: %q -> %q", appliedBefore, appliedAfter)
		}
	})

	t.Run("never-authorized first pass leaves no corpus", func(t *testing.T) {
		home := t.TempDir()
		srv := newFakeServer()
		srv.setRequireToken("required-but-not-sent")
		ts := httptest.NewServer(srv.handler())
		defer ts.Close()

		converge(ts.URL, home, "t1", "", testClient()) // must not panic

		if _, err := os.Lstat(filepath.Join(home, "corpus")); !os.IsNotExist(err) {
			t.Fatalf("corpus must not exist when every pass was unauthorized, lstat err = %v", err)
		}
		if _, err := os.Stat(filepath.Join(home, ".applied")); !os.IsNotExist(err) {
			t.Fatalf(".applied must not exist when every pass was unauthorized, stat err = %v", err)
		}
	})
}

// TestConvergeTLSPinning exercises the TLS-CA path: the subscriber
// must verify the server's certificate against the operator-supplied
// CA bundle (a self-signed cert from httptest.NewTLSServer fails
// against the system roots), and a junk-PEM CA file must surface as a
// hard error at client construction time.
func TestConvergeTLSPinning(t *testing.T) {
	t.Run("pinned CA works over https", func(t *testing.T) {
		home := t.TempDir()
		docs := []testDoc{{"kernel/kernel.md", []byte("# Kernel\nhello\n")}}
		hash := computeContentHash(docs)

		srv := newFakeServer()
		srv.setCurrent(currentRelease{ReleaseID: 1, ContentHash: hash, Docs: docsToManifest(docs)})
		srv.setArchive(1, buildTar(docs))
		ts := httptest.NewTLSServer(srv.handler())
		defer ts.Close()

		caFile := filepath.Join(t.TempDir(), "ca.pem")
		if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{
			Type:  "CERTIFICATE",
			Bytes: ts.Certificate().Raw,
		}), 0o600); err != nil {
			t.Fatalf("write ca file: %v", err)
		}
		hc, err := newHTTPClient(caFile)
		if err != nil {
			t.Fatalf("newHTTPClient with valid CA: %v", err)
		}

		converge(ts.URL, home, "t1", "", hc)

		if link := corpusLink(t, home); link != filepath.Join("releases", "1") {
			t.Fatalf("corpus link = %q, want releases/1", link)
		}
		if got := readCorpusFile(t, home, "kernel/kernel.md"); string(got) != string(docs[0].body) {
			t.Fatalf("kernel.md content = %q, want %q", got, docs[0].body)
		}
		// Sanity: the client actually went through TLS verification.
		if hc.Transport == nil {
			t.Fatalf("pinned client must have a non-nil Transport")
		}
	})

	t.Run("junk PEM CA file is rejected", func(t *testing.T) {
		junk := filepath.Join(t.TempDir(), "junk.pem")
		if err := os.WriteFile(junk, []byte("this is not a certificate"), 0o600); err != nil {
			t.Fatalf("write junk: %v", err)
		}
		if _, err := newHTTPClient(junk); err == nil {
			t.Fatalf("newHTTPClient with junk-PEM must error")
		}
	})

	t.Run("no CA file disables pinning (loopback posture)", func(t *testing.T) {
		hc, err := newHTTPClient("")
		if err != nil {
			t.Fatalf("newHTTPClient(\"\"): %v", err)
		}
		if hc.Transport != nil {
			t.Fatalf("client without -ca-file must not pin TLS roots")
		}
	})
}

// TestValidateServerURL (FIX 1, F1) — bearer-on-plaintext is a
// credential leak, so validateServerURL accepts http only to a loopback
// host (and only when a token is set; without a token every URL is OK).
func TestValidateServerURL(t *testing.T) {
	cases := []struct {
		name    string
		url     string
		token   string
		wantErr bool
	}{
		{"https anything + token", "https://ks.example:8471", "tok", false},
		{"http 127.0.0.1 + token", "http://127.0.0.1:8471", "tok", false},
		{"http localhost + token", "http://localhost:8471", "tok", false},
		{"http ipv6 loopback + token", "http://[::1]:8471", "tok", false},
		{"http TEST-NET + token", "http://198.51.100.7:8471", "tok", true},
		{"http TEST-NET no token", "http://198.51.100.7:8471", "", false},
		{"http non-loopback hostname + token", "http://ks.example:8471", "tok", true},
		{"https anything no token", "https://ks.example:8471", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateServerURL(tc.url, tc.token)
			if tc.wantErr && err == nil {
				t.Fatalf("want error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

// TestNewHTTPClientRejectsRedirect (FIX 1, F1) — the knowledge-server
// API never redirects, and Go's default redirect policy forwards the
// Authorization header even across an HTTPS→HTTP downgrade. newHTTPClient
// must refuse every redirect, regardless of token presence. On a
// CheckRedirect error http.Client returns both the last response (with
// Body already closed) and the error.
func TestNewHTTPClientRejectsRedirect(t *testing.T) {
	hc, err := newHTTPClient("")
	if err != nil {
		t.Fatalf("newHTTPClient: %v", err)
	}
	// Drive the redirect through a minimal local server so we exercise
	// the real http.Client code path, including CheckRedirect.
	var targetHits int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&targetHits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer origin.Close()

	req, _ := http.NewRequest(http.MethodGet, origin.URL, nil)
	req.Header.Set("Authorization", "Bearer test-token")
	resp, err := hc.Do(req)
	if err == nil {
		resp.Body.Close()
		t.Fatalf("want redirect-refused error, got nil")
	}
	if atomic.LoadInt32(&targetHits) != 0 {
		t.Fatalf("redirect target hits = %d, want 0 (Authorization must not be replayed across a refused redirect)", targetHits)
	}
	_ = resp // http.Client may return the last response alongside the error; we only care that the target was not requested
}

// TestConvergeRejectsRedirect (FIX 1, F1) — when /current returns 302,
// the subscriber's http.Client must refuse the redirect before the
// Location target is ever requested. Converge fails soft (no panic,
// corpus untouched); the sentinel handler on the same fake server
// records zero hits. Uses newHTTPClient("") so the production
// CheckRedirect is on the wire.
func TestConvergeRejectsRedirect(t *testing.T) {
	home := t.TempDir()
	docs := []testDoc{{"kernel/kernel.md", []byte("# Kernel\nhello\n")}}
	hash := computeContentHash(docs)

	srv := newFakeServer()
	srv.setRequireToken("host-secret-token")
	srv.setCurrent(currentRelease{ReleaseID: 1, ContentHash: hash, Docs: docsToManifest(docs)})
	srv.setArchive(1, buildTar(docs))
	srv.setRedirectOnCurrent(true)
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	hc, err := newHTTPClient("")
	if err != nil {
		t.Fatalf("newHTTPClient: %v", err)
	}
	converge(ts.URL, home, "", "host-secret-token", hc) // must not panic

	if hits := srv.sentinelHitCount(); hits != 0 {
		t.Fatalf("sentinel hits = %d, want 0 (redirect must be refused client-side, not followed)", hits)
	}
	if _, err := os.Lstat(filepath.Join(home, "corpus")); !os.IsNotExist(err) {
		t.Fatalf("corpus must not exist when the first pass was a refused redirect, lstat err = %v", err)
	}
}
