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
	"encoding/json"
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

CREATE TABLE IF NOT EXISTS doc_links (
  doc_version_id   INTEGER NOT NULL REFERENCES doc_versions(id),
  position         INTEGER NOT NULL CHECK (position >= 0),
  relation         TEXT NOT NULL CHECK (relation IN ('reference','supersedes')),
  target_collection TEXT NOT NULL,
  target_family_id  TEXT NOT NULL,
  target_version    INTEGER NOT NULL CHECK (target_version > 0),
  PRIMARY KEY (doc_version_id, position),
  UNIQUE (doc_version_id, relation, target_collection, target_family_id, target_version)
);

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

CREATE TABLE IF NOT EXISTS conflicts (
  id               INTEGER PRIMARY KEY AUTOINCREMENT,
  kind             TEXT NOT NULL CHECK (kind IN ('edit','claim','policy')),
  status           TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open','resolved')),
  collection       TEXT NOT NULL,
  family_id        TEXT NOT NULL,
  other_collection TEXT NOT NULL DEFAULT '',
  other_family_id  TEXT NOT NULL DEFAULT '',
  base_version     INTEGER NOT NULL DEFAULT 0,
  their_version    INTEGER NOT NULL DEFAULT 0,
  attempts         INTEGER NOT NULL DEFAULT 1 CHECK (attempts >= 1),
  attempted        TEXT NOT NULL DEFAULT '',
  detail           TEXT NOT NULL DEFAULT '',
  opened_by        TEXT NOT NULL DEFAULT '',
  opened_at        TEXT NOT NULL,
  resolved_by      TEXT NOT NULL DEFAULT '',
  resolved_at      TEXT NOT NULL DEFAULT '',
  resolution       TEXT NOT NULL DEFAULT '',
  winning_version  INTEGER NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX IF NOT EXISTS one_open_conflict_per_kind_family
  ON conflicts (kind, collection, family_id) WHERE status = 'open';
CREATE TRIGGER IF NOT EXISTS conflicts_no_delete
  BEFORE DELETE ON conflicts
  BEGIN SELECT RAISE(ABORT, 'conflicts are append-only'); END;
CREATE TRIGGER IF NOT EXISTS conflicts_no_rewrite
  BEFORE UPDATE ON conflicts WHEN OLD.status = 'resolved'
  BEGIN SELECT RAISE(ABORT, 'resolved conflicts are immutable'); END;
`

type Store struct {
	db *sql.DB
}

type DocSave struct {
	Title       string    `json:"title"`
	Status      string    `json:"status"`
	Tier        string    `json:"tier"`
	Triggers    []string  `json:"triggers"`
	Owner       string    `json:"owner"`
	Audience    string    `json:"audience"`
	Body        string    `json:"body"`
	Links       []DocLink `json:"links"`
	Editor      string    `json:"-"`
	BaseVersion *int      `json:"base_version,omitempty"`
}

// DocLink is one immutable, version-specific relationship owned by a
// document version. Targets deliberately have no database foreign key:
// drafts may carry forward references, while release linting is the policy
// gate that requires an active reference or an existing superseded target.
type DocLink struct {
	Relation   string `json:"relation"`
	Collection string `json:"collection"`
	FamilyID   string `json:"family_id"`
	Version    int    `json:"version"`
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

// DocMeta is the per-version metadata returned by list and history
// views. Bodies are intentionally absent — callers that need the body
// fetch a specific version through GetDoc.
type DocMeta struct {
	Collection string `json:"collection"`
	FamilyID   string `json:"family_id"`
	Version    int    `json:"version"`
	Title      string `json:"title"`
	Status     string `json:"status"`
	Tier       string `json:"tier"`
	Editor     string `json:"editor"`
	CreatedAt  string `json:"created_at"`
}

// Doc is the full version view returned by GetDoc. DocMeta is embedded
// so the per-version fields share the JSON tags above without
// repetition; consumers that only need metadata can read DocMeta.
type Doc struct {
	DocMeta
	Owner    string    `json:"owner"`
	Audience string    `json:"audience"`
	Triggers []string  `json:"triggers"`
	Body     string    `json:"body"`
	Links    []DocLink `json:"links"`
}

// ConflictMeta is the list-view row: per the brief, no attempted
// payload is exposed here. The omitempty on optional fields keeps
// claim/policy rows from advertising zero-valued bookends.
type ConflictMeta struct {
	ID              int64  `json:"id"`
	Kind            string `json:"kind"`
	Status          string `json:"status"`
	Collection      string `json:"collection"`
	FamilyID        string `json:"family_id"`
	OtherCollection string `json:"other_collection,omitempty"`
	OtherFamilyID   string `json:"other_family_id,omitempty"`
	BaseVersion     int    `json:"base_version,omitempty"`
	TheirVersion    int    `json:"their_version,omitempty"`
	Attempts        int    `json:"attempts"`
	Detail          string `json:"detail"`
	OpenedBy        string `json:"opened_by"`
	OpenedAt        string `json:"opened_at"`
	ResolvedBy      string `json:"resolved_by,omitempty"`
	ResolvedAt      string `json:"resolved_at,omitempty"`
	Resolution      string `json:"resolution,omitempty"`
	WinningVersion  int    `json:"winning_version,omitempty"`
}

// Conflict is the full record view. Attempted carries the LATEST
// rejected DocSave for edit conflicts; for claim/policy rows and for
// empty attempted columns it stays nil. v1 retains only the latest
// rejected payload — the attempts counter is the durable record that
// earlier attempts occurred.
type Conflict struct {
	ConflictMeta
	Attempted *DocSave `json:"attempted,omitempty"`
}

// ConflictRecordedError carries the id of the conflict record that
// was committed alongside a rejected operation. Unwrap surfaces the
// wrapped ErrConflict / ErrLint so the existing errors.Is mapping is
// unchanged — the API layer's writeErr translates ErrConflict/ErrLint
// to the same 409 status, and the API adds a conflict_id field only
// when errors.As(err, &cre) matches.
type ConflictRecordedError struct {
	ID  int64
	Err error
}

func (e *ConflictRecordedError) Error() string { return e.Err.Error() }
func (e *ConflictRecordedError) Unwrap() error { return e.Err }

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

// validateDocSave enforces the SaveDoc field constraints that are
// independent of any database lookup: status enum, trigger grammar,
// and (caller-validated) idents. Status and trigger grammar live
// here so ResolveConflict's save path can reuse the same checks; the
// idents are validated by SaveDoc's caller (the route handler) and
// by ResolveConflict (which also validates them before reaching the
// save path).
func validateDocSave(in DocSave) error {
	if in.Status != "draft" && in.Status != "active" {
		return fmt.Errorf("%w: status must be draft or active", ErrInvalid)
	}
	// Trigger grammar is closed at the write door: the stored form is a
	// comma-joined string, so any value containing ',' or empty value
	// would round-trip as the wrong number of triggers. Reject up front
	// rather than silently joining and re-splitting a lossy form.
	for _, t := range in.Triggers {
		if t == "" {
			return fmt.Errorf("%w: trigger values must be non-empty", ErrInvalid)
		}
		if strings.Contains(t, ",") {
			return fmt.Errorf("%w: trigger %q contains ','", ErrInvalid, t)
		}
	}
	seenLinks := make(map[string]struct{}, len(in.Links))
	for i, link := range in.Links {
		if link.Relation != "reference" && link.Relation != "supersedes" {
			return fmt.Errorf("%w: link %d relation must be reference or supersedes", ErrInvalid, i)
		}
		if err := validateIdent("link collection", link.Collection); err != nil {
			return err
		}
		if err := validateIdent("link family", link.FamilyID); err != nil {
			return err
		}
		if link.Version <= 0 {
			return fmt.Errorf("%w: link %d version must be positive", ErrInvalid, i)
		}
		key := fmt.Sprintf("%s\x00%s\x00%s\x00%d", link.Relation, link.Collection, link.FamilyID, link.Version)
		if _, ok := seenLinks[key]; ok {
			return fmt.Errorf("%w: duplicate link at position %d", ErrInvalid, i)
		}
		seenLinks[key] = struct{}{}
	}
	return nil
}

// saveDocTx is the post-validation body of SaveDoc: collection-exists
// check, current-version read, stale-base check (returning plain
// ErrConflict — NO conflict recording at this layer; the caller
// orchestrates the record-and-commit dance), supersede-update, insert.
// Must be called inside a transaction; never touches s.db (the store
// pool is pinned to one connection and a concurrent s.db call would
// deadlock).
func (s *Store) saveDocTx(tx *sql.Tx, collection, family string, in DocSave) (DocVersion, error) {
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
	for i, link := range in.Links {
		if link.Collection == collection && link.FamilyID == family && link.Version == next {
			return DocVersion{}, fmt.Errorf("%w: link %d targets the saved version itself", ErrInvalid, i)
		}
	}

	if in.Status == "active" {
		if _, err := tx.Exec(`UPDATE doc_versions SET status = 'superseded'
			WHERE collection = ? AND family_id = ? AND status = 'active'`, collection, family); err != nil {
			return DocVersion{}, err
		}
	}
	created := now()
	res, err := tx.Exec(`INSERT INTO doc_versions
		(collection, family_id, version, title, status, owner, audience, tier, triggers, body, editor, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		collection, family, next, in.Title, in.Status, in.Owner, in.Audience,
		in.Tier, strings.Join(in.Triggers, ","), in.Body, in.Editor, created)
	if err != nil {
		return DocVersion{}, err
	}
	docVersionID, err := res.LastInsertId()
	if err != nil {
		return DocVersion{}, err
	}
	for position, link := range in.Links {
		if _, err := tx.Exec(`INSERT INTO doc_links
			(doc_version_id, position, relation, target_collection, target_family_id, target_version)
			VALUES (?, ?, ?, ?, ?, ?)`,
			docVersionID, position, link.Relation, link.Collection, link.FamilyID, link.Version); err != nil {
			return DocVersion{}, err
		}
	}
	return DocVersion{
		Collection: collection, FamilyID: family, Version: next,
		Status: in.Status, CreatedAt: created, Editor: in.Editor,
	}, nil
}

// SaveDoc inserts a new immutable version. Saving as active demotes
// the previous active version to superseded in the same transaction.
// When BaseVersion is non-nil it is the optimistic-lock base: the save
// proceeds only if the current MAX(version) for (collection, family)
// equals *BaseVersion. A stale base returns a ConflictRecordedError
// wrapping ErrConflict — a conflict record is committed alongside the
// rejection so the merge view has the rejected payload to render and
// any tab can pick the open conflict up. nil opts out of the check
// and restores last-writer-wins (no conflict is recorded in that
// branch).
func (s *Store) SaveDoc(collection, family string, in DocSave) (DocVersion, error) {
	if err := validateIdent("collection", collection); err != nil {
		return DocVersion{}, err
	}
	if err := validateIdent("family", family); err != nil {
		return DocVersion{}, err
	}
	if err := validateDocSave(in); err != nil {
		return DocVersion{}, err
	}

	tx, err := s.db.Begin()
	if err != nil {
		return DocVersion{}, err
	}
	defer tx.Rollback()

	v, err := s.saveDocTx(tx, collection, family, in)
	if err == nil {
		if err := tx.Commit(); err != nil {
			return DocVersion{}, err
		}
		return v, nil
	}
	if !errors.Is(err, ErrConflict) || in.BaseVersion == nil {
		return DocVersion{}, err
	}
	// Stale base: saveDocTx rejects BEFORE any write, so the open tx
	// holds only reads. Record the conflict row in this SAME tx and
	// commit it — the recorded their_version and the version check
	// share one snapshot, and no second Begin is ever needed (a second
	// Begin while this tx is alive would deadlock the single-connection
	// pool).
	id, recErr := s.upsertEditConflictTx(tx, collection, family, in)
	if recErr != nil {
		return DocVersion{}, recErr
	}
	if cErr := tx.Commit(); cErr != nil {
		return DocVersion{}, cErr
	}
	return DocVersion{}, &ConflictRecordedError{ID: id, Err: err}
}

// upsertEditConflictTx records (or refreshes) the open edit conflict
// for (collection, family) inside the CALLER's transaction. Dedup is
// the SELECT-then-INSERT/UPDATE shape pinned by the brief: the partial
// unique index one_open_conflict_per_kind_family refuses a second open
// row for the same (kind,collection,family), and the check-then-act is
// safe because the store is the only opener of this database file
// (SetMaxOpenConns(1) plus the component contract). A second
// connection could race this — the comment is left in deliberately as
// the regression marker if that ever changes.
func (s *Store) upsertEditConflictTx(tx *sql.Tx, collection, family string, in DocSave) (int64, error) {
	var current int
	if err := tx.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM doc_versions
		WHERE collection = ? AND family_id = ?`, collection, family).Scan(&current); err != nil {
		return 0, err
	}

	attempted, err := json.Marshal(in)
	if err != nil {
		return 0, fmt.Errorf("marshal rejected DocSave: %w", err)
	}

	var existingID int64
	err = tx.QueryRow(`SELECT id FROM conflicts
		WHERE kind = 'edit' AND status = 'open' AND collection = ? AND family_id = ?`,
		collection, family).Scan(&existingID)
	if err == nil {
		// The open row reflects the LATEST rejected attempt: every
		// attempt-scoped field refreshes, including base_version — a
		// later attempt from a different stale base must not leave the
		// first attempt's base in the audit row.
		if _, err := tx.Exec(`UPDATE conflicts
			SET base_version = ?, their_version = ?, attempts = attempts + 1,
			    attempted = ?, detail = ?, opened_by = ?, opened_at = ?
			WHERE id = ?`,
			*in.BaseVersion, current, string(attempted),
			fmt.Sprintf("save with stale base_version %d (current %d)", *in.BaseVersion, current),
			in.Editor, now(), existingID); err != nil {
			return 0, err
		}
		return existingID, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}

	res, err := tx.Exec(`INSERT INTO conflicts
		(kind, status, collection, family_id, other_collection, other_family_id,
		 base_version, their_version, attempts, attempted, detail, opened_by, opened_at)
		VALUES ('edit', 'open', ?, ?, '', '', ?, ?, 1, ?, ?, ?, ?)`,
		collection, family, *in.BaseVersion, current, string(attempted),
		fmt.Sprintf("save with stale base_version %d (current %d)", *in.BaseVersion, current),
		in.Editor, now())
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	return id, nil
}

// relDoc carries the per-doc slice of a release candidate. id is the
// doc_versions row id (used to FK into release_docs); body is needed
// for the per-doc and content hashes.
type relDoc struct {
	id         int64
	collection string
	path       string
	version    int
	family     string
	body       string
}

// lintError is the typed wrapper that releaseCandidate returns for
// a doc-scoped kernel or document-link lint. It unwraps to ErrLint (so the existing
// errors.Is(err, ErrLint) → 409 mapping keeps working in the API
// layer), and carries the offending (collection, family, version,
// reason) so CutRelease can auto-record a policy conflict. The
// empty-release lint does NOT use this type: it carries no doc, so
// no conflict row can be opened for it.
type lintError struct {
	Collection string
	Family     string
	Version    int
	Reason     string
}

func (e *lintError) Error() string { return e.Reason }
func (e *lintError) Unwrap() error { return ErrLint }

// releaseCandidate computes the would-be release manifest inside the
// caller's transaction. It returns the manifest with ReleaseID == 0
// (the caller assigns the inserted row's id) and the per-doc rows
// needed to populate release_docs. Kernel lints (byte + word cap), document-
// link lints, and the empty-release lint run here; all surface as ErrLint and
// the caller rolls back.
//
// Transaction ownership is part of the contract: the store pool is
// pinned to ONE connection (SetMaxOpenConns(1)), so this helper must
// NEVER touch s.db — a s.db.Query inside an open transaction would
// deadlock, and computing the candidate outside the cut's transaction
// would let it change between hash check and insert, defeating the
// CutRelease expectedHash precondition.
func (s *Store) releaseCandidate(tx *sql.Tx) (Manifest, []relDoc, error) {
	rows, err := tx.Query(`SELECT d.id, d.collection, d.family_id, d.version, d.body
		FROM doc_versions d JOIN collections c ON c.name = d.collection
		WHERE d.status = 'active' AND c.in_release = 1
		ORDER BY d.collection, d.family_id`)
	if err != nil {
		return Manifest{}, nil, err
	}
	var docs []relDoc
	for rows.Next() {
		var d relDoc
		if err := rows.Scan(&d.id, &d.collection, &d.family, &d.version, &d.body); err != nil {
			rows.Close()
			return Manifest{}, nil, err
		}
		d.path = d.collection + "/" + d.family + ".md"
		if d.collection == "kernel" {
			if len(d.body) > KernelByteCap {
				rows.Close()
				return Manifest{}, nil, &lintError{
					Collection: d.collection, Family: d.family, Version: d.version,
					Reason: fmt.Sprintf("kernel %q exceeds %d-byte cap", d.family, KernelByteCap),
				}
			}
			if len(strings.Fields(d.body)) > KernelWordCap {
				rows.Close()
				return Manifest{}, nil, &lintError{
					Collection: d.collection, Family: d.family, Version: d.version,
					Reason: fmt.Sprintf("kernel %q exceeds %d-word cap", d.family, KernelWordCap),
				}
			}
		}
		docs = append(docs, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return Manifest{}, nil, err
	}
	for _, d := range docs {
		if err := lintDocLinksTx(tx, d); err != nil {
			return Manifest{}, nil, err
		}
	}
	if len(docs) == 0 {
		// Empty-release lint carries no doc: it MUST NOT carry a
		// (collection, family) context, and CutRelease therefore
		// must not record a policy conflict for this case.
		return Manifest{}, nil, fmt.Errorf("%w: release would be empty", ErrLint)
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
	return manifest, docs, nil
}

// lintDocLinksTx enforces link policy for one active release candidate doc.
// Link targets are looked up only after the candidate query is closed so the
// single transaction connection never has nested live result sets.
func lintDocLinksTx(tx *sql.Tx, source relDoc) error {
	rows, err := tx.Query(`SELECT l.relation, l.target_collection, l.target_family_id,
		l.target_version, COALESCE(t.status, ''), COALESCE(c.in_release, 0)
		FROM doc_links l
		LEFT JOIN doc_versions t
		  ON t.collection = l.target_collection
		 AND t.family_id = l.target_family_id
		 AND t.version = l.target_version
		LEFT JOIN collections c ON c.name = l.target_collection
		WHERE l.doc_version_id = ?
		ORDER BY l.position`, source.id)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var link DocLink
		var status string
		var inRelease int
		if err := rows.Scan(&link.Relation, &link.Collection, &link.FamilyID,
			&link.Version, &status, &inRelease); err != nil {
			return err
		}
		target := fmt.Sprintf("%s/%s version %d", link.Collection, link.FamilyID, link.Version)
		reason := ""
		switch link.Relation {
		case "reference":
			switch {
			case status == "":
				reason = fmt.Sprintf("reference target %s does not exist", target)
			case status != "active":
				reason = fmt.Sprintf("reference target %s is %s; target must be active in the release", target, status)
			case inRelease != 1:
				reason = fmt.Sprintf("reference target %s is not release-bearing", target)
			}
		case "supersedes":
			switch {
			case status == "":
				reason = fmt.Sprintf("supersedes target %s does not exist", target)
			case status != "superseded":
				reason = fmt.Sprintf("supersedes target %s is %s; target must be superseded", target, status)
			}
		}
		if reason != "" {
			return &lintError{
				Collection: source.collection, Family: source.family, Version: source.version,
				Reason: fmt.Sprintf("document %s/%s: %s", source.collection, source.family, reason),
			}
		}
	}
	return rows.Err()
}

// PreviewRelease computes the would-be release manifest inside a
// transaction that is rolled back before return — PreviewRelease
// never inserts. The returned Manifest has ReleaseID == 0 to make
// its preview-only nature unambiguous to callers. Lint failures
// surface as ErrLint exactly like a real cut.
func (s *Store) PreviewRelease() (Manifest, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return Manifest{}, err
	}
	defer tx.Rollback()
	m, _, err := s.releaseCandidate(tx)
	return m, err
}

// CutRelease snapshots every active doc in release-bearing collections.
// When expectedHash is non-empty the candidate content_hash must match
// it exactly; a mismatch returns ErrConflict and cuts nothing. Pass
// "" to opt out (the unconditional legacy behavior).
//
// A doc-scoped kernel or document-link lint auto-opens a policy
// conflict for the offending (collection, family). releaseCandidate
// rejects BEFORE any write, so the open tx holds only reads and the
// row is recorded in the SAME tx and committed — no second Begin (a
// second Begin while this tx is alive would deadlock the single-
// connection pool). The ConflictRecordedError wraps the typed ErrLint
// so the API layer reports it as 409 lint with a numeric conflict_id.
// The empty-release lint carries no doc and records nothing.
//
// On success, every open policy conflict is resolved in the same tx
// as the release insert: resolution="cleared by release <id>",
// resolved_by=editor, and winning_version = the family's version
// listed in the new release's manifest (0 when the family is not in
// the manifest, e.g. demoted to draft before the cut). Edit and claim
// conflicts are NOT touched.
func (s *Store) CutRelease(expectedHash, editorName string) (Manifest, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return Manifest{}, err
	}
	defer tx.Rollback()

	manifest, docs, err := s.releaseCandidate(tx)
	if err != nil {
		var le *lintError
		if errors.Is(err, ErrLint) && errors.As(err, &le) && le.Collection != "" {
			// releaseCandidate rejected before any write: the tx holds
			// only reads, so record the policy row here and commit it.
			id, recErr := s.upsertPolicyConflictTx(tx, le.Collection, le.Family, le.Version, editorName, le.Reason)
			if recErr != nil {
				return Manifest{}, recErr
			}
			if cErr := tx.Commit(); cErr != nil {
				return Manifest{}, cErr
			}
			return Manifest{}, &ConflictRecordedError{ID: id, Err: err}
		}
		return Manifest{}, err
	}
	if expectedHash != "" && manifest.ContentHash != expectedHash {
		return Manifest{}, fmt.Errorf("%w: release candidate changed", ErrConflict)
	}
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
	// Resolve every open policy conflict in the same tx as the
	// release insert. winning_version = the version of this family's
	// entry in the new manifest (0 when the family is not in the
	// manifest). Already-resolved rows are not touched; the trigger
	// refuses rewrites of resolved rows so a second successful cut
	// cannot mutate them.
	manifestByCollection := map[string]map[string]int{}
	for _, d := range manifest.Docs {
		col := strings.SplitN(d.Path, "/", 2)[0]
		if _, ok := manifestByCollection[col]; !ok {
			manifestByCollection[col] = map[string]int{}
		}
		manifestByCollection[col][d.FamilyID] = d.Version
	}
	rows, err := tx.Query(`SELECT id, collection, family_id FROM conflicts
		WHERE kind = 'policy' AND status = 'open'`)
	if err != nil {
		return Manifest{}, err
	}
	type policyRow struct {
		id         int64
		collection string
		family     string
	}
	var policies []policyRow
	for rows.Next() {
		var p policyRow
		if err := rows.Scan(&p.id, &p.collection, &p.family); err != nil {
			rows.Close()
			return Manifest{}, err
		}
		policies = append(policies, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return Manifest{}, err
	}
	resolvedAt := now()
	for _, p := range policies {
		wv := 0
		if fams, ok := manifestByCollection[p.collection]; ok {
			wv = fams[p.family]
		}
		if _, err := tx.Exec(`UPDATE conflicts
			SET status = 'resolved', resolved_by = ?, resolved_at = ?,
			    resolution = ?, winning_version = ?
			WHERE id = ? AND status = 'open'`,
			editorName, resolvedAt,
			fmt.Sprintf("cleared by release %d", manifest.ReleaseID), wv, p.id); err != nil {
			return Manifest{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

// upsertPolicyConflictTx records (or refreshes) the open policy
// conflict for (collection, family) inside the CALLER's transaction.
// Dedup is the same SELECT-then-INSERT/UPDATE shape as
// upsertEditConflictTx: a partial unique index on (kind='policy',
// collection, family_id) refuses a second open row for the same
// family, and the check-then-act is safe under SetMaxOpenConns(1).
func (s *Store) upsertPolicyConflictTx(tx *sql.Tx, collection, family string, version int, editorName, reason string) (int64, error) {
	var existingID int64
	err := tx.QueryRow(`SELECT id FROM conflicts
		WHERE kind = 'policy' AND status = 'open' AND collection = ? AND family_id = ?`,
		collection, family).Scan(&existingID)
	if err == nil {
		if _, err := tx.Exec(`UPDATE conflicts
			SET their_version = ?, attempts = attempts + 1,
			    detail = ?, opened_by = ?, opened_at = ?
			WHERE id = ?`,
			version, reason, editorName, now(), existingID); err != nil {
			return 0, err
		}
		return existingID, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	res, err := tx.Exec(`INSERT INTO conflicts
		(kind, status, collection, family_id, other_collection, other_family_id,
		 base_version, their_version, attempts, attempted, detail, opened_by, opened_at)
		VALUES ('policy', 'open', ?, ?, '', '', 0, ?, 1, '', ?, ?, ?)`,
		collection, family, version, reason, editorName, now())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ListDocs returns one DocMeta per family: the max-version row of
// that family. Empty store → empty non-nil slice (so the JSON wire
// form is "[]", never "null"). The latest-per-family join shape is
// pinned by the SQL below — do not switch to a bare GROUP BY over
// non-aggregated columns, which is undefined in SQL.
func (s *Store) ListDocs() ([]DocMeta, error) {
	rows, err := s.db.Query(`SELECT d.collection, d.family_id, d.version, d.title, d.status,
		d.tier, d.editor, d.created_at
		FROM doc_versions d
		JOIN (SELECT collection, family_id, MAX(version) AS v
		      FROM doc_versions GROUP BY collection, family_id) m
		  ON m.collection = d.collection AND m.family_id = d.family_id
		     AND m.v = d.version
		ORDER BY d.collection, d.family_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DocMeta{}
	for rows.Next() {
		var d DocMeta
		if err := rows.Scan(&d.Collection, &d.FamilyID, &d.Version, &d.Title,
			&d.Status, &d.Tier, &d.Editor, &d.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// DocHistory returns every version of one family, newest first.
// Unknown family → ErrNotFound. Both idents are validated.
func (s *Store) DocHistory(collection, family string) ([]DocMeta, error) {
	if err := validateIdent("collection", collection); err != nil {
		return nil, err
	}
	if err := validateIdent("family", family); err != nil {
		return nil, err
	}
	rows, err := s.db.Query(`SELECT collection, family_id, version, title, status,
		tier, editor, created_at
		FROM doc_versions
		WHERE collection = ? AND family_id = ?
		ORDER BY version DESC`, collection, family)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DocMeta{}
	for rows.Next() {
		var d DocMeta
		if err := rows.Scan(&d.Collection, &d.FamilyID, &d.Version, &d.Title,
			&d.Status, &d.Tier, &d.Editor, &d.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, ErrNotFound
	}
	return out, nil
}

// GetDoc returns the full Doc for one version. version <= 0 means
// latest (max version in the family). Unknown family/version →
// ErrNotFound. Both idents are validated. Triggers decode from the
// comma-joined stored form: empty stored string yields a non-nil
// empty slice, never []string{""}.
func (s *Store) GetDoc(collection, family string, version int) (Doc, error) {
	if err := validateIdent("collection", collection); err != nil {
		return Doc{}, err
	}
	if err := validateIdent("family", family); err != nil {
		return Doc{}, err
	}
	var (
		row   *sql.Row
		doc   Doc
		docID int64
		trig  string
	)
	if version <= 0 {
		row = s.db.QueryRow(`SELECT id, collection, family_id, version, title, status,
			owner, audience, tier, triggers, body, editor, created_at
			FROM doc_versions
			WHERE collection = ? AND family_id = ?
			ORDER BY version DESC LIMIT 1`, collection, family)
	} else {
		row = s.db.QueryRow(`SELECT id, collection, family_id, version, title, status,
			owner, audience, tier, triggers, body, editor, created_at
			FROM doc_versions
			WHERE collection = ? AND family_id = ? AND version = ?`,
			collection, family, version)
	}
	err := row.Scan(&docID, &doc.Collection, &doc.FamilyID, &doc.Version, &doc.Title,
		&doc.Status, &doc.Owner, &doc.Audience, &doc.Tier, &trig, &doc.Body,
		&doc.Editor, &doc.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Doc{}, ErrNotFound
	}
	if err != nil {
		return Doc{}, err
	}
	doc.Triggers = []string{}
	if trig != "" {
		doc.Triggers = strings.Split(trig, ",")
	}
	doc.Links = []DocLink{}
	rows, err := s.db.Query(`SELECT relation, target_collection, target_family_id, target_version
		FROM doc_links WHERE doc_version_id = ? ORDER BY position`, docID)
	if err != nil {
		return Doc{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var link DocLink
		if err := rows.Scan(&link.Relation, &link.Collection, &link.FamilyID, &link.Version); err != nil {
			return Doc{}, err
		}
		doc.Links = append(doc.Links, link)
	}
	if err := rows.Err(); err != nil {
		return Doc{}, err
	}
	return doc, nil
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

// HostStatus is one fleet-page row: the union of everything the server
// knows about a host — last heartbeat, pending force-resync, token
// issuance. A host appears if it exists in ANY of the three tables; a
// host the server has never heard of (no heartbeat, no resync request,
// no token) does not exist as far as the fleet is concerned.
// SeenAt == "" means the host has never heartbeated (ReleaseID/OK/Error
// are zero-valued and meaningless in that case — consumers must check
// SeenAt first). ResyncRequestedAt == "" means no resync is pending.
// TokenCreatedAt == "" means no token has been issued.
type HostStatus struct {
	Host              string `json:"host"`
	ReleaseID         int64  `json:"release_id"`
	OK                bool   `json:"ok"`
	Error             string `json:"error,omitempty"`
	SeenAt            string `json:"seen_at,omitempty"`
	ResyncRequestedAt string `json:"resync_requested_at,omitempty"`
	TokenCreatedAt    string `json:"token_created_at,omitempty"`
}

// ListHosts returns one HostStatus per known host, ordered by host name.
// Union across heartbeats, resync_requests, and host_tokens; absent
// fields coalesce to zero values. Empty store → non-nil empty slice
// (wire form "[]", never "null"). Read-only: no ident validation, no
// transaction needed.
func (s *Store) ListHosts() ([]HostStatus, error) {
	rows, err := s.db.Query(`
		SELECT u.host,
		       COALESCE(h.release_id, 0), COALESCE(h.ok, 0),
		       COALESCE(h.error, ''), COALESCE(h.seen_at, ''),
		       COALESCE(r.requested_at, ''), COALESCE(t.created_at, '')
		FROM (SELECT host FROM heartbeats
		      UNION SELECT host FROM resync_requests
		      UNION SELECT host FROM host_tokens) u
		LEFT JOIN heartbeats       h ON h.host = u.host
		LEFT JOIN resync_requests  r ON r.host = u.host
		LEFT JOIN host_tokens      t ON t.host = u.host
		ORDER BY u.host`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []HostStatus{}
	for rows.Next() {
		var hs HostStatus
		var okInt int
		if err := rows.Scan(&hs.Host, &hs.ReleaseID, &okInt,
			&hs.Error, &hs.SeenAt,
			&hs.ResyncRequestedAt, &hs.TokenCreatedAt); err != nil {
			return nil, err
		}
		hs.OK = okInt != 0
		out = append(out, hs)
	}
	return out, rows.Err()
}

// scanConflictMeta reads one conflict row's list-view fields. The
// schema is shared between ListConflicts (no attempted) and the
// per-row resolve path (also no attempted); the get/flag/resolve
// methods that need the attempted column read it separately.
func scanConflictMeta(row interface {
	Scan(...any) error
}) (ConflictMeta, error) {
	var c ConflictMeta
	var attempted string
	if err := row.Scan(
		&c.ID, &c.Kind, &c.Status,
		&c.Collection, &c.FamilyID,
		&c.OtherCollection, &c.OtherFamilyID,
		&c.BaseVersion, &c.TheirVersion,
		&c.Attempts, &attempted,
		&c.Detail,
		&c.OpenedBy, &c.OpenedAt,
		&c.ResolvedBy, &c.ResolvedAt,
		&c.Resolution, &c.WinningVersion,
	); err != nil {
		return ConflictMeta{}, err
	}
	return c, nil
}

// ListConflicts returns one ConflictMeta per conflict row, newest
// first (ORDER BY id DESC). status "" returns all rows; "open" or
// "resolved" filter accordingly; any other value is ErrInvalid. An
// empty store returns a non-nil empty slice so the JSON wire form is
// "[]", never "null". List rows carry NO attempted payload — callers
// that need the rejected DocSave call GetConflict.
func (s *Store) ListConflicts(status string) ([]ConflictMeta, error) {
	var (
		rows *sql.Rows
		err  error
	)
	switch status {
	case "":
		rows, err = s.db.Query(`SELECT id, kind, status,
			collection, family_id, other_collection, other_family_id,
			base_version, their_version, attempts, attempted,
			detail, opened_by, opened_at,
			resolved_by, resolved_at, resolution, winning_version
			FROM conflicts ORDER BY id DESC`)
	case "open", "resolved":
		rows, err = s.db.Query(`SELECT id, kind, status,
			collection, family_id, other_collection, other_family_id,
			base_version, their_version, attempts, attempted,
			detail, opened_by, opened_at,
			resolved_by, resolved_at, resolution, winning_version
			FROM conflicts WHERE status = ? ORDER BY id DESC`, status)
	default:
		return nil, fmt.Errorf("%w: status must be '', 'open', or 'resolved'", ErrInvalid)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ConflictMeta{}
	for rows.Next() {
		c, err := scanConflictMeta(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// GetConflict returns the full Conflict for one id. Unknown id →
// ErrNotFound. attempted column non-empty → unmarshal into
// Attempted; an unmarshal failure surfaces as a plain (non-typed)
// error, so the API layer maps it to 500 internal — it means we
// wrote a bad row, never silently swallowed.
func (s *Store) GetConflict(id int64) (Conflict, error) {
	var (
		c         Conflict
		attempted string
	)
	err := s.db.QueryRow(`SELECT id, kind, status,
		collection, family_id, other_collection, other_family_id,
		base_version, their_version, attempts, attempted,
		detail, opened_by, opened_at,
		resolved_by, resolved_at, resolution, winning_version
		FROM conflicts WHERE id = ?`, id).
		Scan(
			&c.ID, &c.Kind, &c.Status,
			&c.Collection, &c.FamilyID,
			&c.OtherCollection, &c.OtherFamilyID,
			&c.BaseVersion, &c.TheirVersion,
			&c.Attempts, &attempted,
			&c.Detail,
			&c.OpenedBy, &c.OpenedAt,
			&c.ResolvedBy, &c.ResolvedAt,
			&c.Resolution, &c.WinningVersion,
		)
	if errors.Is(err, sql.ErrNoRows) {
		return Conflict{}, ErrNotFound
	}
	if err != nil {
		return Conflict{}, err
	}
	if attempted != "" {
		var ds DocSave
		if err := json.Unmarshal([]byte(attempted), &ds); err != nil {
			return Conflict{}, fmt.Errorf("conflict %d attempted column: %w", id, err)
		}
		c.Attempted = &ds
	}
	return c, nil
}

// FlagClaimConflict opens a manual claim-conflict record. The (other_*)
// pair is optional: both empty is the "single-doc claim" form. When
// present the two pairs are canonicalized by lex order so A→B and
// B→A dedup to the same primary family. A second open claim against
// the canonicalized primary is ErrConflict; in that case the operator
// should fold the second other-home into the existing row's detail.
//
// Returns the created record.
func (s *Store) FlagClaimConflict(collection, family, otherCollection, otherFamily, detail, openedBy string) (Conflict, error) {
	if err := validateIdent("collection", collection); err != nil {
		return Conflict{}, err
	}
	if err := validateIdent("family", family); err != nil {
		return Conflict{}, err
	}
	trimmedDetail := strings.TrimSpace(detail)
	if trimmedDetail == "" {
		return Conflict{}, fmt.Errorf("%w: detail is required", ErrInvalid)
	}
	// The other pair is optional but must be all-or-nothing: half-set
	// (one field non-empty, the other empty) is an obvious client
	// bug, so reject up front.
	hasOther := otherCollection != "" || otherFamily != ""
	if hasOther {
		if otherCollection == "" || otherFamily == "" {
			return Conflict{}, fmt.Errorf("%w: other pair must be fully set or fully empty", ErrInvalid)
		}
		if err := validateIdent("other_collection", otherCollection); err != nil {
			return Conflict{}, err
		}
		if err := validateIdent("other_family", otherFamily); err != nil {
			return Conflict{}, err
		}
		// No self-claims: the other pair must reference a different doc.
		if otherCollection == collection && otherFamily == family {
			return Conflict{}, fmt.Errorf("%w: claim conflict must reference two distinct docs", ErrInvalid)
		}
		// Canonicalize: the (post-swap) primary is the lex-smaller
		// pair. After canonicalization, A→B and B→A both write to
		// the same primary (A).
		if otherCollection < collection ||
			(otherCollection == collection && otherFamily < family) {
			collection, otherCollection = otherCollection, collection
			family, otherFamily = otherFamily, family
		}
	}

	tx, err := s.db.Begin()
	if err != nil {
		return Conflict{}, err
	}
	defer tx.Rollback()

	// Snapshot versions at flag time (absent families → 0).
	var baseVersion, theirVersion int
	if err := tx.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM doc_versions
		WHERE collection = ? AND family_id = ?`, collection, family).Scan(&baseVersion); err != nil {
		return Conflict{}, err
	}
	if hasOther {
		if err := tx.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM doc_versions
			WHERE collection = ? AND family_id = ?`, otherCollection, otherFamily).Scan(&theirVersion); err != nil {
			return Conflict{}, err
		}
	}

	var existingID int64
	err = tx.QueryRow(`SELECT id FROM conflicts
		WHERE kind = 'claim' AND status = 'open' AND collection = ? AND family_id = ?`,
		collection, family).Scan(&existingID)
	if err == nil {
		// Duplicate open claim. The operator should fold additional
		// other-homes into the existing row's detail; the API layer
		// surfaces this as 409 so the UI can guide the operator.
		return Conflict{}, fmt.Errorf("%w: open claim already exists for (%s/%s)", ErrConflict, collection, family)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Conflict{}, err
	}

	res, err := tx.Exec(`INSERT INTO conflicts
		(kind, status, collection, family_id, other_collection, other_family_id,
		 base_version, their_version, attempts, attempted, detail, opened_by, opened_at)
		VALUES ('claim', 'open', ?, ?, ?, ?, ?, ?, 1, '', ?, ?, ?)`,
		collection, family,
		otherCollection, otherFamily,
		baseVersion, theirVersion,
		trimmedDetail, openedBy, now())
	if err != nil {
		return Conflict{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Conflict{}, err
	}
	if err := tx.Commit(); err != nil {
		return Conflict{}, err
	}
	return s.GetConflict(id)
}

// ResolveConflict resolves a conflict record. One transaction.
//
//   - Load the row; unknown id → ErrNotFound; status == 'resolved'
//     → ErrConflict "already resolved".
//   - resolution := strings.TrimSpace(resolution); empty after trim →
//     ErrInvalid. A padded resolution is stored trimmed.
//   - expectedAttempts < 0 skips the precondition (internal paths
//     only — the API rejects < 1). expectedAttempts >= 0 must equal
//     the row's attempts, else ErrConflict "conflict changed —
//     reload" (nothing written). attempts is CHECK >= 1 so a passed
//     0 can never match — fail-closed.
//   - save != nil: legal only for kind='edit' (else ErrInvalid "save
//     applies to edit conflicts only"). Run validateDocSave +
//     saveDocTx(tx, c, f, *save) in this tx. A stale base inside
//     resolve returns plain ErrConflict — the conflict row is
//     untouched, the tx is rolled back, NOTHING is recorded.
//     winning_version = the new doc version.
//   - save == nil: winning_version = current MAX(version) of the
//     (collection, family) (0 when the family has no versions). For
//     kind='policy' a manual resolve always writes winning_version=0
//     (only a clearing cut knows a release version).
//   - Set resolved_by, resolved_at=now(), resolution,
//     winning_version, status='resolved'. Return the updated full
//     record.
func (s *Store) ResolveConflict(id int64, resolution string, expectedAttempts int, save *DocSave, resolvedBy string) (Conflict, error) {
	resolution = strings.TrimSpace(resolution)
	if resolution == "" {
		return Conflict{}, fmt.Errorf("%w: resolution is required", ErrInvalid)
	}

	tx, err := s.db.Begin()
	if err != nil {
		return Conflict{}, err
	}
	defer tx.Rollback()

	var (
		c         ConflictMeta
		attempted string
	)
	err = tx.QueryRow(`SELECT id, kind, status,
		collection, family_id, other_collection, other_family_id,
		base_version, their_version, attempts, attempted,
		detail, opened_by, opened_at,
		resolved_by, resolved_at, resolution, winning_version
		FROM conflicts WHERE id = ?`, id).
		Scan(
			&c.ID, &c.Kind, &c.Status,
			&c.Collection, &c.FamilyID,
			&c.OtherCollection, &c.OtherFamilyID,
			&c.BaseVersion, &c.TheirVersion,
			&c.Attempts, &attempted,
			&c.Detail,
			&c.OpenedBy, &c.OpenedAt,
			&c.ResolvedBy, &c.ResolvedAt,
			&c.Resolution, &c.WinningVersion,
		)
	if errors.Is(err, sql.ErrNoRows) {
		return Conflict{}, ErrNotFound
	}
	if err != nil {
		return Conflict{}, err
	}
	if c.Status == "resolved" {
		return Conflict{}, fmt.Errorf("%w: already resolved", ErrConflict)
	}
	if expectedAttempts >= 0 && expectedAttempts != c.Attempts {
		return Conflict{}, fmt.Errorf("%w: conflict changed — reload", ErrConflict)
	}

	winning := 0
	switch {
	case save != nil:
		if c.Kind != "edit" {
			return Conflict{}, fmt.Errorf("%w: save applies to edit conflicts only", ErrInvalid)
		}
		if err := validateDocSave(*save); err != nil {
			return Conflict{}, err
		}
		v, err := s.saveDocTx(tx, c.Collection, c.FamilyID, *save)
		if err != nil {
			// saveDocTx returns plain ErrConflict on stale base;
			// the brief says the tx is rolled back, the conflict
			// row is untouched, nothing recorded. The deferred
			// tx.Rollback() handles the cleanup.
			return Conflict{}, err
		}
		winning = v.Version
	default:
		if c.Kind != "policy" {
			if err := tx.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM doc_versions
				WHERE collection = ? AND family_id = ?`, c.Collection, c.FamilyID).Scan(&winning); err != nil {
				return Conflict{}, err
			}
		}
		// Policy manual resolve: winning stays 0. Only a clearing
		// cut knows the release version of the family.
	}

	if _, err := tx.Exec(`UPDATE conflicts
		SET status = 'resolved', resolved_by = ?, resolved_at = ?,
		    resolution = ?, winning_version = ?
		WHERE id = ? AND status = 'open'`,
		resolvedBy, now(), resolution, winning, id); err != nil {
		return Conflict{}, err
	}
	if err := tx.Commit(); err != nil {
		return Conflict{}, err
	}
	return s.GetConflict(id)
}
