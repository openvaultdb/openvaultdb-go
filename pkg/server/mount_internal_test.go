package server

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/manifest"
)

// TestMountUnmount_WaitsForInFlight holds a request lease and proves Unmount
// neither returns nor closes the database until the request finishes.
func TestMountUnmount_WaitsForInFlight(t *testing.T) {
	db := &core.Database{Manifest: &manifest.Manifest{Database: manifest.Database{ID: "x"}}}
	closed := make(chan struct{})
	db.OnClose(func() error { close(closed); return nil })
	s := New("test", nil)
	if err := s.Mount(db); err != nil {
		t.Fatal(err)
	}
	held := &leases{}
	r := httptest.NewRequest("GET", "/v1/databases/x", nil)
	r = r.WithContext(context.WithValue(r.Context(), leasesKey{}, held))
	if s.acquire(r, "x") != db {
		t.Fatal("acquire did not return the mounted database")
	}

	done := make(chan error)
	go func() { done <- s.Unmount("x") }()

	deadline := time.After(2 * time.Second)
	for s.getDB("x") != nil { // unrouted at once
		select {
		case <-deadline:
			t.Fatal("database still routed during Unmount")
		case <-time.After(time.Millisecond):
		}
	}
	if s.acquire(r, "x") != nil {
		t.Fatal("new request acquired an unmounting database")
	}
	select {
	case <-done:
		t.Fatal("Unmount returned while a request was in flight")
	case <-closed:
		t.Fatal("database closed while a request was in flight")
	case <-time.After(100 * time.Millisecond):
	}
	held.release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Unmount did not return after the request finished")
	}
	select {
	case <-closed:
	default:
		t.Fatal("Unmount did not close the database")
	}
}

// TestMountUnmountContext_ExpiresThenClosesInBackground: when ctx ends before
// in-flight requests finish, UnmountContext returns ctx.Err() with the
// database unrouted and still open; Close follows once the request ends.
func TestMountUnmountContext_ExpiresThenClosesInBackground(t *testing.T) {
	db := &core.Database{Manifest: &manifest.Manifest{Database: manifest.Database{ID: "x"}}}
	closed := make(chan struct{})
	db.OnClose(func() error { close(closed); return nil })
	s := New("test", nil)
	if err := s.Mount(db); err != nil {
		t.Fatal(err)
	}
	held := &leases{}
	r := httptest.NewRequest("GET", "/v1/databases/x", nil)
	r = r.WithContext(context.WithValue(r.Context(), leasesKey{}, held))
	if s.acquire(r, "x") != db {
		t.Fatal("acquire did not return the mounted database")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := s.UnmountContext(ctx, "x"); err != context.DeadlineExceeded {
		t.Fatalf("UnmountContext = %v, want context.DeadlineExceeded", err)
	}
	if s.getDB("x") != nil {
		t.Fatal("database still routed after UnmountContext expired")
	}
	select {
	case <-closed:
		t.Fatal("database closed while a request was in flight")
	default:
	}
	if err := s.Unmount("x"); err == nil {
		t.Fatal("second Unmount of an expired-unmount id succeeded")
	}
	held.release()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("database not closed in background after the request finished")
	}
}

func TestMountUnmountContext_ReturnsCloseResultWhenDrained(t *testing.T) {
	db := &core.Database{Manifest: &manifest.Manifest{Database: manifest.Database{ID: "y"}}}
	wantErr := errors.New("close failed")
	db.OnClose(func() error { return wantErr })
	s := New("test", nil)
	if err := s.Mount(db); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := s.UnmountContext(ctx, "y"); !errors.Is(err, wantErr) {
		t.Fatalf("UnmountContext = %v, want %v", err, wantErr)
	}
}
