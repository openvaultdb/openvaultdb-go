package server

import (
	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/joinexec"
)

// This file is what the server says about relational queries before a client sends
// one: the query block of /.well-known/openvaultdb and the joins and aggregation
// capabilities of a database. Every value is read from the code that enforces it, so a
// client is never told a database joins and then refused.

// queryFormat is the document format of the DTQL endpoints.
const queryFormat = "dtql-yaml+json"

// advertisesJoins reports whether a client may send a relational document that
// names db: a join, a grouping, an aggregate, a subquery or a source that names its
// database. It is true exactly when the relational handler takes the database,
// because it asks the handler's own rule (relationalRefusal), so the value cannot
// say a database joins and then meet a refusal.
func (s *Server) advertisesJoins(db *core.Database) bool {
	return s.relationalRefusal(db.ID(), db) == nil
}

// queryProfile is the query block of the discovery document. It holds nothing that
// belongs to one database, so it is the same in both auth modes. The limits are
// the ones the server is configured with and the bounds the code enforces: it
// states what a document may ask for, not the capacity of the server.
func (s *Server) queryProfile() map[string]any {
	limits, bounds := s.queryLimits, core.RelationalBounds()
	return map[string]any{
		"endpoint": crossDatabaseDTQLPath,
		"format":   queryFormat,
		"features": map[string]any{
			"joins":              []string{"inner", "left"},
			"groupBy":            true,
			"having":             true,
			"aggregates":         core.AggregateFunctions(),
			"subqueries":         true,
			"crossDatabase":      true,
			"externalSources":    false,
			"windowFunctions":    false,
			"protectedDatabases": false,
			"fieldNames":         "plain",
		},
		"limits": map[string]any{
			"timeoutMs":            limits.Timeout.Milliseconds(),
			"maxSourceRows":        limits.MaxSourceRows,
			"maxSourceBytes":       limits.MaxSourceBytes,
			"maxResultRows":        joinexec.MaxResultRows,
			"maxResultBytes":       joinexec.MaxResultBytes,
			"maxSources":           bounds.MaxSources,
			"maxSubqueryDepth":     bounds.MaxSubqueryDepth,
			"maxLimit":             bounds.MaxLimit,
			"maxOffset":            bounds.MaxOffset,
			"maxInMemoryJoinRows":  joinexec.MaxInMemoryJoinRows,
			"maxInMemoryJoinBytes": joinexec.MaxInMemoryJoinBytes,
			"maxGroups":            joinexec.MaxInMemoryGroups,
		},
		"joinEngines": s.joinEngines(),
	}
}
