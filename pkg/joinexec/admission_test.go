package joinexec

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/record"
)

// exAdmission is an admission function that records what it was asked and what
// was released, and refuses a route when told to.
type exAdmission struct {
	mu       sync.Mutex
	asked    []string
	released int
	refuse   map[string]bool
	// onAsk runs inside every call, before the answer.
	onAsk func()
}

func (a *exAdmission) admit(ctx context.Context, route string) (func(), bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.asked = append(a.asked, route)
	if a.onAsk != nil {
		a.onAsk()
	}
	if a.refuse[route] {
		return func() {
			a.mu.Lock()
			defer a.mu.Unlock()
			a.released += 1000
		}, false
	}
	return func() {
		a.mu.Lock()
		defer a.mu.Unlock()
		a.released++
	}, true
}

func (a *exAdmission) releases() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.released
}

func TestExecuteAsksAdmissionOnTheRouteItChoosesAndReleasesWhenItEnds(t *testing.T) {
	t.Run("database route", func(t *testing.T) {
		sqlite := exMount("chinook", "sqlite", false, nil)
		sqlite.exec.whole = exAnswer(map[string]any{"aid": 1, "bname": "x"})
		admission := &exAdmission{}
		var releasedWhileReading int
		q := exJoin(exRef("", "Invoice", "i"), exRef("", "Customer", "c"), true)
		// The slot is still held while the database reads: ReadTx runs inside Execute.
		admission.onAsk = func() { releasedWhileReading = admission.released }
		res, err := exRun(t, q, "chinook", newExRegistry(sqlite), exAllow, Limits{}, WithAdmission(admission.admit))
		if err != nil || res.Execution.Route != RouteDatabase {
			t.Fatalf("Execute = %+v, %v", res.Execution, err)
		}
		if !reflect.DeepEqual(admission.asked, []string{RouteDatabase}) || releasedWhileReading != 0 || admission.releases() != 1 {
			t.Fatalf("asked %v, released %d (%d before the read), want one ask on %q and one release at the end", admission.asked, admission.releases(), releasedWhileReading, RouteDatabase)
		}
	})
	t.Run("in-memory route", func(t *testing.T) {
		one := exMount("one", "sqlite", false, map[string][]record.Record{"A": exRows("A", "a", 2)})
		two := exMount("two", "sqlite", false, map[string][]record.Record{"B": exRows("B", "b", 2)})
		admission := &exAdmission{}
		q := exJoin(exRef("one", "A", "a"), exRef("two", "B", "b"), true)
		res, err := exRun(t, q, "", newExRegistry(one, two), exAllow, Limits{}, WithAdmission(admission.admit))
		if err != nil || res.Execution.Route != RouteInMemory {
			t.Fatalf("Execute = %+v, %v", res.Execution, err)
		}
		if !reflect.DeepEqual(admission.asked, []string{RouteInMemory}) || admission.releases() != 1 {
			t.Fatalf("asked %v, released %d", admission.asked, admission.releases())
		}
	})
	t.Run("release after a failed read", func(t *testing.T) {
		mount := exMount("db", "sqlite", false, nil)
		mount.exec.openErr = errExBoom
		admission := &exAdmission{}
		if _, err := exRun(t, exPlain("", "A"), "db", newExRegistry(mount), exAllow, Limits{}, WithAdmission(admission.admit)); !errors.Is(err, errExBoom) {
			t.Fatalf("err = %v", err)
		}
		if admission.releases() != 1 {
			t.Fatalf("released %d times, want 1: a failed request gives its slot back", admission.releases())
		}
	})
	t.Run("a release function that is missing", func(t *testing.T) {
		mount := exMount("db", "sqlite", false, nil)
		mount.exec.whole = exAnswer(map[string]any{"a": 1})
		admit := func(context.Context, string) (func(), bool) { return nil, true }
		if _, err := exRun(t, exPlain("", "A"), "db", newExRegistry(mount), exAllow, Limits{}, WithAdmission(admit)); err != nil {
			t.Fatalf("Execute: %v", err)
		}
	})
	t.Run("no admission function", func(t *testing.T) {
		mount := exMount("db", "sqlite", false, nil)
		mount.exec.whole = exAnswer(map[string]any{"a": 1})
		if _, err := exRun(t, exPlain("", "A"), "db", newExRegistry(mount), exAllow, Limits{}, WithAdmission(nil)); err != nil {
			t.Fatalf("Execute: %v", err)
		}
	})
}

func TestExecuteRefusedAdmissionReadsNothingAndReturnsACapacityError(t *testing.T) {
	for _, route := range []string{RouteDatabase, RouteInMemory} {
		t.Run(route, func(t *testing.T) {
			var q dal.StructuredQuery
			var registry *exRegistry
			var mounts []*exSource
			if route == RouteDatabase {
				mount := exMount("db", "sqlite", false, nil)
				mount.exec.whole = exAnswer(map[string]any{"a": 1})
				q, registry, mounts = exPlain("", "A"), newExRegistry(mount), []*exSource{mount}
			} else {
				one := exMount("one", "sqlite", false, map[string][]record.Record{"A": exRows("A", "a", 1)})
				two := exMount("two", "sqlite", false, map[string][]record.Record{"B": exRows("B", "b", 1)})
				q, registry, mounts = exJoin(exRef("one", "A", "a"), exRef("two", "B", "b"), true), newExRegistry(one, two), []*exSource{one, two}
			}
			admission := &exAdmission{refuse: map[string]bool{route: true}}
			res, err := exRun(t, q, "db", registry, exAllow, Limits{}, WithAdmission(admission.admit))
			var capacity *CapacityError
			if !errors.As(err, &capacity) || capacity.Route != route {
				t.Fatalf("err = %v, want *CapacityError for %q", err, route)
			}
			if !strings.Contains(capacity.Error(), route) || len(res.Records) != 0 {
				t.Fatalf("message %q, records %d", capacity.Error(), len(res.Records))
			}
			for _, mount := range mounts {
				if executorCalls, txCalls := mount.counts(); executorCalls != 0 || txCalls != 0 {
					t.Fatalf("a refused request read a source (%d executor calls, %d transactions)", executorCalls, txCalls)
				}
			}
			if admission.releases() != 0 {
				t.Fatal("the release function of a refused admission was called")
			}
		})
	}
}

// A request that fails before the route is chosen holds no slot and asks for none.
func TestExecuteDoesNotAskAdmissionForARequestThatFailsFirst(t *testing.T) {
	rows := map[string][]record.Record{"A": exRows("A", "a", 1)}
	sqlite := exMount("db", "sqlite", false, rows)
	firestore := exMount("fs", "firestore", false, rows)
	noQuery := exMount("nq", "sqlite", false, rows)
	noQuery.noQuery = true
	for name, run := range map[string]func(admit Admit) error{
		"denied": func(admit Admit) error {
			_, err := exRun(t, exPlain("", "A"), "db", newExRegistry(sqlite), func(string, string) bool { return false }, Limits{}, WithAdmission(admit))
			return err
		},
		"unknown database": func(admit Admit) error {
			_, err := exRun(t, exPlain("zz", "A"), "", newExRegistry(sqlite), exAllow, Limits{}, WithAdmission(admit))
			return err
		},
		"engine outside the join set": func(admit Admit) error {
			_, err := exRun(t, exPlain("", "A"), "fs", newExRegistry(firestore), exAllow, Limits{}, WithAdmission(admit))
			return err
		},
		"engine that cannot be queried": func(admit Admit) error {
			_, err := exRun(t, exPlain("", "A"), "nq", newExRegistry(noQuery), exAllow, Limits{}, WithAdmission(admit))
			return err
		},
		"invalid document": func(admit Admit) error {
			// exRun would walk the document to build a profile and fail first.
			_, err := Execute(context.Background(), exWithColumn(dal.NewFieldRef("", "no good")), Profile{Sources: []ProfileSource{{"", "a"}}}, "db", newExRegistry(sqlite), exAllow, Limits{}, WithAdmission(admit))
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			admission := &exAdmission{}
			if err := run(admission.admit); err == nil {
				t.Fatal("the request should have failed")
			}
			if len(admission.asked) != 0 {
				t.Fatalf("admission was asked for %v", admission.asked)
			}
		})
	}
}
