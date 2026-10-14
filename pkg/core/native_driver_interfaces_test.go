package core

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo/dbschema"
	"github.com/dal-go/record"

	"github.com/openvaultdb/openvaultdb-go/pkg/manifest"
	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
)

// schemaReadingDB is a native driver that describes its relations with foreign keys.
type schemaReadingDB struct {
	*namesRecordingDB
	described []string
}

func (f *schemaReadingDB) ListCollections(context.Context, *record.Key) ([]dal.CollectionRef, error) {
	return nil, nil
}

func (f *schemaReadingDB) DescribeCollection(_ context.Context, ref *dal.CollectionRef) (*dbschema.CollectionDef, error) {
	f.described = append(f.described, ref.Schema()+"."+ref.Name())
	return &dbschema.CollectionDef{Name: ref.Name(), ForeignKeys: []dbschema.ForeignKeyDef{
		{Name: "fk_album", Fields: []dal.FieldName{"AlbumId"}, ReferencedCollection: "Album", ReferencedFields: []dal.FieldName{"AlbumId"}},
		{Name: "fk_genre", Fields: []dal.FieldName{"GenreId"}, ReferencedCollection: "Genre", ReferencedFields: []dal.FieldName{"GenreId"}},
		{Name: "fk_media", Fields: []dal.FieldName{"MediaTypeId"}, ReferencedCollection: "MediaType", ReferencedFields: []dal.FieldName{"MediaTypeId"}},
	}}, nil
}

func (f *schemaReadingDB) ListIndexes(context.Context, *dal.CollectionRef) ([]dbschema.IndexDef, error) {
	return nil, nil
}

func (f *schemaReadingDB) ListConstraints(context.Context, *dal.CollectionRef) ([]dbschema.ConstraintDef, error) {
	return nil, nil
}

func (f *schemaReadingDB) ListReferrers(context.Context, *dal.CollectionRef) ([]dbschema.Referrer, error) {
	return nil, nil
}

// The read-transaction wrapper of a native mount must not hide what the driver
// can do besides queries: the foreign keys of a native table come from its
// schema reader.
func TestNativeMountKeepsTheDriversSchemaReader(t *testing.T) {
	var _ dbschema.SchemaReader = (*schemaReadingDB)(nil)
	id, _ := schema.NativePostgresCollectionID("chinook", "Track")
	driver := &schemaReadingDB{namesRecordingDB: &namesRecordingDB{}}
	m := &manifest.Manifest{
		Database: manifest.Database{ID: "chinook", SchemaMode: schema.ModeStrict},
		Storage:  manifest.Storage{Engine: "postgres", Postgres: &manifest.PostgresOptions{ReadOnly: true}},
		Schemas: &schema.Schemas{Collections: map[string]schema.Collection{id: {
			Source: &schema.NativeCollectionSource{Schema: "chinook", Name: "Track"},
			Fields: map[string]schema.Field{"TrackId": {Type: schema.TypeInteger, NativeType: "bigint"}},
		}}},
	}
	db, err := Open(m, driver, []schema.Mode{schema.ModeStrict}, t.TempDir()+"/inferred.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, wrapped := db.db.(nativeReadTxDB); !wrapped {
		t.Fatal("the native driver is not wrapped: the test proves nothing")
	}
	if _, ok := dal.As[dbschema.SchemaReader](db.driver()); !ok {
		t.Fatal("the schema reader is hidden")
	}
	keys, err := db.CollectionForeignKeys(context.Background(), id)
	if err != nil || len(keys) != 3 {
		t.Fatalf("foreign keys = %v, %v; want the driver's three", keys, err)
	}
	if len(driver.described) != 1 || driver.described[0] != "chinook.Track" {
		t.Fatalf("described %v, want the physical relation", driver.described)
	}
	plain := &Database{db: driver}
	if plain.driver() != dal.DB(driver) {
		t.Fatal("driver of an unwrapped database is not its db")
	}
}

// Every lookup of an optional driver interface must go through Database.driver,
// never d.db, which is the read-transaction wrapper on a native mount.
func TestOptionalDriverInterfacesAreLookedUpOnTheUnwrappedDriver(t *testing.T) {
	lookup := regexp.MustCompile(`dal\.As\[[^\]]+\]\(([^)]*\)?)\)`)
	assertion := regexp.MustCompile(`\bd\.db\.\(`)
	for _, dir := range []string{".", "../server", "../joinexec", "../mount"} {
		files, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, file := range files {
			if strings.HasSuffix(file, "_test.go") {
				continue
			}
			text, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			for _, match := range lookup.FindAllStringSubmatch(string(text), -1) {
				if strings.Contains(match[1], "d.db") || strings.Contains(match[1], ".DB()") {
					t.Errorf("%s: %s looks up an optional interface on the wrapped driver; use d.driver()", file, match[0])
				}
			}
			if assertion.MatchString(string(text)) && filepath.Base(file) != "native_read_tx.go" { // driver itself
				t.Errorf("%s asserts an interface on d.db; use d.driver()", file)
			}
		}
	}
}
