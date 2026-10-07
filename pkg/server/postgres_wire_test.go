package server

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/dal-go/dalgo/dal"
	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/manifest"
	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
)

func TestNativePostgresJSONSafeIntegers(t *testing.T) {
	input := map[string]any{
		"minSafe":      int64(-maxJavaScriptSafeInteger),
		"maxSafe":      uint64(maxJavaScriptSafeInteger),
		"wideSigned":   int64(maxJavaScriptSafeInteger + 1),
		"wideNegative": int64(-maxJavaScriptSafeInteger - 1),
		"wideUnsigned": uint64(maxJavaScriptSafeInteger) + 1,
		"smallInt":     int32(7),
		"json":         map[string]any{"n": int64(maxJavaScriptSafeInteger + 1)},
	}
	got := nativePostgresJSONSafeIntegers(input)
	want := map[string]any{
		"minSafe":      int64(-maxJavaScriptSafeInteger),
		"maxSafe":      uint64(maxJavaScriptSafeInteger),
		"wideSigned":   "9007199254740992",
		"wideNegative": "-9007199254740992",
		"wideUnsigned": "9007199254740992",
		"smallInt":     int32(7),
		"json":         map[string]any{"n": int64(maxJavaScriptSafeInteger + 1)},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("nativePostgresJSONSafeIntegers() = %#v, want %#v", got, want)
	}
	if !reflect.DeepEqual(input["wideSigned"], int64(maxJavaScriptSafeInteger+1)) {
		t.Fatal("conversion modified the source row")
	}
}

func TestNativePostgresJSONSafeIntegersCoverScalarAndTypedArrayResults(t *testing.T) {
	if got := nativePostgresJSONSafeValue(int64(9007199254740993)); got != "9007199254740993" {
		t.Fatalf("unsafe scalar int64 = %#v, want decimal string", got)
	}
	if got := nativePostgresJSONSafeValue(uint64(9007199254740993)); got != "9007199254740993" {
		t.Fatalf("unsafe scalar uint64 = %#v, want decimal string", got)
	}
	got := nativePostgresJSONSafeValue([]int64{1, 9007199254740993}).([]any)
	if !reflect.DeepEqual(got, []any{int64(1), "9007199254740993"}) {
		t.Fatalf("typed integer array = %#v", got)
	}
	raw := json.RawMessage(`{"n":9007199254740993}`)
	if got := nativePostgresJSONSafeValue(raw); !reflect.DeepEqual(got, raw) {
		t.Fatalf("raw JSON was changed: %#v", got)
	}
}

func TestNativePostgresJSONValuesPreserveTemporalTypes(t *testing.T) {
	instant := time.Date(2025, time.March, 4, 13, 14, 15, 123000000, time.FixedZone("+02", 2*60*60))
	input := map[string]any{
		"date":           instant,
		"time":           instant,
		"localTimestamp": instant,
		"timestamp":      instant,
		"ordinary":       "unchanged",
	}
	types := map[string]string{
		"date":           "date",
		"time":           "time without time zone",
		"localTimestamp": "timestamp without time zone",
		"timestamp":      "timestamp with time zone",
		"ordinary":       "text",
	}
	want := map[string]any{
		"date":           "2025-03-04",
		"time":           "13:14:15.123",
		"localTimestamp": "2025-03-04T13:14:15.123",
		"timestamp":      "2025-03-04T11:14:15.123Z",
		"ordinary":       "unchanged",
	}
	got := nativePostgresJSONValues(input, types)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("nativePostgresJSONValues() = %#v, want %#v", got, want)
	}
	if !reflect.DeepEqual(input["date"], instant) {
		t.Fatal("conversion modified the source row")
	}
}

func TestNativePostgresJSONValuesEmitValidatedJSONDocuments(t *testing.T) {
	input := map[string]any{
		"json":       `{"n":9007199254740993}`,
		"jsonb":      []byte(`{"n":9007199254740993}`),
		"bad_json":   `not json`,
		"plain_text": `{"n":9007199254740993}`,
	}
	types := map[string]string{"json": "json", "jsonb": "jsonb", "bad_json": "jsonb", "plain_text": "text"}
	got := nativePostgresJSONValues(input, types)
	for _, name := range []string{"json", "jsonb"} {
		raw, ok := got[name].(json.RawMessage)
		if !ok || string(raw) != `{"n":9007199254740993}` {
			t.Errorf("%s = %#v, want validated raw JSON with original number lexeme", name, got[name])
		}
	}
	if got["bad_json"] != "not json" || got["plain_text"] != input["plain_text"] {
		t.Fatalf("non-JSON values were changed: %#v", got)
	}
}

func TestNativePostgresProjectionTypesFollowAliasesJoinsAndSubqueries(t *testing.T) {
	db := nativePostgresProjectionTestDatabase(t)
	endpoint := db
	databases := map[string]*core.Database{"samples": db}
	ref := func(alias string) dal.CollectionRef {
		return dal.NewDatabaseCollectionRef("samples", "sales data", "Order Details", alias)
	}
	columns := []dal.Column{
		{Expression: dal.NewFieldRef("o", "Order ID"), Alias: "order_id"},
		{Expression: dal.NewFieldRef("o", "Created On"), Alias: "created"},
		{Expression: dal.NewFieldRef("o", "Payload Data"), Alias: "payload"},
	}
	t.Run("aliased direct projection", func(t *testing.T) {
		query := dal.From(ref("o")).NewQuery().SelectColumns(columns...)
		if got, want := nativePostgresProjectionTypes(query, endpoint, databases), map[string]string{"order_id": "bigint", "created": "date", "payload": "jsonb"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("projection types = %#v, want %#v", got, want)
		}
	})
	t.Run("multi-source self-join projection", func(t *testing.T) {
		join := dal.NewJoinedSource(ref("i"), dal.JoinInner, dal.NewComparison(dal.NewFieldRef("o", "Order ID"), dal.Equal, dal.NewFieldRef("i", "Order ID")))
		query := dal.From(ref("o")).Join(join).NewQuery().SelectColumns(columns...)
		if got, want := nativePostgresProjectionTypes(query, endpoint, databases), map[string]string{"order_id": "bigint", "created": "date", "payload": "jsonb"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("join projection types = %#v, want %#v", got, want)
		}
	})
	t.Run("derived and scalar subquery projection", func(t *testing.T) {
		inner := dal.From(ref("o")).NewQuery().SelectColumns(columns...)
		derived := dal.NewQuerySource(inner, "d")
		scalarInner := dal.From(ref("o")).NewQuery().SelectColumns(
			dal.Column{Expression: dal.NewFieldRef("o", "Created On"), Alias: "scalar_created"},
		)
		query := dal.From(derived).NewQuery().SelectColumns(
			dal.Column{Expression: dal.NewFieldRef("d", "created"), Alias: "created_at"},
			dal.Column{Expression: dal.NewQueryExpression(scalarInner, "scalar_created"), Alias: "scalar_date"},
		)
		if got, want := nativePostgresProjectionTypes(query, endpoint, databases), map[string]string{"created_at": "date", "scalar_date": "date"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("derived projection types = %#v, want %#v", got, want)
		}
	})
	t.Run("rightmost wildcard field matches DALgo merge precedence", func(t *testing.T) {
		conflictRef := dal.NewDatabaseCollectionRef("samples", "sales data", "Type Conflict", "c")
		join := dal.NewJoinedSource(conflictRef, dal.JoinInner, dal.NewComparison(
			dal.NewFieldRef("o", "Status Name"), dal.Equal, dal.NewFieldRef("c", "Status Name"),
		))
		query := dal.From(ref("o")).Join(join).NewQuery().SelectColumns(
			dal.Column{Wildcard: &dal.WildcardProjection{}},
		)
		got := nativePostgresProjectionTypes(query, endpoint, databases)
		for field, want := range map[string]string{
			"Created On":   "timestamp with time zone",
			"Payload Data": "text",
			"Binary Data":  "text",
		} {
			if got[field] != want {
				t.Errorf("wildcard type for duplicate field %q = %q, want rightmost source type %q", field, got[field], want)
			}
		}
		row := nativePostgresJSONValues(map[string]any{
			"Created On": time.Date(2025, 3, 4, 9, 30, 0, 0, time.FixedZone("UTC+1", 3600)),
		}, got)
		if got, want := row["Created On"], "2025-03-04T08:30:00Z"; got != want {
			t.Errorf("rightmost timestamptz value = %#v, want %q", got, want)
		}
	})
}

func nativePostgresProjectionTestDatabase(t *testing.T) *core.Database {
	t.Helper()
	id, err := schema.NativePostgresCollectionID("sales data", "Order Details")
	if err != nil {
		t.Fatal(err)
	}
	conflictID, err := schema.NativePostgresCollectionID("sales data", "Type Conflict")
	if err != nil {
		t.Fatal(err)
	}
	manifest := &manifest.Manifest{
		Database: manifest.Database{ID: "samples", SchemaMode: schema.ModeStrict},
		Storage:  manifest.Storage{Engine: "postgres", Postgres: &manifest.PostgresOptions{ReadOnly: true}},
		Schemas: &schema.Schemas{Collections: map[string]schema.Collection{
			id: {
				Source: &schema.NativeCollectionSource{Schema: "sales data", Name: "Order Details"},
				Fields: map[string]schema.Field{
					"Order ID":     {Type: schema.TypeInteger, NativeType: "bigint"},
					"Created On":   {Type: schema.TypeString, NativeType: "date"},
					"Payload Data": {Type: schema.TypeString, NativeType: "jsonb"},
				},
			},
			conflictID: {
				Source: &schema.NativeCollectionSource{Schema: "sales data", Name: "Type Conflict"},
				Fields: map[string]schema.Field{
					"Status Name":  {Type: schema.TypeString, NativeType: "text"},
					"Created On":   {Type: schema.TypeString, NativeType: "timestamp with time zone"},
					"Payload Data": {Type: schema.TypeString, NativeType: "text"},
					"Binary Data":  {Type: schema.TypeString, NativeType: "text"},
				},
			},
		}},
	}
	db, err := core.Open(manifest, nil, []schema.Mode{schema.ModeStrict}, t.TempDir()+"/inferred.json")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}
