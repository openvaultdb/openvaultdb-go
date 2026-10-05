package core

import "os"

// PreviewPostgresQueriesEnv names the environment variable of the preview of
// structured queries on PostgreSQL mounts. The preview is on only when the
// variable holds exactly "1". It is read once, when a mount opens (see open), so
// a mount answers by what it read and a change of the environment later changes
// nothing for it.
//
// The switch is temporary. Removing it takes this file, the previewPostgres
// field of Database and its two lines in open, and the one test file of the
// switch; the server's engineCleared, which asks CanQuery of the mounted
// databases only to learn that the preview is on; and Server.nativeEngines (in
// pkg/server/dtql_relational.go), which names PostgreSQL among the engines that run
// a whole relational document in the database only while the preview is on, and
// which then names it always, or goes with the default of pkg/joinexec being
// changed to include it. PostgreSQL then joins queryEngines, after the security
// review of the whole path.
const PreviewPostgresQueriesEnv = "OVDB_PREVIEW_POSTGRES_QUERIES"

// previewPostgresQueries reads the preview switch from the environment of the
// server. It is called once for each mount, when the mount opens.
func previewPostgresQueries() bool { return os.Getenv(PreviewPostgresQueriesEnv) == "1" }

// engineCleared is the one rule that clears a storage engine for structured
// queries: the engines of queryEngines, and PostgreSQL while the preview is on.
// MySQL is not cleared by it: its mount opens the SQL adapter with no dialect, so
// a query would reach the adapter's legacy text emitter.
func engineCleared(engine string, previewPostgres bool) bool {
	return queryEngines[engine] || engine == "postgres" && previewPostgres
}
