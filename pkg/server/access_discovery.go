package server

import (
	"net/http"
	"sort"

	"github.com/dal-go/dalgo/access"
	az "github.com/dal-go/dalgo/dtql/authorization"
	"github.com/openvaultdb/openvaultdb-go/pkg/auth"
)

type accessCapabilities struct {
	Discover         bool `json:"discover"`
	List             bool `json:"list"`
	Read             bool `json:"read"`
	Create           bool `json:"create"`
	Update           bool `json:"update"`
	Delete           bool `json:"delete"`
	Explain          bool `json:"explain"`
	Simulate         bool `json:"simulate"`
	Diagnostics      bool `json:"diagnostics"`
	PolicyAdmin      bool `json:"policyAdmin"`
	InspectProtected bool `json:"inspectProtected"`
	SchemaRead       bool `json:"schemaRead"`
}

func (s *Server) accessCapabilities(r *http.Request, source az.Source) accessCapabilities {
	resource := az.Resource{DatabaseID: source.DatabaseID, Path: "/"}
	can := func(action string) bool { return s.ownerAllows(r, source, action, resource) }
	return accessCapabilities{Discover: can(auth.CapPoliciesDiscover), List: can(auth.CapPoliciesList), Explain: can(auth.CapAccessExplain), Simulate: can(auth.CapAccessSimulate), Diagnostics: can(auth.CapAccessDiagnostics), PolicyAdmin: can(auth.CapPoliciesAdmin)}
}

func (s *Server) handleAccessLayers(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	db := s.db(w, r)
	if db == nil {
		return
	}
	items := []map[string]any{}
	for _, layer := range db.PolicyLayers(r.Context()) {
		source := s.accessSource(db, layer.Kind)
		capabilities := s.accessCapabilities(r, source)
		if !capabilities.Discover {
			continue
		}
		state := "disabled"
		if layer.Enabled {
			state = "enabled"
		}
		if layer.Err != nil {
			state = "unavailable"
		}
		modes := []string{"plan"}
		protected := db.Coordinator() != nil
		if protected {
			modes = append(modes, "inspect", "sample")
		}
		items = append(items, map[string]any{
			"layerId": sourceLayerID(source), "source": source, "aclState": state, "modes": modes, "formats": []string{}, "capabilities": capabilities, "required": true, "opaque": !capabilities.List,
			"executionClasses": []string{"dtql"},
			"enforcement":      map[string]bool{"structuredQuery": true, "fieldReferences": true, "transactionalEvidence": protected, "wholeRecordCAS": protected, "remoteEvidence": protected, "atomicBatch": false},
			"limits":           map[string]int{"queryRows": 1000, "queryOffset": 10000, "bufferBytes": 8 << 20, "executionMilliseconds": 10000, "dryRunMilliseconds": 2000, "sampleRows": 100, "operations": 100, "columns": 32, "responseBytes": 256 << 10, "maxMaskStages": 1024, "maxMaskPatterns": 4096, "maxMaskPatternBytes": 128},
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// Only policy metadata is exposed. Policy administration remains owner files;
// no viewer/editor or HTTP mutation route is part of this MVP.
func (s *Server) handleAccessPolicies(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	db := s.db(w, r)
	if db == nil {
		return
	}
	selected := r.URL.Query().Get("layerId")
	items := []map[string]any{}
	for _, layer := range db.PolicyLayers(r.Context()) {
		source := s.accessSource(db, layer.Kind)
		if selected != sourceLayerID(source) {
			continue
		}
		caps := s.accessCapabilities(r, source)
		if !caps.List {
			continue
		}
		if layer.Err != nil {
			writeError(w, 503, "authorization_unavailable", "policy source unavailable")
			return
		}
		for _, policy := range layer.Policies {
			metadata := access.DescribePolicy(policy)
			if metadata.Visibility != access.PolicyVisibilityPublic && !caps.PolicyAdmin {
				continue
			}
			// Custom providers without versioned metadata remain represented by
			// their mandatory layer. Never invent a document revision or expose
			// their internal Go type, filename, or source locator.
			if metadata.Revision == "" {
				continue
			}
			ref := az.PolicyRef{OwnerID: source.OwnerID, DatabaseID: source.DatabaseID, PolicyID: metadata.ID, Revision: metadata.Revision}
			items = append(items, map[string]any{"source": source, "policyRef": ref, "visibility": metadata.Visibility, "editable": false, "serializable": false, "capabilities": caps})
		}
	}
	sort.Slice(items, func(i, j int) bool {
		return items[i]["policyRef"].(az.PolicyRef).PolicyID < items[j]["policyRef"].(az.PolicyRef).PolicyID
	})
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}
