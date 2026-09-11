package server

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/dal-go/dalgo/access"
	"github.com/dal-go/dalgo/dal"
	az "github.com/dal-go/dalgo/dtql/authorization"
	"github.com/openvaultdb/openvaultdb-go/pkg/auth"
	api "github.com/openvaultdb/openvaultdb-go/pkg/authorizationapi"
	"github.com/openvaultdb/openvaultdb-go/pkg/core"
)

func (s *Server) sampleAccess(w http.ResponseWriter, r *http.Request, db *core.Database, request api.Request, owners []core.PolicyLayer, requester access.Principal) {
	if db.Coordinator() == nil {
		writeError(w, 422, "authorization_unsupported", "protected sampling unavailable")
		return
	}
	template := request.Operations[0]
	if !s.authorize(w, r, db.ID(), auth.CapRecordsRead, template.Resource.Table) {
		return
	}
	query, err := request.Sample.Query.Parse(template.Resource.Table)
	if err != nil {
		writeError(w, 400, "bad_request", "invalid sample query")
		return
	}
	rows, order, selectionErr := db.SelectAccessSample(r.Context(), query, request.Sample.Limit, requester)
	result := newAuthorization(az.ModeSample)
	result.Scope = az.ScopeSample
	result.Result = az.OutcomeConditional
	result.Hypothetical = request.Simulation != nil
	key := "id"
	if db.Manifest.Storage.Engine == "ingitdb" {
		key = "$id"
	}
	summary := &az.Sample{RequestedLimit: request.Sample.Limit, Selection: "readable_candidates", TemplateOperationID: template.ID, SelectionResource: az.Resource{DatabaseID: db.ID(), Path: "/" + template.Resource.Table, Table: template.Resource.Table}, Order: []az.SampleOrder{{Field: []string{key}, Direction: "asc"}}}
	result.Sample = summary
	if len(order) > 0 {
		summary.Order = []az.SampleOrder{}
		for _, item := range order {
			field := item.Expression().(dal.FieldRef)
			direction := "asc"
			if item.Descending() {
				direction = "desc"
			}
			summary.Order = append(summary.Order, az.SampleOrder{Field: strings.Split(field.Name(), "."), Direction: direction})
		}
	}
	if selectionErr != nil {
		result.Result = az.OutcomeIndeterminate
		code := az.CodeEvaluationFailed
		reason := "source_unavailable"
		if errors.Is(selectionErr, access.ErrAccessDenied) {
			decisions := access.DecisionsFromError(selectionErr)
			definitive := len(decisions) == 0
			for _, decision := range decisions {
				if !decision.Allowed && !decision.Code.IsIndeterminate() {
					definitive = true
				}
			}
			if definitive {
				result.Result = az.OutcomeDeny
				code = az.CodeAccessDenied
				reason = "evidence_not_authorized"
			} else {
				code = az.CodeEnforcementUnsupported
				reason = "unsupported"
			}
		}
		result.Coverage.Evaluation = az.EvaluationPartial
		result.Coverage.Disclosure = az.DisclosureRedacted
		result.Coverage.Unevaluated = append(result.Coverage.Unevaluated, az.Unevaluated{OperationID: template.ID, Reason: reason})
		result.Blockers = append(result.Blockers, az.Blocker{OperationID: template.ID, Code: code, Scope: az.ScopeTable})
		writeAuthorization(w, 200, result)
		return
	}
	if len(rows) == 0 {
		writeAuthorization(w, 200, result)
		return
	}
	selected := request
	selected.Mode = az.ModeInspect
	selected.Sample = nil
	selected.Operations = nil
	ops := make([]access.ProtectedOperation, 0, len(rows))
	nextID := 1
	for _, row := range rows {
		op := template
		for fmt.Sprintf("s%d", nextID) == template.ID {
			nextID++
		}
		op.ID = fmt.Sprintf("s%d", nextID)
		nextID++
		op.Resource.RowID = fmt.Sprint(row.Key.ID)
		op.Resource.Path = "/" + op.Resource.Table + "/" + op.Resource.RowID
		if err := op.Normalize(db.ID()); err != nil {
			writeError(w, 422, "authorization_unsupported", "sample key cannot be addressed")
			return
		}
		normalized, err := protectedOperation(op)
		if err != nil {
			writeError(w, 422, "authorization_unsupported", "sample operation unsupported")
			return
		}
		selected.Operations = append(selected.Operations, op)
		ops = append(ops, normalized)
	}
	var visible map[string]bool
	err = db.Coordinator().WithinInspection(r.Context(), ops, func(session access.InspectionSession) error {
		assessment, admissionErr := session.Assess(r.Context())
		if admissionErr != nil && assessment.Outcome == "" {
			return admissionErr
		}
		var err error
		visible, err = session.ReadVisibilityFor(r.Context(), requester)
		if err != nil {
			return err
		}
		// A selection whose visibility changed cannot reveal those keys, even to
		// inspect-protected administrators. Abort its row diagnostics as a unit.
		for _, op := range selected.Operations {
			if !visible[op.ID] {
				return access.ErrAccessDenied
			}
		}
		result = s.projectInspection(r, db, selected, owners, assessment, visible)
		if admissionErr != nil {
			return admissionErr
		}
		return nil
	})
	if err != nil {
		result = newAuthorization(az.ModeSample)
		result.Result = az.OutcomeIndeterminate
		result.Coverage.Evaluation = az.EvaluationPartial
		result.Coverage.Disclosure = az.DisclosureRedacted
		result.Coverage.Unevaluated = append(result.Coverage.Unevaluated, az.Unevaluated{OperationID: template.ID, Reason: "row_evidence_required"})
	} else {
		summary.EvaluatedCount = len(result.Operations)
		for i := range result.Operations {
			result.Operations[i].RequestOperationID = template.ID
		}
		if result.Result == az.OutcomeAllow {
			result.Result = az.OutcomeConditional
		}
	}
	result.Mode = az.ModeSample
	result.Scope = az.ScopeSample
	result.Sample = summary
	result.Allowed = false
	result.Hypothetical = request.Simulation != nil
	writeAuthorization(w, 200, result)
}
