// Package store is the only component that opens the database.
// It consumes validated writes from the API layer and produces rows
// and release snapshots. See docs/plans/knowledge-server.md,
// "Component boundaries and contracts".
package store

import (
	"archive/tar"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// KernelWordCap approximates the Tier A token cap; a kernel doc over
// this word count blocks release cut.
const KernelWordCap = 2000

// KernelByteCap is the hard byte backstop for kernel docs; a kernel doc
// whose stored body exceeds this size blocks release cut, regardless of
// the word count. Bytes are measured before any normalization, on the
// raw body as it is persisted.
const KernelByteCap = 24576

var (
	ErrLint     = errors.New("lint")
	ErrNotFound = errors.New("not found")
	ErrInvalid  = errors.New("invalid")
	ErrConflict = errors.New("conflict")
)

const schema = `
CREATE TABLE IF NOT EXISTS collections (
  name       TEXT PRIMARY KEY,
  delivery   TEXT NOT NULL CHECK (delivery IN ('push','trigger','query')),
  in_release INTEGER NOT NULL DEFAULT 0
);
INSERT OR IGNORE INTO collections (name, delivery, in_release) VALUES
  ('kernel', 'push', 1),
  ('docs', 'trigger', 1);

CREATE TABLE IF NOT EXISTS doc_versions (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  collection TEXT NOT NULL REFERENCES collections(name),
  family_id  TEXT NOT NULL,
  version    INTEGER NOT NULL,
  title      TEXT NOT NULL DEFAULT '',
  status     TEXT NOT NULL CHECK (status IN ('draft','active','superseded')),
  owner      TEXT NOT NULL DEFAULT '',
  audience   TEXT NOT NULL DEFAULT '',
  tier       TEXT NOT NULL DEFAULT '',
  triggers   TEXT NOT NULL DEFAULT '',
  body       TEXT NOT NULL,
  editor     TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  UNIQUE (collection, family_id, version)
);
CREATE UNIQUE INDEX IF NOT EXISTS one_active_per_family
  ON doc_versions (collection, family_id) WHERE status = 'active';

CREATE TABLE IF NOT EXISTS releases (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  content_hash TEXT NOT NULL,
  created_at   TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS release_docs (
  release_id     INTEGER NOT NULL REFERENCES releases(id),
  doc_version_id INTEGER NOT NULL REFERENCES doc_versions(id),
  path           TEXT NOT NULL,
  sha256         TEXT NOT NULL,
  PRIMARY KEY (release_id, doc_version_id)
);

CREATE TABLE IF NOT EXISTS heartbeats (
  host       TEXT PRIMARY KEY,
  release_id INTEGER,
  ok         INTEGER NOT NULL,
  error      TEXT NOT NULL DEFAULT '',
  seen_at    TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS resync_requests (
  host         TEXT PRIMARY KEY,
  requested_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS host_tokens (
  host       TEXT PRIMARY KEY,
  token_hash TEXT NOT NULL UNIQUE,
  created_at TEXT NOT NULL
);
`

type Store struct {
	db *sql.DB
}

type DocSave struct {
	Title       string   `json:"title"`
	Status      string   `json:"status"`
	Tier        string   `json:"tier"`
	Triggers    []string `json:"triggers"`
	Owner       string   `json:"owner"`
	Audience    string   `json:"audience"`
	Body        string   `json:"body"`
	Editor      string   `json:"-"`
	BaseVersion *int     `json:"base_version,omitempty"`
}

type DocVersion struct {
	Collection string `json:"collection"`
	FamilyID   string `json:"family_id"`
	Version    int    `json:"version"`
	Status     string `json:"status"`
	CreatedAt  string `json:"created_at"`
	Editor     string `json:"editor"`
}

type ManifestDoc struct {
	Path     string `json:"path"`
	FamilyID string `json:"family_id"`
	Version  int    `json:"version"`
	SHA256   string `json:"sha256"`
}

type Manifest struct {
	ReleaseID   int64         `json:"release_id"`
	ContentHash string        `json:"content_hash"`
	CreatedAt   string        `json:"created_at"`
	Docs        []ManifestDoc `json:"docs"`
	Resync      bool          `json:"resync,omitempty"`
}

func Open(path string) (*Store, error) {
	// WAL + busy_timeout + a single in-process connection make
	// SQLITE_BUSY structurally impossible for the v1 single-writer
	// server. The pragma order in the DSN is the documented one for
	// modernc.org/sqlite.
	db, err := sql.Open("sqlite",
		"file:"+path+
			"?_pragma=foreign_keys(1)"+
			"&_pragma=busy_timeout(5000)"+
			"&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func now() string { return time.Now().UTC().Format(time.RFC3339) }

// validateIdent enforces the path-safe identifier shape required for any
// collection or family id that will land in a tar entry or filesystem
// path. The store is the trust boundary: callers are not pre-screened.
// Rejects: empty, leading '.', or any of '/', '\\', '..' substring, NUL,
// or an ASCII control character (< 0x20 or 0x7f).
func validateIdent(field, value string) error {
	if value == "" {
		return fmt.Errorf("%w: empty %s", ErrInvalid, field)
	}
	if strings.HasPrefix(value, ".") {
		return fmt.Errorf("%w: %s %q has leading '.'", ErrInvalid, field, value)
	}
	if strings.ContainsAny(value, "/\\") {
		return fmt.Errorf("%w: %s %q contains path separator", ErrInvalid, field, value)
	}
	if strings.Contains(value, "..") {
		return fmt.Errorf("%w: %s %q contains '..'", ErrInvalid, field, value)
	}
	for _, r := range value {
		if r == 0 || r < 0x20 || r == 0x7f {
			return fmt.Errorf("%w: %s %q contains control byte", ErrInvalid, field, value)
		}
		// Identifiers are ASCII by contract; a non-ASCII rune would
		// not be caught by the control-byte check above, so add an
		// explicit non-ASCII guard here.
		if r > 0x7f {
			return fmt.Errorf("%w: %s %q is not ASCII", ErrInvalid, field, value)
		}
	}
	return nil
}

// SaveDoc inserts a new immutable version. Saving as active demotes
// the previous active version to superseded in the same transaction.
// When BaseVersion is non-nil it is the optimistic-lock base: the save
// proceeds only if the current MAX(version) for (collection, family)
// equals *BaseVersion. A stale base returns ErrConflict. nil opts out
// of the check and restores last-writer-wins.
func (s *Store) SaveDoc(collection, family string, in DocSave) (DocVersion, error) {
	if err := validateIdent("collection", collection); err != nil {
		return DocVersion{}, err
	}
	if err := validateIdent("family", family); err != nil {
		return DocVersion{}, err
	}
	if in.Status != "draft" && in.Status != "active" {
		return DocVersion{}, fmt.Errorf("%w: status must be draft or active", ErrInvalid)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return DocVersion{}, err
	}
	defer tx.Rollback()

	var exists int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM collections WHERE name = ?`, collection).Scan(&exists); err != nil {
		return DocVersion{}, err
	}
	if exists == 0 {
		return DocVersion{}, fmt.Errorf("%w: unknown collection %q", ErrInvalid, collection)
	}

	var current int
	if err := tx.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM doc_versions
		WHERE collection = ? AND family_id = ?`, collection, family).Scan(&current); err != nil {
		return DocVersion{}, err
	}
	if in.BaseVersion != nil && *in.BaseVersion != current {
		return DocVersion{}, fmt.Errorf("%w: base_version %d does not match current %d",
			ErrConflict, *in.BaseVersion, current)
	}
	next := current + 1

	if in.Status == "active" {
		if _, err := tx.Exec(`UPDATE doc_versions SET status = 'superseded'
			WHERE collection = ? AND family_id = ? AND status = 'active'`, collection, family); err != nil {
			return DocVersion{}, err
		}
	}
	created := now()
	if _, err := tx.Exec(`INSERT INTO doc_versions
		(collection, family_id, version, title, status, owner, audience, tier, triggers, body, editor, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		collection, family, next, in.Title, in.Status, in.Owner, in.Audience,
		in.Tier, strings.Join(in.Triggers, ","), in.Body, in.Editor, created); err != nil {
		return DocVersion{}, err
	}
	if err := tx.Commit(); err != nil {
		return DocVersion{}, err
	}
	return DocVersion{
		Collection: collection, FamilyID: family, Version: next,
		Status: in.Status, CreatedAt: created, Editor: in.Editor,
	}, nil
}

// CutRelease snapshots every active doc in release-bearing collections.
// Publish lints run here; a lint failure cuts nothing.
func (s *Store) CutRelease() (Manifest, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return Manifest{}, err
	}
	defer tx.Rollback()

	rows, err := tx.Query(`SELECT d.id, d.collection, d.family_id, d.version, d.body
		FROM doc_versions d JOIN collections c ON c.name = d.collection
		WHERE d.status = 'active' AND c.in_release = 1
		ORDER BY d.collection, d.family_id`)
	if err != nil {
		return Manifest{}, err
	}
	type relDoc struct {
		id      int64
		path    string
		version int
		family  string
		body    string
	}
	var docs []relDoc
	for rows.Next() {
		var d relDoc
		var collection string
		if err := rows.Scan(&d.id, &collection, &d.family, &d.version, &d.body); err != nil {
			rows.Close()
			return Manifest{}, err
		}
		d.path = collection + "/" + d.family + ".md"
		if collection == "kernel" {
			if len(d.body) > KernelByteCap {
				rows.Close()
				return Manifest{}, fmt.Errorf("%w: kernel %q exceeds %d-byte cap",
					ErrLint, d.family, KernelByteCap)
			}
			if len(strings.Fields(d.body)) > KernelWordCap {
				rows.Close()
				return Manifest{}, fmt.Errorf("%w: kernel %q exceeds %d-word cap",
					ErrLint, d.family, KernelWordCap)
			}
		}
		docs = append(docs, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return Manifest{}, err
	}
	if len(docs) == 0 {
		return Manifest{}, fmt.Errorf("%w: release would be empty", ErrLint)
	}

	content := sha256.New()
	manifest := Manifest{CreatedAt: now()}
	for _, d := range docs {
		docSum := sha256.Sum256([]byte(d.body))
		io.WriteString(content, d.path)
		content.Write([]byte{0})
		content.Write(docSum[:])
		manifest.Docs = append(manifest.Docs, ManifestDoc{
			Path: d.path, FamilyID: d.family, Version: d.version,
			SHA256: hex.EncodeToString(docSum[:]),
		})
	}
	manifest.ContentHash = "sha256:" + hex.EncodeToString(content.Sum(nil))

	res, err := tx.Exec(`INSERT INTO releases (content_hash, created_at) VALUES (?, ?)`,
		manifest.ContentHash, manifest.CreatedAt)
	if err != nil {
		return Manifest{}, err
	}
	manifest.ReleaseID, err = res.LastInsertId()
	if err != nil {
		return Manifest{}, err
	}
	for i, d := range docs {
		if _, err := tx.Exec(`INSERT INTO release_docs (release_id, doc_version_id, path, sha256)
			VALUES (?, ?, ?, ?)`, manifest.ReleaseID, d.id, d.path, manifest.Docs[i].SHA256); err != nil {
			return Manifest{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

func (s *Store) manifest(where string, arg any) (Manifest, error) {
	var m Manifest
	err := s.db.QueryRow(`SELECT id, content_hash, created_at FROM releases `+where, arg).
		Scan(&m.ReleaseID, &m.ContentHash, &m.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Manifest{}, ErrNotFound
	}
	if err != nil {
		return Manifest{}, err
	}
	// Manifest doc order matches the hash construction order:
	// (collection, family_id). See docs/plans/knowledge-server.md,
	// "Manifest contract".
	rows, err := s.db.Query(`SELECT r.path, r.sha256, d.family_id, d.version
		FROM release_docs r JOIN doc_versions d ON d.id = r.doc_version_id
		WHERE r.release_id = ?
		ORDER BY d.collection, d.family_id`, m.ReleaseID)
	if err != nil {
		return Manifest{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var d ManifestDoc
		if err := rows.Scan(&d.Path, &d.SHA256, &d.FamilyID, &d.Version); err != nil {
			return Manifest{}, err
		}
		m.Docs = append(m.Docs, d)
	}
	return m, rows.Err()
}

func (s *Store) CurrentRelease() (Manifest, error) {
	return s.manifest(`WHERE id = (SELECT MAX(id) FROM releases) AND ? = 1`, 1)
}

func (s *Store) Release(id int64) (Manifest, error) {
	return s.manifest(`WHERE id = ?`, id)
}

// WriteArchive streams the release as a tar whose entry paths exactly
// match the manifest path fields.
func (s *Store) WriteArchive(id int64, w io.Writer) error {
	m, err := s.Release(id)
	if err != nil {
		return err
	}
	created, err := time.Parse(time.RFC3339, m.CreatedAt)
	if err != nil {
		return err
	}
	tw := tar.NewWriter(w)
	for _, d := range m.Docs {
		var body string
		if err := s.db.QueryRow(`SELECT d.body FROM release_docs r
			JOIN doc_versions d ON d.id = r.doc_version_id
			WHERE r.release_id = ? AND r.path = ?`, id, d.Path).Scan(&body); err != nil {
			return err
		}
		if err := tw.WriteHeader(&tar.Header{
			Name: d.Path, Mode: 0o644, Size: int64(len(body)), ModTime: created,
		}); err != nil {
			return err
		}
		if _, err := io.WriteString(tw, body); err != nil {
			return err
		}
	}
	return tw.Close()
}

// Heartbeat upserts the host's last-seen row. Observability only —
// never delivery or routing state. When ok and resyncApplied are both
// true the host's resync_requests row is deleted in the same
// transaction; this is the ONLY clear path.
func (s *Store) Heartbeat(host string, releaseID int64, ok bool, errMsg string, resyncApplied bool) error {
	if err := validateIdent("host", host); err != nil {
		return err
	}
	okInt := 0
	if ok {
		okInt = 1
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO heartbeats (host, release_id, ok, error, seen_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (host) DO UPDATE SET release_id = excluded.release_id,
		  ok = excluded.ok, error = excluded.error, seen_at = excluded.seen_at`,
		host, releaseID, okInt, errMsg, now()); err != nil {
		return err
	}
	if ok && resyncApplied {
		if _, err := tx.Exec(`DELETE FROM resync_requests WHERE host = ?`, host); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// RequestResync records a force-resync pull-flag for the host. The
// flag survives until the host heartbeats with ok==true AND
// resync_applied==true. There is no other clear path.
func (s *Store) RequestResync(host string) error {
	if err := validateIdent("host", host); err != nil {
		return err
	}
	_, err := s.db.Exec(`INSERT INTO resync_requests (host, requested_at) VALUES (?, ?)
		ON CONFLICT (host) DO UPDATE SET requested_at = excluded.requested_at`,
		host, now())
	return err
}

// IssueHostToken generates a fresh 256-bit bearer token for host and
// stores only its SHA-256 digest; the plaintext is returned exactly
// once and never persisted. Issuing for a host that already has a
// token replaces it, so reissue doubles as rotation and the old token
// stops verifying immediately.
func (s *Store) IssueHostToken(host string) (string, error) {
	if err := validateIdent("host", host); err != nil {
		return "", err
	}
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	token := hex.EncodeToString(raw[:])
	digest := sha256.Sum256([]byte(token))
	if _, err := s.db.Exec(`INSERT INTO host_tokens (host, token_hash, created_at)
		VALUES (?, ?, ?)
		ON CONFLICT (host) DO UPDATE SET token_hash = excluded.token_hash,
		  created_at = excluded.created_at`,
		host, hex.EncodeToString(digest[:]), now()); err != nil {
		return "", err
	}
	return token, nil
}

// RevokeHostToken deletes the host's token; its bearer stops verifying
// immediately. Revoking a host with no token is ErrNotFound.
func (s *Store) RevokeHostToken(host string) error {
	if err := validateIdent("host", host); err != nil {
		return err
	}
	res, err := s.db.Exec(`DELETE FROM host_tokens WHERE host = ?`, host)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%w: no token for host %q", ErrNotFound, host)
	}
	return nil
}

// HostForToken resolves a presented bearer token to its bound host by
// digest lookup — plaintext tokens are never stored, and an unknown
// token is (found=false, err=nil), not an error.
func (s *Store) HostForToken(token string) (string, bool, error) {
	digest := sha256.Sum256([]byte(token))
	var host string
	err := s.db.QueryRow(`SELECT host FROM host_tokens WHERE token_hash = ?`,
		hex.EncodeToString(digest[:])).Scan(&host)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return host, true, nil
}

// ResyncPending reports whether a force-resync pull-flag is set for
// the given host. An empty host returns false (not an error): the API
// treats "no host" as "no per-host resync status".
func (s *Store) ResyncPending(host string) (bool, error) {
	if host == "" {
		return false, nil
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM resync_requests WHERE host = ?`, host).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}
