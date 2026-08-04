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
	"strconv"

	"agent-knowledge-kit/knowledge-server/store"
)

type api struct {
	st   *store.Store
	auth *authState // nil = authentication disabled (loopback posture)
}

// newMux wires each route through secure with its access policy:
// writes and fleet actions are operator-only; release reads and
// heartbeats accept any valid token, with host binding enforced in the
// handlers. With auth == nil every request passes as the operator
// principal (pre-authN behavior, unchanged).
func newMux(st *store.Store, auth *authState) *http.ServeMux {
	a := &api{st: st, auth: auth}
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /api/docs/{collection}/{family}", a.secure(true, a.saveDoc))
	mux.HandleFunc("POST /api/releases", a.secure(true, a.cutRelease))
	mux.HandleFunc("GET /api/releases/current", a.secure(false, a.currentRelease))
	mux.HandleFunc("GET /api/releases/{id}/archive", a.secure(false, a.archive))
	mux.HandleFunc("POST /api/heartbeats", a.secure(false, a.heartbeat))
	mux.HandleFunc("POST /api/hosts/{host}/resync", a.secure(true, a.requestResync))
	mux.HandleFunc("POST /api/hosts/{host}/token", a.secure(true, a.issueToken))
	mux.HandleFunc("DELETE /api/hosts/{host}/token", a.secure(true, a.revokeToken))
	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
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
	writeJSON(w, status, map[string]string{"error": code, "detail": err.Error()})
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
	m, err := a.st.CutRelease()
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, m)
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
