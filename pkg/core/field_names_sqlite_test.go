package core_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/mount"
)

// quotedNameClasses are the character classes the quoted-name rule accepts and
// the strict rule refuses. Every one is queried against a real SQLite file
// below: a name that reached the SQL text unquoted would be a syntax error or
// would change the statement (a "--" comments out the rest of it, a leading "-"
// is a unary minus, "(" opens a group, "?" and "$1" are placeholders).
var quotedNameClasses = map[string][]string{
	"inner space":              {"zip code", "first  name", "a b c"},
	"no-break space inside":    {"a\u00a0b"},
	"comment markers":          {"a--b", "a/*b", "a*/b", "--", "/* */"},
	"leading hyphen":           {"-name", "-1"},
	"parentheses and brackets": {"a(b)", "(x)", "a[b]", "a{b}"},
	"operators":                {"a=b", "a<b>c", "a+b", "a*b", "a|b", "a&b", "a!b", "a^b", "a~b", "a%b"},
	"separators":               {"a,b", "a:b", "a/b", "a@b", "a#b"},
	"placeholders":             {"a?b", "?", "$1", "a$b", "$"},
	"non-letter unicode":       {"temp °C", "price €", "emoji \U0001F600", "名前 ★"},
	"invisible and separator":  {"a\u200bb", "a\u2028b", "a\ufeffb", "a\u202eb"},
}

// newQuotedNamesDB creates a SQLite file with one table whose columns carry
// every name of quotedNameClasses and two rows, and mounts it. The names are
// quoted here, in the test's own DDL, as any SQLite client would.
func newQuotedNamesDB(t *testing.T) (db *core.Database, names []string) {
	t.Helper()
	dir := t.TempDir()
	storage, err := sql.Open("sqlite", filepath.Join(dir, "data.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	quote := func(name string) string { return `"` + strings.ReplaceAll(name, `"`, `""`) + `"` }
	columns := []string{`"id" TEXT PRIMARY KEY`}
	for _, class := range quotedNameClasses {
		for _, name := range class {
			names = append(names, name)
			columns = append(columns, quote(name)+" TEXT")
		}
	}
	if _, err := storage.Exec(`CREATE TABLE "things" (` + strings.Join(columns, ", ") + `)`); err != nil {
		t.Fatal(err)
	}
	for _, prefix := range []string{"v", "w"} {
		fields, args := []string{`"id"`}, []any{prefix + "-row"}
		for _, name := range names {
			fields = append(fields, quote(name))
			args = append(args, prefix+":"+name)
		}
		placeholders := strings.TrimSuffix(strings.Repeat("?, ", len(fields)), ", ")
		if _, err := storage.Exec(`INSERT INTO "things" (`+strings.Join(fields, ", ")+`) VALUES (`+placeholders+`)`, args...); err != nil {
			t.Fatal(err)
		}
	}
	if err := storage.Close(); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(dir, "db.yaml")
	declaration := "database: {id: quoted, schema_mode: strict}\nstorage: {engine: sqlite, path: data.sqlite}\nschemas:\n  collections:\n    things:\n      fields:\n        id: {type: string}\n"
	if err := os.WriteFile(manifestPath, []byte(declaration), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err = mount.File(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, names
}

// yamlString renders s as a YAML double-quoted scalar (JSON strings are).
func yamlString(t *testing.T, s string) string {
	t.Helper()
	out, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// TestQuotedFieldNamesArriveQuotedAtSQLite runs, for every name of every
// accepted class, a DTQL query and a wire query that put the name in a column
// list, a WHERE and an ORDER BY against a real SQLite file. A name that was not
// quoted by the compiler would fail to parse or select the wrong row; both
// queries must return exactly the row whose value matches.
func TestQuotedFieldNamesArriveQuotedAtSQLite(t *testing.T) {
	db, names := newQuotedNamesDB(t)
	ctx := context.Background()
	for class, members := range quotedNameClasses {
		for _, name := range members {
			t.Run(class+"/"+name, func(t *testing.T) {
				want := "v:" + name
				doc := fmt.Sprintf("from: {name: things}\ncolumns: [{field: id}, {field: %[1]s}]\nwhere: {op: '==', left: {field: %[1]s}, right: {value: %[2]s}}\norderBy: [{field: %[1]s, desc: true}]\n",
					yamlString(t, name), yamlString(t, want))
				records, err := db.ExecuteDTQL(ctx, []byte(doc))
				if err != nil {
					t.Fatalf("dtql: %v", err)
				}
				if len(records) != 1 || records[0].Data[name] != want {
					t.Fatalf("dtql records = %+v, want one row with %q = %q", records, name, want)
				}
				wire, err := db.Execute(ctx, core.Query{
					Collection: "things",
					Where:      []core.Filter{{Field: name, Op: "==", Value: want}},
					OrderBy:    []core.OrderBy{{Field: name}},
				})
				if err != nil {
					t.Fatalf("wire: %v", err)
				}
				if len(wire) != 1 || wire[0].Data[name] != want {
					t.Fatalf("wire records = %+v, want one row with %q = %q", wire, name, want)
				}
			})
		}
	}
	if len(names) < 30 {
		t.Fatalf("only %d names are exercised", len(names))
	}
}

// TestNamesTheQuotedRuleRefusesNeverReachSQLite: the rule still refuses what
// could end a quoted identifier or a statement, whatever the engine.
func TestNamesTheQuotedRuleRefusesNeverReachSQLite(t *testing.T) {
	db, _ := newQuotedNamesDB(t)
	ctx := context.Background()
	for _, name := range []string{`a"b`, `a'b`, "a`b", `a\b`, "a;b", "a; DROP TABLE things; --", "a\x00b", "a\nb", "a\tb", "", " a", "a ", "a..b", ".a", "a."} {
		doc := fmt.Sprintf("from: {name: things}\nwhere: {op: '==', left: {field: %s}, right: {value: 1}}\n", yamlString(t, name))
		if _, err := db.ExecuteDTQL(ctx, []byte(doc)); !errors.Is(err, core.ErrInvalidDTQL) {
			t.Errorf("dtql %q: %v", name, err)
		}
		_, err := db.Execute(ctx, core.Query{Collection: "things", Where: []core.Filter{{Field: name, Op: "==", Value: 1}}})
		if !errors.Is(err, core.ErrInvalidQuery) {
			t.Errorf("wire %q: %v", name, err)
		}
	}
	// The table is still there.
	if records, err := db.ExecuteDTQL(ctx, []byte("from: {name: things}\n")); err != nil || len(records) != 2 {
		t.Fatalf("table damaged: %d records, %v", len(records), err)
	}
}
