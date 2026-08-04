package main

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
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
	mu          sync.Mutex
	current     currentRelease
	archives    map[int64][]byte
	archiveHits map[int64]int
	heartbeats  []heartbeatCapture
}

func newFakeServer() *fakeServer {
	return &fakeServer{archives: map[int64][]byte{}, archiveHits: map[int64]int{}}
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
		f.mu.Lock()
		cur := f.current
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(cur)
	})
	mux.HandleFunc("GET /api/releases/{id}/archive", func(w http.ResponseWriter, r *http.Request) {
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
		var hb heartbeatCapture
		json.NewDecoder(r.Body).Decode(&hb)
		f.mu.Lock()
		f.heartbeats = append(f.heartbeats, hb)
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
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

func TestConvergeHappyPath(t *testing.T) {
	home := t.TempDir()
	docs := []testDoc{{"kernel/kernel.md", []byte("# Kernel\nhello\n")}}
	hash := computeContentHash(docs)

	srv := newFakeServer()
	srv.setCurrent(currentRelease{ReleaseID: 1, ContentHash: hash, Docs: docsToManifest(docs)})
	srv.setArchive(1, buildTar(docs))
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	converge(ts.URL, home, "t1", testClient())

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

	converge(ts.URL, home, "t1", testClient())
	if hits := srv.archiveHitCount(1); hits != 1 {
		t.Fatalf("archive hits after first pass = %d, want 1", hits)
	}
	appliedBefore, err := os.ReadFile(filepath.Join(home, ".applied"))
	if err != nil {
		t.Fatalf("read .applied: %v", err)
	}

	converge(ts.URL, home, "t1", testClient())
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

func TestConvergeServerDown(t *testing.T) {
	home := t.TempDir()
	docs := []testDoc{{"kernel/kernel.md", []byte("# Kernel\nhello\n")}}
	hash := computeContentHash(docs)

	srv := newFakeServer()
	srv.setCurrent(currentRelease{ReleaseID: 1, ContentHash: hash, Docs: docsToManifest(docs)})
	srv.setArchive(1, buildTar(docs))
	ts := httptest.NewServer(srv.handler())

	converge(ts.URL, home, "t1", testClient())
	wantLink := corpusLink(t, home)
	wantBody := readCorpusFile(t, home, "kernel/kernel.md")

	ts.Close() // server now refuses connections

	converge(ts.URL, home, "t1", testClient()) // must not panic, must exit cleanly

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

	converge(ts.URL, home, "t1", testClient()) // must not panic

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

	converge(ts.URL, home, "t1", testClient())

	// Release 2: archive content does not match the advertised hash.
	docs2 := []testDoc{{"kernel/kernel.md", []byte("# Kernel\nv2\n")}}
	srv.setCurrent(currentRelease{ReleaseID: 2, ContentHash: "sha256:" + hex.EncodeToString(make([]byte, 32)), Docs: docsToManifest(docs2)})
	srv.setArchive(2, buildTar(docs2))

	converge(ts.URL, home, "t1", testClient())

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

	converge(ts.URL, home, "t1", testClient())

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

	converge(ts.URL, home, "t1", testClient())

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

	converge(ts.URL, home, "t1", testClient())

	evil := []testDoc{{"../escape", []byte("pwned")}}
	evilHash := computeContentHash(evil)
	srv.setCurrent(currentRelease{ReleaseID: 2, ContentHash: evilHash, Docs: docsToManifest(evil)})
	srv.setArchive(2, buildTar(evil))

	converge(ts.URL, home, "t1", testClient())

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

	converge(ts.URL, home, "t1", testClient())
	if hits := srv.archiveHitCount(1); hits != 1 {
		t.Fatalf("archive hits after first pass = %d, want 1", hits)
	}

	// Same release, but the server now reports a pending force-resync.
	srv.setCurrent(currentRelease{ReleaseID: 1, ContentHash: hash, Docs: docsToManifest(docs), Resync: true})

	converge(ts.URL, home, "t1", testClient())

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

// TestConvergeResyncAfterTamper is the F1 regression: the corpus
// checkout is agent-writable (the kit's threat model), so a
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

	converge(ts.URL, home, "t1", testClient())
	if got := readCorpusFile(t, home, "kernel/kernel.md"); string(got) != string(docs[0].body) {
		t.Fatalf("kernel.md after first pass = %q, want %q", got, docs[0].body)
	}

	// Simulate local tampering of the materialized release dir (the
	// corpus checkout is agent-writable per the kit's threat model).
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

	converge(ts.URL, home, "t1", testClient())

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

	converge(ts.URL, home, "t1", testClient())

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

	converge(ts.URL, home, "t1", testClient())

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
