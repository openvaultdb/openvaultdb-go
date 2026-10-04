package core

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/record"

	"github.com/openvaultdb/openvaultdb-go/pkg/manifest"
	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
)

// writeGuardDB extends queryCountingDB (query_guard_test.go) with the
// key-read and write paths of dal.DB, counting each. Every other method still
// panics (nil embedded DB), so a refused request that strays onto any other
// adapter path fails loudly. The transaction body is not run: a write that
// reaches the adapter is counted and reported as applied.
type writeGuardDB struct {
	queryCountingDB
	gets         int
	exists       int
	transactions int
	absent       bool // Get reports the record as not found
}

// reached is the number of adapter calls of any kind.
func (f *writeGuardDB) reached() int {
	return f.queries + f.gets + f.exists + f.transactions
}

func (f *writeGuardDB) Get(_ context.Context, rec record.Record) error {
	f.gets++
	if f.absent {
		rec.SetError(record.ErrRecordNotFound)
		return record.ErrRecordNotFound
	}
	rec.SetError(nil)
	rec.Data().(map[string]any)["name"] = "Ada"
	return nil
}

func (f *writeGuardDB) Exists(context.Context, *record.Key) (bool, error) {
	f.exists++
	return true, nil
}

func (f *writeGuardDB) RunReadwriteTransaction(context.Context, dal.RWTxWorker, ...dal.TransactionOption) error {
	f.transactions++
	return nil
}

// writeGuardEngines are the engines whose adapters build SQL; the document
// engines keep today's collection rule.
var (
	writeGuardSQLEngines      = []string{"sqlite", "postgres", "mysql"}
	writeGuardDocumentEngines = []string{"ingitdb", "firestore"}
)

// writeGuardOpen opens a fake-backed database that declares the given
// collections (one string field "name" each). SQL engines are strict-only;
// document engines are opened schemaless, as they commonly are.
func writeGuardOpen(t *testing.T, engine string, declared ...string) (*Database, *writeGuardDB) {
	t.Helper()
	fake := &writeGuardDB{}
	mode := schema.ModeStrict
	for _, e := range writeGuardDocumentEngines {
		if e == engine {
			mode = schema.ModeSchemaless
		}
	}
	m := &manifest.Manifest{
		Database: manifest.Database{ID: "guarded", SchemaMode: mode},
		Storage:  manifest.Storage{Engine: engine},
	}
	if len(declared) > 0 {
		m.Schemas = &schema.Schemas{Collections: map[string]schema.Collection{}}
		for _, name := range declared {
			m.Schemas.Collections[name] = schema.Collection{Fields: map[string]schema.Field{"name": {Type: schema.TypeString}}}
		}
	}
	db, err := Open(m, fake, []schema.Mode{mode}, filepath.Join(t.TempDir(), "inferred.json"))
	if err != nil {
		t.Fatal(err)
	}
	return db, fake
}

// writeGuardEntryPoints runs every core entry point that reaches an adapter
// by key or writes, for one key and one data/update payload.
func writeGuardEntryPoints(db *Database, key *record.Key, data map[string]any, updates []UpdateOp) map[string]error {
	ctx := context.Background()
	out := map[string]error{}
	_, out["Get"] = db.Get(ctx, key)
	_, out["Exists"] = db.Exists(ctx, key)
	_, out["Apply set"] = db.Apply(ctx, []Op{{Op: "set", Key: key, Data: data}}, "")
	_, out["Apply insert"] = db.Apply(ctx, []Op{{Op: "insert", Key: key, Data: data}}, "")
	_, out["Apply update"] = db.Apply(ctx, []Op{{Op: "update", Key: key, Updates: updates}}, "")
	_, out["Apply delete"] = db.Apply(ctx, []Op{{Op: "delete", Key: key}}, "")
	good := record.NewKeyWithID("customers", "good")
	_, out["Apply batch, bad op last"] = db.Apply(ctx, []Op{
		{Op: "set", Key: good, Data: map[string]any{"name": "Ada"}},
		{Op: "set", Key: key, Data: data},
	}, "")
	_, out["Apply batch, bad op first"] = db.Apply(ctx, []Op{
		{Op: "delete", Key: key},
		{Op: "set", Key: good, Data: map[string]any{"name": "Ada"}},
	}, "")
	return out
}

var writeGuardPlain = map[string]any{"name": "Ada"}
var writeGuardPlainUpdate = []UpdateOp{{FieldName: "name", Value: "Bob"}}

func TestUndeclaredCollectionRefusedBeforeAdapterOnSQLEngines(t *testing.T) {
	keys := map[string]*record.Key{
		"unknown":          record.NewKeyWithID("ghost", "1"),
		"hostile":          record.NewKeyWithID(`customers"; DROP TABLE customers; --`, "1"),
		"spaced":           record.NewKeyWithID("Order Details", "1"),
		"nested leaf":      record.NewKeyWithParentAndID(record.NewKeyWithID("customers", "c1"), "ghost", "g1"),
		"nested root":      record.NewKeyWithParentAndID(record.NewKeyWithID("ghost", "g1"), "customers", "c1"),
		"nested mid-chain": record.NewKeyWithParentAndID(record.NewKeyWithParentAndID(record.NewKeyWithID("customers", "c1"), "ghost", "g1"), "customers", "c2"),
	}
	for _, engine := range writeGuardSQLEngines {
		for label, key := range keys {
			t.Run(engine+"/"+label, func(t *testing.T) {
				db, fake := writeGuardOpen(t, engine, "customers")
				for name, err := range writeGuardEntryPoints(db, key, writeGuardPlain, writeGuardPlainUpdate) {
					if !errors.Is(err, ErrNotFound) {
						t.Errorf("%s: want ErrNotFound, got %v", name, err)
					}
				}
				if fake.reached() != 0 {
					t.Fatalf("adapter reached %d times (gets %d, exists %d, transactions %d)", fake.reached(), fake.gets, fake.exists, fake.transactions)
				}
			})
		}
	}
}

// TestUnrecognisedEngineIsHeldToDeclaredCollections: the allow-list is of the
// document engines, so an engine nobody classified, or a database without a
// manifest, fails closed.
func TestUnrecognisedEngineIsHeldToDeclaredCollections(t *testing.T) {
	for _, engine := range []string{"", "oracle"} {
		db, fake := writeGuardOpen(t, engine, "customers")
		key := record.NewKeyWithID("ghost", "1")
		if _, err := db.Get(context.Background(), key); !errors.Is(err, ErrNotFound) {
			t.Errorf("engine %q: %v", engine, err)
		}
		if fake.reached() != 0 {
			t.Errorf("engine %q: adapter reached", engine)
		}
	}
	// A SQL mount that declares no schemas declares no collection.
	noSchemas, fake := writeGuardOpen(t, "postgres")
	if _, err := noSchemas.Get(context.Background(), record.NewKeyWithID("customers", "1")); !errors.Is(err, ErrNotFound) || fake.reached() != 0 {
		t.Errorf("mount without schemas: err=%v reached=%d", err, fake.reached())
	}
	bare := &Database{}
	if err := bare.GuardKey(record.NewKeyWithID("customers", "1")); !errors.Is(err, ErrNotFound) {
		t.Errorf("database without a manifest: %v", err)
	}
	if err := bare.GuardCollection("customers"); !errors.Is(err, ErrNotFound) {
		t.Errorf("database without a manifest: %v", err)
	}
	// Nor does it have subcollections.
	nested := record.NewKeyWithParentAndID(record.NewKeyWithID("customers", "c1"), "customers", "c2")
	if err := bare.GuardKey(nested); !errors.Is(err, ErrNotFound) {
		t.Errorf("nested key on a database without a manifest: %v", err)
	}
	unclassified, fake := writeGuardOpen(t, "oracle", "customers")
	if _, err := unclassified.Get(context.Background(), nested); !errors.Is(err, ErrNotFound) || fake.reached() != 0 {
		t.Errorf("nested key on an unrecognised engine: err=%v reached=%d", err, fake.reached())
	}
	// A nil key names nothing; the callers that can see one leave it to their own validation.
	if err := bare.GuardKey(nil); err != nil {
		t.Errorf("nil key: %v", err)
	}
}

func TestDeclaredCollectionsReachAdapterOnSQLEngines(t *testing.T) {
	keys := []*record.Key{
		record.NewKeyWithID("customers", "c1"),
		record.NewKeyWithID("Order Details", "1"), // declared with a space in its name
	}
	for _, engine := range writeGuardSQLEngines {
		for _, key := range keys {
			t.Run(engine+"/"+key.String(), func(t *testing.T) {
				db, fake := writeGuardOpen(t, engine, "customers", "Order Details")
				ctx := context.Background()
				data, err := db.Get(ctx, key)
				if err != nil || data["name"] != "Ada" {
					t.Fatalf("Get: %v %v", data, err)
				}
				if ok, err := db.Exists(ctx, key); err != nil || !ok {
					t.Fatalf("Exists: %v %v", ok, err)
				}
				for name, op := range map[string]Op{
					"set":    {Op: "set", Key: key, Data: writeGuardPlain},
					"update": {Op: "update", Key: key, Updates: writeGuardPlainUpdate},
					"delete": {Op: "delete", Key: key},
				} {
					if n, err := db.Apply(ctx, []Op{op}, ""); err != nil || n != 1 {
						t.Errorf("%s: %d %v", name, n, err)
					}
				}
				fake.absent = true
				if n, err := db.Apply(ctx, []Op{{Op: "insert", Key: key, Data: writeGuardPlain}}, ""); err != nil || n != 1 {
					t.Errorf("insert: %d %v", n, err)
				}
				if fake.exists != 1 || fake.transactions != 4 {
					t.Errorf("exists %d transactions %d, want 1 and 4", fake.exists, fake.transactions)
				}
			})
		}
	}
}

// TestNestedKeysRefusedOnSQLEngines: a SQL mount has no subcollections.
// dalgo2sql maps a key with a parent to the recordset <leaf>_<parent>, which no
// mount registers, and its delete statement names only the leaf table, so the
// capability checked on the root collection would not be the table written.
// Even with every segment declared, the key is a 404 before any adapter call.
func TestNestedKeysRefusedOnSQLEngines(t *testing.T) {
	customer := record.NewKeyWithID("customers", "c1")
	keys := map[string]*record.Key{
		"declared leaf under declared root": record.NewKeyWithParentAndID(customer, "Order Details", "1"),
		"same collection twice":             record.NewKeyWithParentAndID(customer, "customers", "c2"),
		"three levels":                      record.NewKeyWithParentAndID(record.NewKeyWithParentAndID(customer, "Order Details", "1"), "customers", "c3"),
	}
	for _, engine := range writeGuardSQLEngines {
		for label, key := range keys {
			t.Run(engine+"/"+label, func(t *testing.T) {
				db, fake := writeGuardOpen(t, engine, "customers", "Order Details")
				for name, err := range writeGuardEntryPoints(db, key, writeGuardPlain, writeGuardPlainUpdate) {
					if !errors.Is(err, ErrNotFound) {
						t.Errorf("%s: want ErrNotFound, got %v", name, err)
					}
				}
				if fake.reached() != 0 {
					t.Fatalf("adapter reached %d times (gets %d, exists %d, transactions %d)", fake.reached(), fake.gets, fake.exists, fake.transactions)
				}
			})
		}
	}
}

func TestDocumentEnginesKeepTodaysCollectionRule(t *testing.T) {
	for _, engine := range writeGuardDocumentEngines {
		t.Run(engine, func(t *testing.T) {
			db, fake := writeGuardOpen(t, engine) // nothing declared: collections are implicit
			ctx := context.Background()
			for _, key := range []*record.Key{
				record.NewKeyWithID("ghost", "1"),
				record.NewKeyWithParentAndID(record.NewKeyWithID("ghost", "g1"), "also-ghost", "1"),
			} {
				if _, err := db.Get(ctx, key); err != nil {
					t.Errorf("Get %s: %v", key, err)
				}
				if ok, err := db.Exists(ctx, key); err != nil || !ok {
					t.Errorf("Exists %s: %v %v", key, ok, err)
				}
				if n, err := db.Apply(ctx, []Op{{Op: "set", Key: key, Data: writeGuardPlain}}, ""); err != nil || n != 1 {
					t.Errorf("set %s: %d %v", key, n, err)
				}
			}
			if fake.reached() == 0 {
				t.Fatal("adapter never reached")
			}
		})
	}
}

func TestHostileFieldNamesRefusedBeforeAdapterOnEveryEngine(t *testing.T) {
	engines := append(append([]string{}, writeGuardSQLEngines...), writeGuardDocumentEngines...)
	key := record.NewKeyWithID("customers", "c1")
	for _, engine := range engines {
		t.Run(engine, func(t *testing.T) {
			db, fake := writeGuardOpen(t, engine, "customers")
			ctx := context.Background()
			for _, name := range unsafeFieldNames {
				ops := map[string][]Op{
					"set data":                {{Op: "set", Key: key, Data: map[string]any{"name": "Ada", name: 1}}},
					"insert data":             {{Op: "insert", Key: key, Data: map[string]any{name: 1}}},
					"update fieldName":        {{Op: "update", Key: key, Updates: []UpdateOp{{FieldName: name, Value: 1}}}},
					"update delete-field":     {{Op: "update", Key: key, Updates: []UpdateOp{{FieldName: name, Delete: true}}}},
					"update fieldPath":        {{Op: "update", Key: key, Updates: []UpdateOp{{FieldPath: []string{name}, Value: 1}}}},
					"update fieldPath, first": {{Op: "update", Key: key, Updates: []UpdateOp{{FieldPath: []string{name, "city"}, Value: 1}}}},
					"update after a good one": {{Op: "update", Key: key, Updates: []UpdateOp{{FieldName: "name", Value: "x"}, {FieldName: name, Value: 1}}}},
					"batch, bad op last": {
						{Op: "set", Key: key, Data: writeGuardPlain},
						{Op: "update", Key: key, Updates: []UpdateOp{{FieldName: name, Delete: true}}},
					},
					"data on a delete op": {{Op: "delete", Key: key, Data: map[string]any{name: 1}}},
				}
				for label, batch := range ops {
					if _, err := db.Apply(ctx, batch, ""); !errors.Is(err, ErrInvalidFieldName) {
						t.Errorf("%s %.30q: %v", label, name, err)
					}
				}
			}
			if fake.reached() != 0 {
				t.Fatalf("adapter reached %d times", fake.reached())
			}
		})
	}
}

// TestHostileFieldNameInAnUndeclaredCollectionIsNotFound: the collection is
// checked first, so a SQL engine answers 404 whatever the body carries.
func TestHostileFieldNameInAnUndeclaredCollectionIsNotFound(t *testing.T) {
	db, fake := writeGuardOpen(t, "postgres", "customers")
	_, err := db.Apply(context.Background(), []Op{{Op: "set", Key: record.NewKeyWithID("ghost", "1"), Data: map[string]any{`a"b`: 1}}}, "")
	if !errors.Is(err, ErrNotFound) || errors.Is(err, ErrInvalidFieldName) || fake.reached() != 0 {
		t.Fatalf("err=%v reached=%d", err, fake.reached())
	}
}

func TestPlainFieldNamesReachAdapterOnDocumentEngines(t *testing.T) {
	for _, engine := range writeGuardDocumentEngines {
		t.Run(engine, func(t *testing.T) {
			db, fake := writeGuardOpen(t, engine, "customers")
			key := record.NewKeyWithID("customers", "c1")
			ctx := context.Background()
			for _, name := range safeFieldNames {
				if _, err := db.Apply(ctx, []Op{{Op: "set", Key: key, Data: map[string]any{name: 1}}}, ""); err != nil {
					t.Errorf("set %.30q: %v", name, err)
				}
				if _, err := db.Apply(ctx, []Op{{Op: "update", Key: key, Updates: []UpdateOp{{FieldName: name, Value: 1}}}}, ""); err != nil {
					t.Errorf("update %.30q: %v", name, err)
				}
				if _, err := db.Apply(ctx, []Op{{Op: "update", Key: key, Updates: []UpdateOp{{FieldPath: []string{"profile", name}, Value: 1}}}}, ""); err != nil {
					t.Errorf("update path %.30q: %v", name, err)
				}
			}
			if fake.transactions != 3*len(safeFieldNames) {
				t.Fatalf("transactions = %d", fake.transactions)
			}
		})
	}
}

// TestUpdateThatNamesNoFieldIsRefusedBeforeTheAdapter: an update with neither
// fieldName nor fieldPath names nothing to write. It used to reach the adapter
// for a key read and fail as a 500; it is now a 400 before any adapter call.
func TestUpdateThatNamesNoFieldIsRefusedBeforeTheAdapter(t *testing.T) {
	db, fake := writeGuardOpen(t, "sqlite", "customers")
	_, err := db.Apply(context.Background(), []Op{{Op: "update", Key: record.NewKeyWithID("customers", "c1"), Updates: []UpdateOp{{Value: 1}}}}, "")
	if !errors.Is(err, ErrInvalidFieldName) || fake.reached() != 0 {
		t.Fatalf("err=%v reached=%d", err, fake.reached())
	}
}

func TestOpWithoutAKeyIsLeftToTheExistingValidation(t *testing.T) {
	db, fake := writeGuardOpen(t, "sqlite", "customers")
	_, err := db.Apply(context.Background(), []Op{{Op: "set", Data: writeGuardPlain}}, "")
	if err == nil || errors.Is(err, ErrInvalidFieldName) || errors.Is(err, ErrNotFound) || fake.reached() != 0 {
		t.Fatalf("err=%v reached=%d", err, fake.reached())
	}
}

// linkagePath is what Sneat's linkage writes through dalgo2openvaultdb: the
// last segment is a map key, `id@spaceID` for a cross-space reference.
var linkagePath = []string{"related", "contactus", "contacts", "c1@space2"}

// mapKeysOnlyDocumentEnginesAccept are later path segments that are not plain
// names but are legal map keys of a document.
var mapKeysOnlyDocumentEnginesAccept = []string{"c1@space2", "na me", `na"me`, "name;", "a--b", "a/b", "名前", "$id", "a.b", "name\u200b"}

// blankOrControlSegments are refused on every engine: update.ByFieldPath
// panics on a blank segment (empty, or only white space), and a control
// character is never a map key.
var blankOrControlSegments = []string{"", " ", "  ", "\t", "\n", "\u00a0", "name\x00", "na\nme", "na\tme", "\x7f", "a\u0085b"}

func TestValidateFieldPath(t *testing.T) {
	sql, _ := writeGuardOpen(t, "sqlite", "customers")
	document, _ := writeGuardOpen(t, "ingitdb", "customers")
	unrecognised, _ := writeGuardOpen(t, "oracle", "customers")
	bare := &Database{}

	for label, db := range map[string]*Database{"sqlite": sql, "ingitdb": document, "unrecognised": unrecognised, "no manifest": bare} {
		for _, path := range [][]string{nil, {}, {""}, {`a"b`}, {"a b", "x"}, {"name;", "x"}, {"$", "x"}} {
			if err := db.ValidateFieldPath(path); !errors.Is(err, ErrInvalidFieldName) {
				t.Errorf("%s: %q accepted: %v", label, path, err)
			}
		}
		for _, path := range [][]string{{"name"}, {"address", "city"}, {"$id"}, {"byYear", "2024"}} {
			if err := db.ValidateFieldPath(path); err != nil {
				t.Errorf("%s: %q refused: %v", label, path, err)
			}
		}
		for _, segment := range blankOrControlSegments {
			if err := db.ValidateFieldPath([]string{"profile", segment}); !errors.Is(err, ErrInvalidFieldName) {
				t.Errorf("%s: later segment %q accepted: %v", label, segment, err)
			}
		}
	}

	// Later segments are map keys: the SQL rule applies on every engine that
	// builds SQL (and on one nobody classified), the document engines refuse
	// only what cannot be a key.
	for label, db := range map[string]*Database{"sqlite": sql, "unrecognised": unrecognised, "no manifest": bare} {
		for _, path := range append([][]string{linkagePath}, pathsEndingIn(unsafeFieldNames)...) {
			if err := db.ValidateFieldPath(path); !errors.Is(err, ErrInvalidFieldName) {
				t.Errorf("%s: %.40q accepted: %v", label, path, err)
			}
		}
	}
	for _, path := range append([][]string{linkagePath}, pathsEndingIn(mapKeysOnlyDocumentEnginesAccept)...) {
		if err := document.ValidateFieldPath(path); err != nil {
			t.Errorf("ingitdb: %q refused: %v", path, err)
		}
	}
}

// pathsEndingIn lists a two-segment path for each name: a plain first segment
// and the name as the map key below it.
func pathsEndingIn(names []string) [][]string {
	paths := make([][]string, len(names))
	for i, name := range names {
		paths[i] = []string{"profile", name}
	}
	return paths
}

// TestLaterFieldPathSegmentsOnSQLEngines: a SQL mount holds every segment of an
// update path to the plain-name rule (fail closed), and Sneat's linkage path is
// no exception there.
func TestLaterFieldPathSegmentsOnSQLEngines(t *testing.T) {
	key := record.NewKeyWithID("customers", "c1")
	for _, engine := range writeGuardSQLEngines {
		t.Run(engine, func(t *testing.T) {
			db, fake := writeGuardOpen(t, engine, "customers")
			for _, path := range append([][]string{linkagePath}, pathsEndingIn(unsafeFieldNames)...) {
				op := Op{Op: "update", Key: key, Updates: []UpdateOp{{FieldPath: path, Value: 1}}}
				if _, err := db.Apply(context.Background(), []Op{op}, ""); !errors.Is(err, ErrInvalidFieldName) {
					t.Errorf("%.40q: %v", path, err)
				}
			}
			if fake.reached() != 0 {
				t.Fatalf("adapter reached %d times", fake.reached())
			}
		})
	}
}

// TestLaterFieldPathSegmentsOnDocumentEngines: segments after the first are map
// keys and never reach SQL on ingitdb and firestore. Sneat's linkage writes
// `id@spaceID` keys through dalgo2openvaultdb, so they stay accepted; a
// segment that is blank (empty, or only white space) or carries a control
// character is refused before the adapter (update.ByFieldPath panics on a
// blank one).
func TestLaterFieldPathSegmentsOnDocumentEngines(t *testing.T) {
	key := record.NewKeyWithID("customers", "c1")
	for _, engine := range writeGuardDocumentEngines {
		t.Run(engine, func(t *testing.T) {
			db, fake := writeGuardOpen(t, engine, "customers")
			paths := append([][]string{linkagePath}, pathsEndingIn(mapKeysOnlyDocumentEnginesAccept)...)
			for _, path := range paths {
				op := Op{Op: "update", Key: key, Updates: []UpdateOp{{FieldPath: path, Value: 1}}}
				if n, err := db.Apply(context.Background(), []Op{op}, ""); err != nil || n != 1 {
					t.Errorf("%q: %d %v", path, n, err)
				}
			}
			if fake.transactions != len(paths) {
				t.Fatalf("transactions = %d, want %d", fake.transactions, len(paths))
			}
			reached := fake.reached()
			for _, path := range pathsEndingIn(blankOrControlSegments) {
				op := Op{Op: "update", Key: key, Updates: []UpdateOp{{FieldPath: path, Value: 1}}}
				if _, err := db.Apply(context.Background(), []Op{op}, ""); !errors.Is(err, ErrInvalidFieldName) {
					t.Errorf("%q: %v", path, err)
				}
			}
			if fake.reached() != reached {
				t.Fatalf("a refused path reached the adapter %d times", fake.reached()-reached)
			}
		})
	}
}

// TestSQLiteQuotedDeclarationsDeclareTheirPublicName: a SQLite manifest may
// key a collection by its SQL-quoted storage identifier; the mount registers
// the public name for it too, so key reads by the public name keep working.
func TestSQLiteQuotedDeclarationsDeclareTheirPublicName(t *testing.T) {
	cases := []struct {
		declared string
		allowed  []string
		refused  []string
	}{
		{`"Order Details"`, []string{`"Order Details"`, "Order Details"}, []string{"Order", "Other Details", `Order Details"`}},
		{`"a""b"`, []string{`"a""b"`, `a"b`}, []string{"a", `a""b`}},
		{`"a"b"`, []string{`"a"b"`}, []string{"a", `a"b`}},                        // a lone inner quote is not an identifier
		{`"a""`, []string{`"a""`}, []string{"a", `a"`}},                           // dangling doubled quote
		{`""`, []string{`""`}, []string{""}},                                      // empty identifier
		{`"`, []string{`"`}, []string{""}},                                        // too short
		{`Order Details`, []string{"Order Details"}, []string{`"Order Details"`}}, // plain declaration stays literal
	}
	for _, c := range cases {
		db, fake := writeGuardOpen(t, "sqlite", c.declared)
		for _, name := range c.allowed {
			if err := db.GuardCollection(name); err != nil {
				t.Errorf("declared %s: %s refused: %v", c.declared, name, err)
			}
		}
		for _, name := range c.refused {
			if err := db.GuardCollection(name); !errors.Is(err, ErrNotFound) {
				t.Errorf("declared %s: %q accepted: %v", c.declared, name, err)
			}
		}
		if fake.reached() != 0 {
			t.Errorf("declared %s: adapter reached", c.declared)
		}
	}
	// Only SQLite registers the public name; elsewhere the quoted key is just
	// a different string.
	for _, engine := range []string{"postgres", "mysql"} {
		db, _ := writeGuardOpen(t, engine, `"Order Details"`)
		if err := db.GuardCollection("Order Details"); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: public name of a quoted declaration accepted: %v", engine, err)
		}
		if err := db.GuardCollection(`"Order Details"`); err != nil {
			t.Errorf("%s: exact declaration refused: %v", engine, err)
		}
	}
}

// TestDeclaredSetIsFixedWhenTheMountOpens: the allow-list is what the mount
// registered with the driver, not the live manifest. The cloud embedder mounts
// SQLite manifests keyed by the quoted identifier ("Order Details" with the
// quotes), renames the manifest keys to the public names, and rewrites key
// reads to the quoted name: that name must stay declared after the rename.
func TestDeclaredSetIsFixedWhenTheMountOpens(t *testing.T) {
	db, fake := writeGuardOpen(t, "sqlite", `"Order Details"`, `"a""b"`, "customers")
	db.Manifest.Schemas.Collections = map[string]schema.Collection{
		"Order Details": {}, `a"b`: {}, "customers": {},
	}
	for _, name := range []string{`"Order Details"`, "Order Details", `"a""b"`, `a"b`, "customers"} {
		if err := db.GuardCollection(name); err != nil {
			t.Errorf("%s refused after the rename: %v", name, err)
		}
	}
	if _, err := db.Get(context.Background(), record.NewKeyWithID(`"Order Details"`, "1")); err != nil || fake.gets != 1 {
		t.Errorf("read of the quoted name: err=%v gets=%d", err, fake.gets)
	}
	for _, name := range []string{"ghost", `Order Details"`, `"Order Details`, "Order"} {
		if err := db.GuardCollection(name); !errors.Is(err, ErrNotFound) {
			t.Errorf("%q accepted: %v", name, err)
		}
	}
	// Nothing that appears in the live manifest after open is declared: the
	// mount registered no recordset for it, so no adapter may be asked.
	db.Manifest.Schemas.Collections["late"] = schema.Collection{}
	if err := db.GuardCollection("late"); !errors.Is(err, ErrNotFound) {
		t.Errorf("a collection added to the live manifest after open is declared: %v", err)
	}
	// A Database that was not built by Open declares nothing.
	built := &Database{Manifest: db.Manifest}
	if err := built.GuardCollection("customers"); !errors.Is(err, ErrNotFound) {
		t.Errorf("a database not built by Open declares %q: %v", "customers", err)
	}
}

func TestSQLiteLogicalName(t *testing.T) {
	for in, want := range map[string]string{`"Order Details"`: "Order Details", `"a""b"`: `a"b`, `"x"`: "x", `""""`: `"`} {
		if got, ok := SQLiteLogicalName(in); !ok || got != want {
			t.Errorf("%s: got %q %v, want %q", in, got, ok, want)
		}
	}
	for _, in := range []string{"", `"`, `""`, "Order Details", `"a"b"`, `"a""`, `"a`, `a"`, "'a'"} {
		if got, ok := SQLiteLogicalName(in); ok {
			t.Errorf("%q accepted as %q", in, got)
		}
	}
}

func TestInvalidFieldNameErrorNamesTheOffender(t *testing.T) {
	db, _ := writeGuardOpen(t, "sqlite", "customers")
	_, err := db.Apply(context.Background(), []Op{{Op: "set", Key: record.NewKeyWithID("customers", "c1"), Data: map[string]any{"na me": 1}}}, "")
	want := fmt.Sprintf("%q", "na me")
	if err == nil || !errors.Is(err, ErrInvalidFieldName) || !strings.Contains(err.Error(), want) {
		t.Fatalf("got %v", err)
	}
}
