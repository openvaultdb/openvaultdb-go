package server

import (
	"net/http"
	"strings"
)

// Wire-compatibility with deployed clients that still call the pre-rename DTQL
// forms. Everything in this file is removed in TugQL cutover step 9 once
// deployed clients use /tugql.

// legacyDTQLCrossDatabasePath is the old cross-database endpoint; removed in TugQL
// cutover step 9 once deployed clients use /tugql.
const legacyDTQLCrossDatabasePath = "/v1/dtql"

// legacyDTQLDatabaseSuffix is the old per-database endpoint suffix; removed in
// TugQL cutover step 9 once deployed clients use /tugql.
const legacyDTQLDatabaseSuffix = "/dtql"

// legacyDTQLOperationMediaType is the old protected-operation media type; removed in
// TugQL cutover step 9 once deployed clients use /tugql.
const legacyDTQLOperationMediaType = "application/vnd.dtql.operation+json"

// legacyDTQLEndpointKey is the old discovery metadata endpoints key; removed in
// TugQL cutover step 9 once deployed clients use /tugql.
const legacyDTQLEndpointKey = "dtql"

// registerLegacyDTQLRoutes serves the old routes through the TugQL handlers;
// removed in TugQL cutover step 9 once deployed clients use /tugql.
func (s *Server) registerLegacyDTQLRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/databases/{db}"+legacyDTQLDatabaseSuffix, s.handleTugQL)
	mux.HandleFunc("GET /v1/databases/{db}"+legacyDTQLDatabaseSuffix, s.handleTugQL)
	mux.HandleFunc("POST "+legacyDTQLCrossDatabasePath, s.handleCrossDatabaseTugQL)
	mux.HandleFunc("GET "+legacyDTQLCrossDatabasePath, s.handleCrossDatabaseTugQL)
}

// isTugQLEndpointPath reports whether path ends in a TugQL endpoint, including the
// legacy /dtql suffix, so the legacy routes keep every TugQL guard (provider
// request profile, continuation refusal, cache Vary). In TugQL cutover step 9 it
// becomes strings.HasSuffix(path, "/tugql").
func isTugQLEndpointPath(path string) bool {
	return strings.HasSuffix(path, "/tugql") || strings.HasSuffix(path, legacyDTQLDatabaseSuffix)
}
