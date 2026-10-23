package server

import (
	"context"
	"errors"
	"testing"

	"github.com/dal-go/record"

	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/manifest"
	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
)

// The capabilities map of a database descriptor. The helpers of this file all start
// with capabilities so they cannot clash with the others of the package.

// capabilitiesNativePostgresCollection is the logical ID of the one table a native
// read-only mount in the tests maps.
func capabilitiesNativePostgresCollection(t *testing.T) string {
	t.Helper()
	id, err := schema.NativePostgresCollectionID("public", "orders")
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// capabilitiesNativePostgres is a PostgreSQL mount that is read-only: the shape of
// a native read-only mount, with one mapped table.
func capabilitiesNativePostgres(t *testing.T, id string) *core.Database {
	t.Helper()
	db, err := core.Open(&manifest.Manifest{
		Database: manifest.Database{ID: id, SchemaMode: schema.ModeStrict},
		Storage:  manifest.Storage{Engine: "postgres", Postgres: &manifest.PostgresOptions{ReadOnly: true}},
		Schemas: &schema.Schemas{Collections: map[string]schema.Collection{
			capabilitiesNativePostgresCollection(t): {Source: &schema.NativeCollectionSource{Schema: "public", Name: "orders"}},
		}},
	}, relFakeDB{}, []schema.Mode{schema.ModeStrict}, t.TempDir()+"/inferred.json")
	if err != nil {
		t.Fatal(err)
	}
	return db
}

// capabilitiesHTTP is an HTTP source mount, which is read-only by construction. It
// uses the IANA profile, whose three native fields the manifest declares.
func capabilitiesHTTP(t *testing.T, id string) *core.Database {
	t.Helper()
	db, err := core.Open(&manifest.Manifest{
		Database: manifest.Database{ID: id, SchemaMode: schema.ModeStrict},
		Storage:  manifest.Storage{Engine: "http", HTTP: &manifest.HTTPOptions{Profile: manifest.HTTPProfileIANAHTTPStatus, Collection: "rows"}},
		Schemas: &schema.Schemas{Collections: map[string]schema.Collection{"rows": {Fields: map[string]schema.Field{
			"Value": {Type: schema.TypeString}, "Description": {Type: schema.TypeString}, "Reference": {Type: schema.TypeString},
		}}}},
	}, relFakeDB{}, []schema.Mode{schema.ModeStrict}, "")
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func capabilitiesFlag(t *testing.T, caps map[string]bool, name string) bool {
	t.Helper()
	value, ok := caps[name]
	if !ok {
		t.Fatalf("capabilities has no %q: %v", name, caps)
	}
	return value
}

// TestDatabaseCapabilitiesReadWriteDeleteResetPersistent: read is whether keyed
// record reads are served, so it is false for a read-only mount; write and delete
// are the same flag, false under the server-wide switch or for a read-only mount;
// reset is false and persistent is true for a server-hosted database. Query and
// tugql do not depend on read.
func TestDatabaseCapabilitiesReadWriteDeleteResetPersistent(t *testing.T) {
	for _, c := range []struct {
		name      string
		serverRO  bool
		db        *core.Database
		wantRead  bool
		wantWrite bool
		wantTugQL bool
	}{
		{"writable database", false, relFakeMount("alpha", "sqlite", ""), true, true, true},
		{"server --read-only", true, relFakeMount("alpha", "sqlite", ""), true, false, true},
		{"native postgres read-only", false, capabilitiesNativePostgres(t, "pg"), false, false, true},
		{"native postgres read-only under server --read-only", true, capabilitiesNativePostgres(t, "pg"), false, false, true},
		{"http read-only mount", false, capabilitiesHTTP(t, "h"), false, false, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := New("test", map[string]*core.Database{c.db.ID(): c.db}, WithReadOnly(c.serverRO))
			caps := s.databaseCapabilities(c.db)
			if got := capabilitiesFlag(t, caps, "read"); got != c.wantRead {
				t.Errorf("read = %v, want %v", got, c.wantRead)
			}
			if got := capabilitiesFlag(t, caps, "write"); got != c.wantWrite {
				t.Errorf("write = %v, want %v", got, c.wantWrite)
			}
			if got := capabilitiesFlag(t, caps, "delete"); got != c.wantWrite {
				t.Errorf("delete = %v, want %v (the same flag as write)", got, c.wantWrite)
			}
			if got := capabilitiesFlag(t, caps, "tugql"); got != c.wantTugQL {
				t.Errorf("tugql = %v, want %v", got, c.wantTugQL)
			}
			if capabilitiesFlag(t, caps, "reset") {
				t.Errorf("reset = true, want false for a server-hosted database")
			}
			if !capabilitiesFlag(t, caps, "persistent") {
				t.Errorf("persistent = false, want true")
			}
		})
	}
}

// TestDatabaseCapabilitiesReadMatchesKeyedReads: the read flag is the behaviour of
// Get. On a read-only mount Get refuses with the mount's unsupported error, which is
// exactly when read is false. On a mount that serves keyed reads Get does not refuse
// that way, so read is true.
func TestDatabaseCapabilitiesReadMatchesKeyedReads(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name       string
		db         *core.Database
		collection string // declared on a read-only mount, undeclared on the others
		refusal    error
	}{
		{"native postgres read-only", capabilitiesNativePostgres(t, "pg"), capabilitiesNativePostgresCollection(t), core.ErrNativePostgresOperationUnsupported},
		{"http read-only mount", capabilitiesHTTP(t, "h"), "rows", core.ErrHTTPOperationUnsupported},
		// A writable mount is not refused by the read-only check. Its collection is
		// undeclared, so Get stops at the declaration guard before the driver.
		{"writable database", relFakeMount("alpha", "sqlite", ""), "undeclared", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := New("test", map[string]*core.Database{c.db.ID(): c.db})
			read := capabilitiesFlag(t, s.databaseCapabilities(c.db), "read")
			_, err := c.db.Get(ctx, record.NewKeyWithID(c.collection, "1"))
			refused := c.refusal != nil && errors.Is(err, c.refusal)
			if refused == read {
				t.Errorf("read = %v but Get refused with the unsupported error = %v (err %v)", read, refused, err)
			}
			if !read && !refused {
				t.Errorf("read is false but Get did not refuse with %v: %v", c.refusal, err)
			}
		})
	}
}

// TestDatabaseCapabilitiesNilDatabase: a listed id without a mount is described as
// before: read and writable unless the server is read-only.
func TestDatabaseCapabilitiesNilDatabase(t *testing.T) {
	for _, c := range []struct {
		serverRO bool
		want     bool
	}{{false, true}, {true, false}} {
		s := New("test", map[string]*core.Database{}, WithReadOnly(c.serverRO))
		caps := s.databaseCapabilities(nil)
		if !capabilitiesFlag(t, caps, "read") || capabilitiesFlag(t, caps, "query") || capabilitiesFlag(t, caps, "tugql") {
			t.Errorf("nil database: %v", caps)
		}
		if capabilitiesFlag(t, caps, "write") != c.want || capabilitiesFlag(t, caps, "delete") != c.want {
			t.Errorf("server read-only %v: write/delete %v, want %v", c.serverRO, caps, c.want)
		}
	}
}

// TestDatabaseDescriptorsCarryTheCapabilities: the metadata of a database and the
// well-known document publish the same map, read-only mounts included.
func TestDatabaseDescriptorsCarryTheCapabilities(t *testing.T) {
	mounts := []*core.Database{relFakeMount("alpha", "sqlite", ""), capabilitiesNativePostgres(t, "pg")}
	_, host := relFakeServer(t, &relFakeExecutor{}, mounts)
	wellKnown := discoveryGet(t, host, "/.well-known/openvaultdb")
	for _, id := range []string{"alpha", "pg"} {
		meta := discoveryObject(t, discoveryGet(t, host, "/v1/databases/"+id), "capabilities")
		entry := discoveryObject(t, discoveryDatabase(t, wellKnown, id), "capabilities")
		for _, name := range []string{"read", "write", "delete", "reset", "persistent"} {
			if _, ok := meta[name].(bool); !ok {
				t.Errorf("%s: metadata capabilities.%s is not a boolean: %v", id, name, meta)
			}
			if meta[name] != entry[name] {
				t.Errorf("%s: %s is %v in metadata and %v in the well-known document", id, name, meta[name], entry[name])
			}
		}
		if meta["reset"] != false || meta["persistent"] != true {
			t.Errorf("%s: reset/persistent = %v/%v", id, meta["reset"], meta["persistent"])
		}
	}
	pg := discoveryObject(t, discoveryGet(t, host, "/v1/databases/pg"), "capabilities")
	if pg["read"] != false || pg["write"] != false || pg["delete"] != false {
		t.Errorf("native postgres read/write/delete = %v/%v/%v, want false", pg["read"], pg["write"], pg["delete"])
	}
}
