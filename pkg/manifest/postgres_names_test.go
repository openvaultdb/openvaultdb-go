package manifest_test

import (
	"strings"
	"testing"

	"github.com/openvaultdb/openvaultdb-go/pkg/manifest"
	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
)

// A PostgreSQL mount keeps a name of 63 bytes at most, and the adapter writes only a
// plain identifier (ASCII letters, digits and underscores, not starting with a digit)
// into a statement that creates a table or a column. A manifest of such a mount that
// declares any other collection or field name is refused when it is loaded, with a
// sentence that names the entry and the rule. Another engine is not held to it.

// namedManifest declares a collection and a field by the given names on engine.
func namedManifest(engine, collection, field string) *manifest.Manifest {
	return &manifest.Manifest{
		Database: manifest.Database{ID: "names", SchemaMode: schema.ModeStrict},
		Storage:  manifest.Storage{Engine: engine, Path: "data"},
		Schemas: &schema.Schemas{Collections: map[string]schema.Collection{
			"customers": {Fields: map[string]schema.Field{"name": {Type: schema.TypeString}}},
			collection:  {Fields: map[string]schema.Field{field: {Type: schema.TypeString}}},
		}},
	}
}

func TestPostgresManifestNamesAreRefusedAtLoad(t *testing.T) {
	long := strings.Repeat("n", 64)
	for _, tc := range []struct {
		what, collection, field, want string
	}{
		{"a collection of 64 bytes", long, "f",
			`schemas.collections: collection "` + long + `" cannot be mounted on PostgreSQL: its name is 64 bytes and the most is 63`},
		{"a field of 64 bytes", "orders", long,
			`schemas.collections.orders.fields: field "` + long + `" cannot be mounted on PostgreSQL: its name is 64 bytes and the most is 63`},
		{"a collection with a space", "order details", "f",
			`schemas.collections: collection "order details" cannot be mounted on PostgreSQL: a name holds only ASCII letters, digits and underscores and does not start with a digit`},
		{"a collection with a path", "spaces/ext", "f",
			`schemas.collections: collection "spaces/ext" cannot be mounted on PostgreSQL: a name holds only ASCII letters, digits and underscores and does not start with a digit`},
		{"a field with a quote", "orders", `a"b`,
			`schemas.collections.orders.fields: field "a\"b" cannot be mounted on PostgreSQL: a name holds only ASCII letters, digits and underscores and does not start with a digit`},
		{"a field that starts with a digit", "orders", "1a",
			`schemas.collections.orders.fields: field "1a" cannot be mounted on PostgreSQL: a name holds only ASCII letters, digits and underscores and does not start with a digit`},
		{"a name that is longer once lower-cased", "orders", strings.Repeat("Ⱥ", 31) + "a",
			`schemas.collections.orders.fields: field "` + strings.Repeat("Ⱥ", 31) + `a" cannot be mounted on PostgreSQL: its name is 94 bytes and the most is 63`},
	} {
		t.Run(tc.what, func(t *testing.T) {
			err := namedManifest("postgres", tc.collection, tc.field).Validate()
			if err == nil || err.Error() != tc.want {
				t.Fatalf("Validate() = %v, want %q", err, tc.want)
			}
			if again := namedManifest("postgres", tc.collection, tc.field).CheckPostgresNames(); again == nil || again.Error() != tc.want {
				t.Fatalf("CheckPostgresNames() = %v, want %q", again, tc.want)
			}
		})
	}
}

func TestPostgresManifestNameRefusalIsBoundedAndNamesTheFirstEntryInOrder(t *testing.T) {
	huge := strings.Repeat("n", 1<<16)
	err := namedManifest("postgres", "orders", huge).Validate()
	if err == nil || len(err.Error()) > 1024 || !strings.Contains(err.Error(), "65536 bytes") {
		t.Fatalf("a name of 64 KiB: %d bytes of message: %.120v", len(err.Error()), err)
	}
	// The first refused entry in name order is the one named: collections, then the
	// fields of each.
	m := namedManifest("postgres", "b b", "f")
	m.Schemas.Collections["a a"] = schema.Collection{Fields: map[string]schema.Field{"x": {Type: schema.TypeString}}}
	if err := m.Validate(); err == nil || !strings.Contains(err.Error(), `collection "a a"`) {
		t.Fatalf("Validate() = %v, want the entry \"a a\" named first", err)
	}
	m = namedManifest("postgres", "orders", "f")
	m.Schemas.Collections["orders"] = schema.Collection{Fields: map[string]schema.Field{"y y": {Type: schema.TypeString}, "x x": {Type: schema.TypeString}}}
	if err := m.Validate(); err == nil || !strings.Contains(err.Error(), `field "x x"`) {
		t.Fatalf("Validate() = %v, want the field \"x x\" named first", err)
	}
}

func TestPostgresManifestNamesAtTheLimitAreAccepted(t *testing.T) {
	at := strings.Repeat("n", 63)
	for _, name := range []string{at, strings.ToUpper(at), "_", "select", "Orders", "a1_B2", "id"} {
		if err := namedManifest("postgres", name, name).Validate(); err != nil {
			t.Errorf("Validate() of the name %q = %v, want it accepted", name, err)
		}
	}
	// A manifest that declares nothing has nothing to refuse.
	m := namedManifest("postgres", "orders", "f")
	m.Schemas = nil
	if err := m.CheckPostgresNames(); err != nil {
		t.Errorf("CheckPostgresNames() of a manifest with no schemas = %v", err)
	}
}

func TestOtherEnginesAreNotHeldToThePostgresNameRule(t *testing.T) {
	long := strings.Repeat("n", 64)
	for _, engine := range []string{"sqlite", "ingitdb", "mysql"} {
		m := namedManifest(engine, long, "a b")
		if err := m.CheckPostgresNames(); err != nil {
			t.Errorf("%s: CheckPostgresNames() = %v, want nothing", engine, err)
		}
		if err := m.Validate(); err != nil {
			t.Errorf("%s: Validate() = %v, want the manifest accepted", engine, err)
		}
	}
}
