package server

import "github.com/openvaultdb/openvaultdb-go/pkg/core"

// databaseCapabilities is the capabilities map of a database descriptor: the
// metadata of GET /v1/databases/{db} and each entry of /.well-known/openvaultdb.
// A nil database is described as the server lists it when it has none.
//
// read is whether keyed record reads are served (core.Database.KeyedRecordReadsServed):
// false for a read-only mount, which refuses Get and Exists. Clients browse through
// query and tugql, not through read. write and delete are the same flag, false under
// a server-wide read-only switch or for a read-only mount. reset is always false for
// a server-hosted database, and persistent is always true.
func (s *Server) databaseCapabilities(db *core.Database) map[string]bool {
	canTugQL := s.advertisesOrdinaryQuery(db)
	canQuery := canTugQL && db.CanCollectionQuery()
	joins := db != nil && s.advertisesJoins(db)
	keyed := db == nil || db.KeyedRecordReadsServed()
	writable := !s.readOnly && keyed
	return map[string]bool{
		"read":                 keyed,
		"query":                canQuery,
		"tugql":                canTugQL,
		"tugqlStreaming":       canTugQL,
		"tugqlStreamingErrors": canTugQL,
		"write":                writable,
		"delete":               writable,
		"reset":                false,
		"persistent":           true,
		"joins":                joins,
		"aggregation":          joins,
	}
}
