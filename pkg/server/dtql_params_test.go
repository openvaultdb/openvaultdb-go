package server

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/openvaultdb/openvaultdb-go/pkg/core"
)

func TestBoundDTQLParameters(t *testing.T) {
	const query = "from: {name: Album}\nwhere: {op: '>=', left: {field: ArtistId}, right: {param: MinArtistID}}\nlimit: 50\n"
	body := `{"query":` + quoteJSON(query) + `,"parameters":{"MinArtistID":1}}`
	doc, err := bindDTQLParameters([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(doc), "param:") {
		t.Fatalf("parameter was not bound: %s", doc)
	}
	if _, collection, err := core.ParseDTQL(doc); err != nil || collection != "Album" {
		t.Fatalf("parsed bound query collection=%q err=%v: %s", collection, err, doc)
	}
}

func TestBoundDTQLParametersRejectsMissingAndStructuralValues(t *testing.T) {
	const query = "from: {name: Album}\nwhere: {op: '==', left: {field: ArtistId}, right: {param: P}}\n"
	for _, parameters := range []string{`{}`, `{"P":{"field":"ArtistId"}}`, `{"P":null}`, `{"P":1,"Unused":2}`} {
		body := `{"query":` + quoteJSON(query) + `,"parameters":` + parameters + `}`
		if _, err := bindDTQLParameters([]byte(body)); err == nil {
			t.Errorf("parameters %s unexpectedly accepted", parameters)
		}
	}
}

func TestBoundDTQLParameterCannotChangeQueryShape(t *testing.T) {
	const query = "from: {name: Album}\nwhere: {op: '==', left: {field: Title}, right: {param: Title}}\n"
	value := `x"}]}\nfrom: {name: Customer}`
	body := `{"query":` + quoteJSON(query) + `,"parameters":{"Title":` + quoteJSON(value) + `}}`
	doc, err := bindDTQLParameters([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if _, collection, err := core.ParseDTQL(doc); err != nil || collection != "Album" {
		t.Fatalf("injected value changed query: collection=%q err=%v: %s", collection, err, doc)
	}
}

func quoteJSON(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}
