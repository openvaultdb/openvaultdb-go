// Command layered-acl starts a local, authenticated query demonstration with
// real SQLite and InGitDB storage. It creates fixtures in a fresh directory.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/dal-go/dalgo/access"
	"github.com/dal-go/record"
	"github.com/openvaultdb/openvaultdb-go/pkg/auth"
	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/mount"
	"github.com/openvaultdb/openvaultdb-go/pkg/server"
)

var (
	logFatalf = log.Fatalf
	mkdirTemp = os.MkdirTemp
	readDir   = os.ReadDir
	writeFile = os.WriteFile
	mountFile = mount.File
	coreApply = func(db *core.Database, ctx context.Context, ops []core.Op, message string) error {
		_, err := db.Apply(ctx, ops, message)
		return err
	}
	storeCreateGrant = func(store *auth.Store, grant *auth.Grant, token string) error {
		return store.CreateGrant(grant, token)
	}
	serveHTTP = func(srv *http.Server) error {
		return srv.ListenAndServe()
	}
)

func main() {
	dir := flag.String("dir", "", "fresh fixture directory (default: temporary directory)")
	reuse := flag.Bool("reuse", false, "remount existing fixtures without seeding or changing policies")
	listen := flag.String("listen", "127.0.0.1:8899", "HTTP listen address")
	flag.Parse()
	if err := run(*dir, *reuse, *listen); err != nil {
		logFatalf("%v", err)
	}
}

func run(dir string, reuse bool, listen string) error {
	token := os.Getenv("OVDB_OWNER_TOKEN")
	queryToken := os.Getenv("OVDB_QUERY_TOKEN")
	if token == "" || queryToken == "" || token == queryToken {
		return fmt.Errorf("set distinct OVDB_OWNER_TOKEN and OVDB_QUERY_TOKEN credentials")
	}
	if dir == "" {
		var err error
		dir, err = mkdirTemp("", "ovdb-layered-acl-")
		if err != nil {
			return err
		}
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	entries, err := readDir(dir)
	if err != nil {
		return err
	}
	if len(entries) != 0 && !reuse {
		return fmt.Errorf("fixture directory must be empty; existing data is never overwritten")
	}
	if !reuse {
		for _, engine := range []string{"ingitdb", "sqlite"} {
			if err := seed(dir, engine); err != nil {
				return err
			}
		}
	}
	dbs, err := mount.Dir(dir)
	if err != nil {
		return err
	}
	if len(dbs) != 2 || dbs["ingitdb"] == nil || dbs["sqlite"] == nil {
		return fmt.Errorf("fixture directory must contain the ingitdb and sqlite manifests")
	}
	store, err := auth.OpenStore(filepath.Join(dir, "auth.json"))
	if err != nil {
		return err
	}
	subject := access.PrincipalRef{Realm: "local-demo", Kind: access.PrincipalKindUser, ID: "demo-user"}
	actor := access.PrincipalRef{Realm: "local-demo", Kind: access.PrincipalKindApplication, ID: "datatug-demo"}
	if reuse && store.Lookup(queryToken) == nil {
		return fmt.Errorf("query credential is missing, expired or revoked; reuse does not issue credentials")
	}
	if store.Lookup(queryToken) == nil {
		// This fixture server hosts exactly two databases. The query credential
		// has customer read/write capability, intersected with each owner's policy.
		if err := storeCreateGrant(store, &auth.Grant{Subject: &subject, Actor: &actor, Capabilities: []auth.Capability{{Action: auth.CapRecordsRead, Collection: "customers"}, {Action: auth.CapRecordsWrite, Collection: "customers"}, {Action: auth.CapAccessDiagnostics, Collection: "customers"}}}, queryToken); err != nil {
			return err
		}
	}
	handler := server.New("layered-acl-demo", dbs, server.WithAuth(&auth.Config{OwnerToken: token, Store: store}), server.WithGrantIdentity(server.GrantIdentityConfig{
		Bootstrap: access.PrincipalRef{Realm: "local-demo", Kind: access.PrincipalKindService, ID: "demo-bootstrap"},
		Resolve: func(_ context.Context, ref access.PrincipalRef) (server.Membership, error) {
			if ref == subject {
				return server.Membership{Roles: []string{"reader"}, Revision: "fixture-1"}, nil
			}
			return server.Membership{}, nil
		},
	})).Handler()
	log.Printf("fixtures: %s; DTQL: http://%s/v1/databases/{ingitdb,sqlite}/dtql", dir, listen)
	srv := &http.Server{Addr: listen, Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 15 * time.Second}
	return serveHTTP(srv)
}

func write(root, name, text string) error {
	path := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	return writeFile(path, []byte(text), 0600)
}

func policy(database, name, field, value string) string {
	fields := "id, name"
	if database == "ingitdb" {
		fields += ", $id"
	}
	return fmt.Sprintf(`apiVersion: dtql.org/access/v1
kind: AccessPolicy
metadata: {name: %s}
target: {database: %s}
composition: dalgo-hierarchical-v1
default: deny
ruleSets:
  reader:
    - path: /customers
      rules:
        - id: visible-customers
          effect: allow
          operations: [query, get, update]
          where:
            op: "=="
            left: {field: %s}
            right: {value: %s}
          fields: [%s]
bindings:
  roles: {reader: [reader]}
`, name, database, field, value, fields)
}

func seed(dir, engine string) error {
	storage := engine + "-data"
	if engine == "sqlite" {
		storage += ".sqlite"
	}
	manifest := fmt.Sprintf(`database: {id: %s, schema_mode: strict}
storage: {engine: %s, path: %s}
schemas:
  collections:
    customers:
      fields:
        name: {type: string}
        tenant: {type: string}
        country: {type: string}
        secret: {type: string}
`, engine, engine, storage)
	name := engine + ".yaml"
	if err := write(dir, name, manifest); err != nil {
		return err
	}
	db, err := mountFile(filepath.Join(dir, name))
	if err != nil {
		return err
	}
	for _, row := range []struct{ id, tenant, country string }{{"01", "B", "IE"}, {"02", "A", "US"}, {"03", "A", "IE"}} {
		err = coreApply(db, context.Background(), []core.Op{{Op: "insert", Key: record.NewKeyWithID("customers", row.id), Data: map[string]any{"name": "Customer " + row.id, "tenant": row.tenant, "country": row.country, "secret": "hidden"}}}, "demo seed")
		if err != nil {
			return err
		}
	}
	policyPath := "policies/" + engine + "-upper.yaml"
	if err := write(dir, policyPath, policy(engine, "ireland", "country", "IE")); err != nil {
		return err
	}
	if err := write(dir, name, manifest+fmt.Sprintf("acl:\n  enabled: true\n  realm: local-demo\n  policies: [%s]\n", policyPath)); err != nil {
		return err
	}
	if engine == "ingitdb" {
		root := filepath.Join(dir, storage, ".ingitdb", "access")
		if err := write(root, "tenant.yaml", policy(engine, "tenant-a", "tenant", "A")); err != nil {
			return err
		}
		return write(root, "manifest.yaml", "enabled: true\nrealm: local-demo\ndatabase: ingitdb\npolicies: [tenant.yaml]\n")
	}
	return nil
}
