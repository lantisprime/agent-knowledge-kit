// Authentication for the HTTP API: an operator bearer token (full
// access, loaded from a file at startup) and per-host subscriber
// tokens (issued by the server, stored digest-only — see
// store.IssueHostToken). The token IS the host identity: handlers that
// carry a host name resolve it from the token, so a subscriber can
// never speak for another host. See docs/plans/knowledge-server.md,
// build-order step 3.
package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"strings"
)

// maxBodyBytes caps every request body. The largest legitimate body is
// a doc save; the kernel byte cap is 24 KiB and Tier B docs are
// KB-scale, so 1 MiB is generous headroom, not a tuning knob.
const maxBodyBytes = 1 << 20

var (
	errUnauthorized = errors.New("missing or invalid bearer token")
	errForbidden    = errors.New("forbidden")
	errTooLarge     = errors.New("request body too large")
)

// principal is the authenticated caller of one request. operator has
// full access; otherwise host names the subscriber the bearer token is
// bound to. With auth disabled (no --operator-token-file) every caller
// is the operator principal — that is the pre-authN loopback posture,
// unchanged.
type principal struct {
	operator bool
	host     string
}

type ctxKey int

const principalKey ctxKey = 0

// authState holds the operator credential digest. A nil *authState
// means authentication is disabled.
type authState struct {
	operatorHash [sha256.Size]byte
}

// loadOperatorToken reads the operator bearer token from path,
// tolerating surrounding whitespace/newline. Only the digest is kept
// in memory. A minimum length of 32 characters is enforced to bound
// brute-force exposure of the bearer secret; the operator can
// generate one with `openssl rand -hex 32` or with the server's
// `-init-operator-token` flag (see initOperatorToken).
func loadOperatorToken(path string) (*authState, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	token := strings.TrimSpace(string(b))
	if token == "" {
		return nil, fmt.Errorf("operator token file %s is empty", path)
	}
	if len(token) < 32 {
		return nil, fmt.Errorf("operator token must be at least 32 characters; generate with e.g. openssl rand -hex 32")
	}
	return &authState{operatorHash: sha256.Sum256([]byte(token))}, nil
}

// initOperatorToken generates a fresh 64-character hex operator token
// (256 bits from crypto/rand) and writes it to path with mode 0600.
// Called only when the file does not yet exist; O_CREATE|O_EXCL
// refuses to overwrite an existing file. The plaintext token lives on
// disk in this one place; the operator is expected to move it out of
// band before the server is reachable from anywhere it is not already
// trusted to be.
func initOperatorToken(path string) error {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return fmt.Errorf("generate operator token: %w", err)
	}
	token := hex.EncodeToString(raw[:])
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("refusing to overwrite existing operator token file %s", path)
		}
		return fmt.Errorf("write operator token to %s: %w", path, err)
	}
	defer f.Close()
	if _, err := f.WriteString(token); err != nil {
		return err
	}
	return nil
}

// bootstrapOperatorToken resolves the operator credential file at
// startup. With init == true and the file missing, a fresh token is
// generated and written (mode 0600); with init == false and the file
// missing, an error recommends `-init-operator-token`. With init ==
// true and the file already present, initOperatorToken's O_EXCL check
// refuses and the existing file is untouched. With the file present
// and init == false, it is loaded as normal. An empty path returns
// (nil, nil): the operator credential is disabled, every caller is
// the operator principal (the pre-authN loopback posture).
func bootstrapOperatorToken(path string, init bool) (*authState, error) {
	if path == "" {
		return nil, nil
	}
	if init {
		if err := initOperatorToken(path); err != nil {
			return nil, err
		}
	} else {
		if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("operator token file %s not found; supply an existing file or pass -init-operator-token to generate one", path)
		} else if err != nil {
			return nil, fmt.Errorf("stat operator token file %s: %w", path, err)
		}
	}
	return loadOperatorToken(path)
}

// authenticate resolves the request's principal. The operator token is
// compared digest-to-digest in constant time; anything else is looked
// up as a host token by digest.
func (a *api) authenticate(r *http.Request) (principal, error) {
	if a.auth == nil {
		return principal{operator: true}, nil
	}
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || token == "" {
		return principal{}, errUnauthorized
	}
	digest := sha256.Sum256([]byte(token))
	if subtle.ConstantTimeCompare(digest[:], a.auth.operatorHash[:]) == 1 {
		return principal{operator: true}, nil
	}
	host, found, err := a.st.HostForToken(token)
	if err != nil {
		return principal{}, err
	}
	if !found {
		return principal{}, errUnauthorized
	}
	return principal{host: host}, nil
}

// secure wraps a handler with the request-body cap, authentication,
// and the route's access policy (operator-only or any valid token).
// The resolved principal is stashed in the request context for
// handlers that enforce host binding.
func (a *api) secure(operatorOnly bool, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		p, err := a.authenticate(r)
		if err != nil {
			writeErr(w, err)
			return
		}
		if operatorOnly && !p.operator {
			writeErr(w, fmt.Errorf("%w: operator credential required", errForbidden))
			return
		}
		h(w, r.WithContext(context.WithValue(r.Context(), principalKey, p)))
	}
}

// requestPrincipal returns the principal secure stashed in the
// context. The zero principal (non-operator, empty host) is never
// stored, so ok==false only in tests that bypass secure.
func requestPrincipal(r *http.Request) (principal, bool) {
	p, ok := r.Context().Value(principalKey).(principal)
	return p, ok
}
