// Command subscriber is the thin consumer-host materializer: it polls a
// knowledge-server for the current release, verifies it, and atomically
// flips the local `corpus` symlink onto a freshly materialized release
// directory. It talks to the server over plain HTTP with its own minimal
// JSON decode types — it does NOT import the store package, so the wire
// contract (docs/plans/knowledge-server.md, "Component boundaries and
// contracts" and "Content-hash construction") is reproduced here by hand
// and must be kept in sync with store.CutRelease if that ever changes.
//
// Hard boundaries (see docs/plans/knowledge-server.md, "Subscriber
// contract"): no intelligence, no reconciliation, no merge logic. Its
// only local state is the `.applied` marker. On any failure the previous
// `corpus` is left exactly as-is and the process exits 0 — an
// unreachable or misbehaving server must never break a session.
package main

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// currentRelease is the subscriber's own decode shape for
// `GET /api/releases/current`. It intentionally mirrors only the fields
// the subscriber needs, not store.Manifest.
type currentRelease struct {
	ReleaseID   int64         `json:"release_id"`
	ContentHash string        `json:"content_hash"`
	Docs        []manifestDoc `json:"docs"`
	Resync      bool          `json:"resync"`
}

type manifestDoc struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// heartbeatBody is the subscriber's own encode shape for
// `POST /api/heartbeats`.
type heartbeatBody struct {
	Host          string  `json:"host"`
	ReleaseID     int64   `json:"release_id"`
	OK            bool    `json:"ok"`
	Error         *string `json:"error"`
	ResyncApplied bool    `json:"resync_applied"`
}

func main() {
	server := flag.String("server", "", "knowledge-server base URL, e.g. https://ks.example:8471 (required)")
	home := flag.String("home", defaultHome(), "KNOWLEDGE_HOME directory")
	host := flag.String("host", defaultHost(), "host id reported to the server; ignored when -token-file is set (the token's bound identity is used)")
	tokenFile := flag.String("token-file", "", "path to this host's bearer token; sent as Authorization: Bearer on every request. Assumes the server has auth enabled; against an auth-disabled server, corpus materialization still works but heartbeats and force-resync are inert.")
	caFile := flag.String("ca-file", "", "PEM CA bundle used to verify the server's TLS certificate (for private/self-signed CAs)")
	interval := flag.Duration("interval", 60*time.Second, "poll interval between converge passes")
	once := flag.Bool("once", false, "run a single converge pass and exit (for cron and tests)")
	flag.Parse()

	if *server == "" {
		log.Fatal("-server is required")
	}
	// Flag misconfiguration fails hard (like a missing -server): it is
	// an operator error at setup time, not a runtime outage the
	// fail-soft contract covers.
	token := ""
	if *tokenFile != "" {
		b, err := os.ReadFile(*tokenFile)
		if err != nil {
			log.Fatalf("read -token-file: %v", err)
		}
		if token = strings.TrimSpace(string(b)); token == "" {
			log.Fatalf("-token-file %s is empty", *tokenFile)
		}
	}
	if err := validateServerURL(*server, token); err != nil {
		log.Fatal(err)
	}
	hc, err := newHTTPClient(*caFile)
	if err != nil {
		log.Fatal(err)
	}

	// With a token the server resolves this host's identity from it
	// (the name bound at issuance), so no hostname travels on the wire
	// and issuance-vs-hostname drift cannot cause a mismatch.
	wireHost := *host
	if token != "" {
		wireHost = ""
	}

	base := strings.TrimRight(*server, "/")
	converge(base, *home, wireHost, token, hc)
	if *once {
		return
	}
	for {
		time.Sleep(*interval)
		converge(base, *home, wireHost, token, hc)
	}
}

// newHTTPClient builds the API client. With caFile it pins TLS server
// verification to that PEM bundle instead of the system roots.
// CheckRedirect is always set to refuse redirects: the knowledge-server
// API never redirects, and Go's default policy forwards Authorization
// on same-host redirects even across an HTTPS→HTTP downgrade, which
// would leak the bearer token in plaintext to the redirect target.
func newHTTPClient(caFile string) (*http.Client, error) {
	hc := &http.Client{Timeout: 30 * time.Second}
	hc.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		return fmt.Errorf("refusing redirect to %s", req.URL)
	}
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("read -ca-file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("-ca-file %s: no certificates found", caFile)
		}
		hc.Transport = &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}
	}
	return hc, nil
}

// validateServerURL rejects configurations that would send the bearer
// token in plaintext to a non-loopback host. With token == "" there is
// no credential to leak and every URL is accepted. With token != "",
// https is always allowed and http is allowed only to a loopback host
// (localhost, 127.0.0.0/8, ::1). Called from main() before any
// network I/O; failure is an operator-config error and fails hard.
func validateServerURL(server, token string) error {
	if token == "" {
		return nil
	}
	u, err := url.Parse(server)
	if err != nil {
		return fmt.Errorf("invalid -server URL: %w", err)
	}
	if u.Scheme == "https" {
		return nil
	}
	if u.Scheme == "http" && isLoopbackHost(u.Hostname()) {
		return nil
	}
	return fmt.Errorf("refusing to send bearer token over %s to non-loopback host %q (use https, or http to a loopback address)", u.Scheme, u.Hostname())
}

// isLoopbackHost mirrors the server's check (main.go) so the client
// uses the same definition of "loopback": the literal "localhost"
// string, IPv4 127.0.0.0/8, and IPv6 ::1.
func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

func defaultHome() string {
	if h := os.Getenv("KNOWLEDGE_HOME"); h != "" {
		return h
	}
	return filepath.Join(os.Getenv("HOME"), ".config", "agent-knowledge")
}

func defaultHost() string {
	h, err := os.Hostname()
	if err != nil {
		return ""
	}
	return h
}

// converge runs one poll-fetch-verify-flip-heartbeat pass. It never
// returns an error: every failure is logged and reported (best-effort)
// via a heartbeat, and the previous corpus is left untouched. This is
// the fail-soft contract — an unreachable or misbehaving server must
// never break the caller.
func converge(server, home, host, token string, hc *http.Client) {
	cur, err := fetchCurrent(hc, server, host, token)
	if err != nil {
		log.Printf("subscriber: fetch current release: %v", err)
		heartbeat(hc, server, host, token, 0, false, err.Error(), false)
		return
	}

	appliedPath := filepath.Join(home, ".applied")

	// Force-resync is belief-erasure only: drop the local applied
	// marker so the "applied != current" check below always fails and
	// the normal re-materialize path does the rest. No other logic.
	resyncFired := false
	if cur.Resync {
		if err := os.Remove(appliedPath); err != nil && !os.IsNotExist(err) {
			log.Printf("subscriber: clear applied marker for resync: %v", err)
			heartbeat(hc, server, host, token, cur.ReleaseID, false, err.Error(), false)
			return
		}
		resyncFired = true
	}

	curIDStr := strconv.FormatInt(cur.ReleaseID, 10)
	if appliedID, appliedHash, ok := readApplied(appliedPath); ok &&
		appliedID == curIDStr && appliedHash == cur.ContentHash {
		// Already converged.
		heartbeat(hc, server, host, token, cur.ReleaseID, true, "", false)
		return
	}

	releasesDir := filepath.Join(home, "releases")
	if err := os.MkdirAll(releasesDir, 0o755); err != nil {
		log.Printf("subscriber: create releases dir: %v", err)
		heartbeat(hc, server, host, token, cur.ReleaseID, false, err.Error(), false)
		return
	}

	tmpDir := filepath.Join(releasesDir, fmt.Sprintf(".%s.tmp.%d", curIDStr, os.Getpid()))

	// The manifest is server-supplied and untrusted: validate every
	// doc path (no abs path, no '..' element, no escape from tmpDir)
	// BEFORE it is ever used to build a filesystem path — for the
	// want-set membership check below, for the post-extraction
	// completeness check, and for recomputeContentHash's reads. A
	// malformed manifest is rejected outright, before any archive
	// fetch.
	want, err := buildWantSet(tmpDir, cur.Docs)
	if err != nil {
		log.Printf("subscriber: invalid manifest: %v", err)
		heartbeat(hc, server, host, token, cur.ReleaseID, false, err.Error(), false)
		return
	}

	os.RemoveAll(tmpDir) // best-effort: guarantee a fresh temp dir
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		log.Printf("subscriber: create temp dir: %v", err)
		heartbeat(hc, server, host, token, cur.ReleaseID, false, err.Error(), false)
		return
	}

	if err := fetchAndExtractArchive(hc, server, token, cur.ReleaseID, tmpDir, want); err != nil {
		os.RemoveAll(tmpDir)
		log.Printf("subscriber: fetch/extract archive: %v", err)
		heartbeat(hc, server, host, token, cur.ReleaseID, false, err.Error(), false)
		return
	}

	gotHash, err := recomputeContentHash(tmpDir, cur.Docs)
	if err != nil || gotHash != cur.ContentHash {
		os.RemoveAll(tmpDir)
		msg := fmt.Sprintf("content hash mismatch: want %s got %s", cur.ContentHash, gotHash)
		if err != nil {
			msg = fmt.Sprintf("recompute content hash: %v", err)
		}
		log.Printf("subscriber: %s", msg)
		heartbeat(hc, server, host, token, cur.ReleaseID, false, msg, false)
		return
	}

	// Always install the freshly-downloaded, hash-verified tree as a
	// NEW directory — never reuse a pre-existing releases/<id> dir.
	// The materialized corpus is agent-writable, so a pre-existing dir
	// with this same id could have been tampered with locally; reusing
	// it would serve tampered content while reporting success on a
	// force-resync, defeating the recovery tool. Old dirs are simply
	// left in place: no pruning in v1 (corpus is KB-scale), and the
	// dir `corpus` currently points at is the fail-soft rollback
	// target that must never be deleted here.
	releaseDir, err := freshReleaseDir(releasesDir, curIDStr)
	if err != nil {
		os.RemoveAll(tmpDir)
		log.Printf("subscriber: pick release dir: %v", err)
		heartbeat(hc, server, host, token, cur.ReleaseID, false, err.Error(), false)
		return
	}
	if err := os.Rename(tmpDir, releaseDir); err != nil {
		os.RemoveAll(tmpDir)
		log.Printf("subscriber: install release dir: %v", err)
		heartbeat(hc, server, host, token, cur.ReleaseID, false, err.Error(), false)
		return
	}

	if err := flipCorpus(home, filepath.Base(releaseDir)); err != nil {
		log.Printf("subscriber: flip corpus symlink: %v", err)
		heartbeat(hc, server, host, token, cur.ReleaseID, false, err.Error(), false)
		return
	}

	if err := writeApplied(appliedPath, curIDStr, cur.ContentHash); err != nil {
		log.Printf("subscriber: write applied marker: %v", err)
		heartbeat(hc, server, host, token, cur.ReleaseID, false, err.Error(), false)
		return
	}

	heartbeat(hc, server, host, token, cur.ReleaseID, true, "", resyncFired)
}

// apiGet issues an authenticated GET; with an empty token it is a
// plain GET (the pre-authN loopback posture).
func apiGet(hc *http.Client, u, token string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return hc.Do(req)
}

func fetchCurrent(hc *http.Client, server, host, token string) (currentRelease, error) {
	// With a token the host id is resolved server-side from the token;
	// only tokenless (pre-authN) subscribers name themselves.
	u := server + "/api/releases/current"
	if host != "" {
		u += "?host=" + url.QueryEscape(host)
	}
	resp, err := apiGet(hc, u, token)
	if err != nil {
		return currentRelease{}, fmt.Errorf("GET %s: %w", u, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return currentRelease{}, fmt.Errorf("GET %s: unexpected status %s", u, resp.Status)
	}
	var cur currentRelease
	if err := json.NewDecoder(resp.Body).Decode(&cur); err != nil {
		return currentRelease{}, fmt.Errorf("decode current release: %w", err)
	}
	return cur, nil
}

func fetchAndExtractArchive(hc *http.Client, server, token string, releaseID int64, destDir string, want map[string]struct{}) error {
	u := fmt.Sprintf("%s/api/releases/%d/archive", server, releaseID)
	resp, err := apiGet(hc, u, token)
	if err != nil {
		return fmt.Errorf("GET %s: %w", u, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: unexpected status %s", u, resp.Status)
	}
	return extractTar(resp.Body, destDir, want)
}

// validateEntryPath rejects an absolute path, any path containing a
// '..' element, or a path that (after Join+Clean against destDir)
// would resolve outside destDir. It returns the safe, joined+cleaned
// target path on success. Used for BOTH tar entry names and
// server-supplied manifest doc paths: a manifest path must never be
// used to build a filesystem path (for a stat, a read, or a want-set
// membership check) before passing through this same guard.
func validateEntryPath(destDir, name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("refusing empty entry path")
	}
	if filepath.IsAbs(name) {
		return "", fmt.Errorf("refusing absolute entry path %q", name)
	}
	for _, part := range strings.Split(filepath.ToSlash(name), "/") {
		if part == ".." {
			return "", fmt.Errorf("refusing entry path with '..' element %q", name)
		}
	}
	destClean := filepath.Clean(destDir)
	destPrefix := destClean + string(filepath.Separator)
	target := filepath.Clean(filepath.Join(destDir, name))
	if target != destClean && !strings.HasPrefix(target, destPrefix) {
		return "", fmt.Errorf("refusing entry path %q escaping extraction dir", name)
	}
	return target, nil
}

// buildWantSet validates every manifest doc path and returns the exact
// set of paths the materialized tree must contain — nothing more,
// nothing less (closes the "extra tar entry gets silently materialized"
// gap, since extractTar rejects any entry not in this set).
func buildWantSet(destDir string, docs []manifestDoc) (map[string]struct{}, error) {
	want := make(map[string]struct{}, len(docs))
	for _, d := range docs {
		if _, err := validateEntryPath(destDir, d.Path); err != nil {
			return nil, fmt.Errorf("manifest doc: %w", err)
		}
		want[d.Path] = struct{}{}
	}
	return want, nil
}

// extractTar streams a tar archive into destDir, a fresh temp directory.
// For every entry it rejects an absolute name, a name containing a `..`
// path element, a name whose joined+cleaned path escapes destDir, a
// non-regular entry type, and — since want is built from the (already
// validated) manifest doc set — any entry not present in want, so the
// materialized tree can never contain an unmanifested file. After the
// stream ends it requires every path in want to have been written,
// so a manifest doc silently missing from the archive is also refused.
// Parent directories are created as needed since the server only emits
// regular-file entries (see store.WriteArchive).
func extractTar(r io.Reader, destDir string, want map[string]struct{}) error {
	seen := make(map[string]struct{}, len(want))
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read tar: %w", err)
		}
		name := hdr.Name
		target, err := validateEntryPath(destDir, name)
		if err != nil {
			return err
		}
		if hdr.Typeflag != tar.TypeReg {
			return fmt.Errorf("refusing non-regular tar entry %q", name)
		}
		if _, ok := want[name]; !ok {
			return fmt.Errorf("refusing unmanifested tar entry %q", name)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return fmt.Errorf("mkdir for %q: %w", name, err)
		}
		f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			return fmt.Errorf("create %q: %w", name, err)
		}
		if _, err := io.Copy(f, tr); err != nil {
			f.Close()
			return fmt.Errorf("write %q: %w", name, err)
		}
		if err := f.Close(); err != nil {
			return fmt.Errorf("close %q: %w", name, err)
		}
		seen[name] = struct{}{}
	}
	for p := range want {
		if _, ok := seen[p]; !ok {
			return fmt.Errorf("manifest doc %q missing from archive", p)
		}
	}
	return nil
}

// freshReleaseDir returns a path under releasesDir that does not yet
// exist: releases/<id> if free, else releases/<id>.<n> for the
// smallest free n >= 1. The caller must always install a freshly
// downloaded, hash-verified tree here rather than reusing whatever
// (possibly locally-tampered) directory a prior run may have left at
// releases/<id> — see the force-resync/tamper note in converge.
func freshReleaseDir(releasesDir, id string) (string, error) {
	base := filepath.Join(releasesDir, id)
	if _, err := os.Lstat(base); os.IsNotExist(err) {
		return base, nil
	} else if err != nil {
		return "", err
	}
	for n := 1; ; n++ {
		candidate := filepath.Join(releasesDir, fmt.Sprintf("%s.%d", id, n))
		if _, err := os.Lstat(candidate); os.IsNotExist(err) {
			return candidate, nil
		} else if err != nil {
			return "", err
		}
	}
}

// recomputeContentHash reproduces store.CutRelease's content-hash
// construction bit-for-bit from the extracted files: for each doc in
// manifest order, write its path bytes, one 0x00 byte, then the raw
// 32-byte SHA-256 of the doc body; the final hash is "sha256:"+hex(sum).
// See docs/plans/knowledge-server.md, "Content-hash construction". Each
// d.Path was already run through validateEntryPath by buildWantSet
// before extraction, so the join below is safe.
func recomputeContentHash(destDir string, docs []manifestDoc) (string, error) {
	h := sha256.New()
	for _, d := range docs {
		body, err := os.ReadFile(filepath.Join(destDir, d.Path))
		if err != nil {
			return "", fmt.Errorf("read %q: %w", d.Path, err)
		}
		sum := sha256.Sum256(body)
		io.WriteString(h, d.Path)
		h.Write([]byte{0})
		h.Write(sum[:])
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

// flipCorpus atomically points `corpus` at releases/<dirName> (dirName
// is the release dir's base name — normally the release id, or
// <id>.<n> when freshReleaseDir had to disambiguate) using a relative
// symlink, via the create-temp-then-rename-over pattern (a rename over
// an existing symlink is atomic on POSIX).
func flipCorpus(home, dirName string) error {
	corpusPath := filepath.Join(home, "corpus")
	tmpLink := filepath.Join(home, fmt.Sprintf(".corpus.tmp.%d", os.Getpid()))
	os.Remove(tmpLink) // best-effort: clear any stale leftover
	relTarget := filepath.Join("releases", dirName)
	if err := os.Symlink(relTarget, tmpLink); err != nil {
		return fmt.Errorf("create temp symlink: %w", err)
	}
	if err := os.Rename(tmpLink, corpusPath); err != nil {
		os.Remove(tmpLink)
		return fmt.Errorf("rename symlink onto corpus: %w", err)
	}
	return nil
}

// readApplied reads the `.applied` marker: "<release_id> <content_hash>"
// on one line. ok is false if the marker is absent or malformed.
func readApplied(path string) (id, hash string, ok bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", "", false
	}
	fields := strings.Fields(string(b))
	if len(fields) != 2 {
		return "", "", false
	}
	return fields[0], fields[1], true
}

func writeApplied(path, id, hash string) error {
	return os.WriteFile(path, []byte(id+" "+hash+"\n"), 0o644)
}

// heartbeat POSTs the pass outcome. It is best-effort observability:
// every failure is logged and swallowed, never propagated, per the
// fail-soft contract.
func heartbeat(hc *http.Client, server, host, token string, releaseID int64, ok bool, errMsg string, resyncApplied bool) {
	body := heartbeatBody{Host: host, ReleaseID: releaseID, OK: ok, ResyncApplied: resyncApplied}
	if errMsg != "" {
		body.Error = &errMsg
	}
	b, err := json.Marshal(body)
	if err != nil {
		log.Printf("subscriber: marshal heartbeat: %v", err)
		return
	}
	req, err := http.NewRequest(http.MethodPost, server+"/api/heartbeats", bytes.NewReader(b))
	if err != nil {
		log.Printf("subscriber: build heartbeat request: %v", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := hc.Do(req)
	if err != nil {
		log.Printf("subscriber: heartbeat: %v", err)
		return
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusNoContent {
		log.Printf("subscriber: heartbeat: unexpected status %s", resp.Status)
	}
}
