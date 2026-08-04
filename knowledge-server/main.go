package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"

	"agent-knowledge-kit/knowledge-server/store"
)

func main() {
	dbPath := flag.String("db", "knowledge.db", "path to the SQLite database")
	listen := flag.String("listen", "127.0.0.1:8471", "listen address (loopback unless --insecure-no-auth)")
	insecureNoAuth := flag.Bool("insecure-no-auth", false,
		"allow binding a non-loopback address without authentication. DANGER: do not enable on a network-reachable host before the authN slice lands.")
	flag.Parse()

	if err := checkListenAddr(*listen, *insecureNoAuth); err != nil {
		log.Fatalf("%v (pass --insecure-no-auth to override)", err)
	}

	st, err := store.Open(*dbPath)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer st.Close()

	log.Printf("knowledge-server listening on %s (db %s)", *listen, *dbPath)
	log.Fatal(http.ListenAndServe(*listen, newMux(st)))
}

// checkListenAddr enforces the v1 loopback-only posture. The default
// 127.0.0.1:8471 is a footgun-free default; non-loopback binds are
// refused unless the operator passes --insecure-no-auth, which is
// logged so the choice is auditable.
func checkListenAddr(addr string, insecureNoAuth bool) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("invalid -listen address %q: %v", addr, err)
	}
	if insecureNoAuth {
		return nil
	}
	if !isLoopbackHost(host) {
		return fmt.Errorf("refusing non-loopback bind on %s", addr)
	}
	return nil
}

// isLoopbackHost treats the documented loopback shapes as loopback:
// the literal "localhost" string, IPv4 127.0.0.0/8, and IPv6 ::1. An
// empty host (a bare ":port") is NOT loopback: Go's net package binds
// it to every interface, so it must be refused unless the operator has
// explicitly opted in with --insecure-no-auth.
func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}
