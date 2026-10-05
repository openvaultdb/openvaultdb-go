package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/dal-go/dalgo/access"
	az "github.com/dal-go/dalgo/dtql/authorization"
	"github.com/dal-go/record"

	api "github.com/openvaultdb/openvaultdb-go/pkg/authorizationapi"
	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/manifest"
	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
)

// scriptedInspector stands for the coordinator of an inspection: it records the
// operation ids of each protected session it is asked for and answers it with
// the function it was given.
type scriptedInspector struct {
	calls  [][]string
	answer func(call int, ops []access.ProtectedOperation, inspect func(access.InspectionSession) error) error
}

func (c *scriptedInspector) WithinInspection(_ context.Context, ops []access.ProtectedOperation, inspect func(access.InspectionSession) error) error {
	ids := make([]string, len(ops))
	for i, op := range ops {
		ids[i] = op.ID()
	}
	c.calls = append(c.calls, ids)
	return c.answer(len(c.calls)-1, ops, inspect)
}

// scriptedSession is an inspection session that answers the two questions the
// inspection asks of it; any other method is the nil embedded session's.
type scriptedSession struct {
	access.InspectionSession
	assessment access.Assessment
	assessErr  error
	visible    map[string]bool
	visibleErr error
}

func (s scriptedSession) Assess(context.Context) (access.Assessment, error) {
	return s.assessment, s.assessErr
}

func (s scriptedSession) ReadVisibilityFor(context.Context, access.Principal) (map[string]bool, error) {
	return s.visible, s.visibleErr
}

// sessionFor is the session of a coordinator that accepts ops and allows each of
// them: one policy decision for each, in order, and every record readable.
func sessionFor(ops []access.ProtectedOperation) scriptedSession {
	session := scriptedSession{assessment: access.Assessment{Outcome: access.AssessmentAllow, Complete: true}, visible: map[string]bool{}}
	for _, op := range ops {
		session.assessment.Policies = append(session.assessment.Policies, access.PolicyAssessment{OperationID: op.ID(), Decision: access.Decision{Allowed: true}})
		session.visible[op.ID()] = true
	}
	return session
}

// protectedReads is a read of the record of each id of the table t, in the order
// given, as the request and as the protected operations.
func protectedReads(t *testing.T, ids ...string) (api.Request, []access.ProtectedOperation) {
	t.Helper()
	request := api.Request{APIVersion: az.APIVersion, Mode: az.ModeInspect, DiagnosticLevel: "references"}
	var ops []access.ProtectedOperation
	for _, id := range ids {
		request.Operations = append(request.Operations, api.Operation{ID: id, Action: "get", ExecutionClass: az.ExecutionDTQL, Resource: az.Resource{DatabaseID: "crm", Table: "t", RowID: id}})
		op, err := access.NewProtectedRead(id, access.Get, record.NewKeyWithID("t", id))
		if err != nil {
			t.Fatal(err)
		}
		ops = append(ops, op)
	}
	return request, ops
}

// inspectionServerAndDatabase is a server and a database of one declared table,
// for the functions that project an inspection without a handler.
func inspectionServerAndDatabase(t *testing.T) (*Server, *core.Database) {
	t.Helper()
	m := &manifest.Manifest{
		Database: manifest.Database{ID: "crm", SchemaMode: schema.ModeStrict},
		Storage:  manifest.Storage{Engine: "sqlite"},
		Schemas:  &schema.Schemas{Collections: map[string]schema.Collection{"t": {Fields: map[string]schema.Field{"name": {Type: schema.TypeString}}}}},
	}
	db, err := core.Open(m, guardOperationDB{}, []schema.Mode{schema.ModeStrict}, "")
	if err != nil {
		t.Fatal(err)
	}
	return New("test", nil), db
}

// TestInspectProtectedAnswersAnOperationTheSessionRefuses: a protected session that
// is refused before it assesses anything does not say which operation it could not
// prepare. Each operation is then assessed in a session of its own, the ones that
// session refuses are answered as hidden, and the answer for the rest is the one
// the session would have given. A refusal is never the answer, and a failure of a
// session, or a refusal after the assessment, is returned as it came.
func TestInspectProtectedAnswersAnOperationTheSessionRefuses(t *testing.T) {
	s, db := inspectionServerAndDatabase(t)
	request, ops := protectedReads(t, "a", "b")
	boom := errors.New("session failed")
	var denied error = &access.DeniedError{Decision: access.Decision{Code: access.CodeEnforcementUnsupported}}
	// refuses is a coordinator whose session is refused, before anything is
	// assessed, when it holds an operation of ids, or holds more than max operations
	// (none when max is 0); it assesses and allows the others.
	refuses := func(max int, ids ...string) func(int, []access.ProtectedOperation, func(access.InspectionSession) error) error {
		return func(_ int, held []access.ProtectedOperation, inspect func(access.InspectionSession) error) error {
			for _, op := range held {
				if slices.Contains(ids, op.ID()) {
					return denied
				}
			}
			if max > 0 && len(held) > max {
				return denied
			}
			return inspect(sessionFor(held))
		}
	}
	// admits is a coordinator that accepts every session and answers each with
	// session(held).
	admits := func(session func(held []access.ProtectedOperation) scriptedSession) func(int, []access.ProtectedOperation, func(access.InspectionSession) error) error {
		return func(_ int, held []access.ProtectedOperation, inspect func(access.InspectionSession) error) error {
			return inspect(session(held))
		}
	}
	withAdmission := func(err error, hiddenIDs ...string) func([]access.ProtectedOperation) scriptedSession {
		return func(held []access.ProtectedOperation) scriptedSession {
			session := sessionFor(held)
			session.assessErr = err
			for _, id := range hiddenIDs {
				session.visible[id] = false
			}
			return session
		}
	}
	for _, c := range []struct {
		name       string
		answer     func(int, []access.ProtectedOperation, func(access.InspectionSession) error) error
		wantCalls  [][]string
		wantErr    error
		wantHidden []string
		wantResult az.Outcome
	}{
		{"nothing refused", refuses(0), [][]string{{"a", "b"}}, nil, nil, az.OutcomeAllow},
		{"one operation refused", refuses(0, "a"), [][]string{{"a", "b"}, {"a"}, {"b"}}, nil, []string{"a"}, az.OutcomeDeny},
		{"every operation refused", refuses(0, "a", "b"), [][]string{{"a", "b"}, {"a"}, {"b"}}, nil, []string{"a", "b"}, az.OutcomeDeny},
		{"a refusal that no operation explains alone", refuses(1), [][]string{{"a", "b"}, {"a"}, {"b"}}, nil, nil, az.OutcomeAllow},
		{"a failure of the session", func(int, []access.ProtectedOperation, func(access.InspectionSession) error) error { return boom },
			[][]string{{"a", "b"}}, boom, nil, ""},
		{"a failure while one operation is assessed alone", func(call int, held []access.ProtectedOperation, inspect func(access.InspectionSession) error) error {
			if call == 0 {
				return denied
			}
			if call == 1 {
				return inspect(sessionFor(held))
			}
			return boom
		}, [][]string{{"a", "b"}, {"a"}, {"b"}}, boom, nil, ""},
		{"a refusal after the assessment", func(_ int, held []access.ProtectedOperation, inspect func(access.InspectionSession) error) error {
			session := sessionFor(held)
			session.visibleErr = denied
			return inspect(session)
		}, [][]string{{"a", "b"}}, denied, nil, ""},
		{"a refusal after the assessment of one operation alone", func(call int, held []access.ProtectedOperation, inspect func(access.InspectionSession) error) error {
			if call == 0 {
				return denied
			}
			session := sessionFor(held)
			session.visibleErr = denied
			return inspect(session)
		}, [][]string{{"a", "b"}, {"a"}}, denied, nil, ""},
		{"an assessment that fails before it has an outcome", func(_ int, _ []access.ProtectedOperation, inspect func(access.InspectionSession) error) error {
			return inspect(scriptedSession{assessErr: boom})
		}, [][]string{{"a", "b"}}, boom, nil, ""},
		{"an admission that fails for a record the caller cannot read", admits(withAdmission(access.ErrProtectedResourceUnavailable, "a")),
			[][]string{{"a", "b"}}, nil, nil, az.OutcomeDeny},
		{"an admission that fails for records the caller can read", admits(withAdmission(access.ErrProtectedResourceUnavailable)),
			[][]string{{"a", "b"}}, access.ErrProtectedResourceUnavailable, nil, ""},
		{"an admission that fails for a record the caller cannot read, each operation alone", func(call int, held []access.ProtectedOperation, inspect func(access.InspectionSession) error) error {
			if call == 0 {
				return denied
			}
			return inspect(withAdmission(access.ErrProtectedResourceUnavailable, "a")(held))
		}, [][]string{{"a", "b"}, {"a"}, {"b"}}, nil, nil, az.OutcomeDeny},
		{"an admission that fails for records the caller can read, each operation alone", func(call int, held []access.ProtectedOperation, inspect func(access.InspectionSession) error) error {
			if call == 0 {
				return denied
			}
			return inspect(withAdmission(access.ErrProtectedResourceUnavailable)(held))
		}, [][]string{{"a", "b"}, {"a"}, {"b"}}, access.ErrProtectedResourceUnavailable, nil, ""},
		{"a revision conflict, each operation alone", func(call int, held []access.ProtectedOperation, inspect func(access.InspectionSession) error) error {
			if call == 0 {
				return denied
			}
			return inspect(withAdmission(access.ErrDataRevisionConflict)(held))
		}, [][]string{{"a", "b"}, {"a"}, {"b"}}, access.ErrDataRevisionConflict, nil, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			coordinator := &scriptedInspector{answer: c.answer}
			hidden := map[string]bool{}
			r := httptest.NewRequest("POST", "/", nil)
			result, err := s.inspectProtected(r, db, coordinator, request, nil, access.Principal{}, ops, hidden)
			if !errors.Is(err, c.wantErr) {
				t.Fatalf("error %v, want %v", err, c.wantErr)
			}
			if !reflect.DeepEqual(coordinator.calls, c.wantCalls) {
				t.Errorf("sessions %v, want %v", coordinator.calls, c.wantCalls)
			}
			var gotHidden []string
			for id := range hidden {
				gotHidden = append(gotHidden, id)
			}
			slices.Sort(gotHidden)
			if !slices.Equal(gotHidden, c.wantHidden) {
				t.Errorf("hidden %v, want %v", gotHidden, c.wantHidden)
			}
			if c.wantErr == nil && result.Result != c.wantResult {
				t.Errorf("result %q, want %q", result.Result, c.wantResult)
			}
		})
	}
}

// TestInspectProtectedOfNoOperationAsksNoSession: an inspection whose operations
// are all on tables the database does not declare has nothing to assess.
func TestInspectProtectedOfNoOperationAsksNoSession(t *testing.T) {
	s, db := inspectionServerAndDatabase(t)
	request, _ := protectedReads(t, "a")
	coordinator := &scriptedInspector{answer: func(int, []access.ProtectedOperation, func(access.InspectionSession) error) error {
		return errors.New("a session was asked for")
	}}
	hidden := map[string]bool{"a": true}
	result, err := s.inspectProtected(httptest.NewRequest("POST", "/", nil), db, coordinator, request, nil, access.Principal{}, nil, hidden)
	if err != nil || result.Result != az.OutcomeDeny || len(coordinator.calls) != 0 {
		t.Errorf("result %q, error %v, sessions %v", result.Result, err, coordinator.calls)
	}
}

// TestInspectEachPutsTheAssessmentsTogether: the operations that are assessed one
// by one give the assessment one session gives: decisions and restrictions in the
// order of the operations, the worst outcome, complete only when every operation
// is, the records the caller may read, and the admission error only when the
// outcome is an allow, a revision conflict after any other.
func TestInspectEachPutsTheAssessmentsTogether(t *testing.T) {
	_, ops := protectedReads(t, "a", "b", "c")
	boom := errors.New("admission failed")
	// answers is the session of each operation alone, by id.
	type answer struct {
		outcome   access.AssessmentOutcome
		complete  bool
		readable  bool
		admission error
		refused   bool
	}
	run := func(answers map[string]answer) (inspected, []string, map[string]bool, error) {
		coordinator := &scriptedInspector{answer: func(_ int, held []access.ProtectedOperation, inspect func(access.InspectionSession) error) error {
			a := answers[held[0].ID()]
			if a.refused {
				return &access.DeniedError{Decision: access.Decision{Code: access.CodeEnforcementUnsupported}}
			}
			session := scriptedSession{
				assessment: access.Assessment{
					Outcome: a.outcome, Complete: a.complete,
					Policies:     []access.PolicyAssessment{{OperationID: held[0].ID()}},
					Restrictions: []access.AssessmentRestriction{{OperationID: held[0].ID()}},
				},
				assessErr: a.admission,
				visible:   map[string]bool{held[0].ID(): a.readable},
			}
			return inspect(session)
		}}
		hidden := map[string]bool{}
		got, unprepared, err := inspectEach(context.Background(), coordinator, ops, access.Principal{}, hidden)
		return got, unprepared, hidden, err
	}
	allow := answer{outcome: access.AssessmentAllow, complete: true, readable: true}
	t.Run("the worst outcome and the order of the decisions", func(t *testing.T) {
		got, unprepared, hidden, err := run(map[string]answer{
			"a": allow,
			"b": {outcome: access.AssessmentDeny, complete: true},
			"c": {outcome: access.AssessmentIndeterminate, readable: true},
		})
		if err != nil || len(unprepared) != 0 || len(hidden) != 0 {
			t.Fatalf("error %v, unprepared %v, hidden %v", err, unprepared, hidden)
		}
		var ids []string
		for _, pa := range got.assessment.Policies {
			ids = append(ids, pa.OperationID)
		}
		var restrictions []string
		for _, restriction := range got.assessment.Restrictions {
			restrictions = append(restrictions, restriction.OperationID)
		}
		if got.assessment.Outcome != access.AssessmentDeny || got.assessment.Complete || !slices.Equal(ids, []string{"a", "b", "c"}) || !slices.Equal(restrictions, ids) {
			t.Errorf("assessment %+v, decisions %v, restrictions %v", got.assessment, ids, restrictions)
		}
		if !reflect.DeepEqual(got.visible, map[string]bool{"a": true, "b": false, "c": true}) || got.admissionErr != nil {
			t.Errorf("visible %v, admission %v", got.visible, got.admissionErr)
		}
	})
	t.Run("a complete assessment of allowed operations", func(t *testing.T) {
		got, _, _, err := run(map[string]answer{"a": allow, "b": allow, "c": allow})
		if err != nil || got.assessment.Outcome != access.AssessmentAllow || !got.assessment.Complete || got.admissionErr != nil {
			t.Errorf("assessment %+v, admission %v, error %v", got.assessment, got.admissionErr, err)
		}
	})
	t.Run("a refused operation is not assessed", func(t *testing.T) {
		got, unprepared, hidden, err := run(map[string]answer{"a": allow, "b": {refused: true}, "c": {refused: true}})
		if err != nil || !slices.Equal(unprepared, []string{"t", "t"}) || !reflect.DeepEqual(hidden, map[string]bool{"b": true, "c": true}) {
			t.Fatalf("error %v, unprepared %v, hidden %v", err, unprepared, hidden)
		}
		if len(got.assessment.Policies) != 1 || got.assessment.Policies[0].OperationID != "a" || len(got.visible) != 1 {
			t.Errorf("assessment %+v, visible %v", got.assessment, got.visible)
		}
	})
	t.Run("nothing assessed", func(t *testing.T) {
		got, _, _, err := run(map[string]answer{"a": {refused: true}, "b": {refused: true}, "c": {refused: true}})
		if err != nil || !reflect.DeepEqual(got.assessment, access.Assessment{}) || got.visible == nil || got.admissionErr != nil {
			t.Errorf("assessment %+v, visible %v, admission %v, error %v", got.assessment, got.visible, got.admissionErr, err)
		}
	})
	t.Run("an admission error of an allow", func(t *testing.T) {
		admitted := allow
		admitted.admission = boom
		got, _, _, err := run(map[string]answer{"a": allow, "b": admitted, "c": allow})
		if err != nil || !errors.Is(got.admissionErr, boom) {
			t.Errorf("admission %v, error %v", got.admissionErr, err)
		}
	})
	t.Run("a revision conflict that comes before an admission error", func(t *testing.T) {
		conflict, admitted := allow, allow
		conflict.admission, admitted.admission = access.ErrDataRevisionConflict, boom
		got, _, _, err := run(map[string]answer{"a": conflict, "b": admitted, "c": allow})
		if err != nil || !errors.Is(got.admissionErr, boom) {
			t.Errorf("admission %v, error %v", got.admissionErr, err)
		}
	})
	t.Run("a revision conflict alone", func(t *testing.T) {
		conflict := allow
		conflict.admission = access.ErrDataRevisionConflict
		got, _, _, err := run(map[string]answer{"a": allow, "b": conflict, "c": allow})
		if err != nil || !errors.Is(got.admissionErr, access.ErrDataRevisionConflict) {
			t.Errorf("admission %v, error %v", got.admissionErr, err)
		}
	})
	t.Run("an admission error that another operation's denial outweighs", func(t *testing.T) {
		admitted := allow
		admitted.admission = boom
		got, _, _, err := run(map[string]answer{"a": admitted, "b": {outcome: access.AssessmentDeny, complete: true}, "c": allow})
		if err != nil || got.admissionErr != nil || got.assessment.Outcome != access.AssessmentDeny {
			t.Errorf("assessment %+v, admission %v, error %v", got.assessment, got.admissionErr, err)
		}
	})
	t.Run("a failure of a session", func(t *testing.T) {
		failed := allow
		failed.outcome, failed.admission = "", boom
		if _, _, _, err := run(map[string]answer{"a": allow, "b": failed, "c": allow}); !errors.Is(err, boom) {
			t.Errorf("error %v, want %v", err, boom)
		}
	})
}

// TestReduceAssessment: of two outcomes of an assessment the worse is the result,
// from allow through conditional and indeterminate to deny.
func TestReduceAssessment(t *testing.T) {
	order := []access.AssessmentOutcome{access.AssessmentAllow, access.AssessmentConditional, access.AssessmentIndeterminate, access.AssessmentDeny}
	for i, a := range order {
		for j, b := range order {
			want := order[max(i, j)]
			if got := reduceAssessment(a, b); got != want {
				t.Errorf("%s and %s: %s, want %s", a, b, got, want)
			}
		}
	}
}

// TestLogUnpreparedCollections: the collections a protected session could not
// prepare are one warning for the request, with the method, the route path and
// the names once each, in order, and nothing when there are none.
func TestLogUnpreparedCollections(t *testing.T) {
	var logs bytes.Buffer
	s := New("test", nil, WithLogger(slog.New(slog.NewJSONHandler(&logs, nil))))
	s.logUnpreparedCollections(httptest.NewRequest("POST", "/v1/databases/crm/access/evaluate?x=1", nil), nil)
	if logs.Len() != 0 {
		t.Fatalf("logged %s", logs.String())
	}
	s.logUnpreparedCollections(httptest.NewRequest("PATCH", "/v1/databases/crm/records/b/01?x=1", nil), []string{"b", "a", "b"})
	var entry struct {
		Level       string
		Msg         string
		Method      string
		Path        string
		Collections []string
	}
	if err := json.Unmarshal(logs.Bytes(), &entry); err != nil {
		t.Fatalf("%v: %s", err, logs.String())
	}
	if entry.Msg != "protected session refused an operation" {
		t.Errorf("logged the message %q", entry.Msg)
	}
	if entry.Level != "WARN" || entry.Method != "PATCH" || entry.Path != "/v1/databases/crm/records/b/01" || !slices.Equal(entry.Collections, []string{"a", "b"}) || strings.Count(logs.String(), "\n") != 1 {
		t.Errorf("logged %s", logs.String())
	}
}

// TestAdmittedOperations: an operation is admitted when a decision of the policy
// for it is allowed, or is denied for a reason that the stored row decides, and
// none refuses it whatever the row. A decision the policy could not make says
// nothing, so an operation with no other decision is not admitted, and neither is
// one with no decision at all.
func TestAdmittedOperations(t *testing.T) {
	decide := func(id string, allowed bool, code access.ReasonCode) access.PolicyAssessment {
		return access.PolicyAssessment{OperationID: id, Decision: access.Decision{Allowed: allowed, Code: code}}
	}
	assessment := access.Assessment{Policies: []access.PolicyAssessment{
		decide("allowed", true, ""),
		decide("no-match", false, access.CodeNoMatch),
		decide("rule", false, access.CodeRuleDenied),
		decide("collection", false, access.CodeCollectionDenied),
		decide("row", false, access.CodeRowPredicateFailed),
		decide("image", false, access.CodePostImageFailed),
		decide("column", false, access.CodeColumnDenied),
		decide("failed", false, access.CodeEvaluationFailed),
		decide("unresolved", false, access.CodePrincipalUnresolved),
		decide("source", false, access.CodeSourceUnavailable),
		decide("configuration", false, access.CodeConfigurationInvalid),
		decide("unsupported", false, access.CodeEnforcementUnsupported),
		decide("allowed-then-failed", true, ""),
		decide("allowed-then-failed", false, access.CodeEvaluationFailed),
		decide("row-then-no-match", false, access.CodeRowPredicateFailed),
		decide("row-then-no-match", false, access.CodeNoMatch),
		decide("no-match-then-row", false, access.CodeNoMatch),
		decide("no-match-then-row", false, access.CodeRowPredicateFailed),
		decide("allowed-then-rule", true, ""),
		decide("allowed-then-rule", false, access.CodeRuleDenied),
		decide("failed-then-allowed", false, access.CodePrincipalUnresolved),
		decide("failed-then-allowed", true, ""),
	}}
	got := admittedOperations(assessment)
	for id, want := range map[string]bool{
		"allowed": true, "row": true, "image": true, "column": true, "allowed-then-failed": true, "failed-then-allowed": true,
		"no-match": false, "rule": false, "collection": false, "failed": false, "unresolved": false, "source": false, "configuration": false,
		"unsupported": false, "row-then-no-match": false, "no-match-then-row": false, "allowed-then-rule": false, "absent": false,
	} {
		if got[id] != want {
			t.Errorf("%s: admitted %t, want %t", id, got[id], want)
		}
	}
}
