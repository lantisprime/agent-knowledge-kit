package store

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"reflect"
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

	m1, err := s.CutRelease("", "tester")
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

	m2, err := s.CutRelease("", "tester")
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
	if _, err := s.CutRelease("", "tester"); !errors.Is(err, ErrLint) {
		t.Fatalf("empty release: want ErrLint, got %v", err)
	}
	big := strings.Repeat("word ", KernelWordCap+1)
	if _, err := s.SaveDoc("kernel", "kernel", DocSave{Status: "active", Body: big}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CutRelease("", "tester"); !errors.Is(err, ErrLint) {
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
		if _, err := s.CutRelease("", "tester"); !errors.Is(err, ErrLint) {
			t.Fatalf("want ErrLint, got %v", err)
		}
	})

	t.Run("plain oversized body", func(t *testing.T) {
		s := open(t)
		if _, err := s.SaveDoc("kernel", "plain", DocSave{Status: "active", Body: strings.Repeat("x", KernelByteCap+1)}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.CutRelease("", "tester"); !errors.Is(err, ErrLint) {
			t.Fatalf("want ErrLint, got %v", err)
		}
	})

	t.Run("small kernel cuts", func(t *testing.T) {
		s := open(t)
		if _, err := s.SaveDoc("kernel", "small", DocSave{Status: "active", Body: "small"}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.CutRelease("", "tester"); err != nil {
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
	cut, err := s.CutRelease("", "tester")
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
	cut, err := s.CutRelease("", "tester")
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
	if _, err := s.CutRelease(preview.ContentHash, "tester"); !errors.Is(err, ErrConflict) {
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
	cut, err := s.CutRelease(cur.ContentHash, "tester")
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

// --- conflict records: build-order step 5 ------------------------------

// TestConflictsSchemaIdempotent — opening, closing, and reopening the
// SAME database file must leave the conflicts table usable: rows
// written before the reopen survive, and the schema IF NOT EXISTS is
// a no-op on the second Open.
func TestConflictsSchemaIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	// Seed an open edit conflict so the reopen assertion exercises
	// the actual row, not just the empty table.
	stale := 0
	if _, err := s.SaveDoc("docs", "x", DocSave{Status: "draft", Body: "v1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveDoc("docs", "x", DocSave{Status: "active", Body: "v2"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveDoc("docs", "x", DocSave{Status: "active", Body: "stale", BaseVersion: &stale}); err == nil {
		t.Fatal("want stale-base ErrConflict, got nil")
	}
	conflicts, err := s.ListConflicts("")
	if err != nil {
		t.Fatal(err)
	}
	if len(conflicts) != 1 {
		t.Fatalf("want 1 conflict, got %d", len(conflicts))
	}
	preID := conflicts[0].ID
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { s2.Close() })

	conflicts2, err := s2.ListConflicts("")
	if err != nil {
		t.Fatalf("reopen list: %v", err)
	}
	if len(conflicts2) != 1 || conflicts2[0].ID != preID {
		t.Fatalf("conflict must survive reopen: got %+v, want id=%d", conflicts2, preID)
	}
}

// TestStaleSaveOpensConflictRecord — a stale-base save commits an
// open edit conflict whose Attempted column round-trips the rejected
// DocSave (Editor field NOT persisted: opened_by carries identity).
func TestStaleSaveOpensConflictRecord(t *testing.T) {
	s := open(t)
	if _, err := s.SaveDoc("docs", "f", DocSave{Status: "active", Body: "v1", Editor: "alice"}); err != nil {
		t.Fatal(err)
	}
	stale := 0
	rejected := DocSave{
		Title: "attempted", Status: "draft", Tier: "B", Triggers: []string{"a", "b"},
		Owner: "alice", Audience: "ops", Body: "attempted-body",
		Editor: "alice", BaseVersion: &stale,
	}
	_, err := s.SaveDoc("docs", "f", rejected)
	if err == nil {
		t.Fatal("want stale-base error, got nil")
	}
	var cre *ConflictRecordedError
	if !errors.As(err, &cre) {
		t.Fatalf("want *ConflictRecordedError, got %T (%v)", err, err)
	}
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("wrapped error must Unwrap to ErrConflict, got %v", errors.Unwrap(err))
	}
	if cre.ID == 0 {
		t.Fatalf("want non-zero conflict id, got 0")
	}

	got, err := s.GetConflict(cre.ID)
	if err != nil {
		t.Fatalf("get conflict: %v", err)
	}
	if got.Kind != "edit" || got.Status != "open" {
		t.Fatalf("kind/status: got %q/%q, want edit/open", got.Kind, got.Status)
	}
	if got.Collection != "docs" || got.FamilyID != "f" {
		t.Fatalf("family: got %q/%q, want docs/f", got.Collection, got.FamilyID)
	}
	if got.BaseVersion != 0 || got.TheirVersion != 1 {
		t.Fatalf("base/their version: got %d/%d, want 0/1", got.BaseVersion, got.TheirVersion)
	}
	if got.Attempts != 1 {
		t.Fatalf("attempts: got %d, want 1", got.Attempts)
	}
	if got.OpenedBy != "alice" {
		t.Fatalf("opened_by: got %q, want alice", got.OpenedBy)
	}
	if got.OpenedAt == "" {
		t.Fatalf("opened_at must be set")
	}
	if !strings.Contains(got.Detail, "stale base_version") {
		t.Fatalf("detail should mention stale base_version: %q", got.Detail)
	}
	if got.Attempted == nil {
		t.Fatalf("Attempted must round-trip")
	}
	if got.Attempted.Title != "attempted" || got.Attempted.Body != "attempted-body" {
		t.Fatalf("attempted fields: %+v", got.Attempted)
	}
	if len(got.Attempted.Triggers) != 2 || got.Attempted.Triggers[0] != "a" || got.Attempted.Triggers[1] != "b" {
		t.Fatalf("attempted triggers: %#v", got.Attempted.Triggers)
	}
	// Editor must NOT be persisted in the attempted JSON (it carries
	// opened_by identity). The tag json:"-" on DocSave.Editor does
	// the work; this assertion is the test-side pin.
	if got.Attempted.Editor != "" {
		t.Fatalf("Editor must not be persisted in attempted, got %q", got.Attempted.Editor)
	}
}

// TestStaleSaveRefreshesSameRow — a second stale save on the same
// family hits the same row id; their_version and attempted refresh;
// attempts increments; exactly one open edit conflict.
func TestStaleSaveRefreshesSameRow(t *testing.T) {
	s := open(t)
	if _, err := s.SaveDoc("docs", "f", DocSave{Status: "active", Body: "v1", Editor: "alice"}); err != nil {
		t.Fatal(err)
	}
	// Advance the family to v2 so the two stale attempts can use two
	// DIFFERENT stale bases (0 then 1): the refresh must overwrite
	// base_version with the latest attempt's base, not keep the first.
	if _, err := s.SaveDoc("docs", "f", DocSave{Status: "active", Body: "v2", Editor: "alice"}); err != nil {
		t.Fatal(err)
	}
	staleA, staleB := 0, 1
	if _, err := s.SaveDoc("docs", "f", DocSave{Status: "draft", Body: "first", Editor: "alice", BaseVersion: &staleA}); err == nil {
		t.Fatal("first stale: want error")
	} else {
		var cre *ConflictRecordedError
		errors.As(err, &cre)
		firstID := cre.ID
		if _, err := s.SaveDoc("docs", "f", DocSave{Status: "draft", Body: "second", Editor: "bob", BaseVersion: &staleB}); err == nil {
			t.Fatal("second stale: want error")
		} else {
			var cre2 *ConflictRecordedError
			errors.As(err, &cre2)
			if cre2.ID != firstID {
				t.Fatalf("second stale must hit same row id: got %d, want %d", cre2.ID, firstID)
			}
		}
		all, err := s.ListConflicts("")
		if err != nil {
			t.Fatal(err)
		}
		if len(all) != 1 {
			t.Fatalf("want exactly one open edit conflict, got %d", len(all))
		}
		if all[0].Attempts != 2 {
			t.Fatalf("attempts: got %d, want 2", all[0].Attempts)
		}
		if all[0].OpenedBy != "bob" {
			t.Fatalf("opened_by should refresh to latest editor, got %q", all[0].OpenedBy)
		}
		got, err := s.GetConflict(firstID)
		if err != nil {
			t.Fatal(err)
		}
		if got.TheirVersion != 2 {
			t.Fatalf("their_version: got %d, want 2 (current max)", got.TheirVersion)
		}
		if got.BaseVersion != 1 {
			t.Fatalf("base_version must refresh to the latest attempt's base: got %d, want 1", got.BaseVersion)
		}
		if got.Attempted == nil || got.Attempted.Body != "second" {
			t.Fatalf("attempted must refresh to latest: %+v", got.Attempted)
		}
	}
}

// TestSuccessfulSaveLeavesOpenConflict — a successful save must NOT
// resolve or remove an existing open edit conflict for the family.
func TestSuccessfulSaveLeavesOpenConflict(t *testing.T) {
	s := open(t)
	if _, err := s.SaveDoc("docs", "f", DocSave{Status: "active", Body: "v1"}); err != nil {
		t.Fatal(err)
	}
	stale := 0
	if _, err := s.SaveDoc("docs", "f", DocSave{Status: "draft", Body: "stale", BaseVersion: &stale}); err == nil {
		t.Fatal("stale: want error")
	}
	// Successful save with nil base_version (last-writer-wins opt-out)
	// must leave the conflict open.
	if _, err := s.SaveDoc("docs", "f", DocSave{Status: "active", Body: "v2"}); err != nil {
		t.Fatalf("successful save: %v", err)
	}
	conflicts, err := s.ListConflicts("open")
	if err != nil {
		t.Fatal(err)
	}
	if len(conflicts) != 1 {
		t.Fatalf("want 1 open conflict after successful save, got %d", len(conflicts))
	}
}

// TestResolveConflictKeepCurrent — winning_version = current MAX
// version; second resolve → ErrConflict.
func TestResolveConflictKeepCurrent(t *testing.T) {
	s := open(t)
	if _, err := s.SaveDoc("docs", "f", DocSave{Status: "active", Body: "v1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveDoc("docs", "f", DocSave{Status: "active", Body: "v2"}); err != nil {
		t.Fatal(err)
	}
	stale := 0
	if _, err := s.SaveDoc("docs", "f", DocSave{Status: "draft", Body: "stale", BaseVersion: &stale}); err == nil {
		t.Fatal("stale: want error")
	}
	conflicts, err := s.ListConflicts("")
	if err != nil {
		t.Fatal(err)
	}
	id := conflicts[0].ID
	resolved, err := s.ResolveConflict(id, "merged by hand", 1, nil, "alice")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolved.Status != "resolved" {
		t.Fatalf("status: got %q, want resolved", resolved.Status)
	}
	if resolved.ResolvedBy != "alice" || resolved.ResolvedAt == "" {
		t.Fatalf("resolved_by/at: %q/%q", resolved.ResolvedBy, resolved.ResolvedAt)
	}
	if resolved.Resolution != "merged by hand" {
		t.Fatalf("resolution: got %q", resolved.Resolution)
	}
	if resolved.WinningVersion != 2 {
		t.Fatalf("winning_version: got %d, want 2 (current max)", resolved.WinningVersion)
	}
	if _, err := s.ResolveConflict(id, "redo", -1, nil, "alice"); !errors.Is(err, ErrConflict) {
		t.Fatalf("second resolve: want ErrConflict, got %v", err)
	}
}

// TestResolveConflictWithSave — atomic doc insert + conflict resolution;
// winning_version = the new version. Stale base inside resolve returns
// plain ErrConflict: tx rolled back, conflict row untouched, no new
// version, no new conflict row.
func TestResolveConflictWithSave(t *testing.T) {
	s := open(t)
	if _, err := s.SaveDoc("docs", "f", DocSave{Status: "active", Body: "v1"}); err != nil {
		t.Fatal(err)
	}
	stale := 0
	if _, err := s.SaveDoc("docs", "f", DocSave{Status: "draft", Body: "stale", BaseVersion: &stale}); err == nil {
		t.Fatal("stale: want error")
	}
	conflicts, err := s.ListConflicts("")
	if err != nil {
		t.Fatal(err)
	}
	id := conflicts[0].ID
	save := DocSave{
		Title: "merged", Status: "active", Body: "merged-body",
		Triggers: []string{"deploy"}, Owner: "alice", Audience: "ops",
		Editor: "alice", BaseVersion: ptrInt(1),
	}
	resolved, err := s.ResolveConflict(id, "merged", 1, &save, "alice")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolved.WinningVersion != 2 {
		t.Fatalf("winning_version: got %d, want 2 (new doc version)", resolved.WinningVersion)
	}
	d, err := s.GetDoc("docs", "f", 2)
	if err != nil {
		t.Fatal(err)
	}
	if d.Body != "merged-body" {
		t.Fatalf("merged body: got %q", d.Body)
	}
	if d.Editor != "alice" {
		t.Fatalf("editor: got %q, want alice", d.Editor)
	}

	// Now the stale-base-inside-resolve path. Save with a wrong base.
	// Open a fresh conflict first.
	if _, err := s.SaveDoc("docs", "g", DocSave{Status: "active", Body: "v1"}); err != nil {
		t.Fatal(err)
	}
	stale2 := 0
	if _, err := s.SaveDoc("docs", "g", DocSave{Status: "draft", Body: "stale", BaseVersion: &stale2}); err == nil {
		t.Fatal("stale: want error")
	}
	conflicts2, _ := s.ListConflicts("")
	var id2 int64
	for _, c := range conflicts2 {
		if c.Collection == "docs" && c.FamilyID == "g" {
			id2 = c.ID
		}
	}
	wrong := DocSave{Status: "active", Body: "wrong", BaseVersion: ptrInt(99)}
	_, err = s.ResolveConflict(id2, "stale-base inside resolve", 1, &wrong, "alice")
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("stale-base inside resolve: want ErrConflict, got %v", err)
	}
	// Conflict still open.
	got, _ := s.GetConflict(id2)
	if got.Status != "open" {
		t.Fatalf("conflict must remain open after stale-base resolve: %+v", got)
	}
	// No new doc version inserted.
	if _, err := s.GetDoc("docs", "g", 2); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stale-base resolve must not insert a new version: %v", err)
	}
	// No new conflict row.
	all, _ := s.ListConflicts("")
	if len(all) != 2 {
		t.Fatalf("want 2 open conflicts, got %d", len(all))
	}
}

// TestResolveConflictExpectedAttemptsMismatch — expectedAttempts = 0
// and a wrong-positive both yield ErrConflict with the row untouched;
// any negative value skips the precondition.
func TestResolveConflictExpectedAttemptsMismatch(t *testing.T) {
	s := open(t)
	if _, err := s.SaveDoc("docs", "f", DocSave{Status: "active", Body: "v1"}); err != nil {
		t.Fatal(err)
	}
	stale := 0
	if _, err := s.SaveDoc("docs", "f", DocSave{Status: "draft", Body: "stale", BaseVersion: &stale}); err == nil {
		t.Fatal("stale: want error")
	}
	var id int64
	for _, c := range mustList(t, s) {
		id = c.ID
	}
	before, err := s.GetConflict(id)
	if err != nil {
		t.Fatal(err)
	}
	// Wrong positive: expected=5, row has 1.
	if _, err := s.ResolveConflict(id, "r", 5, nil, "a"); !errors.Is(err, ErrConflict) {
		t.Fatalf("wrong positive: want ErrConflict, got %v", err)
	}
	// Zero: expected=0, row has 1 → mismatch.
	if _, err := s.ResolveConflict(id, "r", 0, nil, "a"); !errors.Is(err, ErrConflict) {
		t.Fatalf("zero: want ErrConflict, got %v", err)
	}
	// Both rejections must leave the row byte-identical: still open,
	// no resolution fields, attempts unchanged.
	after, err := s.GetConflict(id)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("rejected resolves must not touch the row:\nbefore %+v\nafter  %+v", before, after)
	}
	// Negative: skips → succeeds.
	if _, err := s.ResolveConflict(id, "r", -1, nil, "a"); err != nil {
		t.Fatalf("negative skip: %v", err)
	}
}

func mustList(t *testing.T, s *Store) []ConflictMeta {
	t.Helper()
	out, err := s.ListConflicts("")
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func ptrInt(v int) *int { return &v }

// TestResolveConflictValidationAndEdges — save on claim → ErrInvalid,
// empty/whitespace resolution → ErrInvalid, padded resolution stored
// trimmed, unknown id → ErrNotFound (GetConflict unknown id too),
// keep-current on a family with no versions → winning_version 0, claim
// resolve → winning_version = primary current max, manual policy
// resolve → winning_version 0.
func TestResolveConflictValidationAndEdges(t *testing.T) {
	t.Run("save on claim → ErrInvalid", func(t *testing.T) {
		s := open(t)
		_, err := s.FlagClaimConflict("docs", "f", "", "", "d", "alice")
		if err != nil {
			t.Fatal(err)
		}
		id := mustList(t, s)[0].ID
		save := DocSave{Status: "active", Body: "x"}
		_, err = s.ResolveConflict(id, "r", 1, &save, "a")
		if !errors.Is(err, ErrInvalid) {
			t.Fatalf("want ErrInvalid, got %v", err)
		}
	})
	t.Run("empty resolution → ErrInvalid", func(t *testing.T) {
		s := open(t)
		_, err := s.FlagClaimConflict("docs", "f", "", "", "d", "a")
		if err != nil {
			t.Fatal(err)
		}
		id := mustList(t, s)[0].ID
		if _, err := s.ResolveConflict(id, "", 1, nil, "a"); !errors.Is(err, ErrInvalid) {
			t.Fatalf("want ErrInvalid, got %v", err)
		}
		if _, err := s.ResolveConflict(id, "   \t  ", 1, nil, "a"); !errors.Is(err, ErrInvalid) {
			t.Fatalf("want ErrInvalid, got %v", err)
		}
	})
	t.Run("padded resolution stored trimmed", func(t *testing.T) {
		s := open(t)
		_, err := s.FlagClaimConflict("docs", "f", "", "", "d", "a")
		if err != nil {
			t.Fatal(err)
		}
		id := mustList(t, s)[0].ID
		got, err := s.ResolveConflict(id, "  trimmed  ", 1, nil, "a")
		if err != nil {
			t.Fatal(err)
		}
		if got.Resolution != "trimmed" {
			t.Fatalf("resolution: got %q, want trimmed", got.Resolution)
		}
	})
	t.Run("unknown id → ErrNotFound", func(t *testing.T) {
		s := open(t)
		if _, err := s.GetConflict(99999); !errors.Is(err, ErrNotFound) {
			t.Fatalf("get unknown: %v", err)
		}
		if _, err := s.ResolveConflict(99999, "r", -1, nil, "a"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("resolve unknown: %v", err)
		}
	})
	t.Run("keep-current on family with no versions → winning 0", func(t *testing.T) {
		s := open(t)
		_, err := s.FlagClaimConflict("docs", "ghost", "", "", "d", "a")
		if err != nil {
			t.Fatal(err)
		}
		id := mustList(t, s)[0].ID
		got, err := s.ResolveConflict(id, "r", 1, nil, "a")
		if err != nil {
			t.Fatal(err)
		}
		if got.WinningVersion != 0 {
			t.Fatalf("winning_version: got %d, want 0", got.WinningVersion)
		}
	})
	t.Run("claim resolve → winning_version = primary current max", func(t *testing.T) {
		s := open(t)
		if _, err := s.SaveDoc("docs", "f", DocSave{Status: "active", Body: "v1"}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.SaveDoc("docs", "f", DocSave{Status: "active", Body: "v2"}); err != nil {
			t.Fatal(err)
		}
		_, err := s.FlagClaimConflict("docs", "f", "kernel", "kernel", "d", "a")
		if err != nil {
			t.Fatal(err)
		}
		id := mustList(t, s)[0].ID
		got, err := s.ResolveConflict(id, "r", 1, nil, "a")
		if err != nil {
			t.Fatal(err)
		}
		if got.WinningVersion != 2 {
			t.Fatalf("winning_version: got %d, want 2 (primary current max)", got.WinningVersion)
		}
	})
	t.Run("manual policy resolve → winning_version 0", func(t *testing.T) {
		s := open(t)
		big := strings.Repeat("word ", KernelWordCap+1)
		if _, err := s.SaveDoc("kernel", "kernel", DocSave{Status: "active", Body: big}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.CutRelease("", "alice"); err == nil {
			t.Fatal("lint: want error")
		}
		conflicts := mustList(t, s)
		var policyID int64
		for _, c := range conflicts {
			if c.Kind == "policy" {
				policyID = c.ID
			}
		}
		if policyID == 0 {
			t.Fatal("no policy conflict recorded")
		}
		got, err := s.ResolveConflict(policyID, "manual ack", 1, nil, "a")
		if err != nil {
			t.Fatal(err)
		}
		if got.WinningVersion != 0 {
			t.Fatalf("manual policy resolve: winning_version = %d, want 0", got.WinningVersion)
		}
	})
}

// TestConflictsConstraintEnforcement — DELETE refused, UPDATE of
// resolved row refused, UPDATE attempts = 0 on open row refused (CHECK).
func TestConflictsConstraintEnforcement(t *testing.T) {
	s := open(t)
	_, err := s.FlagClaimConflict("docs", "f", "", "", "d", "a")
	if err != nil {
		t.Fatal(err)
	}
	id := mustList(t, s)[0].ID

	// DELETE refused.
	_, err = s.db.Exec(`DELETE FROM conflicts WHERE id = ?`, id)
	if err == nil {
		t.Fatal("DELETE FROM conflicts must fail (trigger)")
	}

	// UPDATE attempts = 0 on open row refused (CHECK >= 1).
	_, err = s.db.Exec(`UPDATE conflicts SET attempts = 0 WHERE id = ?`, id)
	if err == nil {
		t.Fatal("UPDATE attempts = 0 must fail (CHECK)")
	}

	// Resolve, then UPDATE refused by the immutability trigger.
	got, err := s.ResolveConflict(id, "r", 1, nil, "a")
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.db.Exec(`UPDATE conflicts SET detail = 'tamper' WHERE id = ?`, got.ID)
	if err == nil {
		t.Fatal("UPDATE of resolved row must fail (trigger)")
	}
}

// TestFlagClaimConflict — valid (with AND without the other pair),
// missing/whitespace detail, half-set other pair, self-claim, bad
// idents, duplicate open claim, canonicalization A↔B dedup.
func TestFlagClaimConflict(t *testing.T) {
	t.Run("valid with other pair", func(t *testing.T) {
		s := open(t)
		got, err := s.FlagClaimConflict("docs", "a", "kernel", "kernel", "detail text", "alice")
		if err != nil {
			t.Fatal(err)
		}
		if got.Kind != "claim" || got.Status != "open" {
			t.Fatalf("kind/status: %q/%q", got.Kind, got.Status)
		}
		if got.OtherCollection != "kernel" || got.OtherFamilyID != "kernel" {
			t.Fatalf("other pair: %q/%q", got.OtherCollection, got.OtherFamilyID)
		}
		if got.BaseVersion != 0 || got.TheirVersion != 0 {
			t.Fatalf("absent families snapshot 0: base=%d their=%d", got.BaseVersion, got.TheirVersion)
		}
		if got.Attempts != 1 {
			t.Fatalf("attempts: %d", got.Attempts)
		}
		if got.Detail != "detail text" {
			t.Fatalf("detail: %q", got.Detail)
		}
		if got.OpenedBy != "alice" {
			t.Fatalf("opened_by: %q", got.OpenedBy)
		}
	})
	t.Run("valid without other pair", func(t *testing.T) {
		s := open(t)
		got, err := s.FlagClaimConflict("docs", "f", "", "", "d", "a")
		if err != nil {
			t.Fatal(err)
		}
		if got.OtherCollection != "" || got.OtherFamilyID != "" {
			t.Fatalf("other pair should be empty")
		}
	})
	t.Run("versions snapshotted at flag time", func(t *testing.T) {
		s := open(t)
		if _, err := s.SaveDoc("docs", "f", DocSave{Status: "active", Body: "v1"}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.SaveDoc("docs", "f", DocSave{Status: "active", Body: "v2"}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.SaveDoc("kernel", "kernel", DocSave{Status: "active", Body: "k"}); err != nil {
			t.Fatal(err)
		}
		got, err := s.FlagClaimConflict("docs", "f", "kernel", "kernel", "d", "a")
		if err != nil {
			t.Fatal(err)
		}
		if got.BaseVersion != 2 {
			t.Fatalf("base_version: %d, want 2", got.BaseVersion)
		}
		if got.TheirVersion != 1 {
			t.Fatalf("their_version: %d, want 1", got.TheirVersion)
		}
	})
	t.Run("missing/whitespace detail → ErrInvalid", func(t *testing.T) {
		s := open(t)
		if _, err := s.FlagClaimConflict("docs", "f", "", "", "", "a"); !errors.Is(err, ErrInvalid) {
			t.Fatalf("empty detail: %v", err)
		}
		if _, err := s.FlagClaimConflict("docs", "f", "", "", "   \t  ", "a"); !errors.Is(err, ErrInvalid) {
			t.Fatalf("whitespace detail: %v", err)
		}
	})
	t.Run("padded detail stored trimmed", func(t *testing.T) {
		s := open(t)
		got, err := s.FlagClaimConflict("docs", "f", "", "", "  d  ", "a")
		if err != nil {
			t.Fatal(err)
		}
		if got.Detail != "d" {
			t.Fatalf("detail: %q", got.Detail)
		}
	})
	t.Run("half-set other pair → ErrInvalid", func(t *testing.T) {
		s := open(t)
		if _, err := s.FlagClaimConflict("docs", "f", "kernel", "", "d", "a"); !errors.Is(err, ErrInvalid) {
			t.Fatalf("only other_collection: %v", err)
		}
		if _, err := s.FlagClaimConflict("docs", "f", "", "kernel", "d", "a"); !errors.Is(err, ErrInvalid) {
			t.Fatalf("only other_family: %v", err)
		}
	})
	t.Run("self-claim → ErrInvalid", func(t *testing.T) {
		s := open(t)
		if _, err := s.FlagClaimConflict("docs", "f", "docs", "f", "d", "a"); !errors.Is(err, ErrInvalid) {
			t.Fatalf("self-claim: %v", err)
		}
	})
	t.Run("bad idents → ErrInvalid", func(t *testing.T) {
		s := open(t)
		if _, err := s.FlagClaimConflict("..", "f", "", "", "d", "a"); !errors.Is(err, ErrInvalid) {
			t.Fatalf("bad collection: %v", err)
		}
		if _, err := s.FlagClaimConflict("docs", "with/slash", "", "", "d", "a"); !errors.Is(err, ErrInvalid) {
			t.Fatalf("bad family: %v", err)
		}
		if _, err := s.FlagClaimConflict("docs", "f", "..", "x", "d", "a"); !errors.Is(err, ErrInvalid) {
			t.Fatalf("bad other_collection: %v", err)
		}
	})
	t.Run("duplicate open claim → ErrConflict", func(t *testing.T) {
		s := open(t)
		if _, err := s.FlagClaimConflict("docs", "f", "", "", "first", "a"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.FlagClaimConflict("docs", "f", "", "", "second", "a"); !errors.Is(err, ErrConflict) {
			t.Fatalf("duplicate: %v", err)
		}
	})
	t.Run("canonicalization A→B and B→A dedup", func(t *testing.T) {
		s := open(t)
		if _, err := s.FlagClaimConflict("docs", "a", "kernel", "b", "d", "a"); err != nil {
			t.Fatal(err)
		}
		// "kernel"/"b" sorts AFTER "docs"/"a" (collection first),
		// so no swap; primary is docs/a. B→A should swap to the
		// same primary → ErrConflict.
		if _, err := s.FlagClaimConflict("kernel", "b", "docs", "a", "d", "a"); !errors.Is(err, ErrConflict) {
			t.Fatalf("swap-canonicalize: %v", err)
		}
	})
}

// TestPolicyConflictOnCut — CutRelease with over-cap kernel records a
// policy conflict; detail + their_version populated; repeat failed
// cut increments attempts on the same id. An expected-hash-mismatch
// cut records NOTHING.
func TestPolicyConflictOnCut(t *testing.T) {
	t.Run("byte-cap over", func(t *testing.T) {
		s := open(t)
		if _, err := s.SaveDoc("kernel", "kernel", DocSave{Status: "active", Body: strings.Repeat("x", KernelByteCap+1)}); err != nil {
			t.Fatal(err)
		}
		_, err := s.CutRelease("", "alice")
		if err == nil {
			t.Fatal("want error")
		}
		var cre *ConflictRecordedError
		if !errors.As(err, &cre) {
			t.Fatalf("want *ConflictRecordedError, got %T (%v)", err, err)
		}
		if !errors.Is(err, ErrLint) {
			t.Fatalf("want wrapped ErrLint, got %v", errors.Unwrap(err))
		}
		got, err := s.GetConflict(cre.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Kind != "policy" || got.Status != "open" {
			t.Fatalf("kind/status: %q/%q", got.Kind, got.Status)
		}
		if got.TheirVersion != 1 {
			t.Fatalf("their_version: %d, want 1 (offending doc)", got.TheirVersion)
		}
		if got.OpenedBy != "alice" {
			t.Fatalf("opened_by: %q, want alice", got.OpenedBy)
		}
		if !strings.Contains(got.Detail, "byte") {
			t.Fatalf("detail should mention byte cap: %q", got.Detail)
		}
		// Repeat → same id, attempts=2.
		_, err = s.CutRelease("", "bob")
		if err == nil {
			t.Fatal("repeat: want error")
		}
		errors.As(err, &cre)
		if cre.ID != got.ID {
			t.Fatalf("repeat must hit same row id: got %d, want %d", cre.ID, got.ID)
		}
		all := mustList(t, s)
		if len(all) != 1 || all[0].Attempts != 2 {
			t.Fatalf("want 1 policy conflict at attempts=2, got %+v", all)
		}
	})
	t.Run("word-cap over", func(t *testing.T) {
		s := open(t)
		if _, err := s.SaveDoc("kernel", "kernel", DocSave{Status: "active", Body: strings.Repeat("word ", KernelWordCap+1)}); err != nil {
			t.Fatal(err)
		}
		_, err := s.CutRelease("", "alice")
		if err == nil {
			t.Fatal("want error")
		}
		var cre *ConflictRecordedError
		errors.As(err, &cre)
		got, _ := s.GetConflict(cre.ID)
		if !strings.Contains(got.Detail, "word") {
			t.Fatalf("detail should mention word cap: %q", got.Detail)
		}
	})
	t.Run("expected-hash mismatch records nothing", func(t *testing.T) {
		s := open(t)
		if _, err := s.SaveDoc("kernel", "kernel", DocSave{Status: "active", Body: "v1"}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.CutRelease("sha256:"+strings.Repeat("a", 64), "alice"); !errors.Is(err, ErrConflict) {
			t.Fatalf("want ErrConflict on hash mismatch, got %v", err)
		}
		if got := mustList(t, s); len(got) != 0 {
			t.Fatalf("hash mismatch must not record: %+v", got)
		}
	})
}

// TestPreviewReleaseDoesNotRecord — PreviewRelease over a too-big
// kernel returns ErrLint and writes NO conflict row. A successful
// preview while a policy row is open resolves NOTHING.
func TestPreviewReleaseDoesNotRecord(t *testing.T) {
	s := open(t)
	if _, err := s.SaveDoc("kernel", "kernel", DocSave{Status: "active", Body: strings.Repeat("x", KernelByteCap+1)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PreviewRelease(); !errors.Is(err, ErrLint) {
		t.Fatalf("preview over-cap: %v", err)
	}
	if got := mustList(t, s); len(got) != 0 {
		t.Fatalf("preview must not record a conflict: %+v", got)
	}
	// Successful preview with a policy row already open → still open.
	if _, err := s.CutRelease("", "alice"); err == nil {
		t.Fatal("cut over-cap: want error")
	}
	before := mustList(t, s)
	if len(before) != 1 {
		t.Fatalf("want 1 policy row, got %d", len(before))
	}
	// Fix the kernel so the preview actually SUCCEEDS — only a
	// successful CUT may resolve policy rows; a successful preview
	// must leave them open and untouched (attempts included).
	if _, err := s.SaveDoc("kernel", "kernel", DocSave{Status: "active", Body: "small"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PreviewRelease(); err != nil {
		t.Fatalf("preview after fix must succeed: %v", err)
	}
	after := mustList(t, s)
	if len(after) != 1 || after[0].ID != before[0].ID || after[0].Status != "open" ||
		after[0].Attempts != before[0].Attempts {
		t.Fatalf("successful preview must not resolve or touch the policy row: before=%+v after=%+v", before, after)
	}
}

// TestSuccessfulCutResolvesPolicyConflicts — open policy conflicts
// are resolved with resolution "cleared by release <id>",
// resolved_by=editor, winning_version = family's manifest version
// (and 0 when the family is absent from the manifest). Edit and claim
// conflicts NOT touched; a second successful cut leaves already-
// resolved policy rows untouched (no trigger abort).
//
// The cut iterates by (collection, family_id) and aborts on the
// first over-cap kernel doc, so opening two policy rows in one
// failed cut is impossible. The brief test #10 confirms that
// re-cutting with the same family increments attempts on the same
// row — so we open the two policy rows across two failed cuts with
// the offending family swapped between them.
func TestSuccessfulCutResolvesPolicyConflicts(t *testing.T) {
	t.Run("winning_version from manifest", func(t *testing.T) {
		s := open(t)
		if _, err := s.SaveDoc("kernel", "k1", DocSave{Status: "active", Body: strings.Repeat("x", KernelByteCap+1)}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.CutRelease("", "alice"); err == nil {
			t.Fatal("first cut: want error")
		}
		// Fix k1 and cut again.
		if _, err := s.SaveDoc("kernel", "k1", DocSave{Status: "active", Body: "k1-tiny"}); err != nil {
			t.Fatal(err)
		}
		manifest, err := s.CutRelease("", "alice")
		if err != nil {
			t.Fatalf("clearing cut: %v", err)
		}
		if manifest.ReleaseID == 0 {
			t.Fatal("release_id must be > 0")
		}
		resolved, err := s.ListConflicts("resolved")
		if err != nil {
			t.Fatal(err)
		}
		if len(resolved) != 1 {
			t.Fatalf("want 1 resolved, got %d", len(resolved))
		}
		c := resolved[0]
		if c.ResolvedBy != "alice" {
			t.Fatalf("resolved_by: got %q, want alice", c.ResolvedBy)
		}
		wantRes := fmt.Sprintf("cleared by release %d", manifest.ReleaseID)
		if c.Resolution != wantRes {
			t.Fatalf("resolution: got %q, want %q", c.Resolution, wantRes)
		}
		// k1 is in the manifest at the new (small) version. Find it.
		var want int
		for _, d := range manifest.Docs {
			if d.FamilyID == "k1" {
				want = d.Version
			}
		}
		if c.WinningVersion != want {
			t.Fatalf("winning_version: got %d, want %d", c.WinningVersion, want)
		}
	})
	t.Run("winning_version 0 when family absent from manifest", func(t *testing.T) {
		s := open(t)
		if _, err := s.SaveDoc("kernel", "k1", DocSave{Status: "active", Body: "k1-tiny"}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.SaveDoc("kernel", "k2", DocSave{Status: "active", Body: strings.Repeat("x", KernelByteCap+1)}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.CutRelease("", "alice"); err == nil {
			t.Fatal("first cut: want error")
		}
		// Simulate "demoted to draft before the cut": the active row
		// for k2 is the offending version, and there is no SaveDoc
		// verb that promotes draft→active in place. Direct UPDATE is
		// the test-only path that exercises the same wire shape the
		// operator would arrive at through whatever post-v1 demote
		// verb eventually lands.
		if _, err := s.db.Exec(`UPDATE doc_versions SET status = 'draft' WHERE collection = 'kernel' AND family_id = 'k2' AND status = 'active'`); err != nil {
			t.Fatal(err)
		}
		manifest, err := s.CutRelease("", "alice")
		if err != nil {
			t.Fatalf("clearing cut: %v", err)
		}
		resolved, err := s.ListConflicts("resolved")
		if err != nil {
			t.Fatal(err)
		}
		if len(resolved) != 1 || resolved[0].FamilyID != "k2" {
			t.Fatalf("want 1 resolved for k2, got %+v", resolved)
		}
		// k2 must NOT be in the manifest (it's draft now).
		for _, d := range manifest.Docs {
			if d.FamilyID == "k2" {
				t.Fatalf("k2 must not be in the manifest: %+v", manifest.Docs)
			}
		}
		if resolved[0].WinningVersion != 0 {
			t.Fatalf("winning_version: got %d, want 0 (absent)", resolved[0].WinningVersion)
		}
	})
	t.Run("edit and claim conflicts untouched by a successful cut", func(t *testing.T) {
		s := open(t)
		if _, err := s.SaveDoc("kernel", "k1", DocSave{Status: "active", Body: strings.Repeat("x", KernelByteCap+1)}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.SaveDoc("docs", "x", DocSave{Status: "active", Body: "v1"}); err != nil {
			t.Fatal(err)
		}
		stale := 0
		if _, err := s.SaveDoc("docs", "x", DocSave{Status: "draft", Body: "stale", BaseVersion: &stale}); err == nil {
			t.Fatal("stale: want error")
		}
		if _, err := s.FlagClaimConflict("docs", "q", "", "", "d", "a"); err != nil {
			t.Fatal(err)
		}
		before, err := s.ListConflicts("")
		if err != nil {
			t.Fatal(err)
		}
		openBefore := map[int64]string{}
		for _, c := range before {
			if c.Status == "open" {
				openBefore[c.ID] = c.Kind
			}
		}
		// Fix k1, then cut.
		if _, err := s.SaveDoc("kernel", "k1", DocSave{Status: "active", Body: "k1-tiny"}); err != nil {
			t.Fatal(err)
		}
		manifest, err := s.CutRelease("", "alice")
		if err != nil {
			t.Fatalf("clearing cut: %v", err)
		}
		after, err := s.ListConflicts("")
		if err != nil {
			t.Fatal(err)
		}
		// All previously-open rows must still exist; their status
		// changed ONLY if they were policy rows.
		for id, kind := range openBefore {
			found := false
			for _, c := range after {
				if c.ID == id {
					found = true
					if kind == "policy" {
						if c.Status != "resolved" {
							t.Fatalf("policy id=%d must be resolved after cut: %+v", id, c)
						}
					} else {
						if c.Status != "open" {
							t.Fatalf("%s id=%d must remain open after cut: %+v", kind, id, c)
						}
					}
				}
			}
			if !found {
				t.Fatalf("id=%d (%s) missing from after list", id, kind)
			}
		}
		// Trigger must NOT abort the cut: it ran, and the policy row
		// is now resolved.
		_ = manifest
	})
	t.Run("second cut does not abort on already-resolved policy rows", func(t *testing.T) {
		s := open(t)
		if _, err := s.SaveDoc("kernel", "k1", DocSave{Status: "active", Body: strings.Repeat("x", KernelByteCap+1)}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.CutRelease("", "alice"); err == nil {
			t.Fatal("first cut: want error")
		}
		if _, err := s.SaveDoc("kernel", "k1", DocSave{Status: "active", Body: "k1-tiny"}); err != nil {
			t.Fatal(err)
		}
		m1, err := s.CutRelease("", "alice")
		if err != nil {
			t.Fatal(err)
		}
		// Now a second cut: the resolver UPDATE includes `WHERE status
		// = 'open'` so already-resolved rows are not touched, and the
		// immutability trigger is not fired.
		if _, err := s.SaveDoc("kernel", "k1", DocSave{Status: "active", Body: "k1-tiny-2"}); err != nil {
			t.Fatal(err)
		}
		m2, err := s.CutRelease("", "alice")
		if err != nil {
			t.Fatalf("second cut: %v", err)
		}
		if m2.ReleaseID <= m1.ReleaseID {
			t.Fatalf("release_id must increase: %d then %d", m1.ReleaseID, m2.ReleaseID)
		}
	})
}

// TestEmptyReleaseLintRecordsNothing — CutRelease on an empty store
// returns ErrLint and writes NO conflict row (empty-release lint has
// no offending doc).
func TestEmptyReleaseLintRecordsNothing(t *testing.T) {
	s := open(t)
	_, err := s.CutRelease("", "alice")
	if !errors.Is(err, ErrLint) {
		t.Fatalf("empty release: %v", err)
	}
	var cre *ConflictRecordedError
	if errors.As(err, &cre) {
		t.Fatalf("empty-release lint must not record a conflict: %+v", cre)
	}
	if got := mustList(t, s); len(got) != 0 {
		t.Fatalf("empty-release lint must not write rows: %+v", got)
	}
}

// TestListConflicts — order (newest first), status filter, bad status,
// empty non-nil slice, list rows carry NO attempted payload;
// GetConflict on a claim/policy row → Attempted nil.
func TestListConflicts(t *testing.T) {
	s := open(t)
	// Empty.
	out, err := s.ListConflicts("")
	if err != nil {
		t.Fatal(err)
	}
	if out == nil {
		t.Fatal("empty list: want non-nil empty slice")
	}
	if len(out) != 0 {
		t.Fatalf("empty list: got %d, want 0", len(out))
	}
	// Two claims, in order; resolve the first; verify ordering and filter.
	_, err = s.FlagClaimConflict("docs", "a", "", "", "d", "op")
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.FlagClaimConflict("docs", "b", "", "", "d", "op")
	if err != nil {
		t.Fatal(err)
	}
	all, err := s.ListConflicts("")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("want 2, got %d", len(all))
	}
	if all[0].FamilyID != "b" || all[1].FamilyID != "a" {
		t.Fatalf("order: got %s then %s, want b then a (newest first)", all[0].FamilyID, all[1].FamilyID)
	}
	if all[0].ID <= all[1].ID {
		t.Fatalf("newest-first: %d then %d", all[0].ID, all[1].ID)
	}
	// List rows carry NO attempted payload. ConflictMeta has no
	// Attempted field at all, so the list wire form literally cannot
	// carry one. The full-record shape is the only path that exposes
	// Attempted — covered by the GetConflict check below.
	resolved, err := s.ResolveConflict(all[1].ID, "r", 1, nil, "op")
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Status != "resolved" {
		t.Fatal("resolve failed")
	}
	openOnly, err := s.ListConflicts("open")
	if err != nil {
		t.Fatal(err)
	}
	if len(openOnly) != 1 || openOnly[0].FamilyID != "b" {
		t.Fatalf("open filter: %+v", openOnly)
	}
	resolvedOnly, err := s.ListConflicts("resolved")
	if err != nil {
		t.Fatal(err)
	}
	if len(resolvedOnly) != 1 || resolvedOnly[0].FamilyID != "a" {
		t.Fatalf("resolved filter: %+v", resolvedOnly)
	}
	if _, err := s.ListConflicts("nonsense"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad status: %v", err)
	}
	// GetConflict on a claim → Attempted nil.
	got, err := s.GetConflict(all[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Attempted != nil {
		t.Fatalf("claim/policy row: expected nil Attempted, got %+v", got.Attempted)
	}
}
