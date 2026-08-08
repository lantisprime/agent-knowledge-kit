// The HTTP API is the only write door. All schemas are JSON; errors
// are always {"error": "<code>", "detail": "<text>"}. See
// docs/plans/knowledge-server.md, "Component boundaries and contracts".
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"agent-knowledge-kit/knowledge-server/store"
)

type api struct {
	st   *store.Store
	auth *authState // nil = authentication disabled (loopback posture)
	now  func() time.Time
}

// newMux wires each route through secure with its access policy:
// writes and fleet actions are operator-only; release reads and
// heartbeats accept any valid token, with host binding enforced in the
// handlers. With auth == nil every request passes as the operator
// principal (pre-authN behavior, unchanged).
func newMux(st *store.Store, auth *authState) *http.ServeMux {
	a := &api{st: st, auth: auth, now: time.Now}
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /api/docs/{collection}/{family}", a.secure(true, a.saveDoc))
	mux.HandleFunc("GET /api/docs", a.secure(true, a.listDocs))
	mux.HandleFunc("GET /api/docs/{collection}/{family}", a.secure(true, a.getDoc))
	mux.HandleFunc("GET /api/docs/{collection}/{family}/versions", a.secure(true, a.docHistory))
	mux.HandleFunc("GET /api/releases/preview", a.secure(true, a.previewRelease))
	mux.HandleFunc("POST /api/releases", a.secure(true, a.cutRelease))
	mux.HandleFunc("GET /api/releases/current", a.secure(false, a.currentRelease))
	mux.HandleFunc("GET /api/releases/{id}/archive", a.secure(false, a.archive))
	mux.HandleFunc("POST /api/heartbeats", a.secure(false, a.heartbeat))
	mux.HandleFunc("POST /api/hosts/{host}/resync", a.secure(true, a.requestResync))
	mux.HandleFunc("GET /api/hosts", a.secure(true, a.listHosts))
	mux.HandleFunc("POST /api/hosts/{host}/token", a.secure(true, a.issueToken))
	mux.HandleFunc("DELETE /api/hosts/{host}/token", a.secure(true, a.revokeToken))
	mux.HandleFunc("GET /api/conflicts", a.secure(true, a.listConflicts))
	mux.HandleFunc("GET /api/conflicts/{id}", a.secure(true, a.getConflict))
	mux.HandleFunc("POST /api/conflicts", a.secure(true, a.flagConflict))
	mux.HandleFunc("POST /api/conflicts/{id}/resolve", a.secure(true, a.resolveConflict))
	registerUI(mux)
	return mux
}

// writeJSON sets the hardening headers that pin the curation UI's
// cache/no-sniff contract on EVERY JSON response (success and error),
// then writes the encoded value. The archive handler does not go
// through writeJSON and is unaffected.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	code, status := "internal", http.StatusInternalServerError
	switch {
	case errors.Is(err, store.ErrLint):
		code, status = "lint", http.StatusConflict
	case errors.Is(err, store.ErrConflict):
		code, status = "conflict", http.StatusConflict
	case errors.Is(err, store.ErrInvalid):
		code, status = "invalid", http.StatusBadRequest
	case errors.Is(err, store.ErrNotFound):
		code, status = "not_found", http.StatusNotFound
	case errors.Is(err, errUnauthorized):
		w.Header().Set("WWW-Authenticate", `Bearer realm="knowledge-server"`)
		code, status = "unauthorized", http.StatusUnauthorized
	case errors.Is(err, errForbidden):
		code, status = "forbidden", http.StatusForbidden
	case errors.Is(err, errTooLarge):
		code, status = "too_large", http.StatusRequestEntityTooLarge
	}
	// The envelope is {error, detail, conflict_id?}. conflict_id is
	// added ONLY when the error wraps a ConflictRecordedError — the
	// rejection committed a record (stale-base save, lint-failed cut)
	// and the caller can fetch /api/conflicts/{id} for the full
	// shape. Status / code mapping itself is unchanged: Unwrap keeps
	// errors.Is(err, ErrConflict/ErrLint) working for any caller
	// that does its own classification.
	envelope := map[string]any{"error": code, "detail": err.Error()}
	var cre *store.ConflictRecordedError
	if errors.As(err, &cre) {
		envelope["conflict_id"] = cre.ID
	}
	writeJSON(w, status, envelope)
}

// decodeBody decodes a JSON request body, distinguishing the
// MaxBytesReader cap (413) from plain malformed JSON (400). It also
// enforces that the body contains exactly one JSON value: a second
// successful Decode (or any non-EOF result on the second read) means
// trailing data, which is rejected as 400 invalid. A second read that
// hits the MaxBytesReader cap is rejected as 413 — the cap fires while
// we are draining the extra bytes.
func decodeBody(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(v); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return fmt.Errorf("%w: body exceeds %d bytes", errTooLarge, mbe.Limit)
		}
		return fmt.Errorf("%w: bad JSON: %v", store.ErrInvalid, err)
	}
	var trailing json.RawMessage
	if err := dec.Decode(&trailing); err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return fmt.Errorf("%w: body exceeds %d bytes", errTooLarge, mbe.Limit)
		}
		return fmt.Errorf("%w: trailing data after JSON body", store.ErrInvalid)
	}
	return fmt.Errorf("%w: trailing data after JSON body", store.ErrInvalid)
}

// decodeOptionalBody is decodeBody's variant for routes where the body
// is optional (e.g. POST /api/releases: empty body means "unconditional
// cut"). The "empty" decision is made by the decoder: a FIRST Decode
// returning io.EOF means no JSON value was present (this also covers
// a whitespace-only body, which is fine for the cut path). Every
// other path keeps decodeBody's exact semantics: cap hit → 413,
// malformed JSON → 400, trailing data after one JSON value → 400/413.
// Returns false when the body was empty, true when a value decoded.
func decodeOptionalBody(r *http.Request, v any) (bool, error) {
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(v); err != nil {
		if errors.Is(err, io.EOF) {
			return false, nil
		}
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return false, fmt.Errorf("%w: body exceeds %d bytes", errTooLarge, mbe.Limit)
		}
		return false, fmt.Errorf("%w: bad JSON: %v", store.ErrInvalid, err)
	}
	var trailing json.RawMessage
	if err := dec.Decode(&trailing); err != nil {
		if errors.Is(err, io.EOF) {
			return true, nil
		}
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return true, fmt.Errorf("%w: body exceeds %d bytes", errTooLarge, mbe.Limit)
		}
		return true, fmt.Errorf("%w: trailing data after JSON body", store.ErrInvalid)
	}
	return true, fmt.Errorf("%w: trailing data after JSON body", store.ErrInvalid)
}

// hashPattern is the exact wire shape of expected_content_hash on the
// cut release body: "sha256:" + 64 lowercase hex chars.
const hashPattern = `^sha256:[0-9a-f]{64}$`

func (a *api) saveDoc(w http.ResponseWriter, r *http.Request) {
	var in store.DocSave
	if err := decodeBody(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	in.Editor = r.Header.Get("X-Editor")
	if in.Editor == "" {
		in.Editor = "operator"
	}
	v, err := a.st.SaveDoc(r.PathValue("collection"), r.PathValue("family"), in)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (a *api) cutRelease(w http.ResponseWriter, r *http.Request) {
	// Body shape:
	//   empty (or whitespace-only) → unconditional cut (legacy)
	//   any JSON object → expected_content_hash is mandatory; a
	//     missing, null, empty, or malformed value is 400 invalid
	//     and NO release is cut (the zero value must never silently
	//     become an unconditional cut).
	var in struct {
		ExpectedContentHash *string `json:"expected_content_hash"`
	}
	hadBody, err := decodeOptionalBody(r, &in)
	if err != nil {
		writeErr(w, err)
		return
	}
	expected := ""
	if hadBody {
		if in.ExpectedContentHash == nil {
			writeErr(w, fmt.Errorf("%w: expected_content_hash is required", store.ErrInvalid))
			return
		}
		if !regexp.MustCompile(hashPattern).MatchString(*in.ExpectedContentHash) {
			writeErr(w, fmt.Errorf("%w: expected_content_hash must be %q", store.ErrInvalid, "sha256:"+strings.Repeat("[0-9a-f]", 64)))
			return
		}
		expected = *in.ExpectedContentHash
	}
	editor := r.Header.Get("X-Editor")
	if editor == "" {
		editor = "operator"
	}
	m, err := a.st.CutRelease(expected, editor)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, m)
}

// listDocs returns one DocMeta per family: the max-version row of
// that family. The store is required to return a non-nil empty slice
// for an empty store so the wire form is "[]", never "null".
func (a *api) listDocs(w http.ResponseWriter, r *http.Request) {
	docs, err := a.st.ListDocs()
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"docs": docs})
}

// getDoc returns one Doc. Optional ?version=N: detect presence with
// Has("version") (first value wins if repeated); when present, parse
// even the empty string — empty, non-numeric, < 1, or overflow → 400
// invalid. Absent → latest.
func (a *api) getDoc(w http.ResponseWriter, r *http.Request) {
	collection := r.PathValue("collection")
	family := r.PathValue("family")
	version := 0
	if r.URL.Query().Has("version") {
		raw := r.URL.Query().Get("version")
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			writeErr(w, fmt.Errorf("%w: ?version must be a positive integer", store.ErrInvalid))
			return
		}
		version = n
	}
	doc, err := a.st.GetDoc(collection, family, version)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, doc)
}

// docHistory returns every version of one family, newest first.
// Unknown family → 404 from the store's ErrNotFound mapping.
func (a *api) docHistory(w http.ResponseWriter, r *http.Request) {
	versions, err := a.st.DocHistory(r.PathValue("collection"), r.PathValue("family"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"versions": versions})
}

// previewRelease returns the would-be release manifest with
// ReleaseID == 0. Lint failures surface as 409 via the existing
// ErrLint mapping.
func (a *api) previewRelease(w http.ResponseWriter, r *http.Request) {
	m, err := a.st.PreviewRelease()
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func (a *api) currentRelease(w http.ResponseWriter, r *http.Request) {
	m, err := a.st.CurrentRelease()
	if err != nil {
		writeErr(w, err)
		return
	}
	// Optional ?host=<h> adds the per-host resync pull-flag to the
	// response. The field is omitempty: it is omitted whenever false,
	// whether because host is absent (m.Resync keeps its zero value)
	// or because host is present but no resync is pending. The token
	// is the identity: a subscriber token resolves to its bound host
	// even with no query param, and may not ask about any other host.
	host := r.URL.Query().Get("host")
	if p, _ := requestPrincipal(r); !p.operator {
		if host != "" && host != p.host {
			writeErr(w, fmt.Errorf("%w: token is bound to a different host", errForbidden))
			return
		}
		host = p.host
	}
	if host != "" {
		pending, err := a.st.ResyncPending(host)
		if err != nil {
			writeErr(w, err)
			return
		}
		m.Resync = pending
	}
	writeJSON(w, http.StatusOK, m)
}

func (a *api) archive(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, fmt.Errorf("%w: bad release id", store.ErrInvalid))
		return
	}
	// Confirm the release exists BEFORE committing to a tar
	// Content-Type or any bytes. A missing id must surface as a
	// 404 JSON envelope, not a 200 with an empty body.
	if _, err := a.st.Release(id); err != nil {
		writeErr(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/x-tar")
	if err := a.st.WriteArchive(id, w); err != nil {
		// The tar header and possibly the first entry are already
		// on the wire; the client will see a truncated stream.
		// The pre-stream existence check above guarantees the only
		// way we reach this line is a mid-stream failure.
		log.Printf("archive %d: %v", id, err)
		return
	}
}

func (a *api) requestResync(w http.ResponseWriter, r *http.Request) {
	host := r.PathValue("host")
	if err := a.st.RequestResync(host); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// listHosts returns one HostStatus per known host (union of
// heartbeats, pending resyncs, and issued tokens), the latest
// release id, and a server-clock `now` for the UI to compute
// heartbeat age against the same clock that stamped `seen_at`.
// Operator-only via secure. No release cut yet → latest_release_id
// is 0 (ErrNotFound from CurrentRelease is mapped to 0, not an
// error). now is RFC3339 UTC; the store stamps seen_at and
// requested_at in the same format.
func (a *api) listHosts(w http.ResponseWriter, r *http.Request) {
	hosts, err := a.st.ListHosts()
	if err != nil {
		writeErr(w, err)
		return
	}
	latest := int64(0)
	cur, err := a.st.CurrentRelease()
	if err == nil {
		latest = cur.ReleaseID
	} else if !errors.Is(err, store.ErrNotFound) {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"hosts":             hosts,
		"latest_release_id": latest,
		"now":               a.now().UTC().Format(time.RFC3339),
	})
}

func (a *api) heartbeat(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Host          string  `json:"host"`
		ReleaseID     int64   `json:"release_id"`
		OK            bool    `json:"ok"`
		Error         *string `json:"error"`
		ResyncApplied bool    `json:"resync_applied"`
	}
	if err := decodeBody(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	// Host binding: a subscriber token may only heartbeat as its bound
	// host. An empty body host resolves to the token's host, so a
	// tokened subscriber needs no out-of-band hostname coordination.
	if p, _ := requestPrincipal(r); !p.operator {
		if in.Host != "" && in.Host != p.host {
			writeErr(w, fmt.Errorf("%w: token is bound to a different host", errForbidden))
			return
		}
		in.Host = p.host
	}
	msg := ""
	if in.Error != nil {
		msg = *in.Error
	}
	if err := a.st.Heartbeat(in.Host, in.ReleaseID, in.OK, msg, in.ResyncApplied); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// issueToken mints (or rotates) the bearer token for a host. The
// plaintext appears in this response exactly once; only its digest is
// stored. Operator-only via secure.
func (a *api) issueToken(w http.ResponseWriter, r *http.Request) {
	host := r.PathValue("host")
	token, err := a.st.IssueHostToken(host)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"host": host, "token": token})
}

// revokeToken deletes a host's token; its bearer stops verifying
// immediately. Operator-only via secure.
func (a *api) revokeToken(w http.ResponseWriter, r *http.Request) {
	if err := a.st.RevokeHostToken(r.PathValue("host")); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- conflict endpoints (build-order step 5) ------------------------------

// listConflicts returns conflicts newest-first. ?status=open|resolved
// filters; empty / absent means all. Other values surface as 400
// invalid from the store ErrInvalid mapping — the handler does not
// double-validate.
func (a *api) listConflicts(w http.ResponseWriter, r *http.Request) {
	conflicts, err := a.st.ListConflicts(r.URL.Query().Get("status"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"conflicts": conflicts})
}

// getConflict returns one full record. {id} parsed with
// strconv.ParseInt; parse failure → 400 invalid "bad conflict id".
// Unknown id → 404 from the store ErrNotFound mapping.
func (a *api) getConflict(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, fmt.Errorf("%w: bad conflict id", store.ErrInvalid))
		return
	}
	c, err := a.st.GetConflict(id)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

// flagConflict opens a manual claim conflict. Body:
//
//	{"collection", "family_id", "other_collection"?,
//	 "other_family_id"?, "detail"}
//
// Empty other_* fields are omitted from the body entirely (the store
// accepts them as empty strings). opened_by from X-Editor, defaulting
// "operator". 201 carries the created record; 409 is the duplicate-
// open-claim path; 400 invalid surfaces missing detail, half-set
// other pair, self-claim, and bad idents.
func (a *api) flagConflict(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Collection      string `json:"collection"`
		FamilyID        string `json:"family_id"`
		OtherCollection string `json:"other_collection,omitempty"`
		OtherFamilyID   string `json:"other_family_id,omitempty"`
		Detail          string `json:"detail"`
	}
	if err := decodeBody(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	openedBy := r.Header.Get("X-Editor")
	if openedBy == "" {
		openedBy = "operator"
	}
	c, err := a.st.FlagClaimConflict(in.Collection, in.FamilyID, in.OtherCollection, in.OtherFamilyID, in.Detail, openedBy)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, c)
}

// resolveConflict body:
//
//	{"resolution", "expected_attempts", "save"?}
//
// expected_attempts is REQUIRED and must be >= 1 — the zero value
// must never silently skip the precondition. save is optional and
// only legal for kind=edit. resolved_by from X-Editor defaulting
// "operator"; the nested save's Editor field is the same value.
func (a *api) resolveConflict(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, fmt.Errorf("%w: bad conflict id", store.ErrInvalid))
		return
	}
	var in struct {
		Resolution       string         `json:"resolution"`
		ExpectedAttempts *int           `json:"expected_attempts"`
		Save             *store.DocSave `json:"save"`
	}
	if err := decodeBody(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	if in.ExpectedAttempts == nil || *in.ExpectedAttempts < 1 {
		writeErr(w, fmt.Errorf("%w: expected_attempts is required", store.ErrInvalid))
		return
	}
	resolvedBy := r.Header.Get("X-Editor")
	if resolvedBy == "" {
		resolvedBy = "operator"
	}
	if in.Save != nil {
		// Editor on a nested save is operator-supplied; it is the
		// same identity as resolved_by so the audit trail is one
		// string per action.
		in.Save.Editor = resolvedBy
	}
	c, err := a.st.ResolveConflict(id, in.Resolution, *in.ExpectedAttempts, in.Save, resolvedBy)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}
