package joinexec

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/record"
)

// exAsRows flattens records into their data with every number as a float64
// (the generic join engine normalises numbers), sorted by aid so that the
// streaming join's order does not matter.
func exAsRows(t *testing.T, records []record.Record) []map[string]any {
	t.Helper()
	rows := make([]map[string]any, len(records))
	for i, rec := range records {
		row := map[string]any{}
		for key, value := range rec.Data().(map[string]any) {
			switch n := value.(type) {
			case int:
				row[key] = float64(n)
			case int64:
				row[key] = float64(n)
			default:
				row[key] = value
			}
		}
		rows[i] = row
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i]["aid"].(float64) < rows[j]["aid"].(float64) })
	return rows
}

func TestExecuteDatabaseRouteRunsTheWholeDocumentInOneReadTx(t *testing.T) {
	sqlite := exMount("chinook", "sqlite", false, nil)
	sqlite.exec.whole = exAnswer(map[string]any{"aid": 1, "bname": "x"}, map[string]any{"aid": 2, "bname": "y"})
	q := exJoin(exRef("", "Invoice", "i"), exRef("", "Customer", "c"), true)
	registry := newExRegistry(sqlite)

	res, err := exRun(t, q, "chinook", registry, exAllow, Limits{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	executorCalls, txCalls := sqlite.counts()
	if txCalls != 1 || executorCalls != 0 {
		t.Fatalf("ReadTx called %d times and Executor %d times, want 1 and 0 (no leaf on the database route)", txCalls, executorCalls)
	}
	seen := sqlite.exec.seen()
	if len(seen) != 1 || len(seen[0].(dal.StructuredQuery).From().Joins()) != 1 {
		t.Fatalf("the transaction executor must get the whole document once, got %d queries", len(seen))
	}
	if !reflect.DeepEqual(registry.lookups, []string{"chinook"}) {
		t.Fatalf("lookups = %v: a mount is resolved once", registry.lookups)
	}
	if res.Execution.Route != RouteDatabase || res.Execution.RowsReturned != 2 || len(res.Records) != 2 {
		t.Fatalf("result = %+v", res.Execution)
	}
	if !reflect.DeepEqual(res.Columns, []string{"aid", "bname"}) {
		t.Fatalf("columns = %v", res.Columns)
	}
	// The database ran the document: no per-source rows or times are known.
	want := []ExecutionSource{{Database: "chinook", Collection: "Invoice"}, {Database: "chinook", Collection: "Customer"}}
	if !reflect.DeepEqual(res.Execution.Sources, want) {
		t.Fatalf("sources = %+v, want %+v", res.Execution.Sources, want)
	}
}

func TestExecuteDatabaseRouteDeduplicatesSources(t *testing.T) {
	sqlite := exMount("chinook", "sqlite", false, nil)
	sqlite.exec.whole = exAnswer(map[string]any{"aid": 1, "bname": "x"})
	// A self join reads one collection twice; the summary names it once.
	q := exJoin(exRef("", "Customer", "a"), exRef("", "Customer", "b"), true)
	res, err := exRun(t, q, "chinook", newExRegistry(sqlite), exAllow, Limits{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if want := []ExecutionSource{{Database: "chinook", Collection: "Customer"}}; !reflect.DeepEqual(res.Execution.Sources, want) {
		t.Fatalf("sources = %+v, want %+v", res.Execution.Sources, want)
	}
}

func TestExecuteInMemoryRouteJoinsAcrossMounts(t *testing.T) {
	for name, ordered := range map[string]bool{"streaming join": false, "generic join": true} {
		t.Run(name, func(t *testing.T) {
			a := exMount("one", "sqlite", false, map[string][]record.Record{"A": exRows("A", "a", 3)})
			b := exMount("two", "sqlite", false, map[string][]record.Record{"B": exRows("B", "b", 3)})
			registry := newExRegistry(a, b)
			q := exJoin(exRef("one", "A", "a"), exRef("two", "B", "b"), ordered)

			res, err := exRun(t, q, "", registry, exAllow, Limits{})
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if !reflect.DeepEqual(registry.lookups, []string{"one", "two"}) {
				t.Fatalf("lookups = %v, want each database once in document order", registry.lookups)
			}
			for _, mount := range []*exSource{a, b} {
				executorCalls, txCalls := mount.counts()
				if executorCalls == 0 || txCalls != 0 {
					t.Fatalf("mount %s: Executor %d times, ReadTx %d times", mount.id, executorCalls, txCalls)
				}
			}
			want := []map[string]any{{"aid": 1.0, "bname": "b1"}, {"aid": 2.0, "bname": "b2"}, {"aid": 3.0, "bname": "b3"}}
			if got := exAsRows(t, res.Records); !reflect.DeepEqual(got, want) {
				t.Fatalf("rows = %v, want %v", got, want)
			}
			if !reflect.DeepEqual(res.Columns, []string{"aid", "bname"}) {
				t.Fatalf("columns = %v", res.Columns)
			}
			if res.Execution.Route != RouteInMemory || res.Execution.RowsReturned != 3 {
				t.Fatalf("execution = %+v", res.Execution)
			}
			if len(res.Execution.Sources) != 2 {
				t.Fatalf("sources = %+v", res.Execution.Sources)
			}
			byCollection := map[string]ExecutionSource{}
			for _, s := range res.Execution.Sources {
				byCollection[s.Database+"."+s.Collection] = s
			}
			for key, rows := range map[string]int{"one.A": 3, "two.B": 3} {
				s := byCollection[key]
				if s.Rows == nil || *s.Rows != rows || s.ElapsedMs == nil || *s.ElapsedMs < 0 {
					t.Fatalf("source %s = %+v, want %d rows and a time", key, s, rows)
				}
			}
		})
	}
}

func TestExecuteUnqualifiedDocumentRunsThroughOneLeaf(t *testing.T) {
	// An unqualified document is in the default database. Its mount carries a
	// subquery, so the database cannot run it: DALgo reads through one leaf.
	mount := exMount("shop", "sqlite", false, map[string][]record.Record{
		"Orders":    exRows("Orders", "o", 3),
		"Customers": exRows("Customers", "c", 2),
	})
	inner := dal.From(exRef("", "Customers", "")).NewQuery().SelectColumns(dal.Column{Expression: dal.NewFieldRef("", "id")})
	query := dal.From(exRef("", "Orders", "")).NewQuery().
		Where(dal.NewExistsCondition(inner)).
		SelectColumns(dal.Column{Expression: dal.NewFieldRef("", "id"), Alias: "aid"})
	res, err := exRun(t, query, "shop", newExRegistry(mount), exAllow, Limits{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	executorCalls, txCalls := mount.counts()
	if executorCalls == 0 || txCalls != 0 {
		t.Fatalf("Executor %d times, ReadTx %d times: a subquery must read through the leaf", executorCalls, txCalls)
	}
	if res.Execution.Route != RouteInMemory || res.Execution.RowsReturned != 3 {
		t.Fatalf("execution = %+v", res.Execution)
	}
}

func TestExecuteRoutesByMountEngineProtectionAndSubquery(t *testing.T) {
	subquery := dal.NewExistsCondition(exPlain("", "B"))
	withSubquery := dal.From(exRef("", "A", "")).NewQuery().Where(subquery).SelectIntoRecord(nil)
	plain := exPlain("", "A")
	for _, tc := range []struct {
		name     string
		engine   string
		policies bool
		query    dal.StructuredQuery
		opts     []Option
		want     string
	}{
		{"unprotected native engine", "sqlite", false, plain, nil, RouteDatabase},
		{"subquery", "sqlite", false, withSubquery, nil, RouteInMemory},
		{"access policies", "sqlite", true, plain, nil, RouteInMemory},
		{"engine in the join set but not native", "ingitdb", false, plain, nil, RouteInMemory},
		{"engine made native", "ingitdb", false, plain, []Option{WithNativeEngines("ingitdb")}, RouteDatabase},
		{"sqlite taken out of the native set", "sqlite", false, plain, []Option{WithNativeEngines("postgres"), WithJoinEngines("sqlite", "postgres")}, RouteInMemory},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mount := exMount("db", tc.engine, tc.policies, map[string][]record.Record{"A": exRows("A", "a", 1), "B": exRows("B", "b", 1)})
			mount.exec.whole = exAnswer(map[string]any{"id": 1})
			if tc.want == RouteInMemory {
				mount.exec.whole = nil
			}
			res, err := exRun(t, tc.query, "db", newExRegistry(mount), exAllow, Limits{}, tc.opts...)
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if res.Execution.Route != tc.want {
				t.Fatalf("route = %s, want %s", res.Execution.Route, tc.want)
			}
			if _, txCalls := mount.counts(); (txCalls == 1) != (tc.want == RouteDatabase) {
				t.Fatalf("ReadTx called %d times on the %s route", txCalls, tc.want)
			}
		})
	}
}

func TestExecuteNeverHandsAMultiSourceQueryToAPolicyProtectedMount(t *testing.T) {
	rows := func() map[string][]record.Record {
		return map[string][]record.Record{"A": exRows("A", "a", 3), "B": exRows("B", "b", 3)}
	}
	for _, tc := range []struct {
		name  string
		query dal.StructuredQuery
		def   string
		other bool
	}{
		{"join with a public mount", exJoin(exRef("hr", "A", "a"), exRef("pub", "B", "b"), true), "", true},
		{"streaming join with a public mount", exJoin(exRef("hr", "A", "a"), exRef("pub", "B", "b"), false), "", true},
		{"public mount is the fact side", exJoin(exRef("pub", "B", "b"), exRef("hr", "A", "a"), false), "", true},
		{"join inside the protected mount", exJoin(exRef("", "A", "a"), exRef("", "B", "b"), true), "hr", false},
		{"qualified join inside the protected mount", exJoin(exRef("hr", "A", "a"), exRef("hr", "B", "b"), false), "", false},
		{"count over the protected mount", exCount("", "A"), "hr", false},
		{"qualified count over the protected mount", exCount("hr", "A"), "", false},
		{"grouped count over the protected mount", dal.From(exRef("", "A", "")).NewQuery().GroupBy(dal.NewFieldRef("", "k")).SelectColumns(dal.Column{Expression: dal.NewFieldRef("", "k")}, dal.CountAs(dal.Star(), "n")), "hr", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			protected := exMount("hr", "sqlite", true, rows())
			public := exMount("pub", "sqlite", false, rows())
			res, err := exRun(t, tc.query, tc.def, newExRegistry(protected, public), exAllow, Limits{})
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if res.Execution.Route != RouteInMemory {
				t.Fatalf("route = %s, want in-memory", res.Execution.Route)
			}
			if _, txCalls := protected.counts(); txCalls != 0 {
				t.Fatal("a policy-protected mount never runs a transaction of its own")
			}
			// The mount only ever sees plain single-collection reads: the join
			// and the aggregate run above the secured handle, over the rows it
			// returns.
			seen := protected.exec.seen()
			if len(seen) == 0 {
				t.Fatal("the protected mount was not read")
			}
			for _, query := range seen {
				q := query.(dal.StructuredQuery)
				if len(q.From().Joins()) != 0 || dal.HasAggregation(q) || dal.HasSubquery(q) || len(q.GroupBy()) != 0 {
					t.Fatalf("the protected mount received %s", q)
				}
			}
			// No row count for the protected source, one for the public.
			for _, s := range res.Execution.Sources {
				switch s.Database {
				case "hr":
					if s.Rows != nil {
						t.Fatalf("the protected source reports %d rows", *s.Rows)
					}
					if s.ElapsedMs == nil {
						t.Fatal("the protected source still reports its time")
					}
				case "pub":
					if !tc.other || s.Rows == nil {
						t.Fatalf("public source = %+v", s)
					}
				}
			}
		})
	}
}

func TestExecuteAggregatesInMemoryOverTheRowsOfAProtectedMount(t *testing.T) {
	// The executor of a protected mount hands back only the rows its caller may
	// read, three of the ten. The count is over those.
	protected := exMount("hr", "sqlite", true, map[string][]record.Record{"A": exRows("A", "a", 3)})
	res, err := exRun(t, exCount("", "A"), "hr", newExRegistry(protected), exAllow, Limits{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(res.Records) != 1 {
		t.Fatalf("rows = %d", len(res.Records))
	}
	if n := res.Records[0].Data().(map[string]any)["n"]; n != int64(3) && n != 3 && n != float64(3) {
		t.Fatalf("count = %v (%T), want 3", n, n)
	}
	if !reflect.DeepEqual(res.Columns, []string{"n"}) {
		t.Fatalf("columns = %v", res.Columns)
	}
}

func TestExecuteAuthorisesEverySourceBeforeResolvingAnyMount(t *testing.T) {
	a := exMount("one", "sqlite", false, map[string][]record.Record{"A": exRows("A", "a", 1)})
	registry := newExRegistry(a) // "ghost" is not mounted
	q := exJoin(exRef("one", "A", "a"), exRef("ghost", "G", "g"), true)

	var asked [][2]string
	authorize := func(database, collection string) bool {
		asked = append(asked, [2]string{database, collection})
		return database != "ghost"
	}
	_, err := exRun(t, q, "", registry, authorize, Limits{})
	// The caller may not read ghost.G, and learns nothing about whether it
	// exists: 403 before 404.
	denied := exAsDenied(t, err)
	if denied.Database != "ghost" || denied.Collection != "G" {
		t.Fatalf("denied = %+v", denied)
	}
	if len(registry.lookups) != 0 {
		t.Fatalf("a mount was resolved before authorisation finished: %v", registry.lookups)
	}
	if want := [][2]string{{"one", "A"}, {"ghost", "G"}}; !reflect.DeepEqual(asked, want) {
		t.Fatalf("authorised %v, want %v", asked, want)
	}
	if executorCalls, txCalls := a.counts(); executorCalls != 0 || txCalls != 0 {
		t.Fatal("a source was read although a later source was denied")
	}
}

func TestExecuteDeniesEverythingWithoutAuthorize(t *testing.T) {
	a := exMount("one", "sqlite", false, nil)
	registry := newExRegistry(a)
	_, err := exRun(t, exPlain("one", "A"), "", registry, nil, Limits{})
	_ = exAsDenied(t, err)
	if len(registry.lookups) != 0 {
		t.Fatalf("lookups = %v", registry.lookups)
	}
}

func TestExecuteReportsAnUnknownDatabaseAfterAuthorisation(t *testing.T) {
	registry := newExRegistry()
	_, err := exRun(t, exPlain("ghost", "G"), "", registry, exAllow, Limits{})
	var unknown *UnknownDatabaseError
	if !errors.As(err, &unknown) || unknown.Database != "ghost" {
		t.Fatalf("err = %v, want *UnknownDatabaseError for ghost", err)
	}
	if !strings.Contains(unknown.Error(), `"ghost"`) {
		t.Fatalf("the database id is quoted: %q", unknown.Error())
	}
	// A registry may also answer with a nil source.
	registry.sources["nil"] = nil
	_, err = exRun(t, exPlain("nil", "G"), "", registry, exAllow, Limits{})
	if !errors.As(err, &unknown) || unknown.Database != "nil" {
		t.Fatalf("err = %v", err)
	}
}

func TestExecuteRefusesARegistryThatAnswersForAnotherDatabase(t *testing.T) {
	// The leaf authorises under the source's own id; a source that is not the
	// database asked for would be authorised under the wrong name.
	registry := &exRegistry{sources: map[string]Source{"one": exMount("two", "sqlite", false, nil)}}
	_, err := exRun(t, exPlain("one", "A"), "", registry, exAllow, Limits{})
	if !errors.Is(err, ErrRegistryMismatch) {
		t.Fatalf("err = %v, want ErrRegistryMismatch", err)
	}
}

func TestExecuteNamesTheDefaultDatabase(t *testing.T) {
	cases := []struct {
		name  string
		query dal.StructuredQuery
		def   string
	}{
		{"unqualified without a default", exPlain("", "A"), ""},
		{"unqualified beside another database", exJoin(exRef("", "A", "a"), exRef("two", "B", "b"), true), "one"},
		{"unqualified in a cross-database document", exJoin(exRef("one", "A", "a"), exRef("", "B", "b"), true), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			registry := newExRegistry(exMount("one", "sqlite", false, nil), exMount("two", "sqlite", false, nil))
			_, err := exRun(t, tc.query, tc.def, registry, exAllow, Limits{})
			if !errors.Is(err, ErrSourceWithoutDatabase) {
				t.Fatalf("err = %v, want ErrSourceWithoutDatabase", err)
			}
			if len(registry.lookups) != 0 {
				t.Fatalf("lookups = %v: names are settled before any mount is resolved", registry.lookups)
			}
		})
	}
	// Unqualified sources beside sources qualified with the default database
	// are all in one database.
	mount := exMount("one", "sqlite", false, map[string][]record.Record{"A": exRows("A", "a", 2), "B": exRows("B", "b", 2)})
	mount.exec.whole = exAnswer(map[string]any{"aid": 1, "bname": "b1"})
	res, err := exRun(t, exJoin(exRef("", "A", "a"), exRef("one", "B", "b"), true), "one", newExRegistry(mount), exAllow, Limits{})
	if err != nil || res.Execution.Route != RouteDatabase {
		t.Fatalf("mixed naming inside one database: %+v, %v", res.Execution, err)
	}
	// A different default database is a different name for the same collection.
	var asked [][2]string
	ask := func(database, collection string) bool {
		asked = append(asked, [2]string{database, collection})
		return true
	}
	if _, err := exRun(t, exPlain("", "A"), "one", newExRegistry(mount), ask, Limits{}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if want := [][2]string{{"one", "A"}}; !reflect.DeepEqual(asked, want) {
		t.Fatalf("authorised %v, want %v", asked, want)
	}
}

func TestExecuteRefusesAnEngineThatCannotBeQueriedBeforeTheJoinSetCheck(t *testing.T) {
	rows := map[string][]record.Record{"A": exRows("A", "a", 1), "B": exRows("B", "b", 1)}
	t.Run("even when the operator lists it as a join engine", func(t *testing.T) {
		pg := exGated{exMount("pg", "postgres", false, rows), false}
		registry := newExRegistry(pg)
		_, err := exRun(t, exPlain("", "A"), "pg", registry, exAllow, Limits{}, WithJoinEngines("sqlite", "postgres"), WithNativeEngines("sqlite", "postgres"))
		var unsupported *EngineNotQueryableError
		if !errors.As(err, &unsupported) || unsupported.Database != "pg" || unsupported.Engine != "postgres" {
			t.Fatalf("err = %v, want *EngineNotQueryableError", err)
		}
		if executorCalls, txCalls := pg.counts(); executorCalls != 0 || txCalls != 0 {
			t.Fatal("a mount that cannot be queried was read")
		}
	})
	t.Run("before the join set refusal of another source", func(t *testing.T) {
		// The first source is outside the join set; the second cannot be
		// queried at all. The second refusal wins: 501 before 422.
		mysql := exMount("my", "mysql", false, rows)
		pg := exGated{exMount("pg", "postgres", false, rows), false}
		_, err := exRun(t, exJoin(exRef("my", "A", "a"), exRef("pg", "B", "b"), true), "", newExRegistry(mysql, pg), exAllow, Limits{})
		var unsupported *EngineNotQueryableError
		if !errors.As(err, &unsupported) || unsupported.Database != "pg" {
			t.Fatalf("err = %v, want *EngineNotQueryableError for pg", err)
		}
	})
	t.Run("a queryable engine passes", func(t *testing.T) {
		lite := exGated{exMount("lite", "sqlite", false, rows), true}
		if _, err := exRun(t, exPlain("", "A"), "lite", newExRegistry(lite), exAllow, Limits{}); err != nil {
			t.Fatalf("Execute: %v", err)
		}
	})
	t.Run("the error says which engine", func(t *testing.T) {
		msg := (&EngineNotQueryableError{Database: "pg", Engine: "post gres"}).Error()
		if !strings.Contains(msg, `"pg"`) || !strings.Contains(msg, `"post gres"`) {
			t.Fatalf("message = %q", msg)
		}
	})
}

func TestExecuteRefusesAnEngineOutsideTheJoinSet(t *testing.T) {
	rows := map[string][]record.Record{"A": exRows("A", "a", 1)}
	firestore := exMount("fs", "firestore", false, rows)
	registry := newExRegistry(firestore)
	_, err := exRun(t, exPlain("", "A"), "fs", registry, exAllow, Limits{})
	var refused *EngineNotJoinableError
	if !errors.As(err, &refused) || refused.Database != "fs" || refused.Engine != "firestore" {
		t.Fatalf("err = %v, want *EngineNotJoinableError", err)
	}
	if !strings.Contains(refused.Error(), `"firestore"`) {
		t.Fatalf("message = %q", refused.Error())
	}
	if executorCalls, txCalls := firestore.counts(); executorCalls != 0 || txCalls != 0 {
		t.Fatal("a mount outside the join set was read")
	}
	// The operator can add it, and a call without a list takes the default.
	if _, err := exRun(t, exPlain("", "A"), "fs", newExRegistry(firestore), exAllow, Limits{}, WithJoinEngines("firestore")); err != nil {
		t.Fatalf("configured join engine: %v", err)
	}
	if _, err := exRun(t, exPlain("", "A"), "fs", newExRegistry(firestore), exAllow, Limits{}, WithJoinEngines(), WithNativeEngines()); !errors.As(err, &refused) {
		t.Fatalf("empty lists keep the defaults: %v", err)
	}
	// One refused engine refuses the whole document.
	lite := exMount("lite", "sqlite", false, map[string][]record.Record{"B": exRows("B", "b", 1)})
	_, err = exRun(t, exJoin(exRef("lite", "B", "b"), exRef("fs", "A", "a"), true), "", newExRegistry(lite, firestore), exAllow, Limits{})
	if !errors.As(err, &refused) || refused.Database != "fs" {
		t.Fatalf("err = %v", err)
	}
	if executorCalls, _ := lite.counts(); executorCalls != 0 {
		t.Fatal("a source was read although another was refused")
	}
}

func TestExecuteRefusesAScanOnAProtectedSource(t *testing.T) {
	protected := exMount("hr", "sqlite", true, map[string][]record.Record{"A": exRows("A", "a", 3)})
	public := exMount("pub", "sqlite", false, map[string][]record.Record{"B": exRows("B", "b", 3)})
	scanned := func(database string) dal.StructuredQuery {
		return dal.From(exRef(database, "A", "").WithScan(2, dal.AscendingField("id"))).NewQuery().SelectIntoRecord(nil)
	}
	_, err := exRun(t, scanned("hr"), "", newExRegistry(protected, public), exAllow, Limits{})
	if !errors.Is(err, ErrScanOnProtectedSource) || !strings.Contains(err.Error(), `"hr"."A"`) {
		t.Fatalf("err = %v, want ErrScanOnProtectedSource naming hr.A", err)
	}
	if executorCalls, txCalls := protected.counts(); executorCalls != 0 || txCalls != 0 {
		t.Fatal("a protected source with a scan clause was read")
	}
	// The scan sits in a join beside a public source.
	joined := dal.From(exRef("pub", "B", "b")).Join(dal.NewJoinedSource(exRef("hr", "A", "a").WithScan(2), dal.JoinInner, exKeyEquals(exRef("pub", "B", "b"), exRef("hr", "A", "a")))).NewQuery().SelectIntoRecord(nil)
	if _, err := exRun(t, joined, "", newExRegistry(protected, public), exAllow, Limits{}); !errors.Is(err, ErrScanOnProtectedSource) {
		t.Fatalf("scan in a join: %v", err)
	}
	// A scan on a source without policies is not refused here.
	if _, err := exRun(t, dal.From(exRef("pub", "B", "").WithScan(2, dal.AscendingField("id"))).NewQuery().SelectIntoRecord(nil), "", newExRegistry(protected, public), exAllow, Limits{}); err != nil {
		t.Fatalf("scan on a public source: %v", err)
	}
	// A protected source with no scan is not refused either.
	if _, err := exRun(t, scanned("pub"), "", newExRegistry(protected, public), exAllow, Limits{}); err != nil {
		t.Fatalf("scan on the public mount's collection: %v", err)
	}
}

func TestExecuteRefusesAProfileThatDoesNotDescribeTheQuery(t *testing.T) {
	a := exMount("one", "sqlite", false, map[string][]record.Record{"A": exRows("A", "a", 1)})
	registry := newExRegistry(a)
	q := exPlain("one", "A")
	// A profile that lists less than the query reads would authorise less.
	_, err := Execute(context.Background(), q, Profile{}, "", registry, exAllow, Limits{})
	if !errors.Is(err, ErrInvalidDocument) {
		t.Fatalf("err = %v, want ErrInvalidDocument", err)
	}
	if len(registry.lookups) != 0 {
		t.Fatalf("lookups = %v", registry.lookups)
	}
}

func TestExecuteRefusesAnUnsafeNameBeforeAnythingElse(t *testing.T) {
	a := exMount("one", "sqlite", false, map[string][]record.Record{"A": exRows("A", "a", 1)})
	registry := newExRegistry(a)
	authorised := false
	q := dal.From(exRef("one", "A", "")).NewQuery().SelectColumns(dal.Column{Expression: dal.NewFieldRef("", `x"; DROP TABLE A; --`)})
	_, err := Execute(context.Background(), q, Profile{Sources: []ProfileSource{{"one", "A"}}}, "", registry, func(string, string) bool { authorised = true; return true }, Limits{})
	if !errors.Is(err, ErrInvalidDocument) {
		t.Fatalf("err = %v, want ErrInvalidDocument", err)
	}
	if authorised || len(registry.lookups) != 0 {
		t.Fatal("a document with an unsafe name reached authorisation or a mount")
	}
}

func TestExecuteNeedsAQueryAndARegistry(t *testing.T) {
	_, err := Execute(context.Background(), nil, Profile{}, "", newExRegistry(), exAllow, Limits{})
	if !errors.Is(err, ErrInvalidDocument) {
		t.Fatalf("nil query: %v", err)
	}
	_, err = exRun(t, exPlain("one", "A"), "", nil, exAllow, Limits{})
	if !errors.Is(err, ErrInvalidDocument) || !strings.Contains(err.Error(), "registry") {
		t.Fatalf("nil registry: %v", err)
	}
}

func TestExecuteReturnsAnEmptyResultAsAnEmptySlice(t *testing.T) {
	for name, mount := range map[string]*exSource{
		"database":  exMount("db", "sqlite", false, nil),
		"in-memory": exMount("db", "sqlite", true, map[string][]record.Record{"A": nil}),
	} {
		t.Run(name, func(t *testing.T) {
			mount.exec.whole = nil
			res, err := exRun(t, exPlain("", "A"), "db", newExRegistry(mount), exAllow, Limits{})
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if res.Records == nil || len(res.Records) != 0 || res.Execution.RowsReturned != 0 {
				t.Fatalf("records = %#v", res.Records)
			}
			if res.Columns == nil {
				t.Fatal("columns must not be nil")
			}
		})
	}
}

func TestExecuteCapsTheResultAtOneThousandRows(t *testing.T) {
	for name, tc := range map[string]struct {
		policies bool
		route    string
	}{
		"database":  {false, RouteDatabase},
		"in-memory": {true, RouteInMemory},
	} {
		t.Run(name, func(t *testing.T) {
			mount := exMount("db", "sqlite", tc.policies, map[string][]record.Record{"A": exRows("A", "a", MaxResultRows+1)})
			if !tc.policies {
				mount.exec.whole = exRows("A", "a", MaxResultRows+1)
			}
			q := exPlain("", "A")
			res, err := exRun(t, q, "db", newExRegistry(mount), exAllow, Limits{})
			budget := exAsBudget(t, err)
			if budget.Name != BudgetResponseRows || budget.Limit != MaxResultRows || budget.Route != tc.route {
				t.Fatalf("budget = %+v", budget)
			}
			if len(res.Records) != 0 || res.Execution.RowsReturned != 0 {
				t.Fatalf("a refused result returned rows: %+v", res.Execution)
			}
			// Exactly the cap is fine.
			mount.exec.rows["A"] = exRows("A", "a", MaxResultRows)
			if !tc.policies {
				mount.exec.whole = exRows("A", "a", MaxResultRows)
			}
			res, err = exRun(t, q, "db", newExRegistry(mount), exAllow, Limits{})
			if err != nil || len(res.Records) != MaxResultRows {
				t.Fatalf("a result of exactly %d rows: %d rows, %v", MaxResultRows, len(res.Records), err)
			}
		})
	}
}

func TestExecuteCapsTheResultAtEightMebibytes(t *testing.T) {
	payload := strings.Repeat("x", 1<<20)
	big := func(n int) []record.Record {
		rows := make([]record.Record, n)
		for i := range rows {
			rows[i] = record.NewRecordWithData(record.NewKeyWithID("A", i+1), map[string]any{"id": i + 1, "payload": payload})
		}
		return rows
	}
	mount := exMount("db", "sqlite", false, nil)
	mount.exec.whole = big(9)
	_, err := exRun(t, exPlain("", "A"), "db", newExRegistry(mount), exAllow, Limits{})
	budget := exAsBudget(t, err)
	if budget.Name != BudgetResponseBytes || budget.Limit != MaxResultBytes || budget.Route != RouteDatabase {
		t.Fatalf("budget = %+v", budget)
	}
	mount.exec.whole = big(7)
	if res, err := exRun(t, exPlain("", "A"), "db", newExRegistry(mount), exAllow, Limits{}); err != nil || len(res.Records) != 7 {
		t.Fatalf("seven mebibytes of rows: %d, %v", len(res.Records), err)
	}
}

func TestExecuteResultThatCannotBeEncodedFailsClosed(t *testing.T) {
	mount := exMount("db", "sqlite", false, nil)
	mount.exec.whole = exAnswer(map[string]any{"f": func() {}})
	_, err := exRun(t, exPlain("", "A"), "db", newExRegistry(mount), exAllow, Limits{})
	if !errors.Is(err, ErrRowNotEncodable) {
		t.Fatalf("err = %v, want ErrRowNotEncodable", err)
	}
}

func TestExecuteSourceBudgetErrorsReturnNoRows(t *testing.T) {
	a := exMount("one", "sqlite", false, map[string][]record.Record{"A": exRows("A", "a", 50)})
	b := exMount("two", "sqlite", false, map[string][]record.Record{"B": exRows("B", "b", 50)})
	// Both join shapes: the streaming join delivers rows before its error
	// arrives, so only reading to the end gives a trustworthy answer.
	for name, ordered := range map[string]bool{"streaming join": false, "generic join": true} {
		t.Run(name, func(t *testing.T) {
			res, err := exRun(t, exJoin(exRef("one", "A", "a"), exRef("two", "B", "b"), ordered), "", newExRegistry(a, b), exAllow, Limits{MaxSourceRows: 60})
			budget := exAsBudget(t, err)
			if budget.Name != BudgetSourceRows || budget.Limit != 60 || budget.Route != RouteInMemory {
				t.Fatalf("budget = %+v", budget)
			}
			if res.Records != nil || res.Columns != nil || res.Execution.Sources != nil {
				t.Fatalf("a refused request returned a partial result: %+v", res)
			}
		})
	}
}

func TestExecuteMapsDalgoBoundsToBudgetErrors(t *testing.T) {
	a := exMount("one", "sqlite", false, map[string][]record.Record{"A": exRows("A", "a", 10001)})
	b := exMount("two", "sqlite", false, map[string][]record.Record{"B": exRows("B", "b", 1)})
	_, err := exRun(t, exJoin(exRef("one", "A", "a"), exRef("two", "B", "b"), true), "", newExRegistry(a, b), exAllow, Limits{})
	budget := exAsBudget(t, err)
	if budget.Name != BudgetJoinScan && budget.Name != BudgetJoinFetchedRows {
		t.Fatalf("budget = %+v", budget)
	}
	if budget.Route != RouteInMemory {
		t.Fatalf("route = %s", budget.Route)
	}
}

func TestExecuteKeepsTheErrorOfASourceThatFails(t *testing.T) {
	for name, configure := range map[string]func(*exExecutor){
		"on open": func(e *exExecutor) { e.openErr = errExBoom },
		"on read": func(e *exExecutor) { e.readerErr = errExBoom },
	} {
		t.Run(name, func(t *testing.T) {
			a := exMount("one", "sqlite", true, map[string][]record.Record{"A": exRows("A", "a", 2)})
			configure(a.exec)
			_, err := exRun(t, exPlain("", "A"), "one", newExRegistry(a), exAllow, Limits{})
			if !errors.Is(err, errExBoom) {
				t.Fatalf("err = %v, want the source's own error", err)
			}
			if se := exAsSourceError(t, err); se.Database != "one" || se.Collection != "A" {
				t.Fatalf("source error = %+v", se)
			}
		})
	}
}

func TestExecuteEndsAtTheContext(t *testing.T) {
	a := exMount("one", "sqlite", true, map[string][]record.Record{"A": exRows("A", "a", 2)})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	q := exPlain("", "A")
	_, err := Execute(ctx, q, exProfile(t, q), "one", newExRegistry(a), exAllow, Limits{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if len(a.exec.seen()) != 0 {
		t.Fatal("a source was read after the request ended")
	}
}

func TestExecuteAppliesTheRequestTimeout(t *testing.T) {
	for name, tc := range map[string]struct {
		policies bool
		timeout  time.Duration
		want     time.Duration
	}{
		"a configured timeout":  {true, time.Hour, time.Hour},
		"the default timeout":   {true, 0, DefaultLimits().Timeout},
		"on the database route": {false, time.Hour, time.Hour},
	} {
		t.Run(name, func(t *testing.T) {
			mount := exMount("one", "sqlite", tc.policies, map[string][]record.Record{"A": exRows("A", "a", 2)})
			before := time.Now()
			if _, err := exRun(t, exPlain("", "A"), "one", newExRegistry(mount), exAllow, Limits{Timeout: tc.timeout}); err != nil {
				t.Fatalf("Execute: %v", err)
			}
			deadlines := mount.exec.deadlines()
			if len(deadlines) != 1 || deadlines[0].IsZero() {
				t.Fatalf("deadlines = %v: every read carries the request deadline", deadlines)
			}
			if got := deadlines[0].Sub(before); got > tc.want+time.Minute || got < tc.want-time.Minute {
				t.Fatalf("deadline is %v ahead, want about %v", got, tc.want)
			}
		})
	}
}

func TestExecuteReadTxFailures(t *testing.T) {
	q := exPlain("", "A")
	t.Run("a transaction that cannot start", func(t *testing.T) {
		mount := exMount("db", "sqlite", false, nil)
		mount.txErr = errExBoom
		_, err := exRun(t, q, "db", newExRegistry(mount), exAllow, Limits{})
		if !errors.Is(err, errExBoom) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("a transaction that never runs its function is not an empty result", func(t *testing.T) {
		mount := exMount("db", "sqlite", false, nil)
		mount.skipTx = true
		res, err := exRun(t, q, "db", newExRegistry(mount), exAllow, Limits{})
		if !errors.Is(err, ErrReadTxSkipped) {
			t.Fatalf("err = %v, want ErrReadTxSkipped", err)
		}
		if res.Records != nil {
			t.Fatal("an empty result was returned as success")
		}
	})
	t.Run("a transaction that swallows the error of the function", func(t *testing.T) {
		mount := exMount("db", "sqlite", false, nil)
		mount.exec.openErr = errExBoom
		mount.swallowTx = true
		res, err := exRun(t, q, "db", newExRegistry(mount), exAllow, Limits{})
		if !errors.Is(err, errExBoom) || res.Records != nil {
			t.Fatalf("a failed read became %d rows and %v", len(res.Records), err)
		}
	})
	t.Run("a transaction that fails after the read", func(t *testing.T) {
		mount := exMount("db", "sqlite", false, nil)
		mount.exec.whole = exAnswer(map[string]any{"a": 1})
		mount.afterTxErr = errExBoom
		res, err := exRun(t, q, "db", newExRegistry(mount), exAllow, Limits{})
		if !errors.Is(err, errExBoom) || res.Records != nil {
			t.Fatalf("rows of a transaction that did not commit were returned: %d, %v", len(res.Records), err)
		}
	})
	t.Run("an executor that fails", func(t *testing.T) {
		mount := exMount("db", "sqlite", false, nil)
		mount.exec.openErr = errExBoom
		_, err := exRun(t, q, "db", newExRegistry(mount), exAllow, Limits{})
		if !errors.Is(err, errExBoom) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("a reader that fails after some rows", func(t *testing.T) {
		mount := exMount("db", "sqlite", false, nil)
		mount.exec.whole = exAnswer(map[string]any{"a": 1})
		mount.exec.readerErr = errExBoom
		res, err := exRun(t, q, "db", newExRegistry(mount), exAllow, Limits{})
		if !errors.Is(err, errExBoom) || res.Records != nil {
			t.Fatalf("rows read before an error are not an answer: %d, %v", len(res.Records), err)
		}
	})
	t.Run("an executor that returns neither reader nor error", func(t *testing.T) {
		mount := exMount("db", "sqlite", false, nil)
		mount.exec.nilReader = true
		_, err := exRun(t, q, "db", newExRegistry(mount), exAllow, Limits{})
		if !errors.Is(err, ErrNoReader) {
			t.Fatalf("err = %v, want ErrNoReader", err)
		}
	})
	t.Run("DALgo's own bound inside the transaction is a budget error", func(t *testing.T) {
		mount := exMount("db", "sqlite", false, nil)
		mount.exec.openErr = &dal.JoinValidationError{Category: "join_plan", Message: "joined row bound exceeded"}
		_, err := exRun(t, q, "db", newExRegistry(mount), exAllow, Limits{})
		budget := exAsBudget(t, err)
		if budget.Name != BudgetJoinRows || budget.Route != RouteDatabase {
			t.Fatalf("budget = %+v", budget)
		}
	})
}

func TestExecuteReportsElapsedTimeFromItsClock(t *testing.T) {
	mount := exMount("db", "sqlite", false, nil)
	mount.exec.whole = exAnswer(map[string]any{"a": 1})
	// The clock moves 250 ms at every reading; Execute reads it twice.
	res, err := exRun(t, exPlain("", "A"), "db", newExRegistry(mount), exAllow, Limits{}, withClock(exFixedClock(250*time.Millisecond)))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Execution.ElapsedMs != 250 {
		t.Fatalf("elapsedMs = %d, want 250", res.Execution.ElapsedMs)
	}
}

func TestExecuteUnknownDatabaseInTheResolverFailsClosed(t *testing.T) {
	// DALgo asks for databases by the names in the document; the resolver
	// answers only for mounts the preflight resolved and authorised.
	r := &run{sources: map[string]Source{"one": exMount("one", "sqlite", false, nil)}, guard: NewGuard(exAllow, Limits{})}
	if _, err := r.resolve(context.Background(), "one"); err != nil {
		t.Fatalf("resolve one: %v", err)
	}
	_, err := r.resolve(context.Background(), "two")
	var unknown *UnknownDatabaseError
	if !errors.As(err, &unknown) || unknown.Database != "two" {
		t.Fatalf("err = %v, want *UnknownDatabaseError for two", err)
	}
}

func TestExecutionSerialisesToTheResponseShape(t *testing.T) {
	rows := 3
	ms := int64(4)
	e := Execution{Route: RouteInMemory, ElapsedMs: 9, RowsReturned: 3, Sources: []ExecutionSource{
		{Database: "a", Collection: "A", Rows: &rows, ElapsedMs: &ms},
		{Database: "b", Collection: "B"},
	}}
	got, err := exJSON(e)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"route":"in-memory","elapsedMs":9,"rowsReturned":3,"sources":[{"database":"a","collection":"A","rows":3,"elapsedMs":4},{"database":"b","collection":"B"}]}`
	if got != want {
		t.Fatalf("json = %s\nwant   %s", got, want)
	}
}

func TestExecuteOptionsKeepTheirDefaultsWhenEmpty(t *testing.T) {
	c := newConfig(nil)
	for _, engine := range []string{"sqlite", "ingitdb"} {
		if !c.joinEngines[engine] {
			t.Fatalf("%s must be a default join engine", engine)
		}
	}
	if !c.nativeEngines["sqlite"] || c.nativeEngines["ingitdb"] {
		t.Fatalf("native engines = %v, want only sqlite", c.nativeEngines)
	}
	custom := newConfig([]Option{WithJoinEngines("a", "b"), WithNativeEngines("a")})
	if len(custom.joinEngines) != 2 || !custom.joinEngines["a"] || len(custom.nativeEngines) != 1 {
		t.Fatalf("custom = %+v", custom)
	}
	// Lists are copied: a caller that changes its slice later changes nothing.
	engines := []string{"x"}
	c = newConfig([]Option{WithJoinEngines(engines...)})
	engines[0] = "y"
	if !c.joinEngines["x"] || c.joinEngines["y"] {
		t.Fatalf("join engines = %v", c.joinEngines)
	}
}
