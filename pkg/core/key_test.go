package core

import (
	"fmt"
	"testing"
)

// ---------- ParseKey tests (table-driven) ----------

func TestParseKey(t *testing.T) {
	tests := []struct {
		name         string
		segments     []string
		wantErr      bool
		leafCol      string
		leafID       string
		parentCol    string
		parentID     string
		wantNoParent bool
	}{
		{
			name:         "valid root",
			segments:     []string{"users", "u1"},
			wantErr:      false,
			leafCol:      "users",
			leafID:       "u1",
			wantNoParent: true,
		},
		{
			name:      "valid nested",
			segments:  []string{"orgs", "o1", "members", "m1"},
			wantErr:   false,
			leafCol:   "members",
			leafID:    "m1",
			parentCol: "orgs",
			parentID:  "o1",
		},
		{
			name:     "odd segment count 1",
			segments: []string{"users"},
			wantErr:  true,
		},
		{
			name:     "odd segment count 3",
			segments: []string{"a", "b", "c"},
			wantErr:  true,
		},
		{
			name:     "zero segments",
			segments: []string{},
			wantErr:  true,
		},
		{
			name:     "empty segment",
			segments: []string{"users", ""},
			wantErr:  true,
		},
		{
			name:     "dot segment",
			segments: []string{"users", "."},
			wantErr:  true,
		},
		{
			name:     "dotdot segment",
			segments: []string{"users", ".."},
			wantErr:  true,
		},
		{
			name:     "dotdot in collection",
			segments: []string{"..", "id"},
			wantErr:  true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			key, err := ParseKey(tc.segments...)
			if tc.wantErr {
				if err == nil {
					t.Errorf("expected error but got nil (key=%v)", key)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if key.Collection() != tc.leafCol {
				t.Errorf("Collection(): expected %q, got %q", tc.leafCol, key.Collection())
			}
			if fmt.Sprintf("%v", key.ID) != tc.leafID {
				t.Errorf("ID: expected %q, got %q", tc.leafID, fmt.Sprintf("%v", key.ID))
			}
			if tc.wantNoParent {
				if key.Parent() != nil {
					t.Errorf("expected nil parent, got %v", key.Parent())
				}
			} else {
				p := key.Parent()
				if p == nil {
					t.Fatal("expected non-nil parent")
				}
				if p.Collection() != tc.parentCol {
					t.Errorf("Parent().Collection(): expected %q, got %q", tc.parentCol, p.Collection())
				}
				if fmt.Sprintf("%v", p.ID) != tc.parentID {
					t.Errorf("Parent().ID: expected %q, got %q", tc.parentID, fmt.Sprintf("%v", p.ID))
				}
			}
		})
	}
}

// ---------- ParseKeyPath tests ----------

func TestParseKeyPath(t *testing.T) {
	t.Run("simple two-segment", func(t *testing.T) {
		key, err := ParseKeyPath("users/u1")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if key.Collection() != "users" {
			t.Errorf("expected Collection() == \"users\", got %q", key.Collection())
		}
		if fmt.Sprintf("%v", key.ID) != "u1" {
			t.Errorf("expected ID == \"u1\", got %q", fmt.Sprintf("%v", key.ID))
		}
		if key.Parent() != nil {
			t.Errorf("expected nil parent, got %v", key.Parent())
		}
	})

	t.Run("percent-encoded slash inside ID", func(t *testing.T) {
		key, err := ParseKeyPath("users/foo%2Fbar")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if key.Collection() != "users" {
			t.Errorf("expected Collection() == \"users\", got %q", key.Collection())
		}
		if fmt.Sprintf("%v", key.ID) != "foo/bar" {
			t.Errorf("expected ID == \"foo/bar\", got %q", fmt.Sprintf("%v", key.ID))
		}
	})

	t.Run("nested path", func(t *testing.T) {
		key, err := ParseKeyPath("orgs/o1/projects/p1")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if key.Collection() != "projects" {
			t.Errorf("expected leaf Collection() == \"projects\", got %q", key.Collection())
		}
		if fmt.Sprintf("%v", key.ID) != "p1" {
			t.Errorf("expected leaf ID == \"p1\", got %q", fmt.Sprintf("%v", key.ID))
		}
		p := key.Parent()
		if p == nil {
			t.Fatal("expected non-nil parent")
		}
		if p.Collection() != "orgs" {
			t.Errorf("expected Parent().Collection() == \"orgs\", got %q", p.Collection())
		}
		if fmt.Sprintf("%v", p.ID) != "o1" {
			t.Errorf("expected Parent().ID == \"o1\", got %q", fmt.Sprintf("%v", p.ID))
		}
	})

	t.Run("odd segment count via path", func(t *testing.T) {
		if _, err := ParseKeyPath("users/u1/sub"); err == nil {
			t.Error("expected error for odd-segment path")
		}
	})

	t.Run("dotdot in path", func(t *testing.T) {
		if _, err := ParseKeyPath("users/.."); err == nil {
			t.Error("expected error for \"..\" in path")
		}
	})

	t.Run("empty segment in path", func(t *testing.T) {
		if _, err := ParseKeyPath("users//id"); err == nil {
			t.Error("expected error for empty middle segment")
		}
	})
}
