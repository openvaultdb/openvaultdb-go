package server_test

import (
	"database/sql"
	"encoding/json"
	"fmt"
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
// opts. The CI server is on localhost without TLS, which needs no opt-out.
func pgITNativeMount(t *testing.T, id, dsn string, opts mount.Options) (*core.Database, error) {
	t.Helper()
	t.Setenv(pgITMountDSNEnv, dsn)
	path := filepath.Join(t.TempDir(), id+".yaml")
	manifest := "database: {id: " + id + ", schema_mode: strict}\nstorage: {engine: postgres, postgres: {dsn_env: " + pgITMountDSNEnv + ", read_only: true}}\n"
	if err := os.WriteFile(path, []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
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
		`CREATE TABLE ` + quotedSchema + `."int matrix" (s smallint, i integer, b bigint)`,
		`INSERT INTO ` + quotedSchema + `."int matrix" VALUES (1, 1, 1), (2, 2, 2), (3, 3, 3), (4, 4, 4)`,
		`CREATE TABLE ` + quotedSchema + `."fk parent" (id bigint PRIMARY KEY)`,
		`CREATE TABLE ` + quotedSchema + `."fk child" (id bigint PRIMARY KEY, parent_id bigint CONSTRAINT "child to parent" REFERENCES ` + quotedSchema + `."fk parent" (id))`,
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

	// Foreign keys come from the driver's catalogue, beneath the read-only
	// transaction wrapper: outgoing on the child, incoming on the parent's page.
	childID := pgITCollectionIDs(t, native, "fk child")[0:1]
	if keys, err := native.CollectionForeignKeys(t.Context(), childID[0]); err != nil || len(keys) != 1 || keys[0].ReferencedCollection != "fk parent" {
		t.Fatalf("child foreign keys = %+v, %v; want one", keys, err)
	}
	parentID, _ := native.ResolveNativePostgresCollection(pgITLongSchema, "fk parent")
	parentPage := relHTTPDo(t, base, http.MethodGet, "/ovdb/dbs/hardening/collections/"+url.PathEscape(parentID), "", "", nil)
	if parentPage.status != http.StatusOK || !strings.Contains(parentPage.raw, ">"+pgITLongSchema+".fk child</a>.<code>parent_id</code>") || strings.Contains(parentPage.raw, "No incoming references") {
		t.Fatalf("parent page: %d %d, want the child reference in Referenced by: %q", parentPage.status, len(parentPage.raw), parentPage.raw[max(0, strings.Index(parentPage.raw, "References")):])
	}

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
		if len(id) <= 63 && len(relation) > 30 {
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
	if _, err := pgITNativeMount(t, "as-admin", os.Getenv(pgITDSNEnv), required); err == nil || !strings.Contains(err.Error(), "SUPERUSER") {
		t.Fatalf("superuser mount error = %v, want a superuser refusal", err)
	}
	if _, err := pgITNativeMount(t, "as-admin-unchecked", os.Getenv(pgITDSNEnv), mount.Options{}); err != nil {
		t.Fatalf("without the option the superuser mounts as before: %v", err)
	}

	// {ROLE} stands for the quoted role under test, {SCHEMA} for the schema, {TABLE} for the
	// long table. Each case is refused for one reason only.
	for _, tc := range []struct {
		role       string
		attributes string
		setup      []string
		want       string // a part of the refusal; empty when the role mounts
	}{
		{role: "ovdb select only", setup: []string{`GRANT SELECT ON ALL TABLES IN SCHEMA {SCHEMA} TO {ROLE}`}},
		{role: "ovdb select insert", setup: []string{`GRANT SELECT, INSERT ON {TABLE} TO {ROLE}`}, want: "may write"},
		{role: "ovdb select truncate", setup: []string{`GRANT SELECT, TRUNCATE ON {TABLE} TO {ROLE}`}, want: "may write"},
		{role: "ovdb select trigger", setup: []string{`GRANT SELECT, TRIGGER ON {TABLE} TO {ROLE}`}, want: "may write"},
		{role: "ovdb select references", setup: []string{`GRANT SELECT, REFERENCES ON {TABLE} TO {ROLE}`}, want: "may write"},
		{role: "ovdb column update", setup: []string{`GRANT SELECT ON {TABLE} TO {ROLE}`, `GRANT UPDATE (note) ON {TABLE} TO {ROLE}`}, want: "may write"},
		{role: "ovdb public insert", setup: []string{`GRANT SELECT ON {TABLE} TO {ROLE}`, `GRANT INSERT ON {TABLE} TO PUBLIC`}, want: "may write"},
		{role: "ovdb noinherit owner member", attributes: "NOINHERIT", setup: []string{
			`CREATE ROLE "ovdb owner role" NOLOGIN`,
			`ALTER TABLE {TABLE} OWNER TO "ovdb owner role"`,
			`GRANT SELECT ON {TABLE} TO {ROLE}`,
			`GRANT "ovdb owner role" TO {ROLE}`,
		}, want: `owns relation`},
		{role: "ovdb write all data", attributes: "NOINHERIT", setup: []string{`GRANT SELECT ON {TABLE} TO {ROLE}`, `GRANT pg_write_all_data TO {ROLE}`}, want: "pg_write_all_data"},
		{role: "ovdb server program", setup: []string{`GRANT SELECT ON {TABLE} TO {ROLE}`, `GRANT pg_execute_server_program TO {ROLE}`}, want: "pg_execute_server_program"},
		{role: "ovdb createrole", attributes: "CREATEROLE", setup: []string{`GRANT SELECT ON {TABLE} TO {ROLE}`}, want: "CREATEROLE"},
		{role: "ovdb bypassrls", attributes: "BYPASSRLS", setup: []string{`GRANT SELECT ON {TABLE} TO {ROLE}`}, want: "BYPASSRLS"},
		{role: "ovdb schema create", setup: []string{`GRANT SELECT ON {TABLE} TO {ROLE}`, `GRANT CREATE ON SCHEMA {SCHEMA} TO {ROLE}`}, want: "may create in schema"},
		{role: "ovdb sequence usage", setup: []string{`GRANT SELECT ON {TABLE} TO {ROLE}`, `CREATE SEQUENCE {SCHEMA}."ovdb counter"`, `GRANT USAGE ON SEQUENCE {SCHEMA}."ovdb counter" TO {ROLE}`}, want: "sequence"},
		{role: "ovdb security definer", setup: []string{`GRANT SELECT ON {TABLE} TO {ROLE}`, `CREATE FUNCTION {SCHEMA}."ovdb definer"() RETURNS int LANGUAGE sql SECURITY DEFINER AS 'SELECT 1'`}, want: "SECURITY DEFINER"},
		// Ownership alone: the owner revoked its own privileges, and still owns the
		// table, so it can grant them back.
		{role: "ovdb revoked owner", setup: []string{`ALTER TABLE {TABLE} OWNER TO {ROLE}`, `REVOKE ALL ON {TABLE} FROM {ROLE}`, `GRANT SELECT ON {TABLE} TO {ROLE}`}, want: "owns relation"},
	} {
		t.Run(tc.role, func(t *testing.T) {
			quotedRole := `"` + tc.role + `"`
			if _, err := admin.Exec(`CREATE ROLE ` + quotedRole + ` LOGIN ` + tc.attributes + ` PASSWORD 'test-only-password'`); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				for _, statement := range []string{
					`DROP FUNCTION IF EXISTS ` + quotedSchema + `."ovdb definer"()`,
					`DROP SEQUENCE IF EXISTS ` + quotedSchema + `."ovdb counter"`,
					`REVOKE INSERT ON ` + quotedTable + ` FROM PUBLIC`,
					`ALTER TABLE ` + quotedTable + ` OWNER TO CURRENT_USER`,
					`DROP OWNED BY ` + quotedRole,
					`DROP ROLE IF EXISTS ` + quotedRole,
					`DROP ROLE IF EXISTS "ovdb owner role"`,
				} {
					_, _ = admin.Exec(statement)
				}
			})
			replacer := strings.NewReplacer("{ROLE}", quotedRole, "{SCHEMA}", quotedSchema, "{TABLE}", quotedTable)
			for _, statement := range append([]string{`GRANT USAGE ON SCHEMA {SCHEMA} TO {ROLE}`}, tc.setup...) {
				statement = replacer.Replace(statement)
				if _, err := admin.Exec(statement); err != nil {
					t.Fatalf("%s: %v", statement, err)
				}
			}
			u, err := url.Parse(os.Getenv(pgITDSNEnv))
			if err != nil {
				t.Fatal(err)
			}
			u.User = url.UserPassword(tc.role, "test-only-password")
			_, err = pgITNativeMount(t, "role", u.String(), required)
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("mount as %q refused: %v", tc.role, err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("mount as %q: %v, want a refusal naming %q", tc.role, err, tc.want)
			}
			if tc.role == "ovdb revoked owner" && strings.Contains(err.Error(), "may write") {
				t.Fatalf("the revoked owner was refused for a privilege, not only for owning: %v", err)
			}
		})
	}
}

// Every operator against each integer type, with a fraction, a number beyond the
// type's range, a boolean and text. A value that only an equality could compare
// wrongly (no rows instead of an error) is refused; an order comparison is
// answered with the rows PostgreSQL's numeric comparison selects.
func TestPostgresIntegration_NativeIntegerComparisonsByOperator(t *testing.T) {
	admin := pgITAdmin(t)
	pgITHardeningSchema(t, admin)
	native, err := pgITNativeMount(t, "operators", os.Getenv(pgITDSNEnv), mount.Options{})
	if err != nil {
		t.Fatalf("mount: %v", err)
	}
	base, _ := pgITServeLogged(t, map[string]*core.Database{"operators": native})
	from := map[string]string{"schema": pgITLongSchema, "name": "int matrix"}
	beyond := map[string]any{"s": 40000, "i": json.Number("3000000000"), "b": json.Number("1180591620717411303424")}
	const refused = -1
	// An integer above 2^53 travels as its digits in text, which PostgreSQL parses as
	// the column's type: a smallint or integer column cannot hold it (a 400).
	exactBeyond := map[string]int{"s": refused, "i": refused, "b": 0}
	for _, column := range []string{"s", "i", "b"} {
		for _, tc := range []struct {
			op    string
			value any
			rows  int // refused for a 400
		}{
			{"==", 1.5, refused}, {"==", beyond[column], refused}, {"==", true, refused}, {"==", "abc", refused}, {"==", 2, 1}, {"==", 2.0, 1}, {"==", "2", 1},
			{"In", []any{1, 1.5}, refused}, {"In", []any{1, beyond[column]}, refused}, {"In", []any{1, 2}, 2},
			{"NotIn", []any{1, 1.5}, refused}, {"NotIn", []any{1, 2}, 2},
			{">", 1.5, 3}, {">=", 1.5, 3}, {"<", 1.5, 1}, {"<=", 1.5, 1}, {"<", 2, 1},
			{"<", beyond[column], 4}, {">", beyond[column], 0}, {"<=", beyond[column], 4}, {">=", beyond[column], 0},
			{">", true, refused}, {"<", "abc", refused}, {">", "2", 2}, {"<", "99999999999999999999999", refused}, {">", json.Number("9007199254740993"), exactBeyond[column]},
		} {
			right := map[string]any{"value": tc.value}
			if values, ok := tc.value.([]any); ok {
				right = map[string]any{"values": values}
			}
			where := map[string]any{"op": tc.op, "left": map[string]any{"field": column}, "right": right}
			response := pgITDTQL(t, base, "operators", map[string]any{"from": from, "where": where}, nil)
			label := fmt.Sprintf("%s %s %v", column, tc.op, tc.value)
			switch {
			case tc.rows == refused && response.status != http.StatusBadRequest:
				t.Errorf("%s: %d %s, want 400", label, response.status, response.raw)
			case tc.rows != refused && (response.status != http.StatusOK || len(response.rows(t)) != tc.rows):
				t.Errorf("%s: %d %s, want %d rows", label, response.status, response.raw, tc.rows)
			}
		}
	}
}
