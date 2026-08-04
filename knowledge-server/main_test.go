package main

import "testing"

// TestCheckListenAddr pins down the exposure gate. Loopback shapes
// (127.0.0.0/8, ::1, "localhost") always bind. Every non-loopback
// shape (empty host = wildcard bind, 0.0.0.0, ...) is refused unless
// the server is secured (auth AND TLS) or --insecure-no-auth is set.
func TestCheckListenAddr(t *testing.T) {
	cases := []struct {
		addr           string
		secured        bool
		insecureNoAuth bool
		wantErr        bool
	}{
		{"127.0.0.1:8471", false, false, false},
		{"localhost:1", false, false, false},
		{"[::1]:1", false, false, false},
		{":8494", false, false, true},
		{"0.0.0.0:1", false, false, true},

		// Secured (auth+TLS): non-loopback binds are the point.
		{":8494", true, false, false},
		{"0.0.0.0:1", true, false, false},
		{"127.0.0.1:8471", true, false, false},

		// Explicit insecure override.
		{":8494", false, true, false},
		{"0.0.0.0:1", false, true, false},
	}
	for _, tc := range cases {
		name := tc.addr
		if tc.secured {
			name += "+secured"
		}
		if tc.insecureNoAuth {
			name += "+insecure"
		}
		t.Run(name, func(t *testing.T) {
			err := checkListenAddr(tc.addr, tc.secured, tc.insecureNoAuth)
			if tc.wantErr && err == nil {
				t.Fatalf("want error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}
