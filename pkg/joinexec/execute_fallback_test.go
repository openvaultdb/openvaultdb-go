package joinexec

import (
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/dal-go/dalgo/access"
	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/record"
)

// A SQL adapter that cannot compile a document answers the whole document with
// an enforcement-unsupported denial. A join the database route hands to such an
// adapter is read in memory instead, where each collection is read on its own
// and DALgo joins above the reads. These tests name the helpers of this file
// exFallback so they cannot clash with the others of the package.

// exFallbackUnsupported is the denial an adapter answers with when it cannot
// compile a document.
func exFallbackUnsupported() error {
	return &access.DeniedError{Decision: access.Decision{Operation: access.Query, Code: access.CodeEnforcementUnsupported, Explanation: "structured SQLite query is unsupported"}}
}

// exFallbackMount is a SQLite mount without policies that holds collections A and
// B and whose database route is refused with err.
func exFallbackMount(err error) *exSource {
	mount := exMount("db", "sqlite", false, map[string][]record.Record{"A": exRows("A", "a", 3), "B": exRows("B", "b", 3)})
	mount.txErr = err
	return mount
}

// exFallbackJoin joins A to B inside the default database, as a document of the
// per-database endpoint does.
func exFallbackJoin() dal.StructuredQuery {
	return exJoin(exRef("", "A", "a"), exRef("", "B", "b"), true)
}

func TestExecuteReadsAJoinInMemoryWhenTheDatabaseCannotCompileIt(t *testing.T) {
	t.Run("the join is answered from reads of each collection", func(t *testing.T) {
		mount := exFallbackMount(exFallbackUnsupported())
		res, err := exRun(t, exFallbackJoin(), "db", newExRegistry(mount), exAllow, Limits{})
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if res.Execution.Route != RouteInMemory {
			t.Fatalf("route = %q, want %q", res.Execution.Route, RouteInMemory)
		}
		if got := exAids(t, res.Records); !reflect.DeepEqual(got, []int{1, 2, 3}) {
			t.Fatalf("aid = %v", got)
		}
		if !reflect.DeepEqual(res.Columns, []string{"aid", "bname"}) {
			t.Fatalf("columns = %v", res.Columns)
		}
		if _, txCalls := mount.counts(); txCalls != 1 {
			t.Fatalf("the database was asked %d times, want once", txCalls)
		}
		var read []string
		for _, source := range res.Execution.Sources {
			read = append(read, source.Collection)
			if source.Rows == nil || *source.Rows != 3 {
				t.Fatalf("source %v: a read in memory reports the rows it delivered", source)
			}
		}
		if !reflect.DeepEqual(read, []string{"A", "B"}) {
			t.Fatalf("sources = %v", res.Execution.Sources)
		}
	})
	t.Run("the slot of the database route is given back before one in memory is taken", func(t *testing.T) {
		mount := exFallbackMount(exFallbackUnsupported())
		admission := &exAdmission{}
		var releasedAtAsk []int
		admission.onAsk = func() { releasedAtAsk = append(releasedAtAsk, admission.released) }
		if _, err := exRun(t, exFallbackJoin(), "db", newExRegistry(mount), exAllow, Limits{}, WithAdmission(admission.admit)); err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if !reflect.DeepEqual(admission.asked, []string{RouteDatabase, RouteInMemory}) || !reflect.DeepEqual(releasedAtAsk, []int{0, 1}) || admission.releases() != 2 {
			t.Fatalf("asked %v with %v slots given back at each ask, %d at the end; want the database slot back before the in-memory ask and both back at the end", admission.asked, releasedAtAsk, admission.releases())
		}
	})
	t.Run("a refusal of the in-memory slot reads nothing and gives the first slot back", func(t *testing.T) {
		mount := exFallbackMount(exFallbackUnsupported())
		admission := &exAdmission{refuse: map[string]bool{RouteInMemory: true}}
		_, err := exRun(t, exFallbackJoin(), "db", newExRegistry(mount), exAllow, Limits{}, WithAdmission(admission.admit))
		var capacity *CapacityError
		if !errors.As(err, &capacity) || capacity.Route != RouteInMemory {
			t.Fatalf("err = %v, want *CapacityError for %q", err, RouteInMemory)
		}
		if executorCalls, _ := mount.counts(); executorCalls != 0 || admission.releases() != 1 {
			t.Fatalf("%d reads, %d slots given back; want no read and the database slot back once", executorCalls, admission.releases())
		}
	})
	t.Run("a failure of the read in memory is the answer", func(t *testing.T) {
		mount := exFallbackMount(exFallbackUnsupported())
		mount.exec.openErr = errExBoom
		_, err := exRun(t, exFallbackJoin(), "db", newExRegistry(mount), exAllow, Limits{})
		if !errors.Is(err, errExBoom) || errors.Is(err, access.ErrAccessDenied) {
			t.Fatalf("err = %v, want the failure of the read", err)
		}
	})
	t.Run("the authoriser still gates every read in memory", func(t *testing.T) {
		mount := exFallbackMount(exFallbackUnsupported())
		_, err := exRun(t, exFallbackJoin(), "db", newExRegistry(mount), func(_, collection string) bool { return collection == "A" }, Limits{})
		if denied := exAsDenied(t, err); denied.Collection != "B" {
			t.Fatalf("denied = %+v, want collection B", denied)
		}
		if executorCalls, txCalls := mount.counts(); executorCalls != 0 || txCalls != 0 {
			t.Fatalf("a source the caller may not read was reached (%d reads, %d transactions)", executorCalls, txCalls)
		}
	})
}

// Only the shape that adapters cannot compile because of the key column they add
// is retried: a join with no aggregation. Everything else keeps the answer of the
// database route.
func TestExecuteDoesNotRetryOtherRefusalsOfTheDatabaseRoute(t *testing.T) {
	aggregate := dal.From(exRef("", "A", "a")).Join(dal.NewJoinedSource(exRef("", "B", "b"), dal.JoinInner, exKeyEquals(exRef("", "A", "a"), exRef("", "B", "b")))).NewQuery().
		SelectColumns(dal.CountAs(dal.Star(), "n"))
	for name, tc := range map[string]struct {
		query dal.StructuredQuery
		err   error
	}{
		"a document with one source":      {exPlain("", "A"), exFallbackUnsupported()},
		"a join that aggregates":          {aggregate, exFallbackUnsupported()},
		"a denial of another kind":        {exFallbackJoin(), &access.DeniedError{Decision: access.Decision{Operation: access.Query, Code: access.CodeAccessDenied}}},
		"a denial with several decisions": {exFallbackJoin(), &access.DeniedError{Decisions: []access.Decision{{Code: access.CodeEnforcementUnsupported}, {Code: access.CodeAccessDenied}}}},
		"an error that is not a denial":   {exFallbackJoin(), errExBoom},
		"the denial sentinel by itself":   {exFallbackJoin(), fmt.Errorf("wrapped: %w", access.ErrAccessDenied)},
	} {
		t.Run(name, func(t *testing.T) {
			mount := exFallbackMount(tc.err)
			admission := &exAdmission{}
			_, err := exRun(t, tc.query, "db", newExRegistry(mount), exAllow, Limits{}, WithAdmission(admission.admit))
			if !errors.Is(err, tc.err) {
				t.Fatalf("err = %v, want the error of the database route", err)
			}
			if executorCalls, txCalls := mount.counts(); executorCalls != 0 || txCalls != 1 {
				t.Fatalf("%d reads in memory, %d transactions; want none in memory and one transaction", executorCalls, txCalls)
			}
			if !reflect.DeepEqual(admission.asked, []string{RouteDatabase}) || admission.releases() != 1 {
				t.Fatalf("asked %v, %d slots back", admission.asked, admission.releases())
			}
		})
	}
}
