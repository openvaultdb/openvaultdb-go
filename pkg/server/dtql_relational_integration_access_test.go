package server_test

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dal-go/dalgo/access"
	"github.com/dal-go/dalgo/dtql"
	"github.com/dal-go/record"

	"github.com/openvaultdb/openvaultdb-go/pkg/auth"
	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/mount"
	"github.com/openvaultdb/openvaultdb-go/pkg/server"
)

// A relational document is not run on a database with access policies, whatever it
// names. These tests hold that to the mounts of examples/layered-acl, which are
// rebuilt here from the manifests, rows and policies the example seeds: a SQLite
// mount and a local inGitDB mount (with its second layer of policies) of the
// collection customers, each with a policy that lets the role reader read the
// customers of one country in the fields id and name.

// relIntLayeredRows are the customers the example seeds, as id, tenant and country.
var relIntLayeredRows = []struct{ id, tenant, country string }{{"01", "B", "IE"}, {"02", "A", "US"}, {"03", "A", "IE"}}

// relIntLayeredPolicy is the policy file of examples/layered-acl.
func relIntLayeredPolicy(database, name, field, value string) string {
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

// relIntLayeredMounts seeds the two databases of the example in a fresh directory
// and mounts them as the example does, with the same ids: sqlite and ingitdb.
func relIntLayeredMounts(t *testing.T) map[string]*core.Database {
	t.Helper()
	dir := t.TempDir()
	write := func(name, text string) {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, engine := range []string{"ingitdb", "sqlite"} {
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
		write(engine+".yaml", manifest)
		seed, err := mount.File(filepath.Join(dir, engine+".yaml"))
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range relIntLayeredRows {
			op := core.Op{Op: "insert", Key: record.NewKeyWithID("customers", row.id), Data: map[string]any{
				"name": "Customer " + row.id, "tenant": row.tenant, "country": row.country, "secret": "hidden"}}
			if _, err := seed.Apply(context.Background(), []core.Op{op}, "seed"); err != nil {
				t.Fatal(err)
			}
		}
		if err := seed.Close(); err != nil {
			t.Fatal(err)
		}
		policyPath := "policies/" + engine + "-upper.yaml"
		write(policyPath, relIntLayeredPolicy(engine, "ireland", "country", "IE"))
		write(engine+".yaml", manifest+fmt.Sprintf("acl:\n  enabled: true\n  realm: local-demo\n  policies: [%s]\n", policyPath))
		if engine == "ingitdb" {
			root := filepath.Join(storage, ".ingitdb", "access")
			write(filepath.Join(root, "tenant.yaml"), relIntLayeredPolicy(engine, "tenant-a", "tenant", "A"))
			write(filepath.Join(root, "manifest.yaml"), "enabled: true\nrealm: local-demo\ndatabase: ingitdb\npolicies: [tenant.yaml]\n")
		}
	}
	mounts, err := mount.Dir(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, db := range mounts {
			_ = db.Close()
		}
	})
	for _, id := range []string{"sqlite", "ingitdb"} {
		if mounts[id] == nil || !mounts[id].HasAccessPolicies() {
			t.Fatalf("the mount %s is missing or has no access policies", id)
		}
	}
	return mounts
}

// The tokens of the two principals of the access tests, who read every database.
// What each may read within one is the mount's policy: alice is a member of the
// role reader, bob of none, and the owner, who acts as the bootstrap identity the
// example configures, of none either.
const (
	relIntAlice = "token-of-alice"
	relIntBob   = "token-of-bob"
)

var (
	relIntApplication = access.PrincipalRef{Realm: "local-demo", Kind: access.PrincipalKindApplication, ID: "datatug-demo"}
	relIntAliceRef    = access.PrincipalRef{Realm: "local-demo", Kind: access.PrincipalKindUser, ID: "alice"}
	relIntBobRef      = access.PrincipalRef{Realm: "local-demo", Kind: access.PrincipalKindUser, ID: "bob"}
)

// relIntLayeredServer serves the mounts of the example next to a public SQLite mount
// (directory) to the owner and to two principals that hold a grant of the read
// capability on every database, with the identity wiring of the example.
func relIntLayeredServer(t *testing.T) string {
	t.Helper()
	store, err := auth.OpenStore(filepath.Join(t.TempDir(), "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	for token, subject := range map[string]access.PrincipalRef{relIntAlice: relIntAliceRef, relIntBob: relIntBobRef} {
		grant := &auth.Grant{Subject: &subject, Actor: &relIntApplication, Capabilities: []auth.Capability{{Action: auth.CapRecordsRead}}}
		if err := store.CreateGrant(grant, token); err != nil {
			t.Fatal(err)
		}
	}
	mounts := relIntLayeredMounts(t)
	mounts["directory"] = relHTTPMount(t, "directory", "", map[string][]string{"Country": {"code", "name"}},
		`CREATE TABLE "Country" ("id" TEXT PRIMARY KEY, "code" TEXT, "name" TEXT)`,
		`INSERT INTO "Country" VALUES ('1', 'IE', 'Ireland'), ('2', 'US', 'United States')`)
	return relIntServe(t, mounts,
		server.WithAuth(&auth.Config{OwnerToken: ownerToken, Store: store}),
		server.WithGrantIdentity(server.GrantIdentityConfig{
			Bootstrap: access.PrincipalRef{Realm: "local-demo", Kind: access.PrincipalKindService, ID: "demo-bootstrap"},
			Resolve: func(_ context.Context, ref access.PrincipalRef) (server.Membership, error) {
				if ref == relIntAliceRef {
					return server.Membership{Roles: []string{"reader"}, Revision: "fixture-1"}, nil
				}
				return server.Membership{}, nil
			},
		}))
}

var relIntCallers = []struct{ name, token string }{{"the owner", ownerToken}, {"alice", relIntAlice}, {"bob", relIntBob}}

// Every document of DALgo's join and subquery fixtures that the classifier accepts is a
// 422 authorization_unsupported on the mounts of examples/layered-acl, with one body
// for each mount whatever the document names: alone, joined to a public mount (the
// protected source first and second), on the per-database endpoint and on /v1/dtql,
// for the owner and for two principals. A document the classifier refuses is the 400
// it is on any mount. No answer holds a row.
func TestEveryRelationalShapeOfTheFixturesIsRefusedOnALayeredACLMount(t *testing.T) {
	base := relIntLayeredServer(t)
	dalgo := relIntDalgoDir(t)
	var cases []relIntCase
	for _, corpus := range []string{"joins", "subqueries"} {
		cases = append(cases, relIntLoadCases(t, corpus, filepath.Join(dalgo, "dtql", "testdata", corpus))...)
	}
	baseline := map[string]string{}
	accepted, refused := 0, 0
	for _, c := range cases {
		// The classifier's verdict on the document, which names its database the way
		// /v1/dtql needs.
		named := relIntRewrite(t, c.doc, "sqlite")
		query, err := dtql.Deserialize(named)
		if err == nil {
			_, err = core.ClassifyDTQL(query)
		}
		sources := relIntCountSources(t, c.doc)
		for _, protected := range []string{"sqlite", "ingitdb"} {
			type shape struct {
				name, path string
				doc        []byte
			}
			shapes := []shape{
				{"per-database endpoint", "/v1/databases/" + protected + "/dtql", relIntRewrite(t, c.doc, "")},
				{"alone on /v1/dtql", "/v1/dtql", relIntRewrite(t, c.doc, protected)},
			}
			if sources > 1 {
				shapes = append(shapes,
					shape{"joined to a public mount, protected first", "/v1/dtql", relIntRewriteWith(t, c.doc, func(i int) string {
						return map[bool]string{true: protected, false: "directory"}[i == 0]
					})},
					shape{"joined to a public mount, protected second", "/v1/dtql", relIntRewriteWith(t, c.doc, func(i int) string {
						return map[bool]string{true: "directory", false: protected}[i == 0]
					})})
			}
			for _, s := range shapes {
				for _, caller := range relIntCallers {
					t.Run(c.id+", "+protected+", "+s.name+", "+caller.name, func(t *testing.T) {
						resp := relHTTPDo(t, base, http.MethodPost, s.path, caller.token, string(s.doc), nil)
						if err != nil {
							refused++
							if resp.status != http.StatusBadRequest || resp.errorField("code") != "invalid_dtql" {
								t.Fatalf("a document the classifier refuses (%v) is a 400 invalid_dtql: status %d: %s", err, resp.status, resp.raw)
							}
							return
						}
						accepted++
						if resp.status != http.StatusUnprocessableEntity || resp.errorField("code") != "authorization_unsupported" || resp.body["records"] != nil {
							t.Fatalf("status %d, want a 422 authorization_unsupported with no rows: %s", resp.status, resp.raw)
						}
						if !strings.Contains(resp.errorField("message"), `database "`+protected+`" has access policies`) {
							t.Fatalf("the answer does not name the database %s: %s", protected, resp.raw)
						}
						if want, ok := baseline[protected]; !ok {
							baseline[protected] = resp.raw
						} else if resp.raw != want {
							t.Fatalf("body %s\nwant the body every other document gets for %s: %s", resp.raw, protected, want)
						}
					})
				}
			}
		}
	}
	if accepted == 0 || refused == 0 {
		t.Fatalf("%d documents were accepted and %d refused by the classifier: both kinds are in the fixtures", accepted, refused)
	}
}

// The mounts built above are the example's: the test fails when the example no
// longer seeds the rows, the policies and the realm this file rebuilds.
func TestTheLayeredACLMountsAreTheOnesTheExampleSeeds(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "examples", "layered-acl", "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	source := string(raw)
	for _, want := range []string{
		`{{"01", "B", "IE"}, {"02", "A", "US"}, {"03", "A", "IE"}}`,
		`policy(engine, "ireland", "country", "IE")`,
		`policy(engine, "tenant-a", "tenant", "A")`,
		`id: visible-customers`,
		`fields := "id, name"`,
		`fields += ", $id"`,
		`operations: [query, get, update]`,
		`realm: local-demo`,
		`Realm: "local-demo"`,
		`Roles: []string{"reader"}`,
		`name: {type: string}`,
		`country: {type: string}`,
	} {
		if !strings.Contains(source, want) {
			t.Errorf("examples/layered-acl/main.go no longer holds %q: update the mounts of this file to match", want)
		}
	}
}

// A single-collection read of a mount with access policies is still answered through
// them: each principal gets the customers its roles may read, in the fields its
// policy lists, and the owner, who holds no role of the example, and a principal with
// none get a 403 and no row. The inGitDB mount has a second layer of policy, so it
// shows the intersection of the two.
func TestASingleCollectionReadOfALayeredACLMountReturnsOnlyThePermittedRows(t *testing.T) {
	base := relIntLayeredServer(t)
	const doc = "from: {name: customers}\norderBy: [{field: name}]\n"
	for _, tc := range []struct {
		database, caller, token string
		status                  int
		keys                    []string
	}{
		{"sqlite", "alice", relIntAlice, http.StatusOK, []string{"customers/01", "customers/03"}},
		{"ingitdb", "alice", relIntAlice, http.StatusOK, []string{"customers/03"}},
		{"sqlite", "bob", relIntBob, http.StatusForbidden, nil},
		{"ingitdb", "bob", relIntBob, http.StatusForbidden, nil},
		{"sqlite", "the owner", ownerToken, http.StatusForbidden, nil},
		{"ingitdb", "the owner", ownerToken, http.StatusForbidden, nil},
	} {
		t.Run(tc.database+", "+tc.caller, func(t *testing.T) {
			resp := relHTTPPost(t, base, "/v1/databases/"+tc.database+"/dtql", tc.token, doc)
			if resp.status != tc.status {
				t.Fatalf("status %d, want %d: %s", resp.status, tc.status, resp.raw)
			}
			records, _ := resp.body["records"].([]any)
			var keys []string
			for _, rec := range records {
				entry := rec.(map[string]any)
				keys = append(keys, entry["key"].(string))
				for field := range entry["data"].(map[string]any) {
					if field != "id" && field != "$id" && field != "name" {
						t.Errorf("%v: the field %q is not one the policy lists", entry["key"], field)
					}
				}
			}
			if strings.Join(keys, ",") != strings.Join(tc.keys, ",") {
				t.Fatalf("keys = %v, want %v", keys, tc.keys)
			}
			for _, hidden := range []string{"hidden", "Customer 02"} {
				if strings.Contains(resp.raw, hidden) {
					t.Fatalf("the answer holds %q: %s", hidden, resp.raw)
				}
			}
		})
	}
	t.Run("a field the policy does not list is refused", func(t *testing.T) {
		resp := relHTTPPost(t, base, "/v1/databases/sqlite/dtql", relIntAlice, "from: {name: customers}\ncolumns: [{field: secret}]\n")
		if resp.status != http.StatusForbidden || resp.body["records"] != nil || strings.Contains(resp.raw, "hidden") {
			t.Fatalf("status %d: %s", resp.status, resp.raw)
		}
	})
}

// The shapes the launch limit is for, written with the collection the mounts
// declare: a join to a public mount in both orders, a count, an aggregate over a field
// the policy hides under an alias, a join nested one level deeper and a subquery. On
// both mounts, for the owner and for two principals, each is the same 422 as any
// other document that names the mount, and the answer repeats no collection, field
// or row.
func TestRelationalShapesOverALayeredACLMountAreRefusedWithOneAnswer(t *testing.T) {
	base := relIntLayeredServer(t)
	const (
		onCountry  = "on: [{left: {field: country, source: c}, op: '==', right: {field: code, source: k}}]"
		protectedC = "{database: %[1]s, name: customers, alias: c}"
		publicK    = "{database: directory, name: Country, alias: k}"
	)
	shapes := []struct {
		name, doc string
		perDB     bool // names no database but the mount, so the per-database endpoint takes it
	}{
		{"a join to a public mount, protected first", "from: {database: %[1]s, name: customers, alias: c, joins: [{from: " + publicK + ", " + onCountry + "}]}\ncolumns: [{field: name, source: k}]\n", false},
		{"a join to a public mount, protected second", "from: {database: directory, name: Country, alias: k, joins: [{from: " + protectedC + ", " + onCountry + "}]}\ncolumns: [{field: name, source: c}]\n", false},
		{"a count of its customers", "from: {database: %[1]s, name: customers, alias: c}\ncolumns: [{aggregate: {function: count, args: [{star: true}]}, as: n}]\n", true},
		{"an aggregate over a hidden field, under an alias", "from: {database: %[1]s, name: customers, alias: c}\ncolumns: [{aggregate: {function: max, args: [{field: secret, source: c}]}, as: n}]\n", true},
		{"a join nested one level deeper", "from: {database: directory, name: Country, alias: a, joins: [{from: {database: directory, name: Country, alias: b, joins: [{from: " + protectedC + ", on: [{left: {field: code, source: b}, op: '==', right: {field: country, source: c}}]}]}, on: [{left: {field: code, source: a}, op: '==', right: {field: code, source: b}}]}]}\ncolumns: [{field: name, source: c}]\n", false},
		{"a subquery", "from: {database: %[1]s, name: customers, alias: c}\nwhere: {exists: {query: {from: {database: %[1]s, name: customers, alias: d}}}}\ncolumns: [{field: id, source: c}]\n", true},
	}
	for _, protected := range []string{"sqlite", "ingitdb"} {
		reference := relHTTPPost(t, base, "/v1/dtql", ownerToken, fmt.Sprintf("from: {database: %s, name: ghost}\n", protected))
		if reference.status != http.StatusUnprocessableEntity || reference.errorField("code") != "authorization_unsupported" {
			t.Fatalf("%s: the reference answer is a 422 authorization_unsupported, got %d: %s", protected, reference.status, reference.raw)
		}
		for _, shape := range shapes {
			endpoints := []string{"/v1/dtql"}
			if shape.perDB {
				endpoints = append(endpoints, "/v1/databases/"+protected+"/dtql")
			}
			for _, endpoint := range endpoints {
				for _, caller := range relIntCallers {
					t.Run(protected+", "+shape.name+" on "+endpoint+", "+caller.name, func(t *testing.T) {
						resp := relHTTPPost(t, base, endpoint, caller.token, fmt.Sprintf(shape.doc, protected))
						if resp.status != http.StatusUnprocessableEntity || resp.raw != reference.raw {
							t.Fatalf("status %d: %s\nwant the answer to a document of another shape: %s", resp.status, resp.raw, reference.raw)
						}
						for _, leak := range []string{"customers", "secret", "hidden", "Customer 0", "Ireland", "directory"} {
							if strings.Contains(resp.raw, leak) {
								t.Fatalf("the answer repeats %q: %s", leak, resp.raw)
							}
						}
					})
				}
			}
		}
	}
}
