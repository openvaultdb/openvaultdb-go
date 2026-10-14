package server_test

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/mount"
	"github.com/openvaultdb/openvaultdb-go/pkg/server/servertest"
)

// Names longer than the 63 bytes PostgreSQL keeps would be cut by the server, so
// the relations here are as long as it allows: their encoded collection IDs are
// far longer than 63 bytes, and every read of them must still answer.
const (
	pgITLongSchema = "ovdb hardening schema with a rather long name" // 45 bytes
	pgITLongTable  = "a table whose name is exactly as long as postgres allows: 63 by"
	pgITLongView   = "a view over the long table, with a long name of its own too"
)

// pgITNativeMount mounts the native read-only manifest of the test server with
// opts, on a plain connection (the CI server has no TLS).
func pgITNativeMount(t *testing.T, id, dsn string, opts mount.Options) (*core.Database, error) {
	t.Helper()
	t.Setenv(pgITMountDSNEnv, dsn)
	path := filepath.Join(t.TempDir(), id+".yaml")
	manifest := "database: {id: " + id + ", schema_mode: strict}\nstorage: {engine: postgres, postgres: {dsn_env: " + pgITMountDSNEnv + ", read_only: true}}\n"
	if err := os.WriteFile(path, []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	opts.AllowInsecureNativePostgresTransport = true
	db, err := mount.FileWithOptions(path, opts)
	if err == nil {
		t.Cleanup(func() { _ = db.Close() })
	}
	return db, err
}

func pgITHardeningSchema(t *testing.T, admin *sql.DB) {
	t.Helper()
	if len(pgITLongTable) != 63 {
		t.Fatalf("the long table name is %d bytes, want 63", len(pgITLongTable))
	}
	quotedSchema := `"` + pgITLongSchema + `"`
	if _, err := admin.Exec(`DROP SCHEMA IF EXISTS ` + quotedSchema + ` CASCADE`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.Exec(`DROP SCHEMA IF EXISTS ` + quotedSchema + ` CASCADE`) })
	for _, statement := range []string{
		`CREATE SCHEMA ` + quotedSchema,
		`CREATE TABLE ` + quotedSchema + `."` + pgITLongTable + `" (id bigint PRIMARY KEY, small smallint, f double precision, b bytea, note text)`,
		`INSERT INTO ` + quotedSchema + `."` + pgITLongTable + `" VALUES
			(1, 1, 'NaN', decode('00ff10', 'hex'), 'nan'),
			(2, 2, 'Infinity', decode('01', 'hex'), 'inf'),
			(3, 3, '-Infinity', NULL, 'minus inf'),
			(9007199254740993, 4, 1.5, NULL, 'big id')`,
		`CREATE VIEW ` + quotedSchema + `."` + pgITLongView + `" AS SELECT id, note FROM ` + quotedSchema + `."` + pgITLongTable + `"`,
		// The settings of the transaction a read runs in, as a view can see them, and a
		// view that changes the session default the way a function a view calls can.
		`CREATE VIEW ` + quotedSchema + `."session settings" AS SELECT
			current_setting('transaction_read_only') AS "transaction read only",
			current_setting('default_transaction_read_only') AS "default read only",
			current_setting('statement_timeout') AS "statement timeout"`,
		`CREATE VIEW ` + quotedSchema + `."flip session default" AS SELECT
			set_config('default_transaction_read_only', 'off', false) AS flipped`,
		`CREATE VIEW ` + quotedSchema + `."slow view" AS SELECT pg_sleep(3)::text AS slept`,
	} {
		if _, err := admin.Exec(statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
}

func pgITDTQL(t *testing.T, base, database string, document any, parameters map[string]any) relHTTPResponse {
	t.Helper()
	text, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	body := map[string]any{"query": string(text)}
	if parameters != nil {
		body["parameters"] = parameters
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return relHTTPDo(t, base, http.MethodPost, "/v1/databases/"+database+"/dtql", "", string(raw), map[string]string{"Content-Type": "application/json"})
}

func TestPostgresIntegration_NativeLongNamesValuesAndReadOnlyTransactions(t *testing.T) {
	admin := pgITAdmin(t)
	pgITHardeningSchema(t, admin)
	native, err := pgITNativeMount(t, "hardening", os.Getenv(pgITDSNEnv), mount.Options{})
	if err != nil {
		t.Fatalf("mount: %v", err)
	}
	base, _ := pgITServeLogged(t, map[string]*core.Database{"hardening": native})

	// Every collection answers the whole battery, the long names included.
	servertest.RequireEveryCollection(t, t.Context(), base, "hardening", servertest.Options{
		Collections: pgITCollectionIDs(t, native, pgITLongTable, pgITLongView),
	})

	table := map[string]string{"schema": pgITLongSchema, "name": pgITLongTable}
	byID := func(value any) map[string]any {
		return map[string]any{"op": "==", "left": map[string]any{"field": "id"}, "right": map[string]any{"value": value}}
	}
	// NaN and the infinities are the strings PostgreSQL spells them with, and a
	// filter with that string finds the row.
	special := pgITDTQL(t, base, "hardening", map[string]any{"from": table, "orderBy": []any{map[string]any{"field": "id"}}, "columns": []any{map[string]any{"field": "id"}, map[string]any{"field": "f"}}}, nil)
	if special.status != http.StatusOK {
		t.Fatalf("read special floats: %d %s", special.status, special.raw)
	}
	var floats []any
	for _, row := range special.rows(t) {
		floats = append(floats, row["f"])
	}
	if want := []any{"NaN", "Infinity", "-Infinity", 1.5}; !jsonEqual(floats, want) {
		t.Fatalf("special floats = %v, want %v", floats, want)
	}
	nan := pgITDTQL(t, base, "hardening", map[string]any{"from": table, "where": map[string]any{"op": "==", "left": map[string]any{"field": "f"}, "right": map[string]any{"value": "NaN"}}}, nil)
	if rows := nan.rows(t); nan.status != http.StatusOK || len(rows) != 1 {
		t.Fatalf("filter f == NaN: %d %s", nan.status, nan.raw)
	}

	// An exact integer above 2^53 matches, inline and as a parameter.
	for name, response := range map[string]relHTTPResponse{
		"inline":    pgITDTQL(t, base, "hardening", map[string]any{"from": table, "where": byID(json.Number("9007199254740993"))}, nil),
		"parameter": pgITDTQL(t, base, "hardening", map[string]any{"from": table, "where": map[string]any{"op": "==", "left": map[string]any{"field": "id"}, "right": map[string]any{"param": "id"}}}, map[string]any{"id": json.Number("9007199254740993")}),
	} {
		rows := response.rows(t)
		if response.status != http.StatusOK || len(rows) != 1 || rows[0]["note"] != "big id" {
			t.Errorf("%s exact integer: %d %s", name, response.status, response.raw)
		}
	}
	// A value an integer column cannot hold, and any value on a bytea column, is a 400.
	for name, where := range map[string]any{
		"fraction":            byID(1.5),
		"beyond int64":        byID(json.Number("1180591620717411303424")),
		"boolean":             byID(true),
		"text":                byID("abc"),
		"text beyond int64":   byID("1180591620717411303424"),
		"text exponent":       byID("1e3"),
		"smallint overflow":   map[string]any{"op": "==", "left": map[string]any{"field": "small"}, "right": map[string]any{"value": 40000}},
		"in with a fraction":  map[string]any{"op": "In", "left": map[string]any{"field": "id"}, "right": map[string]any{"values": []any{1, 2.5}}},
		"bytea base64":        map[string]any{"op": "==", "left": map[string]any{"field": "b"}, "right": map[string]any{"value": "AP8Q"}},
		"bytea other operand": map[string]any{"op": "==", "left": map[string]any{"value": "AP8Q"}, "right": map[string]any{"field": "b"}},
	} {
		response := pgITDTQL(t, base, "hardening", map[string]any{"from": table, "where": where}, nil)
		if response.status != http.StatusBadRequest {
			t.Errorf("%s: %d %s, want 400", name, response.status, response.raw)
		}
	}

	// An offset without an order is answered, with a warning in the footer.
	unordered := pgITDTQL(t, base, "hardening", map[string]any{"from": table, "limit": 2, "offset": 1}, nil)
	warnings, _ := unordered.body["warnings"].([]any)
	if unordered.status != http.StatusOK || len(warnings) != 1 || !strings.Contains(warnings[0].(string), "orderBy") {
		t.Fatalf("offset without orderBy: %d %s, want rows and one warning", unordered.status, unordered.raw)
	}
	ordered := pgITDTQL(t, base, "hardening", map[string]any{"from": table, "limit": 2, "offset": 1, "orderBy": []any{map[string]any{"field": "id"}}}, nil)
	if _, warned := ordered.body["warnings"]; ordered.status != http.StatusOK || warned {
		t.Fatalf("ordered offset: %d %s, want rows and no warning", ordered.status, ordered.raw)
	}

	// Every read runs in an explicit READ ONLY transaction: once a view has turned
	// the session default off, a read on the same connection is still read-only.
	settings := map[string]string{"schema": pgITLongSchema, "name": "session settings"}
	flip := map[string]string{"schema": pgITLongSchema, "name": "flip session default"}
	sawFlippedSession := false
	for range 20 {
		if response := pgITDTQL(t, base, "hardening", map[string]any{"from": flip}, nil); response.status != http.StatusOK {
			t.Fatalf("flip: %d %s", response.status, response.raw)
		}
		// The null test sends the document down the in-memory route, the one that
		// does not run inside the database route's transaction.
		for _, document := range []map[string]any{
			{"from": settings},
			{"from": settings, "where": map[string]any{"isNotNull": map[string]any{"field": "statement timeout"}}},
		} {
			response := pgITDTQL(t, base, "hardening", document, nil)
			rows := response.rows(t)
			if response.status != http.StatusOK || len(rows) != 1 {
				t.Fatalf("settings: %d %s", response.status, response.raw)
			}
			if rows[0]["transaction read only"] != "on" {
				t.Fatalf("a read ran in a writable transaction: %v", rows[0])
			}
			if rows[0]["statement timeout"] != "15s" {
				t.Fatalf("statement_timeout = %v, want the 15s default", rows[0]["statement timeout"])
			}
			if rows[0]["default read only"] == "off" {
				sawFlippedSession = true
			}
		}
	}
	if !sawFlippedSession {
		t.Fatal("no read met the session whose default a view turned off: the test proved nothing")
	}
}

func jsonEqual(got, want any) bool {
	a, _ := json.Marshal(got)
	b, _ := json.Marshal(want)
	return string(a) == string(b)
}

// pgITCollectionIDs returns the collection IDs of relations of the long schema.
func pgITCollectionIDs(t *testing.T, db *core.Database, relations ...string) []string {
	t.Helper()
	ids := make([]string, len(relations))
	for i, relation := range relations {
		id, ok := db.ResolveNativePostgresCollection(pgITLongSchema, relation)
		if !ok {
			t.Fatalf("relation %q is not a collection", relation)
		}
		if len(id) <= 63 {
			t.Fatalf("collection ID %q is not longer than 63 bytes: the test proves nothing", id)
		}
		ids[i] = id
	}
	return ids
}

func TestPostgresIntegration_NativeStatementTimeoutEndsASlowRead(t *testing.T) {
	admin := pgITAdmin(t)
	pgITHardeningSchema(t, admin)
	native, err := pgITNativeMount(t, "slow", os.Getenv(pgITDSNEnv), mount.Options{NativePostgresStatementTimeout: 300 * time.Millisecond})
	if err != nil {
		t.Fatalf("mount: %v", err)
	}
	base, _ := pgITServeLogged(t, map[string]*core.Database{"slow": native})
	started := time.Now()
	response := pgITDTQL(t, base, "slow", map[string]any{"from": map[string]string{"schema": pgITLongSchema, "name": "slow view"}}, nil)
	if response.status == http.StatusOK || time.Since(started) > 2*time.Second {
		t.Fatalf("slow view: %d %s after %v, want a refusal well before the 3s sleep ends", response.status, response.raw, time.Since(started))
	}
}

func TestPostgresIntegration_NativeRoleMustBeReadOnlyWhenRequired(t *testing.T) {
	admin := pgITAdmin(t)
	pgITHardeningSchema(t, admin)
	quotedSchema := `"` + pgITLongSchema + `"`
	quotedTable := quotedSchema + `."` + pgITLongTable + `"`
	required := mount.Options{RequireReadOnlyNativePostgresRole: true}

	// The administration role of the test server is a superuser.
	if _, err := pgITNativeMount(t, "as-admin", os.Getenv(pgITDSNEnv), required); err == nil || !strings.Contains(err.Error(), "superuser") {
		t.Fatalf("superuser mount error = %v, want a superuser refusal", err)
	}
	if _, err := pgITNativeMount(t, "as-admin-unchecked", os.Getenv(pgITDSNEnv), mount.Options{}); err != nil {
		t.Fatalf("without the option the superuser mounts as before: %v", err)
	}

	for _, tc := range []struct {
		role   string
		grants []string
		refuse bool
	}{
		{role: "ovdb select only", grants: []string{`GRANT SELECT ON ALL TABLES IN SCHEMA ` + quotedSchema}},
		{role: "ovdb select insert", grants: []string{`GRANT SELECT, INSERT ON ` + quotedTable}, refuse: true},
		{role: "ovdb select truncate", grants: []string{`GRANT SELECT, TRUNCATE ON ` + quotedTable}, refuse: true},
		{role: "ovdb column update", grants: []string{`GRANT SELECT ON ` + quotedTable, `GRANT UPDATE (note) ON ` + quotedTable}, refuse: true},
	} {
		t.Run(tc.role, func(t *testing.T) {
			quotedRole := `"` + tc.role + `"`
			if _, err := admin.Exec(`CREATE ROLE ` + quotedRole + ` LOGIN PASSWORD 'test-only-password'`); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				_, _ = admin.Exec(`DROP OWNED BY ` + quotedRole)
				_, _ = admin.Exec(`DROP ROLE IF EXISTS ` + quotedRole)
			})
			for _, grant := range append([]string{`GRANT USAGE ON SCHEMA ` + quotedSchema}, tc.grants...) {
				if _, err := admin.Exec(grant + ` TO ` + quotedRole); err != nil {
					t.Fatalf("%s: %v", grant, err)
				}
			}
			u, err := url.Parse(os.Getenv(pgITDSNEnv))
			if err != nil {
				t.Fatal(err)
			}
			u.User = url.UserPassword(tc.role, "test-only-password")
			_, err = pgITNativeMount(t, "role", u.String(), required)
			if tc.refuse != (err != nil) {
				t.Fatalf("mount as %q: %v, want refused=%v", tc.role, err, tc.refuse)
			}
			if tc.refuse && !strings.Contains(err.Error(), "owns or may write") {
				t.Fatalf("refusal = %v, want it to say the role may write", err)
			}
		})
	}
}
