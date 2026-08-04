package main

import "testing"

// TestCheckListenAddr pins down the loopback-only posture. Every
// non-loopback shape (empty host = wildcard bind, 0.0.0.0, ...) is
// refused unless --insecure-no-auth is set; the documented loopback
// shapes (127.0.0.0/8, ::1, "localhost") always pass.
func TestCheckListenAddr(t *testing.T) {
	cases := []struct {
		addr            string
		insecureNoAuth  bool
		wantErr         bool
	}{
		{"127.0.0.1:8471", false, false},
		{"localhost:1", false, false},
		{"[::1]:1", false, false},
		{":8494", false, true},
		{"0.0.0.0:1", false, true},

		{"127.0.0.1:8471", true, false},
		{"localhost:1", true, false},
		{"[::1]:1", true, false},
		{":8494", true, false},
		{"0.0.0.0:1", true, false},
	}
	for _, tc := range cases {
		name := tc.addr
		if tc.insecureNoAuth {
			name += "+insecure"
		}
		t.Run(name, func(t *testing.T) {
			err := checkListenAddr(tc.addr, tc.insecureNoAuth)
			if tc.wantErr && err == nil {
				t.Fatalf("want error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}