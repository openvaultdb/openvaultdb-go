package server

import (
	"context"
	"errors"
	"net/http/httptest"
	"reflect"
	"slices"
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

// TestInspectProtectedAnswersAnOperationTheSessionRefuses: a protected session that
// is refused before it assesses anything does not say which operation it could not
// prepare. Each operation is prepared alone, the ones refused are answered as
// hidden and the rest are assessed again, and a refusal that no operation explains
// alone, or that a failure of the session explains, is returned as it came.
func TestInspectProtectedAnswersAnOperationTheSessionRefuses(t *testing.T) {
	m := &manifest.Manifest{
		Database: manifest.Database{ID: "crm", SchemaMode: schema.ModeStrict},
		Storage:  manifest.Storage{Engine: "sqlite"},
		Schemas:  &schema.Schemas{Collections: map[string]schema.Collection{"t": {Fields: map[string]schema.Field{"name": {Type: schema.TypeString}}}}},
	}
	db, err := core.Open(m, guardOperationDB{}, []schema.Mode{schema.ModeStrict}, "")
	if err != nil {
		t.Fatal(err)
	}
	s := New("test", nil)
	request := api.Request{APIVersion: az.APIVersion, Mode: az.ModeInspect, DiagnosticLevel: "references"}
	var ops []access.ProtectedOperation
	for _, id := range []string{"a", "b"} {
		request.Operations = append(request.Operations, api.Operation{ID: id, Action: "get", ExecutionClass: az.ExecutionDTQL, Resource: az.Resource{DatabaseID: "crm", Table: "t", RowID: id}})
		op, err := access.NewProtectedRead(id, access.Get, record.NewKeyWithID("t", id))
		if err != nil {
			t.Fatal(err)
		}
		ops = append(ops, op)
	}
	allowed := scriptedSession{assessment: access.Assessment{Outcome: access.AssessmentAllow, Complete: true}, visible: map[string]bool{"a": true, "b": true}}
	boom := errors.New("session failed")
	var denied error = &access.DeniedError{Decision: access.Decision{Code: access.CodeEnforcementUnsupported}}
	// refuses is a session that is refused, before anything is assessed, when it
	// holds an operation of ids; it assesses the others.
	refuses := func(ids ...string) func(int, []access.ProtectedOperation, func(access.InspectionSession) error) error {
		return func(_ int, ops []access.ProtectedOperation, inspect func(access.InspectionSession) error) error {
			for _, op := range ops {
				if slices.Contains(ids, op.ID()) {
					return denied
				}
			}
			return inspect(allowed)
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
		{"nothing refused", refuses(), [][]string{{"a", "b"}}, nil, nil, az.OutcomeAllow},
		{"one operation refused", refuses("a"), [][]string{{"a", "b"}, {"a"}, {"b"}, {"b"}}, nil, []string{"a"}, az.OutcomeDeny},
		{"every operation refused", refuses("a", "b"), [][]string{{"a", "b"}, {"a"}, {"b"}}, nil, []string{"a", "b"}, az.OutcomeDeny},
		{"a refusal that no operation explains alone", func(_ int, ops []access.ProtectedOperation, inspect func(access.InspectionSession) error) error {
			if len(ops) > 1 {
				return denied
			}
			return inspect(allowed)
		}, [][]string{{"a", "b"}, {"a"}, {"b"}}, denied, nil, ""},
		{"a refusal that comes back for the operations left", func(call int, ops []access.ProtectedOperation, inspect func(access.InspectionSession) error) error {
			// The first session refuses a alone; the second, which holds b alone, is
			// refused although b was prepared alone a moment before.
			if call == 1 || call == 3 || call == 4 {
				return denied
			}
			if call == 2 {
				return inspect(allowed)
			}
			return denied
		}, [][]string{{"a", "b"}, {"a"}, {"b"}, {"b"}, {"b"}}, nil, []string{"a", "b"}, az.OutcomeDeny},
		{"a failure of the session", func(int, []access.ProtectedOperation, func(access.InspectionSession) error) error { return boom },
			[][]string{{"a", "b"}}, boom, nil, ""},
		{"a failure while one operation is prepared alone", func(call int, _ []access.ProtectedOperation, _ func(access.InspectionSession) error) error {
			if call == 0 {
				return denied
			}
			return boom
		}, [][]string{{"a", "b"}, {"a"}}, boom, nil, ""},
		{"a refusal after the assessment", func(_ int, _ []access.ProtectedOperation, inspect func(access.InspectionSession) error) error {
			session := allowed
			session.visibleErr = denied
			return inspect(session)
		}, [][]string{{"a", "b"}}, denied, nil, ""},
		{"an assessment that fails before it has an outcome", func(_ int, _ []access.ProtectedOperation, inspect func(access.InspectionSession) error) error {
			return inspect(scriptedSession{assessErr: boom})
		}, [][]string{{"a", "b"}}, boom, nil, ""},
		{"an admission that fails for a record the caller cannot read", func(_ int, _ []access.ProtectedOperation, inspect func(access.InspectionSession) error) error {
			session := allowed
			session.assessErr = access.ErrProtectedResourceUnavailable
			session.visible = map[string]bool{"a": false, "b": true}
			return inspect(session)
		}, [][]string{{"a", "b"}}, nil, nil, az.OutcomeDeny},
		{"an admission that fails for records the caller can read", func(_ int, _ []access.ProtectedOperation, inspect func(access.InspectionSession) error) error {
			session := allowed
			session.assessErr = access.ErrProtectedResourceUnavailable
			return inspect(session)
		}, [][]string{{"a", "b"}}, access.ErrProtectedResourceUnavailable, nil, ""},
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

// TestDeniedWhateverTheRow: an operation is denied whatever the row when a policy
// refuses it for a reason that the stored row does not decide. A decision that is
// allowed, one the policy could not make and one that the row's data decides do
// not say so.
func TestDeniedWhateverTheRow(t *testing.T) {
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
		decide("both", false, access.CodeRowPredicateFailed),
		decide("both", false, access.CodeNoMatch),
	}}
	got := deniedWhateverTheRow(assessment)
	for id, want := range map[string]bool{"allowed": false, "no-match": true, "rule": true, "collection": true, "row": false, "image": false, "column": false, "failed": false, "both": true, "absent": false} {
		if got[id] != want {
			t.Errorf("%s: denied whatever the row %t, want %t", id, got[id], want)
		}
	}
}
