package server

import (
	"encoding/json"
	"math"
	"net/http"
	"strings"
	"testing"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/record"
	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/joinexec"
	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
)

func TestNativePostgresJSONSpellsSpecialFloats(t *testing.T) {
	row := nativePostgresJSONValue(map[string]any{
		"nan":    math.NaN(),
		"inf":    math.Inf(1),
		"ninf":   float32(math.Inf(-1)),
		"finite": 1.5,
		"small":  float32(2.5),
		"array":  []float64{math.NaN(), 1},
	}, nil)
	encoded, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"array":["NaN",1],"finite":1.5,"inf":"Infinity","nan":"NaN","ninf":"-Infinity","small":2.5}`
	if string(encoded) != want {
		t.Fatalf("row = %s, want %s", encoded, want)
	}
}

func TestExactIntegerValuesRewritesOnlyUnsafeIntegerValues(t *testing.T) {
	for name, tc := range map[string]struct{ in, contains, absent string }{
		"unsafe value":       {in: "where: {op: '==', left: {field: id}, right: {value: 9007199254740993}}\n", contains: `"9007199254740993"`},
		"negative":           {in: "where: {op: '==', left: {field: id}, right: {value: -9007199254740993}}\n", contains: `"-9007199254740993"`},
		"uint64":             {in: "where: {op: '==', left: {field: id}, right: {value: 18446744073709551615}}\n", contains: `"18446744073709551615"`},
		"hex":                {in: "where: {op: '==', left: {field: id}, right: {value: 0x20000000000001}}\n", contains: `"9007199254740993"`},
		"values":             {in: "where: {op: In, left: {field: id}, right: {values: [1, 9007199254740993]}}\n", contains: `"9007199254740993"`},
		"safe untouched":     {in: "where: {op: '==', left: {field: id}, right: {value: 42}}\n", absent: `"42"`},
		"too big is a float": {in: "where: {op: '==', left: {field: id}, right: {value: 1180591620717411303424}}\n", absent: `"1180591620717411303424"`},
		"not a value key":    {in: "limit: 9007199254740993\n", absent: `"9007199254740993"`},
		"values not list":    {in: "where: {op: In, left: {field: id}, right: {values: 9007199254740993}}\n", absent: `"9007199254740993"`},
		"string stays":       {in: "where: {op: '==', left: {field: id}, right: {value: '9007199254740993'}}\n", contains: `'9007199254740993'`},
		"tagged not an int":  {in: "where: {op: '==', left: {field: id}, right: {value: !!int zz}}\n", contains: "zz"},
		"not yaml":           {in: "where: [unclosed\n", contains: "[unclosed"},
	} {
		t.Run(name, func(t *testing.T) {
			got := string(exactIntegerValues([]byte(tc.in)))
			if tc.contains != "" && !strings.Contains(got, tc.contains) {
				t.Fatalf("got %q, want it to contain %s", got, tc.contains)
			}
			if tc.absent != "" && strings.Contains(got, tc.absent) {
				t.Fatalf("got %q, want no %s", got, tc.absent)
			}
		})
	}
}

func TestDTQLParametersKeepExactIntegers(t *testing.T) {
	for raw, want := range map[string]any{
		`9007199254740993`:       int64(9007199254740993),
		`42`:                     int64(42),
		`1.5`:                    1.5,
		`1180591620717411303424`: 1.1805916207174113e+21,
		`"text"`:                 "text",
		`[1, 2.5]`:               []any{int64(1), 2.5},
	} {
		got, err := decodeDTQLParameter(json.RawMessage(raw))
		if err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		a, _ := json.Marshal(got)
		b, _ := json.Marshal(want)
		if string(a) != string(b) {
			t.Errorf("%s = %#v, want %#v", raw, got, want)
		}
	}
	for _, raw := range []string{`1e400`, `[1, 1e400]`, `{`} {
		if _, err := decodeDTQLParameter(json.RawMessage(raw)); err == nil {
			t.Errorf("%s: no error", raw)
		}
	}
	const query = "from: {name: Album}\nwhere: {op: '==', left: {field: id}, right: {param: id}}\n"
	doc, err := bindDTQLParameters([]byte(`{"query":` + quoteJSON(query) + `,"parameters":{"id":9007199254740993}}`))
	if err != nil || !strings.Contains(string(doc), "9007199254740993") {
		t.Fatalf("bound document = %s, %v; want the exact integer", doc, err)
	}
	if _, err := bindDTQLParameters([]byte(`{"query":` + quoteJSON(query) + `,"parameters":{"id":1e400}}`)); err == nil {
		t.Fatal("a number out of range was bound")
	}
}

func TestNativePostgresAnswerWarnsOfAnUnorderedOffsetAndSpellsNaN(t *testing.T) {
	db := nativePostgresProjectionTestDatabase(t)
	id, err := schema.NativePostgresCollectionID("sales data", "Order Details")
	if err != nil {
		t.Fatal(err)
	}
	fake := &relFakeExecutor{result: joinexec.Result{
		Records:   []record.Record{record.NewRecordWithoutKey(map[string]any{"Order ID": int64(1), "ratio": math.NaN()})},
		Columns:   []string{"Order ID", "ratio"},
		Execution: joinexec.Execution{Route: joinexec.RouteDatabase, Sources: []joinexec.ExecutionSource{{Database: "samples", Collection: id}}},
	}}
	_, host := relFakeServer(t, fake, []*core.Database{db})
	for document, warned := range map[string]bool{
		"from: {schema: 'sales data', name: 'Order Details'}\nlimit: 2\noffset: 1\n":                                 true,
		"from: {schema: 'sales data', name: 'Order Details'}\nlimit: 2\noffset: 1\norderBy: [{field: 'Order ID'}]\n": false,
		"from: {schema: 'sales data', name: 'Order Details'}\nlimit: 2\n":                                            false,
	} {
		response := relFakeDo(t, host, http.MethodPost, "/v1/databases/samples/dtql", "", document, nil)
		if response.status != http.StatusOK || !strings.Contains(response.raw, `"ratio":"NaN"`) {
			t.Fatalf("%q: %d %s", document, response.status, response.raw)
		}
		if got := strings.Contains(response.raw, `"warnings":["offset without orderBy`); got != warned {
			t.Fatalf("%q: warned %v, want %v: %s", document, got, warned, response.raw)
		}
	}
	// An unsafe integer literal reaches the executor as the same digits in a string.
	response := relFakeDo(t, host, http.MethodPost, "/v1/databases/samples/dtql", "", "from: {schema: 'sales data', name: 'Order Details'}\nwhere: {op: '==', left: {field: 'Order ID'}, right: {value: 9007199254740993}}\n", nil)
	if response.status != http.StatusOK {
		t.Fatalf("exact integer: %d %s", response.status, response.raw)
	}
	calls := fake.calls
	where, _ := calls[len(calls)-1].query.Where().(dal.Comparison)
	if constant, _ := where.Right.(dal.Constant); constant.Value != "9007199254740993" {
		t.Fatalf("the executor was given %#v, want the digits as text", where.Right)
	}
}
