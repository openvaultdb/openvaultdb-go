package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/dal-go/dalgo/access"
	"github.com/dal-go/dalgo/dal"

	"github.com/openvaultdb/openvaultdb-go/pkg/auth"
	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/joinexec"
	"github.com/openvaultdb/openvaultdb-go/pkg/license"
)

// This file answers a relational DTQL document: a join, a grouping, an alias, a
// subquery, or a source that names its database. Two endpoints reach it. The
// per-database endpoint takes a document whose sources all belong to the
// database in its path; /v1/dtql takes documents that read several databases and
// needs every source to name one.
//
// The handler is where a request is authorised. joinexec.Execute authorises
// every source again, and its leaves once more at each read, but it reads a
// source that names another database whatever the default is, so the handler
// does not rely on it: it settles the database of every source the classifier
// found (the root, joins at any depth, derived sources, subqueries), refuses a
// document that strays outside the endpoint, checks the caller's capability on
// every database and collection, leases the databases, refuses a document that
// names a database with access policies, and only then checks that the
// collections are declared, refuses the paging headers and the engines the server
// does not join, and runs. Nothing is gated or read for a request that fails a
// check before it.
//
// A relational document is not run on a database with access policies. The
// refusal depends on the databases the document names and on nothing else: it
// comes before any collection name is looked up, any field is loaded or any source
// is read, so the answer is the same whichever collections the document carries,
// declared or not, readable or not.

// joinExecuteFunc is the signature of joinexec.Execute: the seam the handler
// runs a relational document through.
type joinExecuteFunc func(ctx context.Context, query dal.StructuredQuery, profile joinexec.Profile, defaultDatabase string, registry joinexec.Registry, authorize joinexec.Authorize, limits joinexec.Limits, opts ...joinexec.Option) (joinexec.Result, error)

// maxEchoLen is the most bytes of a name from the request an error body repeats.
// A name can be as long as the request body, and an error is not the place to
// send it back.
const maxEchoLen = 64

// maxEchoText is the most bytes of a longer message of DALgo an error body repeats.
const maxEchoText = 256

// retryAfterSeconds is the Retry-After of a 503 query_capacity: the gate queues a
// query for about a second before it refuses it, so a client that comes back
// after a second finds the slots the earlier queries released.
const retryAfterSeconds = "1"

// pagingHeaders are the request headers of the snapshot paging protocol.
var pagingHeaders = []string{"OVDB-Page-Size", "OVDB-Page-Token", "OVDB-Page-Close"}

// handleCrossDatabaseDTQL answers POST and GET /v1/dtql. Every document posted
// here is relational: a source that names no database is a 400, because there is
// no database in the path to take it from.
func (s *Server) handleCrossDatabaseDTQL(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	doc, ok := s.readDTQLDocumentForClassification(w, r, true)
	if !ok {
		return
	}
	query, profile, err := classifyDTQLDocument(withoutContinuationFields(doc, false))
	if err != nil {
		s.writeMappedError(w, r, clippedError{err})
		return
	}
	s.serveRelationalDTQL(w, r, nil, query, profile)
}

// relationalTarget is one collection a document reads, with its database
// settled.
type relationalTarget struct {
	database   string
	collection string
}

// serveRelationalDTQL runs a relational document and answers it. endpoint is
// the database of the per-database endpoint, already leased, or nil on
// /v1/dtql.
func (s *Server) serveRelationalDTQL(w http.ResponseWriter, r *http.Request, endpoint *core.Database, query dal.StructuredQuery, profile core.Profile) {
	for _, source := range profile.Sources {
		if source.Schema != "" && (endpoint == nil || !endpoint.NativePostgresReadOnly()) {
			writeError(w, http.StatusBadRequest, "invalid_dtql", "schema-qualified sources are supported only on a native PostgreSQL read-only database endpoint")
			return
		}
	}
	targets, refusal := relationalTargets(profile, endpoint)
	if refusal != "" {
		writeError(w, http.StatusBadRequest, "invalid_dtql", refusal)
		return
	}
	allowed := s.readAuthorizer(r)
	for _, target := range targets {
		if !allowed(target.database, target.collection) {
			writeError(w, http.StatusForbidden, "forbidden", fmt.Sprintf("token does not grant %s on collection %q of database %q",
				auth.CapRecordsRead, clipName(target.collection), clipName(target.database)))
			return
		}
	}
	databases, order, ok := s.leaseRelationalDatabases(w, r, endpoint, targets)
	if !ok {
		return
	}
	for _, db := range databases {
		if !s.guardRetentionRead(w, r, db) {
			return
		}
	}
	if endpoint == nil {
		// Native sources keep the ordinary parser refusal for continuation
		// fields. Classification above removed them only to identify sources.
		doc, ok := s.readDTQLDocument(w, r)
		if !ok {
			return
		}
		if _, _, err := classifyDTQLDocument(doc); err != nil {
			s.writeMappedError(w, r, clippedError{err})
			return
		}
	}
	// A database with access policies is not read by a relational document. The
	// refusal is decided by the databases the document names, before a collection
	// name is looked at, so no answer says which collections that database declares.
	if err := s.refuseEach(stageAccessPolicies, databases, order); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "authorization_unsupported", err.Error())
		return
	}
	if err := s.refuseEach(stageReadProfile, databases, order); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "read_profile_unsupported", err.Error())
		return
	}
	// A collection that a database on an engine that builds SQL does not declare is
	// not read, whatever the grant says, and neither is one the document spells
	// another way than its canonical name. The executor reads one source at a time,
	// so without this check a document that names one would reach the adapter for
	// the sources before it. It comes before the refusal of the paging headers and
	// of an engine, as it does on the other routes: a collection that is not there
	// is a 404 whatever else is wrong with the request.
	for _, target := range targets {
		db := databases[target.database]
		// A name longer than the engine's server keeps is the caller's mistake in the
		// name, a 400 as on the routes that take a key, and not a collection that is
		// missing.
		if err := db.GuardCollection(target.collection); errors.Is(err, core.ErrInvalidKey) {
			writeError(w, http.StatusBadRequest, "invalid_key", err.Error())
			return
		}
		if !readableCollection(db, target.collection) {
			writeError(w, http.StatusNotFound, "not_found", fmt.Sprintf("collection not found: %q in database %q", clipName(target.collection), clipName(target.database)))
			return
		}
	}
	for _, header := range pagingHeaders {
		if headerPresent(r, header) {
			writeError(w, http.StatusUnprocessableEntity, "snapshot_unsupported", "a joined result is returned whole: the paging headers are not supported on a relational query")
			return
		}
	}
	if err := s.checkRelationalEngines(databases, order); err != nil {
		s.writeRelationalError(w, r, err)
		return
	}
	capture, err := s.captureRights(databases, targets)
	if err != nil {
		s.rightsError(w, err)
		return
	}
	defaultDatabase := ""
	if endpoint != nil {
		defaultDatabase = endpoint.ID()
	}
	limits := joinexec.Limits{
		MaxSourceRows:  s.queryLimits.MaxSourceRows,
		MaxSourceBytes: s.queryLimits.MaxSourceBytes,
		Timeout:        s.queryLimits.Timeout,
	}
	opts := []joinexec.Option{joinexec.WithAdmission(s.admitRelationalQuery)}
	if engines := s.joinEngines(); len(engines) > 0 {
		opts = append(opts, joinexec.WithJoinEngines(engines...))
	}
	if endpoint != nil && endpoint.NativePostgresReadOnly() {
		opts = append(opts, joinexec.WithNativePostgresFields())
	}
	if engines := s.nativeEngines(); len(engines) > 0 {
		opts = append(opts, joinexec.WithNativeEngines(engines...))
	}
	result, err := s.joinExecute(r.Context(), query, joinProfile(profile), defaultDatabase, leasedRegistry(databases), allowed, limits, opts...)
	if err != nil {
		s.writeRelationalError(w, r, err)
		return
	}
	facts := make([]cacheFacts, 0, len(order))
	for _, id := range order {
		db := databases[id]
		facts = append(facts, cacheFacts{TTL: db.Manifest.Database.ReadCacheTTL(), HasAccessPolicies: db.HasAccessPolicies(), NoRetention: db.NoRetention()})
	}
	cacheControl := relationalCacheControl(s.readOnly, s.authCfg != nil, r.Method, facts)
	w.Header().Set("Cache-Control", cacheControl)
	if cacheControl != "no-store" {
		w.Header().Add("Vary", strings.Join(pagingHeaders, ", "))
	}
	records := make([]relationalRecord, len(result.Records))
	for i, rec := range result.Records {
		data := rec.Data()
		if endpoint != nil && endpoint.NativePostgresReadOnly() {
			if fields, ok := data.(map[string]any); ok {
				data = nativePostgresJSONSafeIntegers(fields)
			}
		}
		records[i] = relationalRecord{Data: data}
	}
	if len(capture.rights) == 0 {
		writeJSON(w, http.StatusOK, relationalResponse{Records: records, Columns: result.Columns, Execution: result.Execution})
		return
	}
	usedIDs := make([]string, 0, len(result.Execution.Sources))
	for _, source := range result.Execution.Sources {
		usedIDs = append(usedIDs, (license.Identity{ServerID: s.rightsServerID, DatabaseID: source.Database, Recordset: source.Collection}).SourceID())
	}
	used, err := capture.used(usedIDs)
	if err != nil {
		s.rightsError(w, err)
		return
	}
	response := map[string]any{"records": records, "columns": result.Columns, "execution": result.Execution}
	s.writeRightsResult(w, r, response, capture, used, joinexec.MaxResultBytes)
}

// readableCollection reports whether a relational document may name collection of
// db: one the database declares, under its canonical name. A collection a
// database on an engine that builds SQL does not declare is refused (a document
// engine takes any name that passes the path rule). The adapter writes the name
// into a statement and quotes it, so the SQLite spelling that carries the quote
// characters of a declared collection would address a table of that literal name
// that the manifest does not declare; no query needs a spelling other than the
// canonical one.
func readableCollection(db *core.Database, collection string) bool {
	if db.GuardCollection(collection) != nil {
		return false
	}
	canonical, declared := db.CanonicalCollection(collection)
	return !declared || canonical == collection
}

// relationalResponse is the body of a relational answer: rows without keys, the
// columns in the order the document selects them, and how the request ran.
type relationalResponse struct {
	Records   []relationalRecord `json:"records"`
	Columns   []string           `json:"columns"`
	Execution joinexec.Execution `json:"execution"`
}

type relationalRecord struct {
	Data any `json:"data"`
}

// relationalTargets settles the database of every source of the profile. On the
// per-database endpoint a source that names none belongs to the endpoint's
// database and one that names another database is refused; on /v1/dtql a source
// that names none is refused. The second result is the reason of a refusal.
func relationalTargets(profile core.Profile, endpoint *core.Database) ([]relationalTarget, string) {
	targets := make([]relationalTarget, 0, len(profile.Sources))
	for _, source := range profile.Sources {
		database := source.Database
		switch {
		case endpoint == nil && database == "":
			return nil, fmt.Sprintf("this endpoint reads several databases, so every source names its database: collection %q does not", clipName(source.Collection))
		case endpoint != nil && database == "":
			database = endpoint.ID()
		case endpoint != nil && database != endpoint.ID():
			return nil, fmt.Sprintf("a source names database %q but this endpoint reads only database %q: send a document that reads several databases to POST %s",
				clipName(database), endpoint.ID(), crossDatabaseDTQLPath)
		}
		targets = append(targets, relationalTarget{database: database, collection: source.Collection})
	}
	return targets, ""
}

// readAuthorizer is the authoriser of a request: true for every read when auth
// is off, and the capability check of the principal otherwise. No principal
// reads nothing.
func (s *Server) readAuthorizer(r *http.Request) joinexec.Authorize {
	if s.authCfg == nil {
		return func(string, string) bool { return true }
	}
	principal := auth.FromRequest(r)
	return func(database, collection string) bool {
		return principal.Allows(database, auth.CapRecordsRead, collection)
	}
}

// leaseRelationalDatabases leases every database the targets name, so that an
// unmount waits for the request, and returns them with their ids in the order
// the document first names them. The database of the per-database endpoint was
// leased when the request was routed. A database that is not mounted is a 404;
// the caller was cleared to read it already, so the answer says nothing a caller
// who may not read it could learn.
func (s *Server) leaseRelationalDatabases(w http.ResponseWriter, r *http.Request, endpoint *core.Database, targets []relationalTarget) (map[string]*core.Database, []string, bool) {
	databases := map[string]*core.Database{}
	var order []string
	if endpoint != nil {
		databases[endpoint.ID()] = endpoint
		order = append(order, endpoint.ID())
	}
	for _, target := range targets {
		if _, leased := databases[target.database]; leased {
			continue
		}
		db := s.acquire(r, target.database)
		if db == nil {
			writeError(w, http.StatusNotFound, "not_found", "database not found: "+clipName(target.database))
			return nil, nil, false
		}
		databases[target.database] = db
		order = append(order, target.database)
	}
	return databases, order, true
}

// joinEngines is the list of engines whose databases may take part in a relational
// document, and the list discovery advertises: the operator's list, without an
// engine the structured-query guard does not clear (engineCleared, which every
// database is held to before the list is looked at, so a database on such an
// engine is refused with 501 whatever the list says) and without the engine of a
// GitHub-backed inGitDB mount, whose reads go over the network, so no list
// enables it.
func (s *Server) joinEngines() []string {
	engines := make([]string, 0, len(s.queryLimits.JoinEngines))
	seen := make(map[string]bool, len(s.queryLimits.JoinEngines)+1)
	for _, engine := range s.queryLimits.JoinEngines {
		if engine != core.EngineInGitDBGitHub && !seen[engine] && s.engineCleared(engine) {
			engines = append(engines, engine)
			seen[engine] = true
		}
	}
	// A native PostgreSQL read-only mount opts into this bounded query path in
	// its manifest. Existing writable/preview PostgreSQL mounts still require
	// the operator's explicit join-engine setting.
	s.mu.RLock()
	for _, db := range s.dbs {
		if db != nil && db.NativePostgresReadOnly() && db.CanQuery() && !seen["postgres"] {
			engines = append(engines, "postgres")
			seen["postgres"] = true
		}
	}
	s.mu.RUnlock()
	return engines
}

// engineCleared reports whether a structured query may reach engine on this server:
// the engines the allow-list of core clears (core.EngineCanQuery), and the engine of
// a mounted database that is cleared by the preview switch it read when it opened
// (PostgreSQL; see core.PreviewPostgresQueriesEnv). It asks the mounted databases
// because the switch belongs to the mount: core decides, and this only asks.
func (s *Server) engineCleared(engine string) bool {
	if core.EngineCanQuery(engine) {
		return true
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, db := range s.dbs {
		if db.Engine() == engine && db.CanQuery() {
			return true
		}
	}
	return false
}

// nativeEngines is the list of engines that run a whole relational document in the
// database (the database route), when it is not joinexec's own default of SQLite
// alone: SQLite and PostgreSQL, while a mounted PostgreSQL database is cleared for
// queries. It is nil otherwise, and then no option is passed to the executor.
func (s *Server) nativeEngines() []string {
	if !s.engineCleared("postgres") {
		return nil
	}
	return []string{"sqlite", "postgres"}
}

// relationalStage is one test of the rule that decides whether a database takes
// part in a relational document. It returns nil for a database that passes and the
// refusal otherwise.
type relationalStage func(s *Server, id string, db *core.Database) error

// accessPoliciesError is the refusal of a database with access policies: a
// relational document is not run on it (a launch limit).
type accessPoliciesError struct{ Database string }

func (e *accessPoliciesError) Error() string {
	return fmt.Sprintf("database %q has access policies and is read one source at a time: "+
		"a relational document (a join, a grouping, an alias, a subquery or a source that names its database) is not run on it", clipName(e.Database))
}

// stageAccessPolicies refuses a database with access policies.
func stageAccessPolicies(_ *Server, id string, db *core.Database) error {
	if db.HasAccessPolicies() {
		return &accessPoliciesError{Database: id}
	}
	return nil
}

// stageQueryable refuses a database whose engine the structured-query guard does
// not clear (the operator's list never lifts that).
func stageQueryable(_ *Server, id string, db *core.Database) error {
	if !db.CanQuery() {
		return &joinexec.EngineNotQueryableError{Database: id, Engine: db.Engine()}
	}
	return nil
}

// stageJoinable refuses a database whose engine the operator's list leaves out. It
// compares db.Engine(), which tells a GitHub-backed inGitDB mount from a local one,
// not the engine written in the manifest.
func stageJoinable(s *Server, id string, db *core.Database) error {
	for _, engine := range s.joinEngines() {
		if engine == db.Engine() {
			return nil
		}
	}
	return &joinexec.EngineNotJoinableError{Database: id, Engine: db.Engine()}
}

// relationalStages is the rule, in the order a request meets it: a database takes
// part in a relational document, and is advertised as joining, only when every
// stage passes it. The handler runs the stages one at a time, over all the
// databases a document names, because the collection check and the paging headers
// come between the first and the others; discovery runs them together for one
// database (relationalRefusal).
var relationalStages = []relationalStage{stageAccessPolicies, stageReadProfile, stageQueryable, stageJoinable}

// engineStages are the stages that look at the engine.
var engineStages = relationalStages[1:]

// refuseEach runs one stage over the databases in order and returns the first
// refusal.
func (s *Server) refuseEach(stage relationalStage, databases map[string]*core.Database, order []string) error {
	for _, id := range order {
		if err := stage(s, id, databases[id]); err != nil {
			return err
		}
	}
	return nil
}

// checkRelationalEngines refuses a database whose engine the structured-query
// guard does not clear, and then one the operator's list leaves out; the guard is
// checked for every database before the list is.
func (s *Server) checkRelationalEngines(databases map[string]*core.Database, order []string) error {
	for _, stage := range engineStages {
		if err := s.refuseEach(stage, databases, order); err != nil {
			return err
		}
	}
	return nil
}

// relationalRefusal reports why a relational document that names db is refused
// whatever else it says, or nil when db takes part in one. It is the rule the
// handler applies, read for one database: discovery advertises db as joining
// exactly when this is nil.
func (s *Server) relationalRefusal(id string, db *core.Database) error {
	for _, stage := range relationalStages {
		if err := stage(s, id, db); err != nil {
			return err
		}
	}
	return nil
}

// admitRelationalQuery takes a slot of the concurrency gate on the route the
// executor chose.
func (s *Server) admitRelationalQuery(ctx context.Context, route string) (func(), bool) {
	return s.queryGate.acquire(ctx, queryRoute(route))
}

// joinProfile converts the classifier's profile to the executor's.
func joinProfile(profile core.Profile) joinexec.Profile {
	converted := joinexec.Profile{HasSubquery: profile.HasSubquery}
	for _, source := range profile.Sources {
		converted.Sources = append(converted.Sources, joinexec.ProfileSource{Database: source.Database, Collection: source.Collection})
	}
	return converted
}

// leasedRegistry is the executor's registry over databases the request holds.
type leasedRegistry map[string]*core.Database

func (l leasedRegistry) Lookup(database string) (joinexec.Source, bool) {
	db, ok := l[database]
	if !ok {
		return nil, false
	}
	return db, true
}

// relationalErrorBody is errorBody with the fields a budget refusal adds.
type relationalErrorBody struct {
	Error relationalErrorDetail `json:"error"`
}

type relationalErrorDetail struct {
	Code    string        `json:"code"`
	Message string        `json:"message,omitempty"`
	Budget  *budgetDetail `json:"budget,omitempty"`
	Hint    string        `json:"hint,omitempty"`
}

// budgetDetail names the bound a query ran into. It never carries the figure the
// query reached, only the limit.
type budgetDetail struct {
	Name  string `json:"name"`
	Limit int64  `json:"limit"`
	Route string `json:"route"`
	Path  string `json:"path,omitempty"`
}

// writeBudgetRefusal answers a request that reached a bound: 422 query_budget_exceeded, with
// the bound named, its limit and never the figure the request reached, and a hint that says
// what to change.
func writeBudgetRefusal(w http.ResponseWriter, budget *joinexec.BudgetError, hint string) {
	writeJSON(w, http.StatusUnprocessableEntity, relationalErrorBody{Error: relationalErrorDetail{
		Code:    "query_budget_exceeded",
		Message: fmt.Sprintf("the query exceeds the %s limit of a request", clipName(budget.Name)),
		Budget:  &budgetDetail{Name: clipName(budget.Name), Limit: budget.Limit, Route: clipName(budget.Route), Path: clipName(budget.Path)},
		Hint:    hint,
	}})
}

// writeRelationalError answers an error of a relational request. Every refusal
// the caller can act on has a 4xx or 503 of its own and a message that repeats no
// more of the request than a bounded name; an error nothing here knows is the
// generic mapping of the server, a 500 that is logged.
func (s *Server) writeRelationalError(w http.ResponseWriter, r *http.Request, err error) {
	var (
		capacity     *joinexec.CapacityError
		budget       *joinexec.BudgetError
		denied       *joinexec.SourceDeniedError
		unknown      *joinexec.UnknownDatabaseError
		notQueryable *joinexec.EngineNotQueryableError
		notJoinable  *joinexec.EngineNotJoinableError
		joinShape    *dal.JoinValidationError
		queryShape   *dal.QueryValidationError
	)
	switch {
	case errors.As(err, &capacity):
		w.Header().Set("Retry-After", retryAfterSeconds)
		writeError(w, http.StatusServiceUnavailable, "query_capacity", fmt.Sprintf("the server is running as many %s queries as it allows: retry shortly", clipName(capacity.Route)))
	case errors.As(err, &budget):
		writeBudgetRefusal(w, budget, budgetHintOn(budget.Name, budget.Route))
	case errors.As(err, &denied):
		writeError(w, http.StatusForbidden, "forbidden", fmt.Sprintf("token does not grant %s on collection %q of database %q",
			auth.CapRecordsRead, clipName(denied.Collection), clipName(denied.Database)))
	case errors.As(err, &unknown):
		writeError(w, http.StatusNotFound, "not_found", "database not found: "+clipName(unknown.Database))
	case errors.As(err, &notQueryable):
		writeError(w, http.StatusNotImplemented, "query_unsupported", fmt.Sprintf("the %q storage engine of database %q is not yet supported for queries",
			clipName(notQueryable.Engine), clipName(notQueryable.Database)))
	case errors.As(err, &notJoinable):
		writeError(w, http.StatusUnprocessableEntity, "join_engine_unsupported", fmt.Sprintf("the %q storage engine of database %q is not enabled for joins and aggregation",
			clipName(notJoinable.Engine), clipName(notJoinable.Database)))
	case errors.Is(err, context.DeadlineExceeded):
		writeError(w, http.StatusGatewayTimeout, "query_timeout", "the query ran longer than the server allows")
	case errors.Is(err, core.ErrProtectedReadTx), errors.Is(err, joinexec.ErrScanOnProtectedSource):
		writeError(w, http.StatusUnprocessableEntity, "join_unsupported", "this query cannot run on a database with access policies")
	case errors.Is(err, access.ErrAccessDenied):
		s.writeMappedError(w, r, err)
	case errors.As(err, &joinShape) && isRefusedJoin(joinShape):
		writeError(w, http.StatusBadRequest, "invalid_dtql", clipText(joinShape.Error(), maxEchoText))
	case errors.As(err, &queryShape):
		writeError(w, http.StatusBadRequest, "invalid_dtql", clipText(fmt.Sprintf("%s at %s: %s", queryShape.Category, queryShape.Path, queryShape.Message), maxEchoText))
	case errors.Is(err, joinexec.ErrProfileMismatch):
		// The classifier and the executor's walk disagree about a document the
		// classifier passed: a defect of the server, not a mistake of the caller.
		s.writeInternalError(w, r, "internal server error", err)
	case errors.Is(err, joinexec.ErrInvalidDocument), errors.Is(err, joinexec.ErrSourceWithoutDatabase):
		writeError(w, http.StatusBadRequest, "invalid_dtql", clipText(err.Error(), maxEchoText))
	case isUnsupportedConditionError(err):
		writeError(w, http.StatusUnprocessableEntity, "query_unsupported", "the storage engine cannot run a condition of the query")
	default:
		if column, ok := unknownColumn(err); ok {
			writeError(w, http.StatusBadRequest, "invalid_dtql", fmt.Sprintf("the database has no column %q", clipName(column)))
			return
		}
		s.writeMappedError(w, r, clippedError{err})
	}
}

// isRefusedJoin reports whether err is a shape error of the document, which the
// caller made and can change: every category of DALgo's join errors but join_plan,
// and the join_plan refusals of the document (joinexec.IsJoinPlanRefusal). Every other
// join_plan error carries the text of a failed read (a scan, a close, a field load) or of
// an encoding fault, or reports a bound that Execute maps to a budget refusal first, so it
// is answered as a server fault: a logged 500 that repeats nothing of the cause.
func isRefusedJoin(err *dal.JoinValidationError) bool {
	return err.Category != "join_plan" || joinexec.IsJoinPlanRefusal(err.Message)
}

// leafError returns the innermost error of err's chain: the error the source
// reported, without the wrappers around it. A wrapper prints names the caller
// chose (a SourceError prints the collection), so a text the server looks for in
// an error is looked for in the leaf only.
func leafError(err error) error {
	for {
		inner := errors.Unwrap(err)
		if inner == nil {
			return err
		}
		err = inner
	}
}

// unsupportedConditionPattern matches the refusal of an engine adapter to compile
// a condition of the query: the adapters report it as a plain error whose text
// starts "unsupported condition", after at most the name of the adapter, with no
// type to ask for.
var unsupportedConditionPattern = regexp.MustCompile(`^(?:[\w.-]+: )?unsupported condition\b`)

// isUnsupportedConditionError recognises that refusal in the leaf of err.
func isUnsupportedConditionError(err error) bool {
	return unsupportedConditionPattern.MatchString(leafError(err).Error())
}

// unknownColumnPattern matches the whole text SQLite gives for a column it does
// not know, with or without the prefix and the code the driver adds.
var unknownColumnPattern = regexp.MustCompile(`^(?:SQL logic error: )?no such column: (\S+)(?: \(\d+\))?$`)

// unknownColumn reports whether the leaf of err says the database has no such
// column, and the name it gives. A column the caller named that the database does
// not have is the caller's mistake, not the server's.
func unknownColumn(err error) (string, bool) {
	match := unknownColumnPattern.FindStringSubmatch(leafError(err).Error())
	if match == nil {
		return "", false
	}
	return match[1], true
}

// budgetHints tells a caller what to change for each bound.
var budgetHints = map[string]string{
	joinexec.BudgetSourceRows:                "The query reads more rows from its sources than one request may. Add a filter that narrows a source, or read a smaller collection.",
	joinexec.BudgetSourceBytes:               "The query reads more data from its sources than one request may. Add a filter that narrows a source, or select fewer columns.",
	joinexec.BudgetJoinRows:                  "The join holds more rows than the in-memory join allows. Filter the inputs of the join, or join on a more selective condition.",
	joinexec.BudgetJoinResultRows:            "The join produces more rows than the in-memory join allows. Filter the inputs of the join, or join on a more selective condition.",
	joinexec.BudgetJoinFetchedRows:           "A source of the join delivers more rows than the in-memory join accepts. Add a filter that narrows that source.",
	joinexec.BudgetJoinRetainedBytes:         "The join holds more data than the in-memory join allows. Select fewer columns, or filter the inputs of the join.",
	joinexec.BudgetJoinScan:                  "A relation of the join is larger than the in-memory join scans. Add a filter that narrows it.",
	joinexec.BudgetJoinCandidateEvaluations:  "The join compares more pairs of rows than the in-memory join allows. Join on an equality between the two sources, or filter their rows first.",
	joinexec.BudgetAggregationGroups:         "The grouping produces more groups than the server allows. Group by fewer columns, or filter the rows first.",
	joinexec.BudgetAggregationStates:         "The aggregation keeps more state than the server allows. Group by fewer columns, or use fewer aggregates.",
	joinexec.BudgetAggregationBytes:          "The aggregation keeps more data than the server allows. Group by fewer columns, or filter the rows first.",
	joinexec.BudgetAggregationDistinctValues: "A distinct aggregate sees more distinct values than the server allows. Filter the rows first, or aggregate without distinct.",
	joinexec.BudgetAggregationTotalDistinct:  "The distinct aggregates of the query see more distinct values together than the server allows. Use fewer distinct aggregates, or filter the rows first.",
	joinexec.BudgetResponseRows:              "The result has more rows than one response holds. Add a filter or a limit of at most 1000.",
	joinexec.BudgetResponseBytes:             "The result is larger than one response holds. Select fewer columns, or add a filter or a limit.",
}

// genericBudgetHint is the hint of a bound that budgetHints does not list.
const genericBudgetHint = "The query is larger than one request may run. Add a filter, select fewer columns, or read less."

// databaseRouteJoinBudgets are the bounds of a join that DALgo evaluates itself.
// On the database route that happens only for a join the adapter could not write as one
// statement, and DALgo then reads each table whole, with no filter, so the hints of these
// bounds that tell the caller to filter a source cannot help there.
var databaseRouteJoinBudgets = map[string]bool{
	joinexec.BudgetJoinRows:                 true,
	joinexec.BudgetJoinResultRows:           true,
	joinexec.BudgetJoinFetchedRows:          true,
	joinexec.BudgetJoinRetainedBytes:        true,
	joinexec.BudgetJoinScan:                 true,
	joinexec.BudgetJoinCandidateEvaluations: true,
}

// databaseRouteJoinHint is the hint of a bound of those on the database route.
const databaseRouteJoinHint = "The database could not run this join as one statement, so the server read each table whole, with no filter, and joined the rows itself: a filter cannot narrow that read. Join columns of the same type, name the columns instead of a wildcard, or join a smaller table."

// budgetHintOn is the hint of a bound on the route it was reached on: the hint of the
// bound (budgetHint), except for a bound of the in-memory join on the database route,
// where it would tell the caller to filter a read that no filter reaches.
func budgetHintOn(name, route string) string {
	if route == joinexec.RouteDatabase && databaseRouteJoinBudgets[name] {
		return databaseRouteJoinHint
	}
	return budgetHint(name)
}

func budgetHint(name string) string {
	if hint, ok := budgetHints[name]; ok {
		return hint
	}
	return genericBudgetHint
}

// clipName shortens a name from the request to maxEchoLen bytes for an error.
func clipName(name string) string { return clipText(name, maxEchoLen) }

// clipText shortens text to limit bytes, on a character boundary, marking the cut.
func clipText(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + "..."
}
