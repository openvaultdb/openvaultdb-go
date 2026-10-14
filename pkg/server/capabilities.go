package server

import "github.com/openvaultdb/openvaultdb-go/pkg/core"

// databaseCapabilities is the capabilities map of a database descriptor: the
// metadata of GET /v1/databases/{db} and each entry of /.well-known/openvaultdb.
// A nil database is described as the server lists it when it has none.
//
// read is true for every served database: reading does not depend on ReadOnly(),
// which only says that the mount accepts no writes, and a read-only mount (an
// HTTP source or a native PostgreSQL mount) is queried like any other. write and
// delete are the same flag, false under a server-wide read-only switch or for a
// read-only mount. reset is always false for a server-hosted database, and
// persistent is always true.
func (s *Server) databaseCapabilities(db *core.Database) map[string]bool {
	canDTQL := s.advertisesOrdinaryQuery(db)
	canQuery := canDTQL && db.CanCollectionQuery()
	joins := db != nil && s.advertisesJoins(db)
	writable := !s.readOnly && (db == nil || !db.ReadOnly())
	return map[string]bool{
		"read":                true,
		"query":               canQuery,
		"dtql":                canDTQL,
		"dtqlStreaming":       canDTQL,
		"dtqlStreamingErrors": canDTQL,
		"write":               writable,
		"delete":              writable,
		"reset":               false,
		"persistent":          true,
		"joins":               joins,
		"aggregation":         joins,
	}
}
