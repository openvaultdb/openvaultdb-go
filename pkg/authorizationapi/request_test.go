package authorizationapi

import (
	"strings"
	"testing"

	"github.com/dal-go/dalgo/dtql/authorization"
)

const inspect = `{"apiVersion":"dtql.org/authorization/v1","mode":"inspect","diagnosticLevel":"ordinary","operations":[{"id":"u1","action":"update","resource":{"databaseId":"crm","path":"/customers/101"},"mutation":{"changes":[{"op":"set","path":["name"],"value":null}]},"executionClass":"dtql"}]}`

func TestNormalizeInspect(t *testing.T) {
	t.Parallel()
	r, err := Parse([]byte(inspect), "crm")
	if err != nil {
		t.Fatal(err)
	}
	op := r.Operations[0]
	if op.Resource.Table != "customers" || op.Resource.RowID != "101" || len(op.Resource.Columns) != 1 || op.Resource.Columns[0][0] != "name" {
		t.Fatalf("resource not derived: %+v", op.Resource)
	}
	if string(op.Mutation.Changes[0].Value) != "null" {
		t.Fatal("explicit null lost")
	}
}

func TestRejectAmbiguousAuthorizationRequests(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"duplicate":             strings.Replace(inspect, `"mode":"inspect"`, `"mode":"inspect","mode":"plan"`, 1),
		"case alias":            strings.Replace(inspect, `"mode"`, `"Mode"`, 1),
		"unknown":               strings.Replace(inspect, `"mode":"inspect"`, `"mode":"inspect","effectivePrincipal":{}`, 1),
		"execution":             strings.Replace(inspect, `"mode":"inspect"`, `"mode":"execution"`, 1),
		"missing value":         strings.Replace(inspect, `,"value":null`, "", 1),
		"null subject":          strings.Replace(inspect, `"mode":"inspect"`, `"mode":"inspect","subject":null`, 1),
		"null changes":          strings.Replace(inspect, `[{"op":"set","path":["name"],"value":null}]`, `null`, 1),
		"wrong database":        strings.Replace(inspect, `"databaseId":"crm"`, `"databaseId":"other"`, 1),
		"path alias":            strings.Replace(inspect, `/customers/101`, `/customers/../101`, 1),
		"encoded alias":         strings.Replace(inspect, `/customers/101`, `/customers/%31%30%31`, 1),
		"row mismatch":          strings.Replace(inspect, `"path":"/customers/101"`, `"path":"/customers/101","rowId":"102"`, 1),
		"columns mismatch":      strings.Replace(inspect, `"path":"/customers/101"`, `"path":"/customers/101","columns":[["secret"]]`, 1),
		"table inspect":         strings.Replace(inspect, `/customers/101`, `/customers`, 1),
		"unknown execution":     strings.Replace(inspect, `"dtql"`, `"native"`, 1),
		"callable absent":       strings.Replace(inspect, `"dtql"`, `"stored_procedure"`, 1),
		"duplicate changes":     strings.Replace(inspect, `{"op":"set","path":["name"],"value":null}`, `{"op":"set","path":["name"],"value":null},{"op":"set","path":["name"],"value":"Ada"}`, 1),
		"dotted physical field": strings.Replace(inspect, `["name"]`, `["address.city"]`, 1),
		"dotted column":         strings.Replace(inspect, `"path":"/customers/101"`, `"path":"/customers/101","columns":[["address.city"]]`, 1),
		"parent child changes":  strings.Replace(inspect, `{"op":"set","path":["name"],"value":null}`, `{"op":"set","path":["name"],"value":{}},{"op":"set","path":["name","first"],"value":"Ada"}`, 1),
		"two documents":         inspect + inspect,
		"wrong scalar":          strings.Replace(inspect, `"id":"u1"`, `"id":1`, 1),
	}
	for name, input := range cases {
		name, input := name, input
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := Parse([]byte(input), "crm"); err == nil {
				t.Fatal("accepted invalid request")
			}
		})
	}
}

func TestNestedFieldAndDottedRowIdentityRemainDistinct(t *testing.T) {
	t.Parallel()
	input := strings.Replace(inspect, `["name"]`, `["address","city"]`, 1)
	input = strings.Replace(input, `/customers/101`, `/customers/user.101`, 1)
	request, err := Parse([]byte(input), "crm")
	if err != nil {
		t.Fatal(err)
	}
	if request.Operations[0].Resource.RowID != "user.101" || len(request.Operations[0].Resource.Columns[0]) != 2 {
		t.Fatal("normalization confused row identity with nested field segments")
	}
}

func TestPlanAndSampleValidation(t *testing.T) {
	t.Parallel()
	plan := strings.Replace(strings.Replace(inspect, `"inspect"`, `"plan"`, 1), `/customers/101`, `/customers`, 1)
	if _, err := Parse([]byte(plan), "crm"); err != nil {
		t.Fatal(err)
	}
	sample := strings.Replace(plan, `"mode":"plan"`, `"mode":"sample","sample":{"query":{"format":"dtql-yaml","text":"from: {name: customers}\n"},"limit":5}`, 1)
	if _, err := Parse([]byte(sample), "crm"); err != nil {
		t.Fatal(err)
	}
	for _, input := range []string{
		strings.Replace(sample, `"limit":5`, `"limit":101`, 1),
		strings.Replace(sample, `from: {name: customers}`, `from: {name: secrets}`, 1),
		strings.Replace(sample, `"mode":"sample"`, `"mode":"plan"`, 1),
		strings.Replace(sample, `"format":"dtql-yaml"`, `"format":"sql"`, 1),
		strings.Replace(sample, `"format":"dtql-yaml"`, `"format":"dtql-yaml","parameters":{"p":1}`, 1),
		strings.Replace(sample, `"text":"from: {name: customers}\n"`, `"text":""`, 1),
		strings.Replace(sample, `"text":"from: {name: customers}\n"`, `"text":"invalid:yaml:::"`, 1),
		strings.Replace(sample, `"action":"update"`, `"action":"query"`, 1),
	} {
		if _, err := Parse([]byte(input), "crm"); err == nil {
			t.Fatalf("accepted invalid sample: %s", input)
		}
	}
}

func TestStructureAndBodyLimits(t *testing.T) {
	t.Parallel()
	for _, input := range []string{strings.Repeat(" ", MaxRequestBytes+1), strings.Repeat("[", 34) + "0" + strings.Repeat("]", 34)} {
		var dst any
		if err := DecodeStrict([]byte(input), &dst); err == nil {
			t.Fatal("accepted oversized input")
		}
	}
}

func TestDecodeStrictEdgeCases(t *testing.T) {
	t.Parallel()
	var dst any
	// Unexpected delimiter
	if err := DecodeStrict([]byte("}"), &dst); err == nil {
		t.Fatal("expected error on }")
	}
	// Struct expected but got number
	var st struct {
		Ignored  string `json:"-"`
		Untagged string
	}
	if err := DecodeStrict([]byte("42"), &st); err == nil {
		t.Fatal("expected error when struct gets non-object")
	}
	// Struct with untagged and ignored fields
	if err := DecodeStrict([]byte(`{"Untagged":"value"}`), &st); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if st.Untagged != "value" {
		t.Errorf("Untagged = %q, want value", st.Untagged)
	}

	// Slice expected but got object
	var sl []string
	if err := DecodeStrict([]byte(`{"a":"b"}`), &sl); err == nil {
		t.Fatal("expected error when slice gets object")
	}

	// Map expected
	var mp map[string]string
	if err := DecodeStrict([]byte(`["a","b"]`), &mp); err == nil {
		t.Fatal("expected error when map gets array")
	}
	if err := DecodeStrict([]byte(`{"k":"v"}`), &mp); err != nil {
		t.Fatalf("unexpected error for map: %v", err)
	}
	var mpStruct map[string]struct{ A int }
	if err := DecodeStrict([]byte(`{"k":null}`), &mpStruct); err == nil {
		t.Fatal("expected error when map value is null for struct")
	}

	// Scan errors inside object and array
	if err := DecodeStrict([]byte(`{"a":`), &dst); err == nil {
		t.Fatal("expected scan error on truncated object")
	}
	if err := DecodeStrict([]byte(`[{"a":`), &dst); err == nil {
		t.Fatal("expected scan error on truncated array")
	}
}

func TestValidationAPIVersionAndDiagnosticLevels(t *testing.T) {
	t.Parallel()
	// APIVersion invalid
	badVer := strings.Replace(inspect, `"apiVersion":"dtql.org/authorization/v1"`, `"apiVersion":"invalid/v1"`, 1)
	if _, err := Parse([]byte(badVer), "crm"); err == nil {
		t.Fatal("expected APIVersion error")
	}

	// Diagnostic levels
	for _, lvl := range []string{"references", "policy"} {
		req := strings.Replace(inspect, `"diagnosticLevel":"ordinary"`, `"diagnosticLevel":"`+lvl+`"`, 1)
		if _, err := Parse([]byte(req), "crm"); err != nil {
			t.Fatalf("unexpected error for diagnosticLevel %s: %v", lvl, err)
		}
	}
	badLvl := strings.Replace(inspect, `"diagnosticLevel":"ordinary"`, `"diagnosticLevel":"debug"`, 1)
	if _, err := Parse([]byte(badLvl), "crm"); err == nil {
		t.Fatal("expected diagnosticLevel error")
	}
}

func TestSubjectAndSimulationValidation(t *testing.T) {
	t.Parallel()
	// Valid subject
	withSubject := strings.Replace(inspect, `"mode":"inspect"`, `"mode":"inspect","subject":{"realm":"default","kind":"user","id":"alice"}`, 1)
	if _, err := Parse([]byte(withSubject), "crm"); err != nil {
		t.Fatalf("unexpected error for valid subject: %v", err)
	}

	// Invalid subject
	badSubject := strings.Replace(inspect, `"mode":"inspect"`, `"mode":"inspect","subject":{"realm":"","kind":"","id":""}`, 1)
	if _, err := Parse([]byte(badSubject), "crm"); err == nil {
		t.Fatal("expected invalid subject error")
	}

	// Valid simulation
	validSim := strings.Replace(inspect, `"mode":"inspect"`, `"mode":"inspect","simulation":{"roles":["r1"],"groups":["g1"],"attributes":{"s":"str","n":42.5,"b":true,"nil":null}}`, 1)
	if _, err := Parse([]byte(validSim), "crm"); err != nil {
		t.Fatalf("unexpected error for valid simulation: %v", err)
	}

	// Invalid simulation: duplicate roles
	dupRoles := strings.Replace(inspect, `"mode":"inspect"`, `"mode":"inspect","simulation":{"roles":["r1","r1"],"groups":[],"attributes":{}}`, 1)
	if _, err := Parse([]byte(dupRoles), "crm"); err == nil {
		t.Fatal("expected error on duplicate roles in simulation")
	}

	// Invalid simulation: non-scalar attribute
	nonScalar := strings.Replace(inspect, `"mode":"inspect"`, `"mode":"inspect","simulation":{"roles":[],"groups":[],"attributes":{"arr":[1]}}`, 1)
	if _, err := Parse([]byte(nonScalar), "crm"); err == nil {
		t.Fatal("expected error on non-scalar attribute")
	}

	// Invalid simulation: invalid attribute key
	badAttrKey := strings.Replace(inspect, `"mode":"inspect"`, `"mode":"inspect","simulation":{"roles":[],"groups":[],"attributes":{"":1}}`, 1)
	if _, err := Parse([]byte(badAttrKey), "crm"); err == nil {
		t.Fatal("expected error on empty attribute key")
	}
}

func TestOperationActionsAndExecClasses(t *testing.T) {
	t.Parallel()
	// Valid query action
	queryOp := `{"apiVersion":"dtql.org/authorization/v1","mode":"plan","diagnosticLevel":"ordinary","operations":[{"id":"q1","action":"query","resource":{"databaseId":"crm","path":"/customers"},"query":{"format":"dtql-yaml","text":"from: {name: customers}\n"},"executionClass":"dtql"}]}`
	if _, err := Parse([]byte(queryOp), "crm"); err != nil {
		t.Fatalf("unexpected error on valid query operation: %v", err)
	}

	// Query with row specified
	queryWithRow := strings.Replace(queryOp, `"/customers"`, `"/customers/101"`, 1)
	if _, err := Parse([]byte(queryWithRow), "crm"); err == nil {
		t.Fatal("expected error when query specifies row")
	}

	// Query with mutation
	queryWithMut := strings.Replace(queryOp, `"dtql"}`, `"dtql"},"mutation":{"changes":[{"op":"set","path":["name"],"value":null}]}`, 1)
	if _, err := Parse([]byte(queryWithMut), "crm"); err == nil {
		t.Fatal("expected error when query specifies mutation")
	}

	// Truncate action
	truncateOp := `{"apiVersion":"dtql.org/authorization/v1","mode":"plan","diagnosticLevel":"ordinary","operations":[{"id":"t1","action":"truncate","resource":{"databaseId":"crm","path":"/customers"},"executionClass":"dtql"}]}`
	if _, err := Parse([]byte(truncateOp), "crm"); err != nil {
		t.Fatalf("unexpected error on valid truncate: %v", err)
	}
	truncateWithRow := strings.Replace(truncateOp, `"/customers"`, `"/customers/101"`, 1)
	if _, err := Parse([]byte(truncateWithRow), "crm"); err == nil {
		t.Fatal("expected error when truncate specifies row")
	}

	// Insert and Set actions
	insertOp := `{"apiVersion":"dtql.org/authorization/v1","mode":"plan","diagnosticLevel":"ordinary","operations":[{"id":"i1","action":"insert","resource":{"databaseId":"crm","path":"/customers/101"},"mutation":{"data":{"name":"Alice"}},"executionClass":"dtql"}]}`
	if _, err := Parse([]byte(insertOp), "crm"); err != nil {
		t.Fatalf("unexpected error on valid insert: %v", err)
	}
	insertWithChanges := strings.Replace(insertOp, `"data":{"name":"Alice"}`, `"data":{"name":"Alice"},"changes":[{"op":"set","path":["name"],"value":null}]`, 1)
	if _, err := Parse([]byte(insertWithChanges), "crm"); err == nil {
		t.Fatal("expected error when insert specifies changes")
	}
	insertWithoutData := strings.Replace(insertOp, `"data":{"name":"Alice"}`, `"changes":null`, 1)
	if _, err := Parse([]byte(insertWithoutData), "crm"); err == nil {
		t.Fatal("expected error when insert missing data")
	}

	// Delete action
	delOp := `{"apiVersion":"dtql.org/authorization/v1","mode":"plan","diagnosticLevel":"ordinary","operations":[{"id":"d1","action":"delete","resource":{"databaseId":"crm","path":"/customers/101"},"mutation":{},"executionClass":"dtql"}]}`
	if _, err := Parse([]byte(delOp), "crm"); err != nil {
		t.Fatalf("unexpected error on valid delete: %v", err)
	}
	delWithData := strings.Replace(delOp, `"mutation":{}`, `"mutation":{"data":{"x":1}}`, 1)
	if _, err := Parse([]byte(delWithData), "crm"); err == nil {
		t.Fatal("expected error when delete specifies data")
	}

	// Data revision
	withRev := strings.Replace(delOp, `"mutation":{}`, `"mutation":{"ifDataRevision":"rev-123"}`, 1)
	if _, err := Parse([]byte(withRev), "crm"); err != nil {
		t.Fatalf("unexpected error on valid data revision: %v", err)
	}
	withBadRev := strings.Replace(delOp, `"mutation":{}`, `"mutation":{"ifDataRevision":"rev\t1"}`, 1)
	if _, err := Parse([]byte(withBadRev), "crm"); err == nil {
		t.Fatal("expected error on invalid data revision")
	}

	// ExecutionClass: stored_procedure
	spOp := `{"apiVersion":"dtql.org/authorization/v1","mode":"plan","diagnosticLevel":"ordinary","operations":[{"id":"sp1","action":"get","resource":{"databaseId":"crm","path":"/customers/101"},"executionClass":"stored_procedure","callable":{"namespace":"app","name":"do_stuff"}}]}`
	if _, err := Parse([]byte(spOp), "crm"); err != nil {
		t.Fatalf("unexpected error on stored_procedure: %v", err)
	}
	spNoCallable := strings.Replace(spOp, `,"callable":{"namespace":"app","name":"do_stuff"}`, "", 1)
	if _, err := Parse([]byte(spNoCallable), "crm"); err == nil {
		t.Fatal("expected error when stored_procedure misses callable")
	}
	spBadCallable := strings.Replace(spOp, `"name":"do_stuff"`, `"name":""`, 1)
	if _, err := Parse([]byte(spBadCallable), "crm"); err == nil {
		t.Fatal("expected error when callable name is empty")
	}

	// Other execution classes
	for _, ec := range []string{"tugql", "native_sql", "native_graphql"} {
		ecOp := strings.Replace(delOp, `"executionClass":"dtql"`, `"executionClass":"`+ec+`"`, 1)
		if _, err := Parse([]byte(ecOp), "crm"); err != nil {
			t.Fatalf("unexpected error on executionClass %s: %v", ec, err)
		}
	}
}

func TestHelperValidationAndLimits(t *testing.T) {
	t.Parallel()
	// validField limits
	if validField(nil) {
		t.Error("expected false for empty field")
	}
	longField := make([]string, 17)
	for i := range longField {
		longField[i] = "f"
	}
	if validField(longField) {
		t.Error("expected false for field > 16 elements")
	}

	// validNames limits
	if validNames(nil) {
		t.Error("expected false for nil names")
	}
	longNames := make([]string, 1001)
	for i := range longNames {
		longNames[i] = "n"
	}
	if validNames(longNames) {
		t.Error("expected false for > 1000 names")
	}
	if validNames([]string{""}) {
		t.Error("expected false for invalid name")
	}
	if !validNames([]string{"a", "b"}) {
		t.Error("expected true for valid names")
	}

	// scalar
	if !scalar(nil) || !scalar(true) || !scalar("str") || !scalar(12.34) {
		t.Error("expected true for scalars")
	}
	if scalar([]int{1}) || scalar(map[string]any{}) || scalar(10) {
		t.Error("expected false for non-scalars")
	}

	// Columns > 32
	var manyCols [][]string
	for i := 0; i < 33; i++ {
		manyCols = append(manyCols, []string{"col"})
	}
	r := Operation{
		ID:             "o1",
		Action:         "get",
		ExecutionClass: "dtql",
		Resource: authorization.Resource{
			DatabaseID: "crm",
			Path:       "/customers/1",
			Columns:    manyCols,
		},
	}
	if err := r.Normalize("crm"); err == nil {
		t.Fatal("expected error for > 32 columns")
	}

	// Operation without database matching
	r2 := Operation{
		ID:             "o1",
		Action:         "get",
		ExecutionClass: "dtql",
		Resource: authorization.Resource{
			DatabaseID: "other",
			Path:       "/customers/1",
		},
	}
	if err := r2.Normalize("crm"); err == nil {
		t.Fatal("expected error on database mismatch")
	}

	// Truncated object for scan error in loop
	var dst any
	if err := DecodeStrict([]byte(`{"a": 1,`), &dst); err == nil {
		t.Fatal("expected error on truncated object key")
	}

	// Validate with 0 operations
	emptyReq := &Request{
		APIVersion:      authorization.APIVersion,
		Mode:            authorization.ModePlan,
		DiagnosticLevel: "ordinary",
	}
	if err := emptyReq.Validate("crm"); err == nil {
		t.Fatal("expected error for 0 operations")
	}

	// Validate with duplicate operation ID
	dupOpReq := &Request{
		APIVersion:      authorization.APIVersion,
		Mode:            authorization.ModePlan,
		DiagnosticLevel: "ordinary",
		Operations: []Operation{
			{ID: "op1", Action: "get", ExecutionClass: "dtql", Resource: authorization.Resource{DatabaseID: "crm", Path: "/customers/1"}},
			{ID: "op1", Action: "get", ExecutionClass: "dtql", Resource: authorization.Resource{DatabaseID: "crm", Path: "/customers/2"}},
		},
	}
	if err := dupOpReq.Validate("crm"); err == nil {
		t.Fatal("expected error on duplicate operation ID")
	}

	// Sample mode with row ID on operation
	sampleWithRow := &Request{
		APIVersion:      authorization.APIVersion,
		Mode:            authorization.ModeSample,
		DiagnosticLevel: "ordinary",
		Sample:          &Sample{Limit: 5, Query: Query{Format: "dtql-yaml", Text: "from: {name: customers}\n"}},
		Operations: []Operation{
			{ID: "op1", Action: "get", ExecutionClass: "dtql", Resource: authorization.Resource{DatabaseID: "crm", Path: "/customers/1"}},
		},
	}
	if err := sampleWithRow.Validate("crm"); err == nil {
		t.Fatal("expected error on sample mode with row ID")
	}

	// Normalize: callable supplied on DTQL
	opWithCallable := Operation{
		ID:             "op1",
		Action:         "get",
		ExecutionClass: authorization.ExecutionDTQL,
		Callable:       &authorization.Callable{Namespace: "ns", Name: "name"},
		Resource:       authorization.Resource{DatabaseID: "crm", Path: "/customers/1"},
	}
	if err := opWithCallable.Normalize("crm"); err == nil {
		t.Fatal("expected error on callable with ExecutionDTQL")
	}

	// Normalize: query supplied for non-query action
	opWithQuery := Operation{
		ID:             "op1",
		Action:         "get",
		ExecutionClass: "dtql",
		Query:          &Query{Format: "dtql-yaml", Text: "from: {name: customers}\n"},
		Resource:       authorization.Resource{DatabaseID: "crm", Path: "/customers/1"},
	}
	if err := opWithQuery.Normalize("crm"); err == nil {
		t.Fatal("expected error when query supplied for get action")
	}

	// Normalize: query action with invalid query
	opBadQuery := Operation{
		ID:             "op1",
		Action:         "query",
		ExecutionClass: "dtql",
		Query:          &Query{Format: "dtql-yaml", Text: "invalid dtql"},
		Resource:       authorization.Resource{DatabaseID: "crm", Path: "/customers"},
	}
	if err := opBadQuery.Normalize("crm"); err == nil {
		t.Fatal("expected error on invalid query parse")
	}

	// Normalize: unexpected mutation on get action
	opMutGet := Operation{
		ID:             "op1",
		Action:         "get",
		ExecutionClass: "dtql",
		Mutation:       &Mutation{},
		Resource:       authorization.Resource{DatabaseID: "crm", Path: "/customers/1"},
	}
	if err := opMutGet.Normalize("crm"); err == nil {
		t.Fatal("expected error when mutation supplied for get")
	}

	// Normalize: insert action with nil mutation
	opNoMutInsert := Operation{
		ID:             "op1",
		Action:         "insert",
		ExecutionClass: "dtql",
		Resource:       authorization.Resource{DatabaseID: "crm", Path: "/customers/1"},
	}
	if err := opNoMutInsert.Normalize("crm"); err == nil {
		t.Fatal("expected error on insert without mutation")
	}

	// Normalize: update action with Data != nil
	opUpdateData := Operation{
		ID:             "op1",
		Action:         "update",
		ExecutionClass: "dtql",
		Mutation:       &Mutation{Data: map[string]any{"a": 1}},
		Resource:       authorization.Resource{DatabaseID: "crm", Path: "/customers/1"},
	}
	if err := opUpdateData.Normalize("crm"); err == nil {
		t.Fatal("expected error on update with data")
	}

	// Normalize: update action with mismatched columns length
	opUpdateColLen := Operation{
		ID:             "op1",
		Action:         "update",
		ExecutionClass: "dtql",
		Resource: authorization.Resource{
			DatabaseID: "crm",
			Path:       "/customers/1",
			Columns:    [][]string{{"name"}, {"email"}},
		},
		Mutation: &Mutation{
			Changes: []Change{
				{Op: "set", Path: []string{"name"}, Value: []byte(`"Alice"`)},
			},
		},
	}
	if err := opUpdateColLen.Normalize("crm"); err == nil {
		t.Fatal("expected error on update with mismatched columns length")
	}

	// Normalize: unknown action
	opUnknownAction := Operation{
		ID:             "op1",
		Action:         "unknown_action",
		ExecutionClass: "dtql",
		Resource:       authorization.Resource{DatabaseID: "crm", Path: "/customers/1"},
	}
	if err := opUnknownAction.Normalize("crm"); err == nil {
		t.Fatal("expected error on unknown operation action")
	}
}
