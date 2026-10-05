package server

import (
	"time"

	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/joinexec"
)

// This file is what the server says about relational queries before a client sends
// one: the query block of /.well-known/openvaultdb and the joins and aggregation
// flags of a database. Every value is read from the code that enforces it, so a
// client is never told a database joins and then refused.

// queryFormat is the document format of the DTQL endpoints.
const queryFormat = "dtql-yaml+json"

// profileAggregates are the aggregate functions of the relational profile, as a
// document writes them: the ones every route answers. first and last pass the
// classifier but are not listed, because a document over one SQLite database is
// refused for them (the adapter declares no stable row order); a client is only
// told what the launch engine runs. TestEveryAdvertisedAggregateIsAnsweredOnSQLite
// posts each name to real SQLite files.
var profileAggregates = []string{"count", "sum", "avg", "min", "max"}

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
// the ones the server is configured with.
func (s *Server) queryProfile() map[string]any {
	limits := s.queryLimits
	return map[string]any{
		"endpoint": crossDatabaseDTQLPath,
		"format":   queryFormat,
		"features": map[string]any{
			"joins":              []string{"inner", "left"},
			"groupBy":            true,
			"having":             true,
			"aggregates":         profileAggregates,
			"subqueries":         true,
			"crossDatabase":      true,
			"externalSources":    false,
			"windowFunctions":    false,
			"protectedDatabases": false,
			"fieldNames":         "plain",
		},
		"limits": map[string]any{
			"timeoutMs":          limits.Timeout.Milliseconds(),
			"queueWaitMs":        max(limits.QueueWait, 0) / time.Millisecond,
			"maxSourceRows":      limits.MaxSourceRows,
			"maxSourceBytes":     limits.MaxSourceBytes,
			"maxResultRows":      joinexec.MaxResultRows,
			"maxResultBytes":     joinexec.MaxResultBytes,
			"concurrentInMemory": limits.InMemory,
			"concurrentDatabase": limits.Database,
		},
		"joinEngines": s.joinEngines(),
	}
}
