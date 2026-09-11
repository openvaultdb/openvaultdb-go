package authorizationapi

import (
	"strings"
	"testing"
)

const inspect = `{"apiVersion":"dtql.org/authorization/v1","mode":"inspect","diagnosticLevel":"ordinary","operations":[{"id":"u1","action":"update","resource":{"databaseId":"crm","path":"/customers/101"},"mutation":{"changes":[{"op":"set","path":["name"],"value":null}]},"executionClass":"dtql"}]}`

func TestNormalizeInspect(t *testing.T) {
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
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(input), "crm"); err == nil {
				t.Fatal("accepted invalid request")
			}
		})
	}
}

func TestNestedFieldAndDottedRowIdentityRemainDistinct(t *testing.T) {
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
	} {
		if _, err := Parse([]byte(input), "crm"); err == nil {
			t.Fatal("accepted invalid sample")
		}
	}
}

func TestStructureAndBodyLimits(t *testing.T) {
	for _, input := range []string{strings.Repeat(" ", MaxRequestBytes+1), strings.Repeat("[", 34) + "0" + strings.Repeat("]", 34)} {
		var dst any
		if err := DecodeStrict([]byte(input), &dst); err == nil {
			t.Fatal("accepted oversized input")
		}
	}
}
