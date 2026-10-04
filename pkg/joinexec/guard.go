// Package joinexec is the security boundary for joined reads.
//
// DALgo evaluates a joined query by reading each source with a single-source
// query and combining the rows itself. Every one of those reads passes through
// a leaf made by a Guard, and the leaf is the one place that
//
//   - authorises the collection before it is read (a denied collection is
//     never read, and never has its schema looked up),
//   - refuses anything that is not a plain query over one collection,
//   - counts rows and encoded bytes against the request budget while
//     streaming, and stops with a BudgetError when the budget is exceeded,
//   - records per-source statistics for the response.
//
// One Guard serves one request. Create it with NewGuard, take its context from
// Guard.Context, make a leaf per source with Guard.Leaf, hand the leaves to
// DALgo (for example through a dal.DatabaseResolver), and answer the caller
// with Guard.Collect, which drains the reader DALgo returns, closes it and
// returns either every row or no rows and a classified error. A caller that
// reads the reader itself must read it to the end, close it, and only then
// pass the error it ended with (or nil) to Guard.Classify; rows read before an
// error are not an answer, because DALgo's streaming join delivers its rows
// before a budget error arrives.
package joinexec

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/record"
)

// Source is one registered database as the join executor sees it. The server's
// mount type implements it; this package depends on nothing else of the
// server.
type Source interface {
	// ID is the database id the source is registered under. It is the database
	// name Authorize receives and the name a query's collections must carry.
	ID() string
	// Engine is the storage engine name, for routing decisions made by the
	// caller.
	Engine() string
	// HasAccessPolicies reports whether Executor is wrapped in access
	// policies. Statistics of such a source are marked Protected.
	HasAccessPolicies() bool
	// Executor reads the source with the request principal applied. Every read
	// of the in-memory route goes through it.
	Executor() dal.QueryExecutor
	// ReadTx runs fn in one read transaction on the source, for the database
	// route. Reads made through the executor passed to fn are not guarded by a
	// leaf; the caller authorises such a query before running it.
	ReadTx(ctx context.Context, fn func(dal.QueryExecutor) error) error
}

// Authorize reports whether the caller may read the collection of the
// database. It is called before every read; a nil Authorize denies everything.
type Authorize func(database, collection string) bool

// SourceStats describes the reads of one collection during a request.
type SourceStats struct {
	Database   string
	Collection string
	// Rows is the number of rows delivered to the executor.
	Rows int
	// Elapsed is the time readers of this collection were open, summed over
	// reads.
	Elapsed time.Duration
	// Protected is true when the source has access policies. Callers should
	// not report Rows for a protected source.
	Protected bool
}

// Guard holds the budget, statistics and first failure of one request. It is
// safe for concurrent use by the leaves it made.
type Guard struct {
	authorize Authorize
	limits    Limits
	now       func() time.Time

	mu      sync.Mutex
	rows    int64
	bytes   int64
	failure error
	stats   []SourceStats
	index   map[[2]string]int
}

// NewGuard returns the Guard for one request. Zero fields of limits take their
// defaults (see Limits).
func NewGuard(authorize Authorize, limits Limits) *Guard {
	return &Guard{
		authorize: authorize,
		limits:    limits.withDefaults(),
		now:       time.Now,
		index:     map[[2]string]int{},
	}
}

// Context returns parent bounded by the request timeout. The caller must call
// the returned cancel function.
func (g *Guard) Context(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, g.limits.Timeout)
}

// Leaf returns the guarded executor for one source. It implements
// dal.QueryExecutor and dal.JoinFieldsProvider.
func (g *Guard) Leaf(src Source) dal.QueryExecutor {
	return &leaf{guard: g, src: src}
}

// Stats returns the per-collection statistics in the order collections were
// first read. It is complete only after every reader has been closed; Collect
// closes the reader it drains.
func (g *Guard) Stats() []SourceStats {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]SourceStats(nil), g.stats...)
}

// Err returns the first failure a leaf recorded (a SourceDeniedError, a
// BudgetError, a SourceError carrying the source's own error, or a refusal such
// as ErrNotSingleSource), or nil. Once set, every further read through any leaf
// of this Guard fails with it.
func (g *Guard) Err() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.failure
}

// Classify turns what a request ended with into the error to answer with. err
// is the error DALgo returned, or the one a drained reader ended with, or nil.
//
// DALgo reports a leaf's error as text inside its own error (and reads an error
// that wraps io.EOF as the end of a stream), so the recorded failure wins when
// there is one, even for a nil err: a request that recorded a failure never
// ends well. Otherwise DALgo's own bounds map to BudgetError (see
// MapDalgoError) and any other error is returned unchanged. Call it once, after
// the reader was drained and closed.
func (g *Guard) Classify(err error, route string) error {
	if failure := g.Err(); failure != nil {
		return failure
	}
	return MapDalgoError(err, route)
}

// Collect drains reader, closes it and returns every row, or no rows and the
// classified error when the read failed in any way: an error from the reader,
// a failure the guard recorded even if the reader ended cleanly, a close error,
// or the end of ctx. It is the one way to answer a request that cannot return
// a partial result. reader must be non-nil, so a caller whose DALgo call
// returned an error answers with Classify instead.
func (g *Guard) Collect(ctx context.Context, reader dal.RecordsReader, route string) ([]record.Record, error) {
	var records []record.Record
	var readErr error
	for {
		if readErr = ctx.Err(); readErr != nil {
			break
		}
		rec, err := reader.Next()
		// The end of the stream is io.EOF itself or dal.ErrNoMoreRecords (which
		// wraps it), as the leaf and DALgo read it. An error that merely wraps
		// io.EOF is a failed read, which a reader no leaf guards (the database
		// route) records nowhere else.
		if err == io.EOF || errors.Is(err, dal.ErrNoMoreRecords) {
			break
		}
		if err != nil {
			readErr = err
			break
		}
		records = append(records, rec)
	}
	if closeErr := reader.Close(); readErr == nil {
		readErr = closeErr
	}
	if err := g.Classify(readErr, route); err != nil {
		return nil, err
	}
	return records, nil
}

// fail records err as the request failure unless one is already recorded, and
// returns the failure in force.
func (g *Guard) fail(err error) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.failure == nil {
		g.failure = err
	}
	return g.failure
}

// charge adds one row of size bytes to the request budget. It returns the
// recorded BudgetError when the row does not fit; the row is then not counted.
func (g *Guard) charge(size int, path string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.failure != nil {
		return g.failure
	}
	switch {
	case g.rows+1 > int64(g.limits.MaxSourceRows):
		g.failure = &BudgetError{Name: BudgetSourceRows, Limit: int64(g.limits.MaxSourceRows), Route: RouteInMemory, Path: path}
	case g.bytes+int64(size) > g.limits.MaxSourceBytes:
		g.failure = &BudgetError{Name: BudgetSourceBytes, Limit: g.limits.MaxSourceBytes, Route: RouteInMemory, Path: path}
	default:
		g.rows++
		g.bytes += int64(size)
	}
	return g.failure
}

// record adds one finished read to the statistics.
func (g *Guard) record(database, collection string, protected bool, rows int, elapsed time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()
	key := [2]string{database, collection}
	i, ok := g.index[key]
	if !ok {
		i = len(g.stats)
		g.index[key] = i
		g.stats = append(g.stats, SourceStats{Database: database, Collection: collection})
	}
	g.stats[i].Rows += rows
	g.stats[i].Elapsed += elapsed
	g.stats[i].Protected = g.stats[i].Protected || protected
}
