package joinexec

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/dal-go/dalgo/dal"
)

// exRouterOver builds the router of a request over mounts, for a document that
// reads databases.
func exRouterOver(databases []string, mounts ...*exSource) (*router, *run) {
	sources := map[string]Source{}
	for _, mount := range mounts {
		sources[mount.id] = mount
	}
	r := &run{guard: NewGuard(exAllow, Limits{}), sources: sources}
	return newRouter(r, databases), r
}

func TestRouterReadsEachCollectionThroughTheLeafOfItsDatabase(t *testing.T) {
	one := exMount("one", "sqlite", false, nil)
	two := exMount("two", "sqlite", false, nil)
	rt, _ := exRouterOver([]string{"one", "two"}, one, two)
	if _, err := rt.ExecuteQueryToRecordsReader(context.Background(), exPlain("two", "B")); err != nil {
		t.Fatalf("read of two.B: %v", err)
	}
	if len(one.exec.seen()) != 0 || len(two.exec.seen()) != 1 {
		t.Fatalf("one saw %d reads and two saw %d, want 0 and 1", len(one.exec.seen()), len(two.exec.seen()))
	}
	// A document that reads one database reads a collection that names none
	// from it.
	only, _ := exRouterOver([]string{"one"}, one, two)
	if _, err := only.ExecuteQueryToRecordsReader(context.Background(), exPlain("", "A")); err != nil {
		t.Fatalf("read of A: %v", err)
	}
	if len(one.exec.seen()) != 1 {
		t.Fatalf("one saw %d reads, want 1", len(one.exec.seen()))
	}
}

func TestRouterFailsClosedForADatabaseOutsideThePreflightSet(t *testing.T) {
	one := exMount("one", "sqlite", false, nil)
	for name, tc := range map[string]struct {
		databases []string
		query     dal.StructuredQuery
		want      string
	}{
		"a database no source was resolved for":                             {[]string{"one"}, exPlain("ghost", "G"), "ghost"},
		"a collection that names none in a document over several databases": {[]string{"one", "two"}, exPlain("", "A"), ""},
	} {
		t.Run(name, func(t *testing.T) {
			rt, r := exRouterOver(tc.databases, one)
			_, err := rt.ExecuteQueryToRecordsReader(context.Background(), tc.query)
			var unknown *UnknownDatabaseError
			if !errors.As(err, &unknown) || unknown.Database != tc.want {
				t.Fatalf("err = %v, want *UnknownDatabaseError for %q", err, tc.want)
			}
			// The failure is the request's: DALgo rewraps what it is given as text,
			// and the guard still reports the typed one.
			if got := r.guard.Classify(errors.New("join_plan: cannot scan"), RouteInMemory); !errors.As(got, &unknown) {
				t.Fatalf("classified = %v, want the recorded *UnknownDatabaseError", got)
			}
			if len(one.exec.seen()) != 0 {
				t.Fatal("a read went to a mount although the database was not in the set")
			}
		})
	}
}

func TestRouterRefusesWhatIsNotAPlainCollectionRead(t *testing.T) {
	one := exMount("one", "sqlite", false, nil)
	derivedBase := dal.From(exDerived("one", "B", "d")).NewQuery().SelectIntoRecord(nil)
	pointer := exDerived("one", "B", "d")
	for name, read := range map[string]func(*router) error{
		"a text query": func(rt *router) error {
			_, err := rt.ExecuteQueryToRecordsReader(context.Background(), dal.NewTextQuery("SELECT 1", nil))
			return err
		},
		"a query without a from clause": func(rt *router) error {
			_, err := rt.ExecuteQueryToRecordsReader(context.Background(), exShapeQuery{StructuredQuery: exBase(), hasFrom: true})
			return err
		},
		"a query over a derived source": func(rt *router) error {
			_, err := rt.ExecuteQueryToRecordsReader(context.Background(), derivedBase)
			return err
		},
		"the fields of a pointer to a derived source": func(rt *router) error {
			_, err := rt.JoinFields(context.Background(), &pointer)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			rt, r := exRouterOver([]string{"one"}, one)
			if err := read(rt); !errors.Is(err, ErrNotSingleSource) {
				t.Fatalf("err = %v, want ErrNotSingleSource", err)
			}
			if !errors.Is(r.guard.Err(), ErrNotSingleSource) {
				t.Fatalf("recorded failure = %v", r.guard.Err())
			}
			if len(one.exec.seen()) != 0 {
				t.Fatal("a refused read reached a mount")
			}
		})
	}
}

func TestRouterServesFieldsThroughTheLeafAndNothingForADerivedSource(t *testing.T) {
	one := exMount("one", "sqlite", false, nil)
	one.exec.fields = []string{"id", "k"}
	rt, _ := exRouterOver([]string{"one"}, one)
	fields, err := rt.JoinFields(context.Background(), exRef("one", "A", ""))
	if err != nil || !reflect.DeepEqual(fields, []string{"id", "k"}) {
		t.Fatalf("fields = %v, %v", fields, err)
	}
	fields, err = rt.JoinFields(context.Background(), exDerived("one", "B", "d"))
	if err != nil || fields != nil {
		t.Fatalf("a derived source has no schema to serve: %v, %v", fields, err)
	}
	// A collection of a database outside the set is refused, like a read of it.
	if _, err := rt.JoinFields(context.Background(), exRef("ghost", "G", "")); err == nil {
		t.Fatal("the fields of a collection of an unresolved database were served")
	}
}

func TestRouterDoesNotOfferRecordsetReaders(t *testing.T) {
	rt, _ := exRouterOver([]string{"one"}, exMount("one", "sqlite", false, nil))
	if _, err := rt.ExecuteQueryToRecordsetReader(context.Background(), exPlain("one", "A")); !errors.Is(err, ErrRecordsetUnsupported) {
		t.Fatalf("err = %v, want ErrRecordsetUnsupported", err)
	}
}
