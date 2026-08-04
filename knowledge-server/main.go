package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"time"

	"agent-knowledge-kit/knowledge-server/store"
)

func main() {
	dbPath := flag.String("db", "knowledge.db", "path to the SQLite database")
	listen := flag.String("listen", "127.0.0.1:8471", "listen address (loopback unless auth+TLS are enabled or --insecure-no-auth)")
	operatorTokenFile := flag.String("operator-token-file", "",
		"path to the operator bearer token; setting it enables authentication on every endpoint. Must be at least 32 characters; generate with e.g. openssl rand -hex 32")
	tlsCert := flag.String("tls-cert", "", "TLS certificate (PEM); with -tls-key, the server serves HTTPS")
	tlsKey := flag.String("tls-key", "", "TLS private key (PEM); must be set together with -tls-cert")
	insecureNoAuth := flag.Bool("insecure-no-auth", false,
		"allow binding a non-loopback address without authentication and TLS. DANGER: exposes an unauthenticated write API to the network.")
	initOperator := flag.Bool("init-operator-token", false,
		"with -operator-token-file, generate a fresh operator credential (CSPRNG, 64 hex chars, mode 0600) when the file is missing; refuses to overwrite an existing file")
	flag.Parse()

	if (*tlsCert == "") != (*tlsKey == "") {
		log.Fatal("-tls-cert and -tls-key must be set together")
	}
	if *initOperator && *operatorTokenFile == "" {
		log.Fatal("-init-operator-token requires -operator-token-file")
	}

	auth, err := bootstrapOperatorToken(*operatorTokenFile, *initOperator)
	if err != nil {
		log.Fatalf("%v", err)
	}
	// -init-operator-token is a one-shot provisioning verb: generate,
	// report, exit. Serving anyway would leave the flag armed in a
	// service unit, and the next restart would fatal on O_EXCL.
	if *initOperator {
		log.Printf("generated operator token at %s (mode 0600); restart without -init-operator-token to serve", *operatorTokenFile)
		return
	}

	// A non-loopback bind requires BOTH auth and TLS: a bearer token on
	// plaintext HTTP is sniffable, which would defeat the token.
	secured := auth != nil && *tlsCert != ""
	if err := checkListenAddr(*listen, secured, *insecureNoAuth); err != nil {
		log.Fatalf("%v (enable -operator-token-file and TLS, or pass --insecure-no-auth to override)", err)
	}

	st, err := store.Open(*dbPath)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer st.Close()

	srv := &http.Server{
		Addr:              *listen,
		Handler:           newMux(st, auth),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       2 * time.Minute,
		// WriteTimeout is deliberately zero: it would sever long-lived
		// response streams — the release archive on a slow link today,
		// the SSE release stream when it lands.
	}
	log.Printf("knowledge-server listening on %s (db %s, auth %v, tls %v)",
		*listen, *dbPath, auth != nil, *tlsCert != "")
	if *tlsCert != "" {
		log.Fatal(srv.ListenAndServeTLS(*tlsCert, *tlsKey))
	}
	log.Fatal(srv.ListenAndServe())
}

// checkListenAddr gates network exposure. Loopback binds are always
// allowed (the pre-authN colocated posture). A non-loopback bind is
// allowed only when the server is secured (auth AND TLS enabled) or
// the operator explicitly overrides with --insecure-no-auth, which is
// logged so the choice is auditable.
func checkListenAddr(addr string, secured, insecureNoAuth bool) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("invalid -listen address %q: %v", addr, err)
	}
	if secured || insecureNoAuth {
		return nil
	}
	if !isLoopbackHost(host) {
		return fmt.Errorf("refusing non-loopback bind on %s without auth+TLS", addr)
	}
	return nil
}

// isLoopbackHost treats the documented loopback shapes as loopback:
// the literal "localhost" string, IPv4 127.0.0.0/8, and IPv6 ::1. An
// empty host (a bare ":port") is NOT loopback: Go's net package binds
// it to every interface, so it must be refused unless the server is
// secured or the operator has explicitly opted in.
func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}
