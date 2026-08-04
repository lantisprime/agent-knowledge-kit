// The HTTP API is the only write door. All schemas are JSON; errors
// are always {"error": "<code>", "detail": "<text>"}. See
// docs/plans/knowledge-server.md, "Component boundaries and contracts".
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"

	"agent-knowledge-kit/knowledge-server/store"
)

type api struct {
	st *store.Store
}

func newMux(st *store.Store) *http.ServeMux {
	a := &api{st: st}
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /api/docs/{collection}/{family}", a.saveDoc)
	mux.HandleFunc("POST /api/releases", a.cutRelease)
	mux.HandleFunc("GET /api/releases/current", a.currentRelease)
	mux.HandleFunc("GET /api/releases/{id}/archive", a.archive)
	mux.HandleFunc("POST /api/heartbeats", a.heartbeat)
	mux.HandleFunc("POST /api/hosts/{host}/resync", a.requestResync)
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
	}
	writeJSON(w, status, map[string]string{"error": code, "detail": err.Error()})
}

func (a *api) saveDoc(w http.ResponseWriter, r *http.Request) {
	var in store.DocSave
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, fmt.Errorf("%w: bad JSON: %v", store.ErrInvalid, err))
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
	// or because host is present but no resync is pending.
	if host := r.URL.Query().Get("host"); host != "" {
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
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, fmt.Errorf("%w: bad JSON: %v", store.ErrInvalid, err))
		return
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
