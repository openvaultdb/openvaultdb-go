package core

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo/dbschema"
	"github.com/dal-go/dalgo/ddl"
	"github.com/dal-go/record"
	"github.com/dal-go/record/update"

	"github.com/openvaultdb/openvaultdb-go/pkg/manifest"
	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
)

// namesRecordingDB is a writeGuardDB that also records every key the adapter is
// given, as "<call> <collection>", and runs the transaction body against a
// transaction that records its writes the same way. Anything else still panics.
type namesRecordingDB struct {
	writeGuardDB
	seen    []string
	kinds   []reflect.Kind // the ID kind of every key the adapter is given
	created []string       // collections provisioned through ddl.SchemaModifier
}

func (f *namesRecordingDB) Get(ctx context.Context, rec record.Record) error {
	f.seen = append(f.seen, "get "+rec.Key().Collection())
	f.kinds = append(f.kinds, rec.Key().IDKind)
	return f.writeGuardDB.Get(ctx, rec)
}

func (f *namesRecordingDB) Exists(ctx context.Context, key *record.Key) (bool, error) {
	f.seen = append(f.seen, "exists "+key.Collection())
	f.kinds = append(f.kinds, key.IDKind)
	return f.writeGuardDB.Exists(ctx, key)
}

func (f *namesRecordingDB) RunReadwriteTransaction(ctx context.Context, worker dal.RWTxWorker, _ ...dal.TransactionOption) error {
	f.transactions++
	return worker(ctx, &namesRecordingTx{fake: f})
}

// CreateCollection makes the fake a ddl.SchemaModifier.
func (f *namesRecordingDB) CreateCollection(_ context.Context, def dbschema.CollectionDef, _ ...ddl.Option) error {
	f.created = append(f.created, def.Name)
	return nil
}

func (f *namesRecordingDB) DropCollection(context.Context, string, ...ddl.Option) error {
	panic("not used")
}

func (f *namesRecordingDB) AlterCollection(context.Context, string, ...ddl.AlterOp) error {
	panic("not used")
}

type namesRecordingTx struct {
	dal.ReadwriteTransaction
	fake *namesRecordingDB
}

func (t *namesRecordingTx) note(call string, key *record.Key) {
	t.fake.seen = append(t.fake.seen, call+" "+key.String())
	t.fake.kinds = append(t.fake.kinds, key.IDKind)
}

func (t *namesRecordingTx) Set(_ context.Context, rec record.Record) error {
	t.note("set", rec.Key())
	return nil
}

func (t *namesRecordingTx) Insert(_ context.Context, rec record.Record, _ ...dal.InsertOption) error {
	t.note("insert", rec.Key())
	return nil
}

func (t *namesRecordingTx) Update(_ context.Context, key *record.Key, _ []update.Update, _ ...dal.Precondition) error {
	t.note("update", key)
	return nil
}

func (t *namesRecordingTx) Delete(_ context.Context, key *record.Key) error {
	t.note("delete", key)
	return nil
}

// namesOpen opens a database over a namesRecordingDB. Every declared collection
// has the string field "name" and the boolean field "ok".
func namesOpen(t *testing.T, engine string, declared ...string) (*Database, *namesRecordingDB) {
	t.Helper()
	fake := &namesRecordingDB{}
	mode := schema.ModeStrict
	for _, e := range writeGuardDocumentEngines {
		if e == engine {
			mode = schema.ModeSchemaless
		}
	}
	m := &manifest.Manifest{
		Database: manifest.Database{ID: "names", SchemaMode: mode},
		Storage:  manifest.Storage{Engine: engine},
	}
	if len(declared) > 0 {
		m.Schemas = &schema.Schemas{Collections: map[string]schema.Collection{}}
		for _, name := range declared {
			m.Schemas.Collections[name] = schema.Collection{Fields: map[string]schema.Field{
				"name": {Type: schema.TypeString},
				"ok":   {Type: schema.TypeBoolean},
			}}
		}
	}
	db, err := Open(m, fake, []schema.Mode{mode}, filepath.Join(t.TempDir(), "inferred.json"))
	if err != nil {
		t.Fatal(err)
	}
	return db, fake
}

func TestCanonicalCollection(t *testing.T) {
	cases := []struct {
		engine   string
		declared []string
		name     string
		want     string
		ok       bool
	}{
		{"sqlite", []string{`"Order Details"`}, `"Order Details"`, "Order Details", true},
		{"sqlite", []string{`"Order Details"`}, "Order Details", "Order Details", true},
		{"sqlite", []string{`"a""b"`}, `"a""b"`, `a"b`, true},
		{"sqlite", []string{`"a""b"`}, `a"b`, `a"b`, true},
		{"sqlite", []string{"Order Details"}, "Order Details", "Order Details", true},
		{"sqlite", []string{"Order Details"}, `"Order Details"`, "", false}, // a plain declaration stays literal
		{"sqlite", []string{`"Order Details"`}, `Order Details"`, "", false},
		{"sqlite", []string{`"a"b"`}, `"a"b"`, `"a"b"`, true}, // not an identifier: the key is the name
		{"sqlite", []string{`"a"b"`}, `a"b`, "", false},
		{"sqlite", []string{`"Order Details"`, "Order Details"}, `"Order Details"`, "Order Details", true},
		{"sqlite", []string{`"Order Details"`, "Order Details"}, "Order Details", "Order Details", true},
		{"postgres", []string{`"Order Details"`}, `"Order Details"`, `"Order Details"`, true},
		{"postgres", []string{`"Order Details"`}, "Order Details", "", false},
		{"mysql", []string{"customers"}, "customers", "customers", true},
		{"mysql", []string{"customers"}, "Customers", "", false},
		{"ingitdb", []string{"customers"}, "customers", "customers", true},
		{"sqlite", nil, "customers", "", false},
	}
	for _, c := range cases {
		db, _ := namesOpen(t, c.engine, c.declared...)
		got, ok := db.CanonicalCollection(c.name)
		if got != c.want || ok != c.ok {
			t.Errorf("%s declared %q: CanonicalCollection(%q) = %q, %v; want %q, %v", c.engine, c.declared, c.name, got, ok, c.want, c.ok)
		}
	}
	bare := &Database{}
	if got, ok := bare.CanonicalCollection("customers"); ok || got != "" {
		t.Errorf("a database not built by Open declares %q", got)
	}
}

// TestOpenRefusesACollectionNameThatIsAmbiguous: a name that is a spelling of
// one declared collection and the canonical name of another cannot say which
// table it designates, and two keys that are one table cannot each declare its
// fields. Open refuses such a manifest. Keys that are one table with the same
// fields, and any keys of an engine that renames nothing, are fine.
func TestOpenRefusesACollectionNameThatIsAmbiguous(t *testing.T) {
	string1, string2 := map[string]schema.Field{"name": {Type: schema.TypeString}}, map[string]schema.Field{"title": {Type: schema.TypeString}}
	required := map[string]schema.Field{"name": {Type: schema.TypeString, Required: true}}
	for _, c := range []struct {
		label       string
		engine      string
		collections map[string]map[string]schema.Field
		refused     bool
	}{
		{"a spelling of one table is the name of another", "sqlite", map[string]map[string]schema.Field{`"x"`: string1, `"""x"""`: string1}, true},
		{"the same, with different fields", "sqlite", map[string]map[string]schema.Field{`"x"`: string1, `"""x"""`: string2}, true},
		{"one table declared twice with different fields", "sqlite", map[string]map[string]schema.Field{"Orders": string1, `"Orders"`: string2}, true},
		{"one table declared twice, a field required in one", "sqlite", map[string]map[string]schema.Field{"Orders": string1, `"Orders"`: required}, true},
		{"one table declared twice with the same fields", "sqlite", map[string]map[string]schema.Field{"Orders": string1, `"Orders"`: string1}, false},
		{"one table declared twice with no fields", "sqlite", map[string]map[string]schema.Field{"Orders": nil, `"Orders"`: {}}, false},
		{"tables that share no spelling", "sqlite", map[string]map[string]schema.Field{`"x"`: string1, `"y"`: string2, "z": string1}, false},
		{"a key that is not an identifier", "sqlite", map[string]map[string]schema.Field{`"a"b"`: string1, `a"b`: string2}, false},
		{"an engine that renames nothing", "postgres", map[string]map[string]schema.Field{`"x"`: string1, `"""x"""`: string2}, false},
		{"a document engine", "ingitdb", map[string]map[string]schema.Field{`"x"`: string1, `"""x"""`: string2}, false},
	} {
		t.Run(c.label, func(t *testing.T) {
			collections := map[string]schema.Collection{}
			for name, fields := range c.collections {
				collections[name] = schema.Collection{Fields: fields}
			}
			m := &manifest.Manifest{
				Database: manifest.Database{ID: "names", SchemaMode: schema.ModeStrict},
				Storage:  manifest.Storage{Engine: c.engine},
				Schemas:  &schema.Schemas{Collections: collections},
			}
			_, err := Open(m, &namesRecordingDB{}, []schema.Mode{schema.ModeStrict}, filepath.Join(t.TempDir(), "inferred.json"))
			if c.refused != errors.Is(err, ErrCollectionNamesConflict) || !c.refused && err != nil {
				t.Fatalf("got %v, refused = %v", err, c.refused)
			}
		})
	}
}

func TestCollectionSpellings(t *testing.T) {
	db, _ := namesOpen(t, "sqlite", `"Order Details"`, "Order Details", "customers", `"x"`)
	for name, want := range map[string]string{
		"customers":        "customers",
		"Order Details":    `Order Details|"Order Details"`,
		`"Order Details"`:  `Order Details|"Order Details"`,
		"x":                `x|"x"`,
		`"x"`:              `x|"x"`,
		"ghost":            "ghost", // not declared: the name alone
		`"ghost"`:          `"ghost"`,
		`"Order Details`:   `"Order Details`,
		`Order Details"`:   `Order Details"`,
		"Order Details; x": "Order Details; x",
	} {
		if got := strings.Join(db.CollectionSpellings(name), "|"); got != want {
			t.Errorf("CollectionSpellings(%q) = %q, want %q", name, got, want)
		}
	}
	// The result belongs to the caller.
	spellings := db.CollectionSpellings("Order Details")
	spellings[0] = "changed"
	if got := db.CollectionSpellings("Order Details")[0]; got != "Order Details" {
		t.Errorf("a caller changed the database's spellings: %q", got)
	}
	if got := (&Database{}).CollectionSpellings("customers"); len(got) != 1 || got[0] != "customers" {
		t.Errorf("a database not built by Open: %q", got)
	}
}

// TestSpellingsOfACollectionReachTheAdapterAsOneName: a key addressed as
// Order Details and one addressed as "Order Details" are the same collection, so
// every adapter call, in a batch too, is given the name without quotes.
func TestSpellingsOfACollectionReachTheAdapterAsOneName(t *testing.T) {
	ctx := context.Background()
	for _, spelling := range []string{"Order Details", `"Order Details"`} {
		t.Run(spelling, func(t *testing.T) {
			db, fake := namesOpen(t, "sqlite", `"Order Details"`)
			key := record.NewKeyWithID(spelling, "1")
			key.IDKind = reflect.String
			if _, err := db.Get(ctx, key); err != nil {
				t.Fatal(err)
			}
			if ok, err := db.Exists(ctx, key); err != nil || !ok {
				t.Fatalf("Exists: %v %v", ok, err)
			}
			fake.absent = true
			for _, op := range []Op{
				{Op: "set", Key: key, Data: map[string]any{"name": "Ada"}},
				{Op: "insert", Key: key, Data: map[string]any{"name": "Ada"}},
			} {
				if _, err := db.Apply(ctx, []Op{op}, ""); err != nil {
					t.Fatalf("%s: %v", op.Op, err)
				}
			}
			fake.absent = false
			for _, op := range []Op{
				{Op: "update", Key: key, Updates: []UpdateOp{{FieldName: "name", Value: "Bob"}}},
				{Op: "delete", Key: key},
			} {
				if _, err := db.Apply(ctx, []Op{op}, ""); err != nil {
					t.Fatalf("%s: %v", op.Op, err)
				}
			}
			want := []string{
				"get Order Details", "exists Order Details",
				"get Order Details", "set Order Details/1",
				"get Order Details", "insert Order Details/1",
				"get Order Details", "update Order Details/1",
				"get Order Details", "delete Order Details/1",
			}
			if got := strings.Join(fake.seen, "; "); got != strings.Join(want, "; ") {
				t.Fatalf("adapter was given:\n got %s\nwant %s", got, strings.Join(want, "; "))
			}
			// The renamed key is the same key: it keeps the kind of its ID.
			for i, kind := range fake.kinds {
				if kind != reflect.String {
					t.Errorf("call %d (%s): the adapter was given a key whose ID kind is %v, want %v", i, fake.seen[i], kind, reflect.String)
				}
			}
		})
	}
}

// TestBothSpellingsInOneBatchAreOneRecord: the batch validation tracks a record
// by its key, so the same row addressed in both spellings is one row there.
func TestBothSpellingsInOneBatchAreOneRecord(t *testing.T) {
	db, fake := namesOpen(t, "sqlite", `"Order Details"`)
	fake.absent = true
	plain, quoted := record.NewKeyWithID("Order Details", "1"), record.NewKeyWithID(`"Order Details"`, "1")
	data := map[string]any{"name": "Ada"}
	_, err := db.Apply(context.Background(), []Op{
		{Op: "insert", Key: plain, Data: data},
		{Op: "insert", Key: quoted, Data: data},
	}, "")
	if !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("second insert of the same row: %v", err)
	}
	if fake.transactions != 0 {
		t.Fatalf("a batch that conflicts reached the transaction %d times", fake.transactions)
	}
	if got := strings.Join(fake.seen, "; "); got != "get Order Details" {
		t.Fatalf("the row was read more than once: %s", got)
	}
}

// TestSchemaIsFoundUnderEitherSpelling: strict validation and the boolean
// coercion read the declared collection by whichever spelling the key carries,
// before and after an embedder renames the live manifest's keys.
func TestSchemaIsFoundUnderEitherSpelling(t *testing.T) {
	ctx := context.Background()
	rename := func(db *Database) {
		db.Manifest.Schemas.Collections = map[string]schema.Collection{"Order Details": db.Manifest.Schemas.Collections[`"Order Details"`]}
	}
	for label, prepare := range map[string]func(*Database){"as declared": func(*Database) {}, "after the rename": rename} {
		for _, spelling := range []string{"Order Details", `"Order Details"`} {
			t.Run(label+"/"+spelling, func(t *testing.T) {
				db, _ := namesOpen(t, "sqlite", `"Order Details"`)
				prepare(db)
				key := record.NewKeyWithID(spelling, "1")
				if _, err := db.Apply(ctx, []Op{{Op: "set", Key: key, Data: map[string]any{"name": "Ada", "ok": true}}}, ""); err != nil {
					t.Fatalf("a record the schema accepts: %v", err)
				}
				var invalid *schema.ValidationError
				_, err := db.Apply(ctx, []Op{{Op: "set", Key: key, Data: map[string]any{"name": 7}}}, "")
				if !errors.As(err, &invalid) || invalid.Field != "name" {
					t.Fatalf("a record of the wrong type: %v", err)
				}
				coerced := db.coerceToSchema(spelling, map[string]any{"ok": int64(1)})
				if coerced["ok"] != true {
					t.Fatalf("boolean field not coerced: %v", coerced)
				}
			})
		}
	}
	// A collection the manifest does not hold has no schema under any spelling.
	db, _ := namesOpen(t, "sqlite", `"Order Details"`)
	if col := db.schemaCollection("ghost"); col != nil {
		t.Errorf("schema of an undeclared collection: %v", col)
	}
}

// TestProvisioningUsesTheCanonicalName: opening a mount creates the table of a
// quoted key under its public name, and keys of every other kind unchanged.
func TestProvisioningUsesTheCanonicalName(t *testing.T) {
	_, fake := namesOpen(t, "sqlite", `"Order Details"`, "customers", `"a""b"`)
	if got, want := strings.Join(fake.created, "|"), `Order Details|a"b|customers`; got != want {
		t.Fatalf("provisioned %q, want %q", got, want)
	}
	_, fake = namesOpen(t, "postgres", `"Order Details"`)
	if got, want := strings.Join(fake.created, "|"), `"Order Details"`; got != want {
		t.Fatalf("provisioned %q on postgres, want %q", got, want)
	}
}

// TestEngineClassIsFixedWhenTheDatabaseOpens: whether the adapter addresses
// records as documents is recorded at open, beside the declared set, so a later
// change of the live manifest's engine changes no rule.
func TestEngineClassIsFixedWhenTheDatabaseOpens(t *testing.T) {
	nested := record.NewKeyWithParentAndID(record.NewKeyWithID("customers", "c1"), "orders", "o1")
	ghost := record.NewKeyWithID("ghost", "1")
	ctx := context.Background()

	sqlDB, sqlFake := namesOpen(t, "sqlite", "customers")
	sqlDB.Manifest.Storage.Engine = "ingitdb"
	for name, key := range map[string]*record.Key{"undeclared": ghost, "nested": nested} {
		if _, err := sqlDB.Get(ctx, key); !errors.Is(err, ErrNotFound) {
			t.Errorf("sqlite relabelled ingitdb, %s key: %v", name, err)
		}
	}
	if err := sqlDB.ValidateFieldPath([]string{"profile", "c1@space2"}); !errors.Is(err, ErrInvalidFieldName) {
		t.Errorf("sqlite relabelled ingitdb, later path segment: %v", err)
	}
	if sqlFake.reached() != 0 {
		t.Errorf("adapter reached %d times", sqlFake.reached())
	}

	docDB, docFake := namesOpen(t, "ingitdb", "customers")
	docDB.Manifest.Storage.Engine = "sqlite"
	for name, key := range map[string]*record.Key{"undeclared": ghost, "nested": nested} {
		if _, err := docDB.Get(ctx, key); err != nil {
			t.Errorf("ingitdb relabelled sqlite, %s key: %v", name, err)
		}
	}
	if err := docDB.ValidateFieldPath([]string{"profile", "c1@space2"}); err != nil {
		t.Errorf("ingitdb relabelled sqlite, later path segment: %v", err)
	}
	if docFake.gets != 2 {
		t.Errorf("adapter reached %d times, want 2", docFake.gets)
	}
}

// TestNestedKeysReachDocumentEnginesWhole: the adapters of the document engines
// address a nested key as a subcollection of the parent record, so the whole
// key reaches them in every call and the capability stays scoped by the root
// collection.
func TestNestedKeysReachDocumentEnginesWhole(t *testing.T) {
	nested := record.NewKeyWithParentAndID(record.NewKeyWithID("a", "x"), "b", "5")
	if got := RootCollection(nested); got != "a" {
		t.Fatalf("root collection of a nested key: %q", got)
	}
	for _, engine := range writeGuardDocumentEngines {
		t.Run(engine, func(t *testing.T) {
			db, fake := namesOpen(t, engine)
			ctx := context.Background()
			for _, op := range []Op{
				{Op: "set", Key: nested, Data: map[string]any{"name": "Ada"}},
				{Op: "insert", Key: nested, Data: map[string]any{"name": "Ada"}},
				{Op: "update", Key: nested, Updates: []UpdateOp{{FieldName: "name", Value: "Bob"}}},
				{Op: "delete", Key: nested},
			} {
				fake.absent = op.Op == "insert" // an insert needs a row that is not there yet
				if _, err := db.Apply(ctx, []Op{op}, ""); err != nil {
					t.Fatalf("%s: %v", op.Op, err)
				}
			}
			want := []string{
				"get b", "set a/x/b/5",
				"get b", "insert a/x/b/5",
				"get b", "update a/x/b/5",
				"get b", "delete a/x/b/5",
			}
			if got := strings.Join(fake.seen, "; "); got != strings.Join(want, "; ") {
				t.Fatalf("adapter was given:\n got %s\nwant %s", got, strings.Join(want, "; "))
			}
		})
	}
}
