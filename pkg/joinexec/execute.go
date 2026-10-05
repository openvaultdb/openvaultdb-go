package joinexec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/dal-go/dalgo/access"
	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/record"
)

// The result of one request is bounded, whatever the sources hold: a result
// over either bound is refused with a BudgetError, never cut short.
const (
	// MaxResultRows is the most rows a result may have.
	MaxResultRows = 1000
	// MaxResultBytes is the most JSON-encoded bytes a result may have.
	MaxResultBytes = 8 << 20
)

// Profile is the classification of the document, as the profile classifier of
// pkg/core reports it. It is a claim, not evidence: Execute walks the query
// itself and refuses a profile that does not describe it (ErrInvalidDocument).
type Profile struct {
	// Sources lists every collection the document reads, including reads inside
	// derived sources and subquery expressions; a collection read twice is
	// listed twice. Database is empty for a source that names none.
	Sources []ProfileSource
	// HasSubquery reports a derived source, scalar subquery, EXISTS predicate
	// or query-valued comparison operand anywhere in the document.
	HasSubquery bool
}

// ProfileSource is one collection read of a Profile.
type ProfileSource struct {
	Database   string
	Collection string
}

// Registry resolves a database id to the source registered under it. A server
// that keeps a mount alive for the length of a request takes that hold in
// Lookup and lets go of it when the request ends.
type Registry interface {
	// Lookup returns the source registered under database, or false when there
	// is none.
	Lookup(database string) (Source, bool)
}

// Result is the answer to one request.
type Result struct {
	Records []record.Record
	// Columns names the columns of the records in the order they were selected.
	Columns   []string
	Execution Execution
}

// Execution describes how a request ran. It is the execution block of the
// response and never reports how many rows a source holds or was read for,
// only how many rows it delivered (and not even that for a protected source).
type Execution struct {
	// Route is RouteDatabase or RouteInMemory. It names the path, not where every
	// filter ran: on RouteInMemory DALgo evaluates the document above guarded
	// single-collection reads, and a document with one source that names its
	// database and has no subquery, null test, aggregation or scan clause is read
	// with one such query that carries its WHERE, ORDER BY and LIMIT, which the
	// mount's own executor runs. Every other in-memory document (one with a join,
	// GROUP BY, HAVING, an aggregate, a scan clause, or a source that names no
	// database) is read with plain scans of each collection and filtered above
	// them.
	Route        string            `json:"route"`
	ElapsedMs    int64             `json:"elapsedMs"`
	RowsReturned int               `json:"rowsReturned"`
	Sources      []ExecutionSource `json:"sources"`
}

// ExecutionSource describes the reads of one collection.
type ExecutionSource struct {
	Database   string `json:"database"`
	Collection string `json:"collection"`
	// Rows is how many rows the source delivered. It is nil for a source with
	// access policies, whose size a caller may not learn, and on the database
	// route, where the database ran the whole document and reports no more.
	Rows *int `json:"rows,omitempty"`
	// ElapsedMs is the time the source's readers were open, nil on the database
	// route.
	ElapsedMs *int64 `json:"elapsedMs,omitempty"`
}

// Admit takes a slot for a request that is about to read, on the route Execute
// chose for it (RouteDatabase or RouteInMemory). It returns the function that
// gives the slot back and true, or false when there is no slot, in which case the
// release function is not used. Execute calls the release function when the
// request ends, whatever way it ends; a nil release function is treated as one
// that does nothing.
type Admit func(ctx context.Context, route string) (release func(), ok bool)

// config is what the options set.
type config struct {
	joinEngines   map[string]bool
	nativeEngines map[string]bool
	now           func() time.Time
	admit         Admit
}

// Option configures Execute.
type Option func(*config)

func newConfig(opts []Option) *config {
	c := &config{
		joinEngines:   engineSet([]string{"sqlite", "ingitdb"}),
		nativeEngines: engineSet([]string{"sqlite"}),
		now:           time.Now,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

func engineSet(engines []string) map[string]bool {
	set := make(map[string]bool, len(engines))
	for _, engine := range engines {
		set[engine] = true
	}
	return set
}

// WithJoinEngines sets the storage engines whose databases may take part in a
// relational query. The default is sqlite and ingitdb, as the server's default
// query limits; an empty list keeps the default. An engine outside the set is
// refused with an EngineNotJoinableError.
func WithJoinEngines(engines ...string) Option {
	set := engineSet(engines)
	return func(c *config) {
		if len(set) > 0 {
			c.joinEngines = set
		}
	}
}

// WithNativeEngines sets the storage engines that run a whole document
// themselves (the database route). The default is sqlite; an empty list keeps
// the default. An engine listed here must also be in the join set.
func WithNativeEngines(engines ...string) Option {
	set := engineSet(engines)
	return func(c *config) {
		if len(set) > 0 {
			c.nativeEngines = set
		}
	}
}

// WithAdmission makes Execute ask admit for a slot once it knows the route and
// before it reads anything, and hold the slot until it returns. A request that
// fails earlier (a document that is not valid, a source the caller may not read,
// an unknown database, an engine that is refused) holds no slot and asks for
// none. When admit has no slot Execute returns a *CapacityError and has read
// nothing. The wait for a slot is part of the elapsed time Execute reports, and
// not of the request timeout, which starts when the read does. A nil admit
// admits every request.
func WithAdmission(admit Admit) Option {
	return func(c *config) { c.admit = admit }
}

// withClock replaces the clock Execute measures its own elapsed time with.
func withClock(now func() time.Time) Option {
	return func(c *config) { c.now = now }
}

// Execute runs a relational document and returns every row of its result, or no
// rows and an error. It is the one place that reads a joined result, and it
// reads with Guard.Collect, so a request that ran out of budget or lost a source
// part way is never answered with the rows read before the failure.
//
// defaultDatabase is the database of a source that names none; it is empty on
// an endpoint that serves several databases, where every source must name one.
// A source with a database of its own is read from that database whatever the
// default is: an endpoint that serves one database must refuse a document that
// names another itself.
//
// Execute works in this order, and a request that fails at one step has done
// nothing of the steps after it:
//
//  1. Walk the document, check every name in it and check the profile against
//     the walk (ErrInvalidDocument).
//  2. Settle the database of every source (ErrSourceWithoutDatabase).
//  3. Authorise every source the walk found (SourceDeniedError), before any
//     mount is resolved, so a caller learns nothing about databases it may not
//     read: 403 before 404.
//  4. Resolve each database once (UnknownDatabaseError).
//  5. Refuse an engine that cannot be queried (EngineNotQueryableError), for
//     every source, and then one outside the join set (EngineNotJoinableError).
//  6. Refuse a scan clause on a source with access policies
//     (ErrScanOnProtectedSource).
//  7. Choose the route, take a slot for it when WithAdmission is set
//     (CapacityError), and run. The retry in memory takes a slot of its own.
//
// The database route is chosen when every source is in one database, that
// database's engine is native, it has no access policies, the document has no
// subquery and no null test, and no source has a scan clause (no engine adapter
// applies a scan bound or compiles a null test). The whole document then runs in
// one read transaction of the database. A join with no aggregation that the
// database answers with an enforcement-unsupported denial (its adapter cannot
// compile the document, and says so before any row is read) is read again in
// memory, after the first slot is given back; see request.retryInMemory. Every
// other document runs in memory:
// DALgo reads each source with a single-collection query through a guarded leaf
// and joins, filters and aggregates above the leaves, so a source with access
// policies is only ever read through its policy-checked executor and only rows
// the caller may read reach a join or an aggregate. See Execution.Route for what
// the in-memory label does and does not say about where a filter ran.
//
// A Source must answer CanQuery, which says whether its engine can run
// structured queries at all (core.Database has it). A Source without the method
// is treated as one that cannot be queried, so the gate does not depend on the
// method set of whatever type a registry returns. The check comes before the
// join-set check and no join-set setting lifts it.
//
// The walk checks every name of a document (fields, aliases, qualifiers,
// parameters, the result names of scalar subqueries and collections) by the
// strict rules of pkg/core, the arithmetic operators and aggregate names by the
// classifier's lists, refuses a query whose columns carry one output name twice
// (the later column would replace the earlier one in a row), refuses a document
// that still holds a parameter (a parameter is bound before a document runs, and
// DALgo's join evaluates none), and refuses a document whose conditions and
// expressions nest more than 64 levels. A field name must pass the strict rule
// (core.ValidateFieldName) whatever the route and the engine. The classifier of
// pkg/core applies a wider quoted-name rule to the field names of a relational
// document, so a name such as "zip code" classifies and Execute then refuses it
// with ErrInvalidDocument. What only the classifier checks, and Execute does not
// repeat, is the format of a database id, the limit and offset bounds, money,
// cursors, the join types, the number of sources, how many levels of subquery
// nest (four), the refusal of every scan clause (Execute accepts one on a source
// without access policies and reads such a document in memory), and the sixteen
// levels at which its name check stops. A caller must therefore hand Execute a
// document, and a profile, that passed the classifier.
//
// opts, limits and the ordering of results are as documented on Option,
// Limits, MaxResultRows and MaxResultBytes.
func Execute(ctx context.Context, query dal.StructuredQuery, profile Profile, defaultDatabase string, registry Registry, authorize Authorize, limits Limits, opts ...Option) (Result, error) {
	cfg := newConfig(opts)
	started := cfg.now()
	if registry == nil {
		return Result{}, fmt.Errorf("%w: a registry is required", ErrInvalidDocument)
	}
	doc, err := inspect(query)
	if err != nil {
		return Result{}, err
	}
	if err := checkProfile(doc, profile); err != nil {
		return Result{}, err
	}
	targets, databases, qualified, err := settle(doc.sources, defaultDatabase)
	if err != nil {
		return Result{}, err
	}
	for _, target := range targets {
		if authorize == nil || !authorize(target.database, target.collection) {
			return Result{}, &SourceDeniedError{Database: target.database, Collection: target.collection}
		}
	}
	sources, err := resolveSources(registry, databases)
	if err != nil {
		return Result{}, err
	}
	if err := cfg.checkEngines(databases, sources); err != nil {
		return Result{}, err
	}
	if err := checkScans(targets, sources); err != nil {
		return Result{}, err
	}

	req := &request{cfg: cfg, query: query, doc: doc, databases: databases, qualified: qualified, sources: sources, authorize: authorize, limits: limits}
	route := cfg.route(doc, databases, sources)
	records, guard, err := req.attempt(ctx, route)
	if err != nil && route == RouteDatabase && req.retryInMemory(err) {
		// The database could not compile the document: it was refused before any row
		// was read, so nothing was delivered and the guard of the first attempt holds
		// nothing to carry over. The first attempt has given its slot back.
		route = RouteInMemory
		records, guard, err = req.attempt(ctx, route)
	}
	if err != nil {
		return Result{}, err
	}
	if records == nil {
		records = []record.Record{}
	}
	execution := Execution{
		Route:        route,
		ElapsedMs:    cfg.now().Sub(started).Milliseconds(),
		RowsReturned: len(records),
	}
	if route == RouteDatabase {
		execution.Sources = databaseSources(targets)
	} else {
		execution.Sources = inMemorySources(guard.Stats())
	}
	return Result{Records: records, Columns: orderedColumns(query, records), Execution: execution}, nil
}

// settle gives every source a database. It returns the sources with their
// databases, the distinct databases in document order, and whether every
// source named its database itself.
func settle(sources []walkedSource, defaultDatabase string) (targets []walkedSource, databases []string, qualified bool, err error) {
	qualified = true
	seen := map[string]bool{}
	for _, source := range sources {
		if source.database == "" {
			qualified = false
			source.database = defaultDatabase
		}
		if source.database == "" {
			return nil, nil, false, fmt.Errorf("%w: collection %q", ErrSourceWithoutDatabase, clip(source.collection))
		}
		targets = append(targets, source)
		if !seen[source.database] {
			seen[source.database] = true
			databases = append(databases, source.database)
		}
	}
	if len(databases) > 1 && !qualified {
		return nil, nil, false, fmt.Errorf("%w: a document that reads several databases names the database of every source", ErrSourceWithoutDatabase)
	}
	return targets, databases, qualified, nil
}

// resolveSources looks every database up once.
func resolveSources(registry Registry, databases []string) (map[string]Source, error) {
	sources := make(map[string]Source, len(databases))
	for _, database := range databases {
		source, ok := registry.Lookup(database)
		if !ok || source == nil {
			return nil, &UnknownDatabaseError{Database: database}
		}
		if source.ID() != database {
			return nil, fmt.Errorf("%w: %q", ErrRegistryMismatch, database)
		}
		sources[database] = source
	}
	return sources, nil
}

// checkEngines refuses a database whose engine cannot be queried, for every
// database, before it refuses one outside the join set: the first is the
// stronger statement and an operator's join set cannot lift it. A source that
// does not answer the question is one that cannot be queried.
func (c *config) checkEngines(databases []string, sources map[string]Source) error {
	for _, database := range databases {
		source := sources[database]
		gate, ok := source.(interface{ CanQuery() bool })
		if !ok || !gate.CanQuery() {
			return &EngineNotQueryableError{Database: database, Engine: source.Engine()}
		}
	}
	for _, database := range databases {
		if engine := sources[database].Engine(); !c.joinEngines[engine] {
			return &EngineNotJoinableError{Database: database, Engine: engine}
		}
	}
	return nil
}

// checkScans refuses a scan clause on a source with access policies.
func checkScans(targets []walkedSource, sources map[string]Source) error {
	for _, target := range targets {
		if target.scan && sources[target.database].HasAccessPolicies() {
			return fmt.Errorf("%w: %q.%q", ErrScanOnProtectedSource, target.database, clip(target.collection))
		}
	}
	return nil
}

// route chooses where the document runs. Only a document the whole database can
// run goes to it: the SQL adapters drop a scan bound without an error and refuse
// a null test, so a document with either is evaluated by DALgo instead.
func (c *config) route(doc document, databases []string, sources map[string]Source) string {
	if len(databases) == 1 && !doc.hasSubquery && !doc.hasNull && !doc.anyScan() {
		source := sources[databases[0]]
		if c.nativeEngines[source.Engine()] && !source.HasAccessPolicies() {
			return RouteDatabase
		}
	}
	return RouteInMemory
}

// request is one validated, authorised and resolved document, ready to read.
type request struct {
	cfg       *config
	query     dal.StructuredQuery
	doc       document
	databases []string
	qualified bool
	sources   map[string]Source
	authorize Authorize
	limits    Limits
}

// attempt reads the document on route: it takes a slot for the route when
// WithAdmission is set (a *CapacityError when there is none, with nothing read),
// holds it until the read has ended, and reads under a Guard of its own, so that
// the budgets, statistics and failure of one attempt never reach the next. It
// returns the Guard so that the caller can report what the read touched.
func (q *request) attempt(ctx context.Context, route string) ([]record.Record, *Guard, error) {
	if q.cfg.admit != nil {
		release, ok := q.cfg.admit(ctx, route)
		if !ok {
			return nil, nil, &CapacityError{Route: route}
		}
		if release != nil {
			defer release()
		}
	}
	guard := NewGuard(q.authorize, q.limits)
	ctx, cancel := guard.Context(ctx)
	defer cancel()
	r := &run{guard: guard, sources: q.sources}
	var (
		records []record.Record
		err     error
	)
	if route == RouteDatabase {
		records, err = r.database(ctx, q.query, q.sources[q.databases[0]])
	} else {
		records, err = r.inMemory(ctx, q.query, q.doc, q.databases, q.qualified)
	}
	return records, guard, err
}

// retryInMemory reports whether err, the failure of the database route, is one
// the in-memory route can answer instead: the whole-document denial of an adapter
// that could not compile a join that does not aggregate. The SQL adapters answer
// a compile failure of any kind with an enforcement-unsupported denial, and a
// join that selects no unaliased column named like the collection's registered
// key is one they cannot compile: they append an unqualified helper column for
// the key, which the SQLite compiler refuses in a query with a join. A database
// with access policies never takes the database route, so the document is read
// again only from a database that has none, and every read of the retry is
// authorised as any in-memory read is. A document with one source or with an
// aggregation does not have that cause and keeps the adapter's answer.
func (q *request) retryInMemory(err error) bool {
	if len(q.doc.sources) < 2 || dal.HasAggregation(q.query) {
		return false
	}
	decisions := access.DecisionsFromError(err)
	for _, decision := range decisions {
		if decision.Code != access.CodeEnforcementUnsupported {
			return false
		}
	}
	return len(decisions) > 0
}

// run holds what one request reads through.
type run struct {
	guard   *Guard
	sources map[string]Source
}

// leaf returns the guarded leaf of a database the preflight resolved and
// authorised, and nothing else. The failure is recorded on the guard, so that
// DALgo rewrapping it as text does not hide its type.
func (r *run) leaf(database string) (guardedLeaf, error) {
	source, ok := r.sources[database]
	if !ok {
		return nil, r.guard.fail(&UnknownDatabaseError{Database: database})
	}
	return r.guard.Leaf(source).(guardedLeaf), nil
}

// resolve is the dal.DatabaseResolver of DALgo's federated executor.
func (r *run) resolve(_ context.Context, database string) (dal.QueryExecutor, error) {
	leaf, err := r.leaf(database)
	if err != nil {
		return nil, err
	}
	return leaf, nil
}

// database runs the whole document in one read transaction of source. The
// result is drained inside the transaction, which ends when the function
// returns.
func (r *run) database(ctx context.Context, query dal.StructuredQuery, source Source) ([]record.Record, error) {
	var (
		records []record.Record
		failure error
		ran     bool
	)
	txErr := source.ReadTx(ctx, func(executor dal.QueryExecutor) error {
		ran = true
		reader, err := executor.ExecuteQueryToRecordsReader(ctx, query)
		records, failure = r.collect(ctx, reader, err, RouteDatabase)
		return failure
	})
	switch {
	case failure != nil:
		// Not txErr: a transaction that swallows the function's error must not
		// turn a failed read into an empty result.
		return nil, failure
	case txErr != nil:
		return nil, txErr
	case !ran:
		return nil, ErrReadTxSkipped
	}
	return records, nil
}

// inMemory runs the document above guarded leaves. DALgo's federated executor
// runs a document whose sources all name their database and that has no
// subquery, one leaf per database: it keeps the streaming join, and a plain
// one-source document (no null test, ordering expression, aggregation or scan
// clause) is handed to the mount whole, with its WHERE, ORDER BY and LIMIT. DALgo
// reads a one-source document with GROUP BY, HAVING, an aggregate, or a scan
// clause beside a WHERE, ORDER BY or offset, with a plain scan of the collection
// and evaluates it above the leaf. The federated executor cannot read a derived
// source (it asks every FROM node for a database), a one-source document with a null test would
// be handed whole to an engine that cannot compile it, and one that orders by
// arithmetic would be handed to an executor that skips the ordering and
// answers in the order it reads the records, so every other document
// runs through DALgo's recursive executor over a router that picks the leaf of
// each source's database. A join with a null test stays with the federated
// executor: it reads plain collections and evaluates the test above them.
//
// Before either reads a row, the unqualified fields of the document are checked
// against the field lists the sources supply (checkScopes): a query of several
// sources in which one source has no list is refused, with a scope error, for an
// unqualified field, because DALgo would bind it to the first source of the query.
// So is a name that two lists carry, and so is a name that only a later source
// carries in the GROUP BY, HAVING, ORDER BY or columns of a query that aggregates, or in
// an ON condition, because DALgo would read it from the first source. A column field that
// an earlier column of a query that aggregates carries as its alias is refused (DALgo reads
// the column), and so is the ORDER BY of a query that does not aggregate by the alias of
// a column that is an expression, or by a name that no source whose list is supplied
// carries (the executor of a mount that is handed the document whole ignores a field it
// does not know). DALgo is then given the document with the aliases of its select lists
// resolved in HAVING and ORDER BY (resolveAliases), which its own check of the fields
// against the lists does not know, and, in a query that does not aggregate, the alias of a
// field in ORDER BY replaced by the field, so that the answer is sorted the way a SQL
// database that runs the whole document sorts it.
func (r *run) inMemory(ctx context.Context, query dal.StructuredQuery, doc document, databases []string, qualified bool) ([]record.Record, error) {
	var (
		reader dal.RecordsReader
		err    error
	)
	router := newRouter(r, databases)
	// Before anything is read: a field that DALgo cannot bind to a source is refused,
	// not bound to the first one (see checkScopes).
	if err := checkScopes(ctx, query, router.JoinFields); err != nil {
		return nil, r.guard.Classify(err, RouteInMemory)
	}
	query = resolveAliases(query)
	// A document of one source that a mount could not evaluate whole is read as a plain scan
	// and evaluated above it: one with a null test (a SQL adapter cannot compile it), and one
	// that orders by an expression that is not a field (an executor skips it, and answers in
	// the order it reads the records).
	handedWhole := len(doc.sources) == 1 && (doc.hasNull || ordersByExpression(query))
	if qualified && !doc.hasSubquery && !handedWhole {
		reader, err = dal.ExecuteFederatedQueryWithOptions(ctx, query, r.resolve, dal.FederatedQueryOptions{})
	} else {
		reader, err = dal.ExecuteRecursiveQuery(ctx, router, query)
	}
	return r.collect(ctx, reader, err, RouteInMemory)
}

// ordersByExpression reports whether an ordering of query is not a plain field: arithmetic,
// which an executor that is handed the document whole does not apply.
func ordersByExpression(query dal.StructuredQuery) bool {
	for _, order := range query.OrderBy() {
		if _, plain := order.Expression().(dal.FieldRef); !plain {
			return true
		}
	}
	return false
}

// collect is the only place a result is read: it drains reader through
// Guard.Collect, under the result bounds, and classifies whatever DALgo or the
// reader reported. err is what the call that returned reader returned.
func (r *run) collect(ctx context.Context, reader dal.RecordsReader, err error, route string) ([]record.Record, error) {
	if err != nil {
		return nil, r.guard.Classify(err, route)
	}
	if reader == nil {
		return nil, r.guard.Classify(ErrNoReader, route)
	}
	return r.guard.Collect(ctx, &resultCap{RecordsReader: reader, route: route}, route)
}

// resultCap ends a read with a BudgetError when it would deliver more than
// MaxResultRows rows or MaxResultBytes bytes.
type resultCap struct {
	dal.RecordsReader
	route string
	rows  int
	bytes int64
}

func (c *resultCap) Next() (record.Record, error) {
	rec, err := c.RecordsReader.Next()
	switch {
	case err == nil:
	case err == io.EOF || errors.Is(err, dal.ErrNoMoreRecords):
		return nil, err
	case errors.Is(err, io.EOF):
		// Guard.Collect fails a read that ends with an error wrapping io.EOF, as
		// opposed to io.EOF itself or dal.ErrNoMoreRecords, too. This one is a
		// failure: hand it on as ErrReadTruncated without the chain, so that
		// nothing further up takes it for the end of the stream.
		return nil, fmt.Errorf("%w: %v", ErrReadTruncated, err)
	default:
		return nil, err
	}
	encoded, err := json.Marshal(rec.Data())
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrRowNotEncodable, err)
	}
	switch {
	case c.rows+1 > MaxResultRows:
		return nil, &BudgetError{Name: BudgetResponseRows, Limit: MaxResultRows, Route: c.route}
	case c.bytes+int64(len(encoded)) > MaxResultBytes:
		return nil, &BudgetError{Name: BudgetResponseBytes, Limit: MaxResultBytes, Route: c.route}
	}
	c.rows++
	c.bytes += int64(len(encoded))
	return rec, nil
}

// databaseSources lists the collections of a document the database ran, once
// each, without figures: the database reports none.
func databaseSources(targets []walkedSource) []ExecutionSource {
	var sources []ExecutionSource
	seen := map[[2]string]bool{}
	for _, target := range targets {
		key := [2]string{target.database, target.collection}
		if !seen[key] {
			seen[key] = true
			sources = append(sources, ExecutionSource{Database: target.database, Collection: target.collection})
		}
	}
	return sources
}

// inMemorySources reports what the leaves read, without a row count for a
// source with access policies.
func inMemorySources(stats []SourceStats) []ExecutionSource {
	sources := make([]ExecutionSource, len(stats))
	for i, s := range stats {
		elapsed := s.Elapsed.Milliseconds()
		sources[i] = ExecutionSource{Database: s.Database, Collection: s.Collection, ElapsedMs: &elapsed}
		if !s.Protected {
			rows := s.Rows
			sources[i].Rows = &rows
		}
	}
	return sources
}
