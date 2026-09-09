package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/dal-go/dalgo/access"
	az "github.com/dal-go/dalgo/dtql/authorization"
	"github.com/dal-go/record"
	"github.com/openvaultdb/openvaultdb-go/pkg/auth"
	api "github.com/openvaultdb/openvaultdb-go/pkg/authorizationapi"
	"github.com/openvaultdb/openvaultdb-go/pkg/core"
)

// OwnerAuthorization is a trusted deployment binding to each owner's existing
// authorization authority. A grant at OpenVaultDB never grants administration
// or protected-row inspection at a lower owner.
type OwnerAuthorization func(context.Context, *auth.Principal, az.Source, string, az.Resource) bool

func WithOwnerAuthorization(authorize OwnerAuthorization) Option {
	return func(s *Server) { s.accessAuthorization = authorize }
}

// WithAccessInstanceID supplies a stable deployment identity, shared by all
// mounts on this server. It must come from persistent host configuration.
func WithAccessInstanceID(id string) Option {
	return func(s *Server) {
		if id == "" || len(id) > 128 {
			panic("invalid access instance id")
		}
		s.accessInstance = id
	}
}

func (s *Server) accessSource(db *core.Database, kind string) az.Source {
	return az.Source{OwnerID: kind + ":" + s.accessInstance, Provider: kind, DatabaseID: db.ID(), Kind: kind}
}
func sourceLayerID(source az.Source) string { return source.OwnerID + ":" + source.DatabaseID }

func (s *Server) ownerAllows(r *http.Request, source az.Source, capability string, resource az.Resource) bool {
	if s.accessAuthorization != nil {
		return s.accessAuthorization(r.Context(), auth.FromRequest(r), source, capability, resource)
	}
	// Protected facts require an explicit key-scoped owner binding, including
	// for the bootstrap administrator. Policy authority does not imply reads.
	if source.Kind != "openvaultdb" || capability == auth.CapAccessInspectProtected {
		return false
	}
	if s.authCfg == nil {
		return true
	}
	return auth.FromRequest(r).Allows(source.DatabaseID, capability, resource.Table)
}

func (s *Server) handleAccessEvaluate(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	db := s.db(w, r)
	if db == nil {
		return
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, api.MaxRequestBytes))
	if err != nil {
		writeError(w, 400, "bad_request", "invalid request body size")
		return
	}
	request, err := api.Parse(data, db.ID())
	if err != nil {
		writeError(w, 400, "bad_request", err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	r = r.WithContext(ctx)
	requester, _ := access.PrincipalFrom(ctx)
	layers := db.PolicyLayers(ctx)
	if request.Subject != nil || request.Simulation != nil {
		for _, layer := range layers {
			for _, op := range request.Operations {
				source := s.accessSource(db, layer.Kind)
				if request.Subject != nil && !s.ownerAllows(r, source, auth.CapAccessExplain, op.Resource) || request.Simulation != nil && !s.ownerAllows(r, source, auth.CapAccessSimulate, op.Resource) {
					writeError(w, 403, "forbidden", "explanation scope is not authorized at every owner")
					return
				}
			}
		}
		principal, _ := access.PrincipalFrom(ctx)
		if request.Subject != nil {
			if s.explainResolver == nil {
				writeError(w, 503, "authorization_unavailable", "identity resolution unavailable")
				return
			}
			membership, err := s.explainResolver(ctx, *request.Subject)
			if err != nil {
				writeError(w, 503, "authorization_unavailable", "identity resolution unavailable")
				return
			}
			principal.Subject = request.Subject
			principal.ID = nil
			principal.Roles = membership.Roles
			principal.Groups = membership.Groups
			principal.MembershipRevision = membership.Revision
		}
		if request.Simulation != nil {
			if len(request.Simulation.Attributes) > 0 {
				writeError(w, 422, "authorization_unsupported", "attribute simulation is not supported")
				return
			}
			principal.Roles = request.Simulation.Roles
			principal.Groups = request.Simulation.Groups
		}
		ctx = access.WithPrincipal(ctx, principal)
		r = r.WithContext(ctx)
	}
	if request.Mode == az.ModeSample {
		s.sampleAccess(w, r, db, request, layers, requester)
		return
	}
	if request.Mode == az.ModeInspect {
		s.inspectAccess(w, r, db, request, layers, requester)
		return
	}
	if request.Mode != az.ModePlan {
		writeError(w, 422, "authorization_unsupported", "row inspection is not available for this mounted profile yet")
		return
	}
	result := s.assessPlan(r, db, request, layers)
	writeAuthorization(w, http.StatusOK, result)
}

func newAuthorization(mode az.Mode) az.Result {
	id, err := auth.NewGrantID()
	if err != nil {
		panic("request identity entropy unavailable")
	}
	return az.Result{APIVersion: az.APIVersion, RequestID: id, Mode: mode, Scope: az.ScopeRequest, Result: az.OutcomeAllow,
		Operations: []az.OperationResult{}, Layers: []az.Layer{}, Blockers: []az.Blocker{}, Restrictions: []az.Restriction{},
		Coverage: az.Coverage{Evaluation: az.EvaluationComplete, Disclosure: az.DisclosureFull, Unevaluated: []az.Unevaluated{}}}
}

func internalRequest(op api.Operation) (access.Request, error) {
	operations := map[string]access.Operations{"get": access.Get, "exists": access.Exists, "query": access.Query, "insert": access.Insert, "set": access.Set, "update": access.Update, "delete": access.Delete, "truncate": access.Truncate}
	resource := access.CollectionResourceFor(nil, op.Resource.Table)
	if op.Resource.RowID != "" {
		resource = access.RecordResourceForKey(record.NewKeyWithID(op.Resource.Table, op.Resource.RowID))
	}
	target := &access.ExecutionTarget{Class: access.ExecutionClass(op.ExecutionClass)}
	if op.Callable != nil {
		target.Namespace = op.Callable.Namespace
		target.Name = op.Callable.Name
	}
	request := access.Request{Operation: operations[op.Action], Resources: []access.Resource{resource}, Execution: target, Columns: op.Resource.Columns}
	if op.Query != nil {
		query, err := op.Query.Parse(op.Resource.Table)
		if err != nil {
			return request, err
		}
		request.Query = query
	}
	return request, nil
}

func (s *Server) assessPlan(r *http.Request, db *core.Database, request api.Request, layers []core.PolicyLayer) az.Result {
	result := newAuthorization(az.ModePlan)
	result.Hypothetical = request.Simulation != nil
	for _, op := range request.Operations {
		result.Operations = append(result.Operations, az.OperationResult{ID: op.ID, RequestOperationID: op.ID, Action: op.Action, Resource: op.Resource, Result: az.OutcomeAllow, RestrictionIDs: []string{}, AllOf: []string{}, ExecutionClass: op.ExecutionClass, Callable: op.Callable})
	}
	for _, owner := range layers {
		source := s.accessSource(db, owner.Kind)
		layerID := sourceLayerID(source)
		layer := az.Layer{LayerID: layerID, Source: source, ACLState: "disabled", Result: az.OutcomeAllow, Decisions: []az.LayerDecision{}}
		if owner.Enabled {
			layer.ACLState = "enabled"
		}
		for i, op := range request.Operations {
			if !owner.Enabled {
				layer.Decisions = append(layer.Decisions, az.LayerDecision{OperationID: op.ID, Result: az.OutcomeAllow, Scope: az.ScopeOperation, RestrictionIDs: []string{}})
				continue
			}
			internal, err := internalRequest(op)
			if err != nil || owner.Err != nil || r.Context().Err() != nil || len(owner.Policies) == 0 {
				layer.ACLState = "unavailable"
				layer.Result = reduceOutcome(layer.Result, az.OutcomeIndeterminate)
				result.Operations[i].Result = reduceOutcome(result.Operations[i].Result, az.OutcomeIndeterminate)
				result.Coverage.Evaluation = az.EvaluationPartial
				result.Coverage.Unevaluated = append(result.Coverage.Unevaluated, az.Unevaluated{OperationID: op.ID, LayerID: layerID, Reason: "source_unavailable"})
				result.Blockers = append(result.Blockers, az.Blocker{OperationID: op.ID, LayerID: layerID, Code: az.CodeSourceUnavailable, Scope: az.ScopeConfiguration})
				continue
			}
			assessment := access.AssessPlan(r.Context(), internal, owner.Policies)
			outcome := az.Outcome(assessment.Outcome)
			layer.Result = reduceOutcome(layer.Result, outcome)
			result.Operations[i].Result = reduceOutcome(result.Operations[i].Result, outcome)
			if !assessment.Complete {
				result.Coverage.Evaluation = az.EvaluationPartial
				result.Coverage.Unevaluated = append(result.Coverage.Unevaluated, az.Unevaluated{OperationID: op.ID, LayerID: layerID, Reason: "unsupported"})
			}
			diagnostics := request.DiagnosticLevel != "ordinary" && s.ownerAllows(r, source, auth.CapAccessDiagnostics, op.Resource)
			admin := s.ownerAllows(r, source, auth.CapPoliciesAdmin, op.Resource)
			// Hidden policies are coalesced to one owner-level fact regardless of
			// how many policies contributed. No hidden IDs or generation tokens.
			hidden := false
			for _, pa := range assessment.Policies {
				decision := pa.Decision
				individual := az.OutcomeAllow
				if !decision.Allowed {
					individual = az.OutcomeDeny
					if decision.Code.IsIndeterminate() {
						individual = az.OutcomeIndeterminate
					}
				} else if len(decision.Residuals) > 0 || len(decision.Writes) > 0 {
					individual = az.OutcomeConditional
				}
				visible := diagnostics && (pa.Policy.Visibility == access.PolicyVisibilityPublic || admin) && pa.Policy.Revision != ""
				if !visible {
					hidden = true
					continue
				}
				ref := &az.PolicyRef{OwnerID: source.OwnerID, DatabaseID: db.ID(), PolicyID: pa.Policy.ID, Revision: pa.Policy.Revision}
				scope := az.Scope(decision.Scope)
				if scope == "" || scope == az.ScopeRequest {
					scope = az.ScopeOperation
				}
				layer.Decisions = append(layer.Decisions, az.LayerDecision{OperationID: op.ID, Result: individual, PolicyRef: ref, Scope: scope, RestrictionIDs: []string{}})
				if !decision.Allowed {
					result.Blockers = append(result.Blockers, az.Blocker{OperationID: op.ID, LayerID: layerID, PolicyRef: ref, Code: az.ReasonCode(decision.Code), Scope: scope, Slot: string(decision.Slot)})
				}
			}
			if hidden || !admin {
				result.Coverage.Disclosure = az.DisclosureRedacted
				layer.Decisions = append(layer.Decisions, az.LayerDecision{OperationID: op.ID, Result: outcome, Scope: az.ScopeOperation, RestrictionIDs: []string{}})
				if outcome == az.OutcomeDeny || outcome == az.OutcomeIndeterminate {
					code := az.CodeAccessDenied
					if outcome == az.OutcomeIndeterminate {
						code = az.CodeEvaluationFailed
					}
					result.Blockers = append(result.Blockers, az.Blocker{OperationID: op.ID, LayerID: layerID, Code: code, Scope: az.ScopeOperation})
				}
			}
			s.appendPlanRestrictions(r, request.DiagnosticLevel, source, op, &result, &layer, i, assessment, admin)

		}
		result.Layers = append(result.Layers, layer)
	}
	for _, op := range result.Operations {
		result.Result = reduceOutcome(result.Result, op.Result)
	}
	s.addActorCapabilities(r, db, &result)
	result.Allowed = result.Result == az.OutcomeAllow && result.Coverage.Evaluation == az.EvaluationComplete
	return result
}

func capabilityForAction(action string) string {
	switch action {
	case "get", "exists", "query":
		return auth.CapRecordsRead
	case "insert", "set", "update":
		return auth.CapRecordsWrite
	case "delete", "truncate":
		return auth.CapRecordsDelete
	}
	return ""
}

func (s *Server) addActorCapabilities(r *http.Request, db *core.Database, result *az.Result) {
	if s.authCfg == nil {
		return
	}
	layerID := sourceLayerID(s.accessSource(db, "openvaultdb"))
	for i := range result.Operations {
		op := &result.Operations[i]
		if auth.FromRequest(r).Allows(db.ID(), capabilityForAction(op.Action), op.Resource.Table) {
			continue
		}
		op.Result = az.OutcomeDeny
		result.Result = az.OutcomeDeny
		result.Allowed = false
		result.Blockers = append(result.Blockers, az.Blocker{OperationID: op.ID, Code: az.CodeCapabilityDenied, Scope: az.ScopeOperation, LayerID: layerID})
		for j := range result.Layers {
			if result.Layers[j].LayerID == layerID {
				result.Layers[j].Result = az.OutcomeDeny
				result.Layers[j].Decisions = append(result.Layers[j].Decisions, az.LayerDecision{OperationID: op.ID, Result: az.OutcomeDeny, Scope: az.ScopeOperation, RestrictionIDs: []string{}})
			}
		}
	}
}

func reduceOutcome(a, b az.Outcome) az.Outcome {
	rank := map[az.Outcome]int{az.OutcomeAllow: 0, az.OutcomeConditional: 1, az.OutcomeIndeterminate: 2, az.OutcomeDeny: 3}
	if rank[b] > rank[a] {
		return b
	}
	return a
}

func (s *Server) appendPlanRestrictions(r *http.Request, level string, source az.Source, op api.Operation, result *az.Result, layer *az.Layer, index int, assessment access.Assessment, admin bool) {
	if len(assessment.Restrictions) == 0 {
		return
	}
	appendRestriction := func(restriction az.Restriction) {
		restriction.ID = fmt.Sprintf("r%d", len(result.Restrictions)+1)
		restriction.OperationID = op.ID
		restriction.LayerID = layer.LayerID
		result.Restrictions = append(result.Restrictions, restriction)
		result.Operations[index].RestrictionIDs = append(result.Operations[index].RestrictionIDs, restriction.ID)
		result.Operations[index].AllOf = append(result.Operations[index].AllOf, restriction.ID)
		for j := range layer.Decisions {
			if layer.Decisions[j].OperationID == op.ID {
				layer.Decisions[j].RestrictionIDs = append(layer.Decisions[j].RestrictionIDs, restriction.ID)
			}
		}
	}
	// One owner aggregate is present for every constrained owner, independent
	// of the existence or number of private policies. It also represents the
	// ordered write alternatives without incorrectly flattening them to a union.
	appendRestriction(az.Restriction{Representation: "reference", Kind: "opaque", OmissionReason: "not_authorized"})
	result.Coverage.Disclosure = az.DisclosureRedacted
	if level != "policy" || !s.ownerAllows(r, source, auth.CapPoliciesRead, op.Resource) {
		return
	}
	for _, internal := range assessment.Restrictions {
		if internal.PolicyIndex < 0 || internal.PolicyIndex >= len(assessment.Policies) {
			continue
		}
		metadata := assessment.Policies[internal.PolicyIndex].Policy
		if metadata.Visibility != access.PolicyVisibilityPublic && !admin {
			continue
		}
		// Only source expressions are eligible: these preserve parameter names,
		// and never contain values resolved from current identity/context.
		expression, err := internal.DocumentCondition()
		if err != nil || expression == nil {
			continue
		}
		restriction := az.Restriction{Kind: "row_filter", Representation: "expression", Expression: expression}
		if internal.Slot == access.DecisionSlotCheck {
			restriction.Kind = "post_image_check"
		}
		if data, err := json.Marshal(expression); err != nil || len(data) > 16<<10 || conditionNodes(expression) > 256 {
			restriction.Representation = "reference"
			restriction.Expression = nil
			restriction.OmissionReason = "too_large"
		}
		if metadata.Revision != "" && s.ownerAllows(r, source, auth.CapAccessDiagnostics, op.Resource) {
			restriction.PolicyRef = &az.PolicyRef{OwnerID: source.OwnerID, DatabaseID: source.DatabaseID, PolicyID: metadata.ID, Revision: metadata.Revision}
		}
		appendRestriction(restriction)
	}
}

func conditionNodes(condition *access.DocumentCondition) int {
	if condition == nil {
		return 0
	}
	count := 1
	if condition.Left != nil {
		count++
	}
	if condition.Right != nil {
		count++
	}
	for i := range condition.And {
		count += conditionNodes(&condition.And[i])
	}
	for i := range condition.Or {
		count += conditionNodes(&condition.Or[i])
	}
	return count
}

func writeAuthorization(w http.ResponseWriter, status int, result az.Result) {
	result = boundAuthorization(result)
	data, err := az.MarshalResult(result)
	if err != nil {
		writeError(w, 500, "internal", "authorization result could not be encoded")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

func boundAuthorization(result az.Result) az.Result {
	data, err := json.Marshal(result)
	if err == nil && len(data) <= 256<<10 && len(result.Blockers) <= 1000 && len(result.Restrictions) <= 1000 {
		return result
	}
	// Overflow is an explicitly incomplete diagnostic result, never a partial
	// permission grant. Keep request operation IDs while coalescing all facts.
	result.Allowed = false
	result.Result = az.OutcomeIndeterminate
	result.Layers = []az.Layer{}
	result.Blockers = []az.Blocker{}
	result.Restrictions = []az.Restriction{}
	result.Coverage = az.Coverage{Evaluation: az.EvaluationPartial, Disclosure: az.DisclosureRedacted, Truncated: true, Unevaluated: []az.Unevaluated{}}
	for i := range result.Operations {
		op := &result.Operations[i]
		op.Resource = az.Resource{DatabaseID: op.Resource.DatabaseID, Path: "/"}
		op.RestrictionIDs = []string{}
		op.AllOf = []string{}
		if op.Result == az.OutcomeDeny {
			result.Result = az.OutcomeDeny
			result.Blockers = append(result.Blockers, az.Blocker{OperationID: op.ID, Code: az.CodeAccessDenied, Scope: az.ScopeOperation})
		} else {
			op.Result = az.OutcomeIndeterminate
		}
		result.Coverage.Unevaluated = append(result.Coverage.Unevaluated, az.Unevaluated{OperationID: op.ID, Reason: "budget_exceeded"})
	}
	return result
}

// A denied real query can safely continue static assessment at other owners.
// It cannot execute another data query to discover hidden row-level blockers.
func (s *Server) writeQueryAuthorizationError(w http.ResponseWriter, r *http.Request, db *core.Database, op api.Operation, executionErr error) {
	request := api.Request{APIVersion: az.APIVersion, Mode: az.ModePlan, DiagnosticLevel: "ordinary", Operations: []api.Operation{op}}
	result := s.assessPlan(r, db, request, db.PolicyLayers(r.Context()))
	result.Mode = az.ModeExecution
	result.Allowed = false
	var unavailable *access.PolicyProviderError
	if errors.As(executionErr, &unavailable) {
		if result.Result != az.OutcomeDeny {
			result.Result = az.OutcomeIndeterminate
			result.Operations[0].Result = az.OutcomeIndeterminate
		}
	} else {
		definitive := false
		decisions := access.DecisionsFromError(executionErr)
		for _, decision := range decisions {
			if !decision.Allowed && !decision.Code.IsIndeterminate() {
				definitive = true
			}
		}
		if definitive || len(decisions) == 0 {
			result.Result = az.OutcomeDeny
			result.Operations[0].Result = az.OutcomeDeny
		} else if result.Result != az.OutcomeDeny {
			result.Result = az.OutcomeIndeterminate
			result.Operations[0].Result = az.OutcomeIndeterminate
		}
	}
	// These facts were reconstructed after enforcement: do not claim that
	// the error's generation and the metadata assessment shared a snapshot.
	result.Coverage.Evaluation = az.EvaluationPartial
	result.Coverage.Disclosure = az.DisclosureRedacted
	result.Coverage.Unevaluated = append(result.Coverage.Unevaluated, az.Unevaluated{OperationID: op.ID, Reason: "row_evidence_required"})
	if len(result.Blockers) == 0 {
		code := az.CodeAccessDenied
		if result.Result == az.OutcomeIndeterminate {
			code = az.CodeEvaluationFailed
		}
		result.Blockers = append(result.Blockers, az.Blocker{OperationID: op.ID, Code: code, Scope: az.ScopeOperation})
	}
	result = boundAuthorization(result)
	if _, err := az.MarshalResult(result); err != nil {
		writeError(w, 500, "internal", "authorization result could not be encoded")
		return
	}
	status, code := http.StatusForbidden, "access_denied"
	if result.Result == az.OutcomeIndeterminate {
		status, code = http.StatusServiceUnavailable, "authorization_unavailable"
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, status, errorBody{Error: errorDetail{Code: code, RequestID: result.RequestID, Authorization: &result}})
}
