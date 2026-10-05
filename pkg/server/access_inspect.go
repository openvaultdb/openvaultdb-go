package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"github.com/dal-go/dalgo/access"
	az "github.com/dal-go/dalgo/dtql/authorization"
	"github.com/dal-go/record"
	"github.com/dal-go/record/update"
	"github.com/openvaultdb/openvaultdb-go/pkg/auth"
	api "github.com/openvaultdb/openvaultdb-go/pkg/authorizationapi"
	"github.com/openvaultdb/openvaultdb-go/pkg/core"
)

var errProtectedUnsupported = errors.New("protected operation unsupported")

func protectedOperation(op api.Operation) (access.ProtectedOperation, error) {
	if op.ExecutionClass != az.ExecutionDTQL || op.Resource.RowID == "" {
		return access.ProtectedOperation{}, errProtectedUnsupported
	}
	key := record.NewKeyWithID(op.Resource.Table, op.Resource.RowID)
	switch op.Action {
	case "get", "exists":
		action := access.Get
		if op.Action == "exists" {
			action = access.Exists
		}
		if len(op.Resource.Columns) > 0 && action == access.Get {
			return access.NewProtectedEvidenceRead(op.ID, action, key, op.Resource.Columns)
		}
		return access.NewProtectedRead(op.ID, action, key)
	case "insert":
		return access.NewProtectedInsert(op.ID, key, op.Mutation.Data)
	case "set":
		return access.NewProtectedSet(op.ID, key, op.Mutation.Data, op.Mutation.IfDataRevision)
	case "delete":
		revision := ""
		if op.Mutation != nil {
			revision = op.Mutation.IfDataRevision
		}
		return access.NewProtectedDelete(op.ID, key, revision)
	case "update":
		changes := make([]update.Update, 0, len(op.Mutation.Changes))
		for _, c := range op.Mutation.Changes {
			var value any
			if err := json.Unmarshal(c.Value, &value); err != nil {
				return access.ProtectedOperation{}, err
			}
			changes = append(changes, update.ByFieldPath(update.FieldPath(c.Path), value))
		}
		return access.NewProtectedUpdate(op.ID, key, changes, op.Mutation.IfDataRevision)
	}
	return access.ProtectedOperation{}, errProtectedUnsupported
}

// projectInspection uses the same pinned assessment as execution. Metadata is
// taken from that assessment, never reloaded while a policy lease is held.
// hidden holds the ids of operations on a table the database does not declare
// (see inspectAccess) or that the protected session cannot prepare: no layer
// decides them, and they are redacted whatever the caller may inspect, which is
// how an operation on a declared table the policy hides is answered for a caller
// who may not inspect protected rows. A caller who may inspect protected rows is
// given layer detail for a record it cannot read only when the policy admits the
// operation for the stored row to decide (see admittedOperations), so a declared
// table the policy hides, or cannot decide anything about, is answered to that
// caller as an undeclared one is.
func (s *Server) projectInspection(r *http.Request, db *core.Database, request api.Request, owners []core.PolicyLayer, assessment access.Assessment, readable, hidden map[string]bool) az.Result {
	// Data visibility requires both owner ACLs and the actual token's data
	// capability; write-only or policy-admin credentials do not imply reads.
	if s.authCfg != nil {
		for _, op := range request.Operations {
			if !auth.FromRequest(r).Allows(db.ID(), auth.CapRecordsRead, op.Resource.Table) {
				readable[op.ID] = false
			}
		}
	}
	result := newAuthorization(request.Mode)
	result.Hypothetical = request.Simulation != nil
	for _, op := range request.Operations {
		result.Operations = append(result.Operations, az.OperationResult{ID: op.ID, RequestOperationID: op.ID, Action: op.Action, Resource: op.Resource, Result: az.OutcomeAllow, RestrictionIDs: []string{}, AllOf: []string{}, ExecutionClass: op.ExecutionClass, Callable: op.Callable})
	}
	admitted := admittedOperations(assessment)
	for _, owner := range owners {
		source := s.accessSource(db, owner.Kind)
		layer := az.Layer{LayerID: sourceLayerID(source), Source: source, ACLState: "disabled", Result: az.OutcomeAllow, Decisions: []az.LayerDecision{}}
		if owner.Enabled {
			layer.ACLState = "enabled"
		}
		for i, op := range request.Operations {
			if hidden[op.ID] {
				continue
			}
			outcome := az.OutcomeAllow
			matched := !owner.Enabled
			for _, pa := range assessment.Policies {
				if pa.OperationID != op.ID || pa.LayerID != owner.Kind {
					continue
				}
				matched = true
				if pa.Decision.Code == access.CodeSourceUnavailable || pa.Decision.Code == access.CodeConfigurationInvalid {
					layer.ACLState = "unavailable"
				}
				decisionOutcome := az.OutcomeAllow
				if !pa.Decision.Allowed {
					decisionOutcome = az.OutcomeDeny
					if pa.Decision.Code.IsIndeterminate() {
						decisionOutcome = az.OutcomeIndeterminate
					}
				}
				outcome = reduceOutcome(outcome, decisionOutcome)
				details := readable[op.ID] || admitted[op.ID] && s.ownerAllows(r, source, auth.CapAccessInspectProtected, op.Resource)
				visible := details && request.DiagnosticLevel != "ordinary" && s.ownerAllows(r, source, auth.CapAccessDiagnostics, op.Resource) && (pa.Policy.Visibility == access.PolicyVisibilityPublic || s.ownerAllows(r, source, auth.CapPoliciesAdmin, op.Resource)) && pa.Policy.Revision != ""
				if !visible {
					continue
				}
				ref := &az.PolicyRef{OwnerID: source.OwnerID, DatabaseID: db.ID(), PolicyID: pa.Policy.ID, Revision: pa.Policy.Revision}
				scope := az.Scope(pa.Decision.Scope)
				if scope == "" || scope == az.ScopeRequest {
					scope = az.ScopeOperation
				}
				layer.Decisions = append(layer.Decisions, az.LayerDecision{OperationID: op.ID, Result: decisionOutcome, PolicyRef: ref, Scope: scope, RestrictionIDs: []string{}})
				if !pa.Decision.Allowed {
					result.Blockers = append(result.Blockers, az.Blocker{OperationID: op.ID, LayerID: layer.LayerID, PolicyRef: ref, Code: pa.Decision.Code, Scope: scope, Slot: string(pa.Decision.Slot), Columns: pa.Decision.Columns})
				}
			}
			if !matched {
				outcome = az.OutcomeIndeterminate
				layer.ACLState = "unavailable"
			}
			// Owner aggregation is unconditional: neither policy counts nor hidden
			// policy membership can be inferred from the number of public facts.
			layer.Decisions = append(layer.Decisions, az.LayerDecision{OperationID: op.ID, Result: outcome, Scope: az.ScopeOperation, RestrictionIDs: []string{}})
			if outcome == az.OutcomeDeny || outcome == az.OutcomeIndeterminate {
				code := az.CodeAccessDenied
				if outcome == az.OutcomeIndeterminate {
					code = az.CodeEvaluationFailed
				}
				result.Blockers = append(result.Blockers, az.Blocker{OperationID: op.ID, LayerID: layer.LayerID, Code: code, Scope: az.ScopeOperation})
			}
			layer.Result = reduceOutcome(layer.Result, outcome)
			result.Operations[i].Result = reduceOutcome(result.Operations[i].Result, outcome)
		}
		result.Layers = append(result.Layers, layer)
	}
	result.Coverage.Disclosure = az.DisclosureRedacted
	if !assessment.Complete {
		result.Coverage.Evaluation = az.EvaluationPartial
	}
	s.addActorCapabilities(r, db, &result)
	undecided := undecidedOperations(assessment)
	for i, op := range request.Operations {
		details := readable[op.ID]
		if !details {
			details = admitted[op.ID]
			for _, owner := range owners {
				if owner.Enabled && !s.ownerAllows(r, s.accessSource(db, owner.Kind), auth.CapAccessInspectProtected, op.Resource) {
					details = false
				}
			}
		}
		if hidden[op.ID] {
			details = false
		}
		// A successful write may be authorized without read permission. A dry run
		// does not get that exception: it cannot disclose a hidden row's existence.
		if !details && (request.Mode != az.ModeExecution || result.Operations[i].Result != az.OutcomeAllow) {
			redactPoint(&result, op.ID)
		}
		result.Result = reduceOutcome(result.Result, result.Operations[i].Result)
		if details && undecided[op.ID] {
			result.Coverage.Unevaluated = append(result.Coverage.Unevaluated, az.Unevaluated{OperationID: op.ID, Reason: "row_evidence_required"})
		}
	}
	result.Allowed = result.Result == az.OutcomeAllow && result.Coverage.Evaluation == az.EvaluationComplete
	return result
}

// undecidedOperations returns the ids of the operations the assessment could not
// decide: those with a policy decision that is neither allowed nor a definite
// denial (an indeterminate code, which includes the decision recorded when the
// protected session could not prepare the operation's evidence). Whether one
// operation can be decided is a fact about that operation alone, so the
// operations that still need row evidence are taken from these decisions and not
// from the assessment as a whole, which is incomplete when any operation is.
func undecidedOperations(assessment access.Assessment) map[string]bool {
	undecided := map[string]bool{}
	for _, pa := range assessment.Policies {
		if !pa.Decision.Allowed && pa.Decision.Code.IsIndeterminate() {
			undecided[pa.OperationID] = true
		}
	}
	return undecided
}

// admittedOperations returns the ids of the operations that the policy admits for
// the stored row to decide: a decision of it is allowed, or is denied for a reason
// that the row's data decides (a row condition, the image a write would leave, the
// fields of a rule), and none refuses the operation whatever the row (no rule
// admits the table for the action, or a rule refuses it outright). Any other
// operation is not admitted: the policy hides the table from the caller, or every
// decision it has is one the policy could not make (or it has none), and what the
// policy says then does not depend on the record.
func admittedOperations(assessment access.Assessment) map[string]bool {
	admitted, refused := map[string]bool{}, map[string]bool{}
	for _, pa := range assessment.Policies {
		switch {
		case pa.Decision.Allowed, decidedByTheRow(pa.Decision.Code):
			admitted[pa.OperationID] = true
		case !pa.Decision.Code.IsIndeterminate():
			refused[pa.OperationID] = true
		}
	}
	for id := range refused {
		delete(admitted, id)
	}
	return admitted
}

// decidedByTheRow reports whether a denial with code is one that the stored row's
// data decides.
func decidedByTheRow(code access.ReasonCode) bool {
	return code == access.CodeRowPredicateFailed || code == access.CodePostImageFailed || code == access.CodeColumnDenied
}

func redactPoint(result *az.Result, id string) {
	for i := range result.Operations {
		if result.Operations[i].ID == id {
			result.Operations[i].Result = az.OutcomeDeny
		}
	}
	blockers := result.Blockers[:0]
	for _, b := range result.Blockers {
		if b.OperationID != id {
			blockers = append(blockers, b)
		}
	}
	result.Blockers = append(blockers, az.Blocker{OperationID: id, Code: az.CodeAccessDenied, Scope: az.ScopeOperation})
	layers := result.Layers[:0]
	for _, layer := range result.Layers {
		decisions := layer.Decisions[:0]
		for _, decision := range layer.Decisions {
			if decision.OperationID != id {
				decisions = append(decisions, decision)
			}
		}
		if len(decisions) == 0 {
			continue
		}
		layer.Decisions = decisions
		layer.Result = az.OutcomeAllow
		for _, decision := range decisions {
			layer.Result = reduceOutcome(layer.Result, decision.Result)
		}
		layers = append(layers, layer)
	}
	result.Layers = layers
	result.Result = az.OutcomeDeny
	result.Allowed = false
	result.Coverage.Disclosure = az.DisclosureRedacted
	result.Coverage.Evaluation = az.EvaluationPartial
	remaining := result.Coverage.Unevaluated[:0]
	for _, fact := range result.Coverage.Unevaluated {
		if fact.OperationID != id {
			remaining = append(remaining, fact)
		}
	}
	result.Coverage.Unevaluated = append(remaining, az.Unevaluated{OperationID: id, Reason: "evidence_not_authorized"})
}

// inspectAccess answers an inspection of operations on single records. An
// operation on a table the database does not declare is answered as one on a
// declared table the policy hides: 200, redacted, with nothing that tells the two
// apart, and the adapter is never asked about it. Its field names and its
// support by the protected session are checked first (guardOperation), as they
// are for a declared table, so a refusal for them is the same for both. An
// operation on a declared table that the protected session cannot prepare is
// answered the same way (see inspectProtected).
func (s *Server) inspectAccess(w http.ResponseWriter, r *http.Request, db *core.Database, request api.Request, owners []core.PolicyLayer, requester access.Principal) {
	coordinator := db.Coordinator()
	if coordinator == nil {
		writeError(w, 422, "authorization_unsupported", "protected inspection unavailable")
		return
	}
	ops := make([]access.ProtectedOperation, 0, len(request.Operations))
	hidden := map[string]bool{}
	for _, op := range request.Operations {
		internal, err := guardOperation(db, op)
		if errors.Is(err, core.ErrNotFound) {
			hidden[op.ID] = true
			continue
		}
		if err != nil {
			s.refuseOperation(w, r, az.ModeInspect, op, err)
			return
		}
		ops = append(ops, internal)
	}
	result, err := s.inspectProtected(r, db, coordinator, request, owners, requester, ops, hidden)
	if err != nil {
		writeProtectedFailure(w, err)
		return
	}
	writeAuthorization(w, 200, result)
}

// inspectionCoordinator is the part of the coordinator that an inspection uses.
type inspectionCoordinator interface {
	WithinInspection(ctx context.Context, operations []access.ProtectedOperation, inspect func(access.InspectionSession) error) error
}

// inspected is what the protected sessions of an inspection say of its
// operations: the assessment, which of the records the caller may read, and the
// error with which the sessions admitted the operations (see
// access.InspectionSession.Assess).
type inspected struct {
	assessment   access.Assessment
	visible      map[string]bool
	admissionErr error
}

// inspectProtected assesses ops in one protected session and projects the result
// for the whole request, adding the ids of the operations in hidden (see
// projectInspection). A session that is refused with access.ErrAccessDenied before
// anything is assessed could not prepare the operations it was given: one of them
// is for a table whose shape it does not support, for instance, or together they
// hold more evidence than it accepts. It does not say which, so the operations
// are then assessed one by one (see inspectEach) and the answer is the one the
// session gives when it accepts them. The refusal itself is never the answer.
func (s *Server) inspectProtected(r *http.Request, db *core.Database, coordinator inspectionCoordinator, request api.Request, owners []core.PolicyLayer, requester access.Principal, ops []access.ProtectedOperation, hidden map[string]bool) (az.Result, error) {
	got := inspected{visible: map[string]bool{}}
	if len(ops) > 0 {
		var refused bool
		var err error
		got, refused, err = inspectOnce(r.Context(), coordinator, ops, requester)
		if refused {
			var unprepared []string
			got, unprepared, err = inspectEach(r.Context(), coordinator, ops, requester, hidden)
			s.logUnpreparedCollections(r, unprepared)
		}
		if err != nil {
			return az.Result{}, err
		}
	}
	result := s.projectInspection(r, db, request, owners, got.assessment, got.visible, hidden)
	if got.admissionErr != nil {
		// Never publish an allow after failed admission. A hidden or
		// missing point retains the same generic dry-run denial.
		for _, op := range request.Operations {
			if !got.visible[op.ID] {
				redactPoint(&result, op.ID)
			}
		}
		if result.Result != az.OutcomeDeny {
			return az.Result{}, got.admissionErr
		}
	}
	return result, nil
}

// inspectOnce assesses ops in one protected session. refused reports that the
// session was refused with access.ErrAccessDenied before anything was assessed
// while the request was still alive (see refusedWhileAlive).
func inspectOnce(ctx context.Context, coordinator inspectionCoordinator, ops []access.ProtectedOperation, requester access.Principal) (got inspected, refused bool, err error) {
	assessed := false
	err = coordinator.WithinInspection(ctx, ops, func(session access.InspectionSession) error {
		assessed = true
		var admissionErr error
		got.assessment, admissionErr = session.Assess(ctx)
		if admissionErr != nil && got.assessment.Outcome == "" {
			return admissionErr
		}
		got.admissionErr = admissionErr
		var visibleErr error
		got.visible, visibleErr = session.ReadVisibilityFor(ctx, requester)
		return visibleErr
	})
	if assessed {
		return got, false, err
	}
	refused, err = refusedWhileAlive(ctx, err)
	return got, refused, err
}

// refusedWhileAlive tells whether err is a refusal of a protected session, with
// access.ErrAccessDenied, that is answered as a table the caller may not see. That
// holds only while ctx has no error: a session that ran out of time, or whose
// request was canceled, says nothing of the table, so the failure it gets is the
// failure of the context, as any other session failure is answered.
func refusedWhileAlive(ctx context.Context, err error) (bool, error) {
	if !errors.Is(err, access.ErrAccessDenied) {
		return false, err
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return false, ctxErr
	}
	return true, err
}

// inspectEach assesses each of ops in a protected session of its own and puts the
// assessments together as one session would have given them: the policy decisions
// and restrictions in the order of ops, the outcome the worst of them, complete
// only when all are, and the records the caller may read. An operation that its
// own session refuses is not assessed: its id is added to hidden, which answers it
// as an operation on a table the database does not declare, and the name of its
// collection is returned. The admission error of the sessions is kept only when the
// outcome is an allow, as a session keeps it (a revision conflict, which a session
// reports after any other, last).
func inspectEach(ctx context.Context, coordinator inspectionCoordinator, ops []access.ProtectedOperation, requester access.Principal, hidden map[string]bool) (inspected, []string, error) {
	merged := inspected{assessment: access.Assessment{Outcome: access.AssessmentAllow, Complete: true}, visible: map[string]bool{}}
	var unprepared []string
	var admissionErr, conflict error
	prepared := 0
	for _, op := range ops {
		got, refused, err := inspectOnce(ctx, coordinator, []access.ProtectedOperation{op}, requester)
		if refused {
			hidden[op.ID()] = true
			unprepared = append(unprepared, op.Key().Collection())
			continue
		}
		if err != nil {
			return inspected{}, nil, err
		}
		prepared++
		merged.assessment.Outcome = reduceAssessment(merged.assessment.Outcome, got.assessment.Outcome)
		merged.assessment.Complete = merged.assessment.Complete && got.assessment.Complete
		merged.assessment.Policies = append(merged.assessment.Policies, got.assessment.Policies...)
		merged.assessment.Restrictions = append(merged.assessment.Restrictions, got.assessment.Restrictions...)
		for id, visible := range got.visible {
			merged.visible[id] = visible
		}
		switch {
		case errors.Is(got.admissionErr, access.ErrDataRevisionConflict):
			conflict = got.admissionErr
		case got.admissionErr != nil:
			admissionErr = got.admissionErr
		}
	}
	if prepared == 0 {
		merged.assessment = access.Assessment{}
	}
	if merged.assessment.Outcome == access.AssessmentAllow {
		merged.admissionErr = admissionErr
		if admissionErr == nil {
			merged.admissionErr = conflict
		}
	}
	return merged, unprepared, nil
}

// assessmentRanks orders the outcomes of an assessment from the most permissive.
var assessmentRanks = map[access.AssessmentOutcome]int{
	access.AssessmentAllow: 0, access.AssessmentConditional: 1, access.AssessmentIndeterminate: 2, access.AssessmentDeny: 3,
}

// reduceAssessment returns the worse of two outcomes of an assessment.
func reduceAssessment(a, b access.AssessmentOutcome) access.AssessmentOutcome {
	if assessmentRanks[b] > assessmentRanks[a] {
		return b
	}
	return a
}

// logUnpreparedCollections tells the operator which declared collections the
// protected session refused an operation on, so that a table that is answered as
// one the caller may not see can be looked into. It does not state why the session
// refused. One record is written for a request, with the method, the route path and
// the names of the collections, which have passed guardOperation; nothing of it is
// in the response.
func (s *Server) logUnpreparedCollections(r *http.Request, collections []string) {
	if len(collections) == 0 {
		return
	}
	slices.Sort(collections)
	s.logger.WarnContext(r.Context(), "protected session refused an operation",
		slog.String("method", r.Method),
		slog.String("path", r.URL.Path),
		slog.Any("collections", slices.Compact(collections)))
}

func writeProtectedFailure(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, access.ErrDataRevisionConflict):
		writeError(w, 409, "data_revision_conflict", "record changed; reload before retrying")
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		writeError(w, 503, "authorization_unavailable", "authorization deadline exceeded")
	case errors.Is(err, errProtectedUnsupported):
		writeError(w, 422, "authorization_unsupported", "protected operation unsupported")
	default:
		writeError(w, 503, "authorization_unavailable", "protected operation could not be evaluated")
	}
}

// executionCoordinator is the part of the coordinator that a protected update uses.
type executionCoordinator interface {
	WithinExecution(ctx context.Context, operations []access.ProtectedOperation, execute func(access.ExecutionSession) error) error
}

func (s *Server) handleProtectedUpdate(w http.ResponseWriter, r *http.Request, db *core.Database, key *record.Key) {
	w.Header().Set("Cache-Control", "no-store")
	coordinator := db.Coordinator()
	if coordinator == nil {
		writeError(w, 422, "authorization_unsupported", "protected execution unavailable")
		return
	}
	s.updateProtected(w, r, db, key, coordinator)
}

// updateProtected answers a protected PATCH with the session of coordinator.
func (s *Server) updateProtected(w http.ResponseWriter, r *http.Request, db *core.Database, key *record.Key, coordinator executionCoordinator) {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, api.MaxRequestBytes))
	if err != nil {
		writeError(w, 400, "bad_request", "invalid request size")
		return
	}
	var op api.Operation
	if err = api.DecodeStrict(data, &op); err == nil {
		err = op.Normalize(db.ID())
	}
	if err != nil || op.Action != "update" || key.Parent() != nil || op.Resource.Table != key.Collection() || op.Resource.RowID != key.ID {
		writeError(w, 400, "bad_request", "operation and request path must identify the same update")
		return
	}
	internal, err := guardOperation(db, op)
	if err != nil {
		s.refuseOperation(w, r, az.ModeExecution, op, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	r = r.WithContext(ctx)
	requester, _ := access.PrincipalFrom(ctx)
	owners := db.PolicyLayers(ctx)
	request := api.Request{APIVersion: az.APIVersion, Mode: az.ModeExecution, DiagnosticLevel: "references", Operations: []api.Operation{op}}
	var result az.Result
	var revision string
	var visible map[string]bool
	err = coordinator.WithinExecution(ctx, []access.ProtectedOperation{internal}, func(session access.ExecutionSession) error {
		assessment, admissionErr := session.Assess(ctx)
		if admissionErr != nil && assessment.Outcome == "" {
			return admissionErr
		}
		visible, err = session.ReadVisibilityFor(ctx, requester)
		if err != nil {
			return err
		}
		result = s.projectInspection(r, db, request, owners, assessment, visible, nil)
		if admissionErr != nil {
			return admissionErr
		}
		if !result.Allowed {
			return access.ErrAccessDenied
		}
		_, err = session.Execute(ctx)
		if err != nil {
			return err
		}
		revisions, err := session.Revisions(ctx)
		revision = revisions[op.ID]
		return err
	})
	if err != nil {
		if result.RequestID != "" {
			switch {
			case !visible[op.ID] || errors.Is(err, access.ErrProtectedResourceUnavailable):
				// A record the caller may not see is answered as a record of a table
				// the database does not declare is (see refuseOperation), by the same
				// function, so the two bodies are built from the same facts.
				writeUnavailableOperation(w, az.ModeExecution, op)
			case ctx.Err() != nil:
				// The request was canceled or ran past its deadline after the
				// assessment: nothing was found wrong with the candidate, so the
				// failure is the context's, not a refusal of the candidate.
				writeProtectedFailure(w, ctx.Err())
			case errors.Is(err, access.ErrDataRevisionConflict):
				writeError(w, 409, "data_revision_conflict", "record changed; reload before retrying")
			case !errors.Is(err, access.ErrAccessDenied):
				writeError(w, 422, "validation_failed", "candidate could not be accepted")
			default:
				writeJSON(w, 403, errorBody{Error: errorDetail{Code: "access_denied", RequestID: result.RequestID, Authorization: &result}})
			}
			return
		}
		var refused bool
		if refused, err = refusedWhileAlive(ctx, err); refused {
			// The protected session was refused before anything was assessed, while
			// the request was alive: it cannot prepare the operation (for a table
			// whose shape it does not support, for instance). The answer is the one
			// for a record the caller may not see, by the same function the
			// evidence route uses.
			s.logUnpreparedCollections(r, []string{op.Resource.Table})
			writeUnavailableOperation(w, az.ModeExecution, op)
			return
		}
		writeProtectedFailure(w, err)
		return
	}
	if revision == "" {
		writeError(w, 500, "internal", "missing execution receipt")
		return
	}
	writeJSON(w, 200, map[string]any{"authorization": result, "dataRevision": revision})
}

func (s *Server) handleAccessEvidence(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	db := s.db(w, r)
	if db == nil {
		return
	}
	coordinator := db.Coordinator()
	if coordinator == nil {
		writeError(w, 422, "authorization_unsupported", "protected evidence unavailable")
		return
	}
	var body struct {
		APIVersion     string      `json:"apiVersion"`
		Resource       az.Resource `json:"resource"`
		RequiredFields [][]string  `json:"requiredFields"`
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, api.MaxRequestBytes))
	if err != nil {
		writeError(w, 400, "bad_request", "invalid request size")
		return
	}
	if err = api.DecodeStrict(data, &body); err != nil || body.APIVersion != az.APIVersion || len(body.Resource.Columns) > 0 || len(body.RequiredFields) == 0 || len(body.RequiredFields) > 32 {
		writeError(w, 400, "bad_request", "invalid evidence request")
		return
	}
	op := api.Operation{ID: "evidence", Action: "get", Resource: body.Resource, ExecutionClass: az.ExecutionDTQL}
	op.Resource.Columns = body.RequiredFields
	if err = op.Normalize(db.ID()); err != nil || op.Resource.RowID == "" {
		writeError(w, 400, "bad_request", "invalid evidence resource or fields")
		return
	}
	if !s.authorize(w, r, db.ID(), auth.CapRecordsRead, op.Resource.Table) {
		return
	}
	internal, err := guardOperation(db, op)
	if err != nil {
		s.refuseOperation(w, r, az.ModeInspect, op, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	evidence, refused, err := evidenceOnce(ctx, coordinator, internal)
	if refused {
		s.logUnpreparedCollections(r, []string{op.Resource.Table})
	}
	if err != nil || len(evidence) != 1 || !evidence[0].Exists {
		if err != nil && !errors.Is(err, access.ErrAccessDenied) && !errors.Is(err, access.ErrProtectedResourceUnavailable) {
			writeProtectedFailure(w, err)
			return
		}
		// The answer for a record the caller may not see is the one for a table
		// the database does not declare (see refuseOperation).
		writeUnavailableOperation(w, az.ModeInspect, op)
		return
	}
	if evidence[0].DataRevision == "" || len(evidence[0].DataRevision) > 256 {
		writeError(w, 503, "authorization_unavailable", "record revision unavailable")
		return
	}
	fields := make([]map[string]any, 0, len(evidence[0].Fields))
	for _, field := range evidence[0].Fields {
		item := map[string]any{"path": field.Path, "state": "absent"}
		if field.Present {
			item["state"] = "present"
			item["value"] = field.Value
		}
		fields = append(fields, item)
	}
	body.Resource = op.Resource
	body.Resource.Columns = nil
	writeJSON(w, 200, map[string]any{"apiVersion": az.APIVersion, "resource": body.Resource, "exists": true, "dataRevision": evidence[0].DataRevision, "fields": fields})
}

// evidenceOnce asks one protected session for the evidence of op. refused reports
// that the session was refused with access.ErrAccessDenied before it was entered
// while the request was still alive (see refusedWhileAlive).
func evidenceOnce(ctx context.Context, coordinator inspectionCoordinator, op access.ProtectedOperation) (evidence []access.AuthorizedPointEvidence, refused bool, err error) {
	entered := false
	err = coordinator.WithinInspection(ctx, []access.ProtectedOperation{op}, func(session access.InspectionSession) error {
		entered = true
		var err error
		evidence, err = session.Evidence(ctx)
		return err
	})
	if entered {
		return evidence, false, err
	}
	refused, err = refusedWhileAlive(ctx, err)
	return evidence, refused, err
}

func writeUnavailablePoint(w http.ResponseWriter, db *core.Database, key *record.Key, action string) {
	resource := az.Resource{DatabaseID: db.ID(), Path: "/" + key.Collection() + "/" + fmt.Sprint(key.ID), Table: key.Collection(), RowID: fmt.Sprint(key.ID)}
	writeUnavailableOperation(w, az.ModeExecution, api.Operation{ID: "op1", Action: action, Resource: resource, ExecutionClass: az.ExecutionDTQL})
}

// writeUnavailableOperation answers 404 resource_unavailable for one operation:
// the redacted denial a hidden or missing record gets, which says nothing of
// why. The columns of the resource are not echoed.
func writeUnavailableOperation(w http.ResponseWriter, mode az.Mode, op api.Operation) {
	resource := op.Resource
	resource.Columns = nil
	result := newAuthorization(mode)
	result.Operations = append(result.Operations, az.OperationResult{ID: op.ID, RequestOperationID: op.ID, Action: op.Action, Resource: resource, Result: az.OutcomeDeny, RestrictionIDs: []string{}, AllOf: []string{}, ExecutionClass: op.ExecutionClass})
	redactPoint(&result, op.ID)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 404, errorBody{Error: errorDetail{Code: "resource_unavailable", RequestID: result.RequestID, Authorization: &result}})
}
