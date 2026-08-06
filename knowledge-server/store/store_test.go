package store

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestSaveDocVersionsAndSupersession(t *testing.T) {
	s := open(t)
	v1, err := s.SaveDoc("docs", "runbook", DocSave{Status: "active", Body: "v1"})
	if err != nil {
		t.Fatal(err)
	}
	if v1.Version != 1 {
		t.Fatalf("want version 1, got %d", v1.Version)
	}
	v2, err := s.SaveDoc("docs", "runbook", DocSave{Status: "active", Body: "v2"})
	if err != nil {
		t.Fatal(err)
	}
	if v2.Version != 2 {
		t.Fatalf("want version 2, got %d", v2.Version)
	}

	var active int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM doc_versions
		WHERE family_id = 'runbook' AND status = 'active'`).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active != 1 {
		t.Fatalf("want exactly one active version, got %d", active)
	}
	var status string
	if err := s.db.QueryRow(`SELECT status FROM doc_versions
		WHERE family_id = 'runbook' AND version = 1`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "superseded" {
		t.Fatalf("v1 should be superseded, got %q", status)
	}
}

func TestSaveDocRejectsInvalid(t *testing.T) {
	s := open(t)
	if _, err := s.SaveDoc("docs", "x", DocSave{Status: "shipped", Body: "b"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad status: want ErrInvalid, got %v", err)
	}
	if _, err := s.SaveDoc("nope", "x", DocSave{Status: "draft", Body: "b"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown collection: want ErrInvalid, got %v", err)
	}
}

// TestSaveDocValidateIdent (F1) — SaveDoc rejects path-unsafe idents in
// either collection or family, and still accepts a normal name.
func TestSaveDocValidateIdent(t *testing.T) {
	s := open(t)
	cases := []struct {
		name       string
		collection string
		family     string
	}{
		{"family with slash", "docs", "with/slash"},
		{"family bare dotdot", "docs", ".."},
		{"family dotdot substring", "docs", "a..b"},
		{"family leading dot", "docs", ".hidden"},
		{"family backslash", "docs", `back\slash`},
		{"family NUL byte", "docs", "nul\x00byte"},
		{"family control byte", "docs", "ctrl\x01byte"},
		{"family non-ASCII", "docs", "café"},
		{"family invalid UTF-8", "docs", "bad\xffbyte"},
		{"collection with slash", "doc/s", "ok"},
		{"collection backslash", `doc\s`, "ok"},
		{"collection leading dot", ".bad", "ok"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.SaveDoc(tc.collection, tc.family, DocSave{Status: "draft", Body: "b"})
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("want ErrInvalid, got %v", err)
			}
		})
	}

	if _, err := s.SaveDoc("docs", "normal-name_1", DocSave{Status: "draft", Body: "ok"}); err != nil {
		t.Fatalf("normal name must save: %v", err)
	}
}

// TestSaveDocConcurrency (F2) — with WAL + a single in-process
// connection, concurrent SaveDoc calls must all succeed and persist
// without SQLITE_BUSY or lost writes.
func TestSaveDocConcurrency(t *testing.T) {
	s := open(t)
	const N = 8
	var wg sync.WaitGroup
	errs := make([]error, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := s.SaveDoc("docs", "race", DocSave{Status: "active", Body: "w"})
			errs[i] = err
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: unexpected error: %v", i, err)
		}
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM doc_versions
		WHERE family_id = 'race'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != N {
		t.Fatalf("want %d rows for family 'race', got %d", N, n)
	}
}

// TestSaveDocOptimisticLock (F2) — BaseVersion is the optimistic-lock
// base. A stale base returns ErrConflict; the current base succeeds; a
// nil base opts out (last-writer-wins).
func TestSaveDocOptimisticLock(t *testing.T) {
	s := open(t)

	v1, err := s.SaveDoc("docs", "opt", DocSave{Status: "active", Body: "v1"})
	if err != nil {
		t.Fatal(err)
	}
	if v1.Version != 1 {
		t.Fatalf("want v1=1, got %d", v1.Version)
	}

	stale := 0
	if _, err := s.SaveDoc("docs", "opt", DocSave{Status: "active", Body: "stale", BaseVersion: &stale}); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale base: want ErrConflict, got %v", err)
	}

	correct := 1
	v2, err := s.SaveDoc("docs", "opt", DocSave{Status: "active", Body: "v2", BaseVersion: &correct})
	if err != nil {
		t.Fatalf("correct base: %v", err)
	}
	if v2.Version != 2 {
		t.Fatalf("want v2=2, got %d", v2.Version)
	}

	if _, err := s.SaveDoc("docs", "opt", DocSave{Status: "active", Body: "v3"}); err != nil {
		t.Fatalf("nil BaseVersion must opt out: %v", err)
	}
}

func TestCutReleaseExcludesDraftsAndIsDeterministic(t *testing.T) {
	s := open(t)
	if _, err := s.SaveDoc("kernel", "kernel", DocSave{Status: "active", Body: "rules"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveDoc("docs", "runbook", DocSave{Status: "active", Body: "steps"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveDoc("docs", "wip", DocSave{Status: "draft", Body: "unfinished"}); err != nil {
		t.Fatal(err)
	}

	m1, err := s.CutRelease("")
	if err != nil {
		t.Fatal(err)
	}
	paths := make([]string, len(m1.Docs))
	for i, d := range m1.Docs {
		paths[i] = d.Path
	}
	want := []string{"docs/runbook.md", "kernel/kernel.md"}
	if strings.Join(paths, ",") != strings.Join(want, ",") {
		t.Fatalf("want paths %v, got %v", want, paths)
	}

	m2, err := s.CutRelease("")
	if err != nil {
		t.Fatal(err)
	}
	if m1.ContentHash != m2.ContentHash {
		t.Fatalf("same content must hash identically: %s vs %s", m1.ContentHash, m2.ContentHash)
	}
	if m2.ReleaseID <= m1.ReleaseID {
		t.Fatalf("release ids must increase: %d then %d", m1.ReleaseID, m2.ReleaseID)
	}
}

func TestCutReleaseLints(t *testing.T) {
	s := open(t)
	if _, err := s.CutRelease(""); !errors.Is(err, ErrLint) {
		t.Fatalf("empty release: want ErrLint, got %v", err)
	}
	big := strings.Repeat("word ", KernelWordCap+1)
	if _, err := s.SaveDoc("kernel", "kernel", DocSave{Status: "active", Body: big}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CutRelease(""); !errors.Is(err, ErrLint) {
		t.Fatalf("over-cap kernel: want ErrLint, got %v", err)
	}
}

// TestCutReleaseKernelByteCap (F7) — CutRelease must reject a kernel
// doc whose byte length exceeds KernelByteCap, even when strings.Fields
// reports only a single "word". The token/word cap is not the only
// backstop.
func TestCutReleaseKernelByteCap(t *testing.T) {
	t.Run("zero-width-space wide body", func(t *testing.T) {
		s := open(t)
		// 2001 tokens of 10 chars joined by U+200B (zero-width space).
		// strings.Fields treats U+200B as a non-separator, so the
		// result is one "word" — exercising only the byte-cap path.
		token := strings.Repeat("a", 10)
		tokens := make([]string, 2001)
		for i := range tokens {
			tokens[i] = token
		}
		body := strings.Join(tokens, "\u200B")
		if got := len(body); got <= KernelByteCap {
			t.Fatalf("test setup: body len %d must exceed %d", got, KernelByteCap)
		}
		if got := len(strings.Fields(body)); got != 1 {
			t.Fatalf("test setup: want 1 field (U+200B is not whitespace), got %d", got)
		}
		if _, err := s.SaveDoc("kernel", "wide", DocSave{Status: "active", Body: body}); err != nil {
			t.Fatalf("SaveDoc accepts the oversized body: %v", err)
		}
		if _, err := s.CutRelease(""); !errors.Is(err, ErrLint) {
			t.Fatalf("want ErrLint, got %v", err)
		}
	})

	t.Run("plain oversized body", func(t *testing.T) {
		s := open(t)
		if _, err := s.SaveDoc("kernel", "plain", DocSave{Status: "active", Body: strings.Repeat("x", KernelByteCap+1)}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.CutRelease(""); !errors.Is(err, ErrLint) {
			t.Fatalf("want ErrLint, got %v", err)
		}
	})

	t.Run("small kernel cuts", func(t *testing.T) {
		s := open(t)
		if _, err := s.SaveDoc("kernel", "small", DocSave{Status: "active", Body: "small"}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.CutRelease(""); err != nil {
			t.Fatalf("small kernel must cut: %v", err)
		}
	})
}

func TestCurrentReleaseAndArchive(t *testing.T) {
	s := open(t)
	if _, err := s.CurrentRelease(); !errors.Is(err, ErrNotFound) {
		t.Fatal("no releases yet: want ErrNotFound")
	}
	if _, err := s.SaveDoc("kernel", "kernel", DocSave{Status: "active", Body: "rules"}); err != nil {
		t.Fatal(err)
	}
	cut, err := s.CutRelease("")
	if err != nil {
		t.Fatal(err)
	}
	cur, err := s.CurrentRelease()
	if err != nil {
		t.Fatal(err)
	}
	if cur.ReleaseID != cut.ReleaseID || cur.ContentHash != cut.ContentHash {
		t.Fatalf("current mismatch: %+v vs %+v", cur, cut)
	}

	var buf bytes.Buffer
	if err := s.WriteArchive(cut.ReleaseID, &buf); err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(&buf)
	hdr, err := tr.Next()
	if err != nil {
		t.Fatal(err)
	}
	if hdr.Name != "kernel/kernel.md" {
		t.Fatalf("want kernel/kernel.md, got %s", hdr.Name)
	}
	body, err := io.ReadAll(tr)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "rules" {
		t.Fatalf("want body %q, got %q", "rules", string(body))
	}
	if _, err := tr.Next(); err != io.EOF {
		t.Fatal("archive must contain exactly the manifest docs")
	}
}

func TestHeartbeatUpsert(t *testing.T) {
	s := open(t)
	if err := s.Heartbeat("host-a", 1, true, "", false); err != nil {
		t.Fatal(err)
	}
	if err := s.Heartbeat("host-a", 2, false, "hash mismatch", false); err != nil {
		t.Fatal(err)
	}
	var rel int64
	var errMsg string
	if err := s.db.QueryRow(`SELECT release_id, error FROM heartbeats WHERE host = 'host-a'`).
		Scan(&rel, &errMsg); err != nil {
		t.Fatal(err)
	}
	if rel != 2 || errMsg != "hash mismatch" {
		t.Fatalf("upsert failed: release %d error %q", rel, errMsg)
	}
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM heartbeats`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("one row per host: got %d", count)
	}
	if err := s.Heartbeat("", 1, true, "", false); !errors.Is(err, ErrInvalid) {
		t.Fatal("empty host must be invalid")
	}
}

// TestResyncFlow (F5) — RequestResync sets a per-host pull-flag; only a
// Heartbeat with ok==true AND resyncApplied==true clears it.
func TestResyncFlow(t *testing.T) {
	s := open(t)

	// Unknown host is not pending.
	if pending, err := s.ResyncPending("never-seen"); err != nil || pending {
		t.Fatalf("unknown host: pending=%v err=%v", pending, err)
	}

	// RequestResync flips the flag.
	if err := s.RequestResync("h1"); err != nil {
		t.Fatal(err)
	}
	if pending, err := s.ResyncPending("h1"); err != nil || !pending {
		t.Fatalf("after RequestResync: pending=%v err=%v", pending, err)
	}

	// RequestResync works for a host that never heartbeated.
	if err := s.RequestResync("h2"); err != nil {
		t.Fatal(err)
	}
	if pending, err := s.ResyncPending("h2"); err != nil || !pending {
		t.Fatalf("h2 pre-heartbeat: pending=%v err=%v", pending, err)
	}

	// ok=true && resyncApplied=true → cleared.
	if err := s.Heartbeat("h1", 1, true, "", true); err != nil {
		t.Fatal(err)
	}
	if pending, err := s.ResyncPending("h1"); err != nil || pending {
		t.Fatalf("after cleared heartbeat: pending=%v err=%v", pending, err)
	}

	// Re-request, then ok=true && resyncApplied=false → still pending.
	if err := s.RequestResync("h1"); err != nil {
		t.Fatal(err)
	}
	if err := s.Heartbeat("h1", 1, true, "", false); err != nil {
		t.Fatal(err)
	}
	if pending, err := s.ResyncPending("h1"); err != nil || !pending {
		t.Fatalf("resyncApplied=false must not clear: pending=%v err=%v", pending, err)
	}

	// ok=false must not clear either.
	if err := s.Heartbeat("h1", 1, false, "boom", true); err != nil {
		t.Fatal(err)
	}
	if pending, err := s.ResyncPending("h1"); err != nil || !pending {
		t.Fatalf("ok=false must not clear: pending=%v err=%v", pending, err)
	}
}

// TestIssueHostTokenShape — the returned plaintext is a 64-char hex
// string (256 bits of entropy) and is NOT equal to the stored digest.
func TestIssueHostTokenShape(t *testing.T) {
	s := open(t)
	tok, err := s.IssueHostToken("h1")
	if err != nil {
		t.Fatal(err)
	}
	if len(tok) != 64 {
		t.Fatalf("token len = %d, want 64", len(tok))
	}
	if _, err := hex.DecodeString(tok); err != nil {
		t.Fatalf("token is not hex: %v", err)
	}
	// The stored row is the SHA-256 digest of the plaintext, not the
	// plaintext itself — verify against an independently computed hash.
	digest := sha256Hex(tok)
	var stored string
	if err := s.db.QueryRow(`SELECT token_hash FROM host_tokens WHERE host = 'h1'`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != digest {
		t.Fatalf("stored hash = %q, want SHA-256 of plaintext %q", stored, digest)
	}
	if tok == digest {
		t.Fatalf("plaintext must not equal its digest")
	}
}

// TestHostForToken resolves the bound host and treats unknown tokens
// as found=false, err=nil (callers map that to 401, not 500).
func TestHostForToken(t *testing.T) {
	s := open(t)
	tok, err := s.IssueHostToken("h1")
	if err != nil {
		t.Fatal(err)
	}
	host, ok, err := s.HostForToken(tok)
	if err != nil || !ok || host != "h1" {
		t.Fatalf("resolve issued: host=%q ok=%v err=%v", host, ok, err)
	}
	host, ok, err = s.HostForToken("not-a-real-token")
	if err != nil || ok || host != "" {
		t.Fatalf("unknown token: host=%q ok=%v err=%v, want ok=false err=nil", host, ok, err)
	}
}

// TestIssueHostTokenRotation — reissuing for a host replaces the
// stored digest; the old plaintext stops verifying, the new one works.
func TestIssueHostTokenRotation(t *testing.T) {
	s := open(t)
	old, err := s.IssueHostToken("h1")
	if err != nil {
		t.Fatal(err)
	}
	newTok, err := s.IssueHostToken("h1")
	if err != nil {
		t.Fatal(err)
	}
	if old == newTok {
		t.Fatalf("reissue must yield a fresh token")
	}
	if host, ok, err := s.HostForToken(old); err != nil || ok {
		t.Fatalf("old token must stop resolving: host=%q ok=%v err=%v", host, ok, err)
	}
	host, ok, err := s.HostForToken(newTok)
	if err != nil || !ok || host != "h1" {
		t.Fatalf("new token must resolve h1: host=%q ok=%v err=%v", host, ok, err)
	}
}

// TestRevokeHostToken — deleting the row makes the bearer stop
// verifying; revoking an absent host is ErrNotFound (errors.Is).
func TestRevokeHostToken(t *testing.T) {
	s := open(t)
	tok, err := s.IssueHostToken("h1")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeHostToken("h1"); err != nil {
		t.Fatal(err)
	}
	if host, ok, err := s.HostForToken(tok); err != nil || ok {
		t.Fatalf("after revoke: host=%q ok=%v err=%v, want ok=false", host, ok, err)
	}
	if err := s.RevokeHostToken("h1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoke absent: want ErrNotFound, got %v", err)
	}
	if err := s.RevokeHostToken("never-issued"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoke never-issued: want ErrNotFound, got %v", err)
	}
}

// TestIssueHostTokenInvalidHost — the path-safe ident guard from
// validateIdent applies to host tokens too: empty and any '..' shape
// is rejected up front, before any token is generated or stored.
func TestIssueHostTokenInvalidHost(t *testing.T) {
	s := open(t)
	if _, err := s.IssueHostToken(""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty host: want ErrInvalid, got %v", err)
	}
	if _, err := s.IssueHostToken("../evil"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("'..' host: want ErrInvalid, got %v", err)
	}
	if err := s.RevokeHostToken("../evil"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("revoke '..' host: want ErrInvalid, got %v", err)
	}
}

// TestSaveDocRejectsCommaContainingTrigger — trigger grammar is closed
// at the write door: empty values and any value containing ',' are
// rejected with ErrInvalid so the comma-joined stored form cannot
// round-trip as the wrong number of triggers. A round-trip of
// ["deploy","rollback"] succeeds.
func TestSaveDocRejectsCommaContainingTrigger(t *testing.T) {
	s := open(t)
	if _, err := s.SaveDoc("docs", "x", DocSave{Status: "draft", Body: "b", Triggers: []string{"deploy,rollback"}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("comma in trigger: want ErrInvalid, got %v", err)
	}
	if _, err := s.SaveDoc("docs", "x", DocSave{Status: "draft", Body: "b", Triggers: []string{""}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty trigger: want ErrInvalid, got %v", err)
	}
	v, err := s.SaveDoc("docs", "ok", DocSave{Status: "draft", Body: "b", Triggers: []string{"deploy", "rollback"}})
	if err != nil {
		t.Fatalf("two-trigger save: %v", err)
	}
	got, err := s.GetDoc(v.Collection, v.FamilyID, v.Version)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Triggers) != 2 || got.Triggers[0] != "deploy" || got.Triggers[1] != "rollback" {
		t.Fatalf("round-trip triggers: got %#v", got.Triggers)
	}
	// No triggers round-trips as the empty slice.
	v2, err := s.SaveDoc("docs", "empty", DocSave{Status: "draft", Body: "b"})
	if err != nil {
		t.Fatal(err)
	}
	got2, err := s.GetDoc(v2.Collection, v2.FamilyID, v2.Version)
	if err != nil {
		t.Fatal(err)
	}
	if got2.Triggers == nil {
		t.Fatalf("no triggers must round-trip as a non-nil empty slice")
	}
	if len(got2.Triggers) != 0 {
		t.Fatalf("no triggers: len=%d, want 0", len(got2.Triggers))
	}
}

// TestListDocsFieldOracle — construct a family where the max-version
// row differs from older rows in title, status, tier, editor, and
// created_at; assert EVERY returned field is from the max-version
// row. Plus ordering and the empty case (non-nil empty slice).
func TestListDocsFieldOracle(t *testing.T) {
	t.Run("empty store returns non-nil empty slice", func(t *testing.T) {
		s := open(t)
		got, err := s.ListDocs()
		if err != nil {
			t.Fatal(err)
		}
		if got == nil {
			t.Fatalf("empty store: want non-nil empty slice, got nil")
		}
		if len(got) != 0 {
			t.Fatalf("empty store: len=%d, want 0", len(got))
		}
	})
	t.Run("max-version fields win and ordering is (collection, family_id)", func(t *testing.T) {
		s := open(t)
		// family "older": v1 active, then v2 active (demotes v1 to
		// superseded), then v3 active (demotes v2 to superseded). The
		// older rows have their own title/tier/editor/created_at that
		// must NOT bleed into the list view; v3 must win on every
		// field.
		if _, err := s.SaveDoc("docs", "older", DocSave{Status: "active", Title: "v1-title", Tier: "v1-tier", Editor: "v1-editor", Body: "v1"}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.SaveDoc("docs", "older", DocSave{Status: "active", Title: "v2-title", Tier: "v2-tier", Editor: "v2-editor", Body: "v2"}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.SaveDoc("docs", "older", DocSave{Status: "active", Title: "v3-title", Tier: "v3-tier", Editor: "v3-editor", Body: "v3"}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.SaveDoc("kernel", "kernel", DocSave{Status: "active", Title: "k-title", Tier: "A", Editor: "k-editor", Body: "k-body"}); err != nil {
			t.Fatal(err)
		}

		got, err := s.ListDocs()
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 {
			t.Fatalf("want 2 families (docs/older + kernel/kernel), got %d: %#v", len(got), got)
		}
		// ORDER BY collection, family_id → docs/older first.
		if got[0].Collection != "docs" || got[0].FamilyID != "older" {
			t.Fatalf("got[0] = (%s,%s), want (docs,older)", got[0].Collection, got[0].FamilyID)
		}
		if got[1].Collection != "kernel" || got[1].FamilyID != "kernel" {
			t.Fatalf("got[1] = (%s,%s), want (kernel,kernel)", got[1].Collection, got[1].FamilyID)
		}
		// Every field of docs/older must come from the max-version row (v3).
		d := got[0]
		if d.Version != 3 {
			t.Fatalf("docs/older version = %d, want 3", d.Version)
		}
		if d.Title != "v3-title" || d.Tier != "v3-tier" || d.Editor != "v3-editor" {
			t.Fatalf("docs/older fields not from max-version row: %#v", d)
		}
		// v3 was saved as active — the join must NOT report v2's
		// status. Sanity check.
		if d.Status != "active" {
			t.Fatalf("docs/older status = %q, want active", d.Status)
		}
		// ListDocs and DocHistory share the same row source of truth
		// (doc_versions). Asserting field-for-field equality with
		// DocHistory(...)[0] removes the need to time-control
		// created_at: whatever value ListDocs chose, DocHistory must
		// agree on it. This is the join-shape oracle the brief
		// pinned.
		history, err := s.DocHistory("docs", "older")
		if err != nil {
			t.Fatal(err)
		}
		if history[0] != d {
			t.Fatalf("ListDocs row does not match DocHistory[0]: list=%#v history=%#v", d, history[0])
		}
	})
}

// TestDocHistoryNewestFirstAndNotFound — newest-first ordering plus
// ErrNotFound on unknown family. Both idents are validated before the
// query.
func TestDocHistoryNewestFirstAndNotFound(t *testing.T) {
	s := open(t)
	for i := 1; i <= 3; i++ {
		if _, err := s.SaveDoc("docs", "h", DocSave{Status: "draft", Body: "v"}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.DocHistory("docs", "h")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	for i := 0; i < len(got)-1; i++ {
		if got[i].Version <= got[i+1].Version {
			t.Fatalf("not newest-first: %v then %v", got[i].Version, got[i+1].Version)
		}
	}
	if _, err := s.DocHistory("docs", "absent"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("absent family: want ErrNotFound, got %v", err)
	}
	if _, err := s.DocHistory("..", "h"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad collection ident: want ErrInvalid, got %v", err)
	}
	if _, err := s.DocHistory("docs", "with/slash"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad family ident: want ErrInvalid, got %v", err)
	}
}

// TestGetDoc — latest vs explicit version, triggers round-trip
// including the empty case (non-nil empty slice, never []string{""}),
// ErrNotFound on unknown family/version, ident rejection on entry.
func TestGetDoc(t *testing.T) {
	s := open(t)
	if _, err := s.SaveDoc("docs", "g", DocSave{Status: "draft", Body: "v1", Triggers: nil}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveDoc("docs", "g", DocSave{Status: "active", Body: "v2", Triggers: []string{"a", "b"}}); err != nil {
		t.Fatal(err)
	}
	// Latest (version <= 0) → v2 with both triggers.
	got, err := s.GetDoc("docs", "g", 0)
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != 2 || got.Body != "v2" || len(got.Triggers) != 2 || got.Triggers[0] != "a" || got.Triggers[1] != "b" {
		t.Fatalf("latest: %#v", got)
	}
	// Explicit version → v1 with no triggers (empty stored form, non-nil slice).
	v1, err := s.GetDoc("docs", "g", 1)
	if err != nil {
		t.Fatal(err)
	}
	if v1.Version != 1 || v1.Body != "v1" {
		t.Fatalf("v1: %#v", v1)
	}
	if v1.Triggers == nil {
		t.Fatalf("empty triggers must round-trip as non-nil empty slice, got nil")
	}
	if len(v1.Triggers) != 0 {
		t.Fatalf("v1 triggers len=%d, want 0", len(v1.Triggers))
	}
	// Unknown family → ErrNotFound.
	if _, err := s.GetDoc("docs", "absent", 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("absent: want ErrNotFound, got %v", err)
	}
	// Unknown version → ErrNotFound.
	if _, err := s.GetDoc("docs", "g", 99); !errors.Is(err, ErrNotFound) {
		t.Fatalf("absent version: want ErrNotFound, got %v", err)
	}
	// Ident rejection.
	if _, err := s.GetDoc("..", "g", 0); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad collection ident: want ErrInvalid, got %v", err)
	}
	if _, err := s.GetDoc("docs", "with/slash", 0); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad family ident: want ErrInvalid, got %v", err)
	}
}

// TestPreviewReleaseVsDraft — only active rows contribute to the
// release candidate. A newer draft is invisible to PreviewRelease and
// to CutRelease: both ship the active row. The cut manifest's docs
// list must equal the preview docs list path/family_id/version/
// sha256, in order — not just one field.
func TestPreviewReleaseVsDraft(t *testing.T) {
	s := open(t)
	if _, err := s.SaveDoc("docs", "r", DocSave{Status: "active", Body: "v1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveDoc("docs", "r", DocSave{Status: "draft", Body: "v2"}); err != nil {
		t.Fatal(err)
	}
	preview, err := s.PreviewRelease()
	if err != nil {
		t.Fatal(err)
	}
	if preview.ReleaseID != 0 {
		t.Fatalf("preview release_id = %d, want 0", preview.ReleaseID)
	}
	if len(preview.Docs) != 1 || preview.Docs[0].FamilyID != "r" || preview.Docs[0].Version != 1 {
		t.Fatalf("preview docs: %#v", preview.Docs)
	}
	cut, err := s.CutRelease("")
	if err != nil {
		t.Fatal(err)
	}
	if len(cut.Docs) != 1 || cut.Docs[0].Version != 1 || cut.ContentHash != preview.ContentHash {
		t.Fatalf("cut must match preview: cut=%#v preview=%#v", cut, preview)
	}
	// Full-doc-list equality: the cut's release_docs rows equal the
	// preview's would-be rows. After the cut, CurrentRelease (the
	// persisted view) must report the same docs in the same order.
	if !docsEqual(preview.Docs, cut.Docs) {
		t.Fatalf("preview docs != cut docs: prev=%#v cut=%#v", preview.Docs, cut.Docs)
	}
	cur, err := s.CurrentRelease()
	if err != nil {
		t.Fatal(err)
	}
	if !docsEqual(cur.Docs, cut.Docs) {
		t.Fatalf("CurrentRelease docs != cut docs: cur=%#v cut=%#v", cur.Docs, cut.Docs)
	}
}

// docsEqual compares two manifest doc lists path/family_id/version/
// sha256, in order.
func docsEqual(a, b []ManifestDoc) bool {
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

// TestCutReleaseStaleExpectedHash — when expectedHash does not match
// the would-be content_hash, CutRelease returns ErrConflict and
// inserts NO release row.
func TestCutReleaseStaleExpectedHash(t *testing.T) {
	s := open(t)
	if _, err := s.SaveDoc("kernel", "kernel", DocSave{Status: "active", Body: "v1"}); err != nil {
		t.Fatal(err)
	}
	preview, err := s.PreviewRelease()
	if err != nil {
		t.Fatal(err)
	}
	// Change the active doc so the candidate hash diverges.
	if _, err := s.SaveDoc("kernel", "kernel", DocSave{Status: "active", Body: "v2"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CutRelease(preview.ContentHash); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale hash: want ErrConflict, got %v", err)
	}
	// No release row was inserted.
	if _, err := s.CurrentRelease(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stale hash must not insert: CurrentRelease err=%v", err)
	}
	// Matching hash on a re-cut succeeds; release rows equal the
	// preview manifest by construction.
	cur, err := s.PreviewRelease()
	if err != nil {
		t.Fatal(err)
	}
	cut, err := s.CutRelease(cur.ContentHash)
	if err != nil {
		t.Fatal(err)
	}
	if cut.ContentHash != cur.ContentHash || cut.ReleaseID == 0 {
		t.Fatalf("matching cut: cut=%#v cur=%#v", cut, cur)
	}
}

// sha256Hex is a tiny test helper so the shape test can express the
// "stored digest is SHA-256 of the plaintext" invariant without
// inlining the import in the test body.
func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
