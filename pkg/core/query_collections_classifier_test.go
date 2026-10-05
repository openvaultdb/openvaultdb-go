package core

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/dal-go/dalgo/dtql"
)

// The per-database endpoint reads the collections of a document with
// QueryCollections only for a document the classifier calls single-collection
// (every other document the classifier accepts is checked against the sources the
// classifier itself found). Such a document has one root collection and no subquery
// or join, so the two walks count its depth alike, and QueryCollections must list
// it whatever the document nests. The test sends the same body through both walks
// at every nesting depth from none to past the limit of both, in the shapes that put
// the depth in a condition, an ordering and a column.
func TestQueryCollectionsListsEveryDocumentTheClassifierCallsSingleCollection(t *testing.T) {
	nested := func(groups int) string {
		condition := "{op: '==', left: {field: name}, right: {value: x}}"
		for i := 0; i < groups; i++ {
			condition = "{and: [" + condition + ", {op: In, left: {field: id}, right: {values: [a, b]}}]}"
		}
		return condition
	}
	accepted, refused := 0, 0
	for _, shape := range []struct {
		name string
		doc  func(groups int) string
	}{
		{"a condition", func(groups int) string { return "from: {name: customers}\nwhere: " + nested(groups) + "\n" }},
		{"a condition, an ordering and columns", func(groups int) string {
			return "from: {name: customers}\nwhere: " + nested(groups) + "\norderBy: [{field: name, desc: true}, {field: id}]\ncolumns: [{field: id}, {field: name}]\nlimit: 5\n"
		}},
		{"an or of groups", func(groups int) string {
			condition := "{op: '>', left: {field: total}, right: {value: 3}}"
			for i := 0; i < groups; i++ {
				condition = "{or: [" + condition + ", {isNull: {field: name}}]}"
			}
			return "from: {name: customers}\nwhere: " + condition + "\n"
		}},
	} {
		for groups := 0; groups <= 20; groups++ {
			doc := shape.doc(groups)
			query, err := dtql.Deserialize([]byte(doc))
			if err != nil {
				t.Fatalf("%s, %d groups: %v", shape.name, groups, err)
			}
			profile, err := ClassifyDTQL(query)
			if err != nil || profile.Kind != ProfileSingleCollection {
				// Past the depth the classifier allows, or not a single-collection document
				// (an IS NULL test is a relational feature): the listing is not asked.
				refused++
				continue
			}
			accepted++
			got, err := QueryCollections(query)
			if err != nil || !reflect.DeepEqual(got, []string{"customers"}) {
				t.Fatalf("%s, %d groups: the classifier calls the document single-collection and QueryCollections lists %v, %v", shape.name, groups, got, err)
			}
		}
	}
	// The loop is not vacuous: it reaches documents of both kinds, up to the depth at
	// which the classifier stops accepting them.
	if accepted < 30 || refused < 10 {
		t.Fatalf("%d documents classified as single-collection and %d not: the table does not reach the limit", accepted, refused)
	}
}

// A document with a subquery is the one kind for which the classifier and the
// listing can differ, and it is not single-collection: this is the document the two
// answer differently, so that the test above does not hide it.
func TestQueryCollectionsRefusesADocumentTheClassifierAcceptsWithADerivedSourceOnAJoinEdge(t *testing.T) {
	where := "{op: '==', left: {field: name}, right: {value: x}}"
	for i := 0; i < 13; i++ {
		where = "{and: [" + where + "]}"
	}
	doc := "from: {name: customers}\nwhere: {exists: {query: {from: {name: orders, joins: [{from: {query: {as: d, from: {name: customers}, where: " + where +
		"}}, on: [{op: '==', left: {field: id, source: orders}, right: {field: id, source: d}}]}]}}}}\n"
	query, err := dtql.Deserialize([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	profile, err := ClassifyDTQL(query)
	if err != nil || profile.Kind != ProfileRelational || !profile.HasSubquery {
		t.Fatalf("classification = %+v, %v, want a relational document", profile, err)
	}
	if _, err := QueryCollections(query); err == nil || !strings.Contains(err.Error(), "too deep") {
		t.Fatalf("QueryCollections = %v, want a refusal for depth", err)
	}
	if got := fmt.Sprint(len(profile.Sources)); got != "3" {
		t.Fatalf("the classifier lists %s sources, want 3", got)
	}
}
