package core

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/dal-go/dalgo/access"
	"github.com/dal-go/dalgo/dal"
)

// TestSampleReadsThePolicyLayersBeforeLookingAtCollectionNames: a sample on a
// database with access policies reads the policy layers first. While a layer
// cannot be used (its source is unreadable, it holds no policy, or it holds a
// policy not declared safe for inspection), a collection the database does not
// declare gets the refusal a declared collection gets, with the same order, and
// the driver is not reached for either. While the layers can be used, the
// collection rule applies as it does without policies.
func TestSampleReadsThePolicyLayersBeforeLookingAtCollectionNames(t *testing.T) {
	ctx := context.Background()
	parse := func(collection string) dal.StructuredQuery {
		query, _, err := ParseDTQL([]byte("from: {name: " + collection + "}\n"))
		if err != nil {
			t.Fatal(err)
		}
		return query
	}
	pure, err := access.DeclareInspectionPure(joinSrcAllowPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	for _, engine := range []string{"sqlite"} {
		for _, layer := range []struct {
			name   string
			policy access.Policy
			source access.PolicyProvider // replaces the policy source of the owner layer when set
		}{
			{"policy not declared safe for inspection", joinSrcAllowPolicy{}, nil},
			{"unreadable policy source", pure, func(context.Context) ([]access.Policy, error) { return nil, errors.New("source down") }},
			{"policy source holding no policy", pure, func(context.Context) ([]access.Policy, error) { return nil, nil }},
		} {
			t.Run(engine+"/"+layer.name, func(t *testing.T) {
				db, calls := joinSrcProtected(t, engine, layer.policy)
				if layer.source != nil {
					db.ownerPolicies = layer.source
				}
				_, declaredOrder, declaredErr := db.SelectAccessSample(ctx, parse("customers"), 1, access.Principal{})
				_, undeclaredOrder, undeclaredErr := db.SelectAccessSample(ctx, parse("nowhere"), 1, access.Principal{})
				for label, err := range map[string]error{"declared": declaredErr, "undeclared": undeclaredErr} {
					if !errors.Is(err, access.ErrAccessDenied) || errors.Is(err, ErrNotFound) {
						t.Errorf("%s collection: got %v, want the refusal of the policy layer", label, err)
					}
				}
				if !reflect.DeepEqual(access.DecisionsFromError(declaredErr), access.DecisionsFromError(undeclaredErr)) {
					t.Errorf("decisions differ: declared %+v, undeclared %+v", access.DecisionsFromError(declaredErr), access.DecisionsFromError(undeclaredErr))
				}
				if len(declaredOrder) == 0 || !reflect.DeepEqual(declaredOrder, undeclaredOrder) {
					t.Errorf("order differs: declared %v, undeclared %v", declaredOrder, undeclaredOrder)
				}
				if calls.readers != 0 || calls.recordsets != 0 || calls.txStarts != 0 {
					t.Errorf("the driver was reached: %+v", *calls)
				}
			})
		}
		t.Run(engine+"/usable policy layer", func(t *testing.T) {
			db, calls := joinSrcProtected(t, engine, pure)
			_, _, undeclaredErr := db.SelectAccessSample(ctx, parse("nowhere"), 1, access.Principal{})
			if !errors.Is(undeclaredErr, ErrNotFound) {
				t.Errorf("undeclared collection: got %v, want ErrNotFound", undeclaredErr)
			}
			if calls.readers != 0 || calls.recordsets != 0 {
				t.Errorf("the driver was reached for an undeclared collection: %+v", *calls)
			}
			_, _, declaredErr := db.SelectAccessSample(ctx, parse("customers"), 1, access.Principal{})
			if !errors.Is(declaredErr, errJoinSrcReader) || calls.readers+calls.recordsets == 0 {
				t.Errorf("declared collection: got %v, calls %+v, want the driver's error", declaredErr, *calls)
			}
		})
	}
}

// TestSampleSkipsAPolicyLayerThatIsNotEnabled: an inGitDB mount reports a lower
// policy layer whether or not its storage supplies policies. A layer that is not
// enabled is not read, and the sample reads under the owner's policies.
func TestSampleSkipsAPolicyLayerThatIsNotEnabled(t *testing.T) {
	pure, err := access.DeclareInspectionPure(joinSrcAllowPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	db, calls := joinSrcProtected(t, "ingitdb", pure)
	if layers := db.PolicyLayers(context.Background()); len(layers) != 2 || !layers[0].Enabled || layers[1].Enabled {
		t.Fatalf("want an enabled owner layer and a lower layer that is not enabled: %+v", layers)
	}
	query, _, err := ParseDTQL([]byte("from: {name: customers}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = db.SelectAccessSample(context.Background(), query, 1, access.Principal{}); !errors.Is(err, errJoinSrcReader) || calls.readers+calls.recordsets == 0 {
		t.Errorf("got %v, calls %+v, want the driver's error", err, *calls)
	}
}
