package server

import (
	"testing"

	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/manifest"
	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
)

// The capabilities map of a database descriptor. The helpers of this file all start
// with capabilities so they cannot clash with the others of the package.

// capabilitiesNativePostgres is a PostgreSQL mount that the server reads without
// writing: the shape of a native read-only mount, with one mapped table.
func capabilitiesNativePostgres(t *testing.T, id string) *core.Database {
	t.Helper()
	orders, err := schema.NativePostgresCollectionID("public", "orders")
	if err != nil {
		t.Fatal(err)
	}
	db, err := core.Open(&manifest.Manifest{
		Database: manifest.Database{ID: id, SchemaMode: schema.ModeStrict},
		Storage:  manifest.Storage{Engine: "postgres", Postgres: &manifest.PostgresOptions{ReadOnly: true}},
		Schemas: &schema.Schemas{Collections: map[string]schema.Collection{
			orders: {Source: &schema.NativeCollectionSource{Schema: "public", Name: "orders"}},
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

// TestDatabaseCapabilitiesReadWriteDeleteResetPersistent: read is true for every
// served database, whatever its ReadOnly(); write and delete are the same flag and
// are false under the server-wide switch or for a read-only mount; reset is false
// and persistent is true for a server-hosted database.
func TestDatabaseCapabilitiesReadWriteDeleteResetPersistent(t *testing.T) {
	for _, c := range []struct {
		name       string
		serverRO   bool
		db         *core.Database
		wantWrite  bool
		wantRead   bool
		wantDelete bool
	}{
		{"writable database", false, relFakeMount("alpha", "sqlite", ""), true, true, true},
		{"server --read-only", true, relFakeMount("alpha", "sqlite", ""), false, true, false},
		{"native postgres read-only", false, capabilitiesNativePostgres(t, "pg"), false, true, false},
		{"native postgres read-only under server --read-only", true, capabilitiesNativePostgres(t, "pg"), false, true, false},
		{"http read-only mount", false, capabilitiesHTTP(t, "h"), false, true, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := New("test", map[string]*core.Database{c.db.ID(): c.db}, WithReadOnly(c.serverRO))
			caps := s.databaseCapabilities(c.db)
			if got := capabilitiesFlag(t, caps, "write"); got != c.wantWrite {
				t.Errorf("write = %v, want %v", got, c.wantWrite)
			}
			if got := capabilitiesFlag(t, caps, "delete"); got != c.wantDelete {
				t.Errorf("delete = %v, want %v", got, c.wantDelete)
			}
			if got := capabilitiesFlag(t, caps, "read"); got != c.wantRead {
				t.Errorf("read = %v, want %v", got, c.wantRead)
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

// TestDatabaseCapabilitiesNilDatabase: a listed id without a mount is described as
// before: read, and writable unless the server is read-only.
func TestDatabaseCapabilitiesNilDatabase(t *testing.T) {
	for _, c := range []struct {
		serverRO bool
		want     bool
	}{{false, true}, {true, false}} {
		s := New("test", map[string]*core.Database{}, WithReadOnly(c.serverRO))
		caps := s.databaseCapabilities(nil)
		if !capabilitiesFlag(t, caps, "read") || capabilitiesFlag(t, caps, "query") || capabilitiesFlag(t, caps, "dtql") {
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
	_, host := relFakeServer(t, &relFakeExecutor{}, mounts, WithReadOnly(false))
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
		if meta["read"] != true || meta["reset"] != false || meta["persistent"] != true {
			t.Errorf("%s: read/reset/persistent = %v/%v/%v", id, meta["read"], meta["reset"], meta["persistent"])
		}
	}
	if pg := discoveryObject(t, discoveryGet(t, host, "/v1/databases/pg"), "capabilities"); pg["write"] != false || pg["delete"] != false {
		t.Errorf("native postgres write/delete = %v/%v, want false", pg["write"], pg["delete"])
	}
}
