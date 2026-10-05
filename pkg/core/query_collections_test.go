package core

import (
	"errors"
	"reflect"
	"slices"
	"testing"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/record"
)

// TestQueryCollectionsListsTheSourceOfEveryPosition: the list is what the
// source walk visits, so a collection a query reads in any position (a join, a
// derived source, a subquery wherever it sits, a scan order) is in it, on every
// engine. A caller authorises a read per collection from it.
func TestQueryCollectionsListsTheSourceOfEveryPosition(t *testing.T) {
	for _, c := range srcGuardCases("x") {
		got, err := QueryCollections(c.query)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if !slices.Contains(got, "x") {
			t.Errorf("%s: %v does not list x", c.name, got)
		}
		for _, name := range got {
			if name != "x" && name != "customers" && name != "orders" {
				t.Errorf("%s: %v lists a collection the query does not read", c.name, got)
			}
		}
	}
}

// TestQueryCollectionsListsEachCollectionOnceInDocumentOrder.
func TestQueryCollectionsListsEachCollectionOnceInDocumentOrder(t *testing.T) {
	query := selectQuery(fromTree(rootRef("customers"),
		dal.NewJoinedSource(rootRef("orders"), dal.JoinInner, srcGuardJoinOn("customers", "orders")),
		dal.NewJoinedSource(rootRef("customers"), dal.JoinInner, srcGuardJoinOn("customers", "orders"))).NewQuery().
		Where(dal.NewExistsCondition(srcGuardInner("orders"))).
		Where(dal.NewExistsCondition(srcGuardInner("items"))))
	got, err := QueryCollections(query)
	if err != nil || !reflect.DeepEqual(got, []string{"customers", "orders", "items"}) {
		t.Fatalf("got %v, %v", got, err)
	}
}

// TestQueryCollectionsRefusesASourceThatIsNotAPlainRootCollection: a source that
// a parent record, a schema or a database qualifies is not named by one root
// collection, so no capability on a collection covers it.
func TestQueryCollectionsRefusesASourceThatIsNotAPlainRootCollection(t *testing.T) {
	parent := record.NewKeyWithID("customers", "c1")
	for label, source := range map[string]dal.RecordsetSource{
		"parent":            dal.NewCollectionRef("orders", "", parent),
		"schema":            dal.NewQualifiedRootCollectionRef("hr", "orders", ""),
		"database":          dal.NewDatabaseCollectionRef("other", "", "orders", ""),
		"database + schema": dal.NewDatabaseCollectionRef("other", "hr", "orders", ""),
	} {
		for position, query := range map[string]dal.StructuredQuery{
			"root":     selectQuery(fromTree(source).NewQuery()),
			"subquery": withExists("customers", selectQuery(fromTree(source).NewQuery())),
		} {
			got, err := QueryCollections(query)
			if !errors.Is(err, ErrInvalidDTQL) || got != nil {
				t.Errorf("%s %s: got %v, %v", label, position, got, err)
			}
		}
	}
}

// TestQueryCollectionsFailsClosedOnShapesItDoesNotKnow: a shape the walk cannot
// read lists nothing and is refused, as the source guard refuses it.
func TestQueryCollectionsFailsClosedOnShapesItDoesNotKnow(t *testing.T) {
	for label, query := range srcGuardWalkerCases() {
		got, err := QueryCollections(query)
		if !errors.Is(err, ErrInvalidDTQL) || got != nil {
			t.Errorf("%s: got %v, %v", label, got, err)
		}
	}
}
