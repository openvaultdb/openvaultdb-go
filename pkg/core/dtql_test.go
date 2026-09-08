package core

import (
	"errors"
	"testing"
)

func TestParseDTQLSupportedProfile(t *testing.T) {
	for _, doc := range []string{
		"from: {name: customers, alias: c}\n",
		"from: {name: customers}\nlimit: -1\n",
		"from: {name: customers}\nlimit: 1001\n",
		"from: {name: customers}\noffset: 10001\n",
		"from: {name: customers}\ncolumns: [{field: name, as: other}]\n",
		"from: {name: customers}\n---\nfrom: {name: secret}\n",
	} {
		if _, _, err := ParseDTQL([]byte(doc)); !errors.Is(err, ErrInvalidDTQL) {
			t.Errorf("expected invalid query for %q: %v", doc, err)
		}
	}
	query, collection, err := ParseDTQL([]byte("from: {name: customers}\n"))
	if err != nil || collection != "customers" {
		t.Fatalf("parse: %s %v", collection, err)
	}
	bounded := boundedDTQL{query}
	if bounded.Limit() != 1000 || bounded.IntoRecord() == nil {
		t.Fatalf("missing default limit/record factory")
	}
}
