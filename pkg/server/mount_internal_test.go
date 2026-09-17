package server

import (
	"context"
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
