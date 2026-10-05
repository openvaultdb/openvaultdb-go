package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dal-go/dalgo/access"
	az "github.com/dal-go/dalgo/dtql/authorization"
	"github.com/dal-go/record"

	api "github.com/openvaultdb/openvaultdb-go/pkg/authorizationapi"
	"github.com/openvaultdb/openvaultdb-go/pkg/core"
)

// scriptedExecutor stands for the coordinator of a protected execution: it counts
// the sessions it is asked for and answers each with the function it was given.
type scriptedExecutor struct {
	calls  int
	answer func(execute func(access.ExecutionSession) error) error
}

func (c *scriptedExecutor) WithinExecution(_ context.Context, _ []access.ProtectedOperation, execute func(access.ExecutionSession) error) error {
	c.calls++
	return c.answer(execute)
}

// evidenceSession is an inspection session that answers the evidence it is asked
// for; any other method is the nil embedded session's.
type evidenceSession struct {
	access.InspectionSession
	evidence []access.AuthorizedPointEvidence
	err      error
}

func (s evidenceSession) Evidence(context.Context) ([]access.AuthorizedPointEvidence, error) {
	return s.evidence, s.err
}

// loggedServer is a server whose log is the returned buffer.
func loggedServer() (*Server, *bytes.Buffer) {
	var logs bytes.Buffer
	return New("test", nil, WithLogger(slog.New(slog.NewJSONHandler(&logs, nil)))), &logs
}

// requestWithContext is a request to path whose context is ctx.
func requestWithContext(ctx context.Context, method, path string, body string) *http.Request {
	return httptest.NewRequest(method, path, strings.NewReader(body)).WithContext(ctx)
}

// ranOutOfTime are the contexts of a request that has no time left: one that was
// canceled and one whose deadline has passed.
func ranOutOfTime() map[string]func() (context.Context, context.CancelFunc) {
	return map[string]func() (context.Context, context.CancelFunc){
		"canceled": func() (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			return ctx, cancel
		},
		"past its deadline": func() (context.Context, context.CancelFunc) {
			return context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		},
	}
}

// assertUnavailable checks that err is answered 503 authorization_unavailable.
func assertUnavailable(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("no error")
	}
	rec := httptest.NewRecorder()
	writeProtectedFailure(rec, err)
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), `"code":"authorization_unavailable"`) {
		t.Errorf("want 503 authorization_unavailable, got %d %s", rec.Code, rec.Body.String())
	}
}

// TestInspectProtectedAnswersARefusalAsHiddenOnlyWhileTheRequestIsAlive: a
// protected session that is refused before anything is assessed is answered as a
// table the caller may not see, and logged, while the context of the request has no
// error. When the request was canceled or ran past its deadline the refusal is not
// the answer: the failure is the one of any other session (503
// authorization_unavailable), and no warning names a collection. That holds for an
// inspection of one operation and of several, whether the refusal is of the
// operations together or of one of them alone, and whether the request was over
// before the first session or ran out while the operations were assessed one by one.
func TestInspectProtectedAnswersARefusalAsHiddenOnlyWhileTheRequestIsAlive(t *testing.T) {
	var denied error = &access.DeniedError{Decision: access.Decision{Code: access.CodeEnforcementUnsupported}}
	// refuses is a coordinator whose session is refused, before anything is
	// assessed, when it holds an operation of ids (any operation when there are no
	// ids); it assesses and allows the others.
	refuses := func(ids ...string) func(int, []access.ProtectedOperation, func(access.InspectionSession) error) error {
		return func(_ int, held []access.ProtectedOperation, inspect func(access.InspectionSession) error) error {
			for _, op := range held {
				if len(ids) == 0 || slices.Contains(ids, op.ID()) {
					return denied
				}
			}
			return inspect(sessionFor(held))
		}
	}
	for _, c := range []struct {
		name       string
		ids        []string
		refusing   []string
		wantHidden []string
		wantLogged []string
	}{
		{"one operation", []string{"a"}, nil, []string{"a"}, []string{"t"}},
		{"several operations refused together", []string{"a", "b"}, nil, []string{"a", "b"}, []string{"t"}},
		{"several operations of which one is refused alone", []string{"a", "b"}, []string{"a"}, []string{"a"}, []string{"t"}},
	} {
		t.Run(c.name+" with a live context", func(t *testing.T) {
			s, logs := loggedServer()
			db := mustInspectionDatabase(t)
			request, ops := protectedReads(t, c.ids...)
			coordinator := &scriptedInspector{answer: refuses(c.refusing...)}
			hidden := map[string]bool{}
			result, err := s.inspectProtected(requestWithContext(context.Background(), "POST", "/v1/databases/crm/access/evaluate", ""), db, coordinator, request, nil, access.Principal{}, ops, hidden)
			if err != nil || result.Result != az.OutcomeDeny {
				t.Fatalf("result %q, error %v", result.Result, err)
			}
			if len(hidden) != len(c.wantHidden) {
				t.Errorf("hidden %v, want %v", hidden, c.wantHidden)
			}
			for _, id := range c.wantHidden {
				if !hidden[id] {
					t.Errorf("%s is not answered as hidden: %v", id, hidden)
				}
			}
			if !strings.Contains(logs.String(), `"collections":["t"]`) || strings.Count(logs.String(), "\n") != 1 {
				t.Errorf("logged %s", logs.String())
			}
		})
		for name, expired := range ranOutOfTime() {
			t.Run(c.name+" with a context that is "+name, func(t *testing.T) {
				s, logs := loggedServer()
				db := mustInspectionDatabase(t)
				request, ops := protectedReads(t, c.ids...)
				ctx, cancel := expired()
				defer cancel()
				coordinator := &scriptedInspector{answer: refuses(c.refusing...)}
				hidden := map[string]bool{}
				_, err := s.inspectProtected(requestWithContext(ctx, "POST", "/v1/databases/crm/access/evaluate", ""), db, coordinator, request, nil, access.Principal{}, ops, hidden)
				assertUnavailable(t, err)
				if len(hidden) != 0 || logs.Len() != 0 {
					t.Errorf("hidden %v, logged %s", hidden, logs.String())
				}
				if len(coordinator.calls) != 1 {
					t.Errorf("sessions %v: the operations were assessed one by one after the request was over", coordinator.calls)
				}
			})
		}
		t.Run(c.name+" with a context that runs out while the operations are assessed one by one", func(t *testing.T) {
			s, logs := loggedServer()
			db := mustInspectionDatabase(t)
			request, ops := protectedReads(t, c.ids...)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			coordinator := &scriptedInspector{answer: func(call int, held []access.ProtectedOperation, inspect func(access.InspectionSession) error) error {
				if call == 1 {
					// The first session that holds one operation alone ends the request.
					cancel()
				}
				return refuses()(call, held, inspect)
			}}
			hidden := map[string]bool{}
			_, err := s.inspectProtected(requestWithContext(ctx, "POST", "/v1/databases/crm/access/evaluate", ""), db, coordinator, request, nil, access.Principal{}, ops, hidden)
			assertUnavailable(t, err)
			if logs.Len() != 0 {
				t.Errorf("logged %s", logs.String())
			}
		})
	}
}

// mustInspectionDatabase is the database of inspectionServerAndDatabase.
func mustInspectionDatabase(t *testing.T) *core.Database {
	t.Helper()
	_, db := inspectionServerAndDatabase(t)
	return db
}

// updateRequest is a protected PATCH of the record a of the table t.
func updateRequest(ctx context.Context) (*http.Request, *record.Key) {
	op := api.Operation{ID: "op1", Action: "update", ExecutionClass: az.ExecutionDTQL, Resource: az.Resource{DatabaseID: "crm", Path: "/t/a"},
		Mutation: &api.Mutation{Changes: []api.Change{{Op: "set", Path: []string{"name"}, Value: json.RawMessage(`"x"`)}}}}
	data, _ := json.Marshal(op)
	r := requestWithContext(ctx, "PATCH", "/v1/databases/crm/records/t/a", string(data))
	r.Header.Set("Content-Type", "application/vnd.dtql.operation+json")
	return r, record.NewKeyWithID("t", "a")
}

// TestProtectedUpdateAnswersARefusalAsHiddenOnlyWhileTheRequestIsAlive: a protected
// PATCH whose session is refused before anything is assessed is answered 404
// resource_unavailable, and logged, while the context of the request has no error;
// when the request was canceled or ran past its deadline, before the session or
// while it was refused, the answer is 503 authorization_unavailable and nothing is
// logged.
func TestProtectedUpdateAnswersARefusalAsHiddenOnlyWhileTheRequestIsAlive(t *testing.T) {
	var denied error = &access.DeniedError{Decision: access.Decision{Code: access.CodeEnforcementUnsupported}}
	refused := func(execute func(access.ExecutionSession) error) error { return denied }
	t.Run("a live context", func(t *testing.T) {
		s, logs := loggedServer()
		_, db := inspectionServerAndDatabase(t)
		r, key := updateRequest(context.Background())
		w := httptest.NewRecorder()
		s.updateProtected(w, r, db, key, &scriptedExecutor{answer: refused})
		if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), `"code":"resource_unavailable"`) {
			t.Errorf("want 404 resource_unavailable, got %d %s", w.Code, w.Body.String())
		}
		if !strings.Contains(logs.String(), `"collections":["t"]`) || strings.Count(logs.String(), "\n") != 1 {
			t.Errorf("logged %s", logs.String())
		}
	})
	for name, expired := range ranOutOfTime() {
		t.Run("a context that is "+name, func(t *testing.T) {
			s, logs := loggedServer()
			_, db := inspectionServerAndDatabase(t)
			ctx, cancel := expired()
			defer cancel()
			r, key := updateRequest(ctx)
			w := httptest.NewRecorder()
			s.updateProtected(w, r, db, key, &scriptedExecutor{answer: refused})
			if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), `"code":"authorization_unavailable"`) {
				t.Errorf("want 503 authorization_unavailable, got %d %s", w.Code, w.Body.String())
			}
			if logs.Len() != 0 {
				t.Errorf("logged %s", logs.String())
			}
		})
	}
	t.Run("a context that runs out while the session is refused", func(t *testing.T) {
		s, logs := loggedServer()
		_, db := inspectionServerAndDatabase(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		r, key := updateRequest(ctx)
		w := httptest.NewRecorder()
		s.updateProtected(w, r, db, key, &scriptedExecutor{answer: func(func(access.ExecutionSession) error) error {
			cancel()
			return denied
		}})
		if w.Code != http.StatusServiceUnavailable || logs.Len() != 0 {
			t.Errorf("got %d %s, logged %s", w.Code, w.Body.String(), logs.String())
		}
	})
}

// TestEvidenceOnceAnswersARefusalAsHiddenOnlyWhileTheRequestIsAlive: the session of
// an evidence request that is refused before the evidence is asked for is a
// refusal while the context has no error, and the failure of the context when it
// has one; a refusal after the session was entered, and any other failure, are
// returned as they came, and the evidence is returned with no refusal.
func TestEvidenceOnceAnswersARefusalAsHiddenOnlyWhileTheRequestIsAlive(t *testing.T) {
	_, ops := protectedReads(t, "a")
	var denied error = &access.DeniedError{Decision: access.Decision{Code: access.CodeEnforcementUnsupported}}
	boom := errors.New("session failed")
	found := []access.AuthorizedPointEvidence{{Exists: true, DataRevision: "r1"}}
	refuses := func(int, []access.ProtectedOperation, func(access.InspectionSession) error) error { return denied }
	enters := func(session evidenceSession, after error) func(int, []access.ProtectedOperation, func(access.InspectionSession) error) error {
		return func(_ int, _ []access.ProtectedOperation, inspect func(access.InspectionSession) error) error {
			if err := inspect(session); err != nil {
				return err
			}
			return after
		}
	}
	live := func() (context.Context, context.CancelFunc) { return context.WithCancel(context.Background()) }
	canceled := ranOutOfTime()["canceled"]
	for _, c := range []struct {
		name         string
		ctx          func() (context.Context, context.CancelFunc)
		answer       func(int, []access.ProtectedOperation, func(access.InspectionSession) error) error
		wantEvidence int
		wantRefused  bool
		wantErr      error
	}{
		{"a refusal of a live request", live, refuses, 0, true, denied},
		{"a refusal of a request that is over", canceled, refuses, 0, false, context.Canceled},
		{"a refusal after the session was entered", live, enters(evidenceSession{err: denied}, nil), 0, false, denied},
		{"a refusal after the session was entered of a request that is over", canceled, enters(evidenceSession{err: denied}, nil), 0, false, denied},
		{"a failure", live, func(int, []access.ProtectedOperation, func(access.InspectionSession) error) error { return boom }, 0, false, boom},
		{"a failure of a request that is over", canceled, func(int, []access.ProtectedOperation, func(access.InspectionSession) error) error { return boom }, 0, false, boom},
		{"the evidence", live, enters(evidenceSession{evidence: found}, nil), 1, false, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx, cancel := c.ctx()
			defer cancel()
			coordinator := &scriptedInspector{answer: c.answer}
			evidence, refused, err := evidenceOnce(ctx, coordinator, ops[0])
			if !errors.Is(err, c.wantErr) || refused != c.wantRefused || len(evidence) != c.wantEvidence {
				t.Errorf("evidence %v, refused %t, error %v; want %d, %t, %v", evidence, refused, err, c.wantEvidence, c.wantRefused, c.wantErr)
			}
			if len(coordinator.calls) != 1 || len(coordinator.calls[0]) != 1 {
				t.Errorf("sessions %v", coordinator.calls)
			}
		})
	}
}
