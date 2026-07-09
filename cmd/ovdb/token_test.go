package main

import (
	"testing"
)

func TestScopeCapabilities(t *testing.T) {
	for _, tc := range []struct {
		scope   string
		want    []string
		wantErr bool
	}{
		{
			scope: "read-only",
			want:  []string{"records:read", "collections:read", "schema:read"},
		},
		{
			scope: "read-write",
			want:  []string{"records:read", "collections:read", "schema:read", "records:write", "records:delete"},
		},
		{
			scope: "", // default = read-write
			want:  []string{"records:read", "collections:read", "schema:read", "records:write", "records:delete"},
		},
		{
			scope:   "superuser",
			wantErr: true,
		},
	} {
		caps, err := scopeCapabilities(tc.scope)
		if tc.wantErr {
			if err == nil {
				t.Errorf("scope %q: want error, got %v", tc.scope, caps)
			}
			continue
		}
		if err != nil {
			t.Errorf("scope %q: %v", tc.scope, err)
			continue
		}
		if len(caps) != len(tc.want) {
			t.Errorf("scope %q: got %v, want %v", tc.scope, caps, tc.want)
			continue
		}
		for i, c := range caps {
			if c != tc.want[i] {
				t.Errorf("scope %q: caps[%d] = %q, want %q", tc.scope, i, c, tc.want[i])
			}
		}
	}
}

func TestServerBase(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"127.0.0.1:6832", "http://127.0.0.1:6832"},
		{"http://127.0.0.1:6832", "http://127.0.0.1:6832"},
		{"http://127.0.0.1:6832/", "http://127.0.0.1:6832"},
		{"https://example.com/", "https://example.com"},
	} {
		if got := serverBase(tc.in); got != tc.want {
			t.Errorf("serverBase(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
