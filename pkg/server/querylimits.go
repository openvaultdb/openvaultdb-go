package server

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// QueryLimits bounds relational (join and aggregation) queries so one request,
// or a crowd of identical ones, cannot exhaust a small instance. A zero or
// negative numeric field takes its default; a negative QueueWait means a query
// that finds the gate full is refused at once instead of queueing.
type QueryLimits struct {
	// Timeout is the longest a relational query may run.
	Timeout time.Duration
	// InMemory is how many in-memory (leaf read plus join above) queries may
	// run at once.
	InMemory int
	// Database is how many database-route (pushed down) queries may run at once.
	Database int
	// QueueWait is how long a query waits for a free slot before it is refused.
	QueueWait time.Duration
	// MaxSourceRows is the most rows one request may read from sources.
	MaxSourceRows int
	// MaxSourceBytes is the most bytes one request may read from sources.
	MaxSourceBytes int64
	// JoinEngines lists the storage engines whose databases may take part in a
	// join. "ingitdb" means a local working tree only: a GitHub-backed inGitDB
	// database (Storage.InGitDB.GitHub != nil) is never eligible, whatever this
	// list says.
	JoinEngines []string
}

// DefaultQueryLimits returns the default limits. A 512 MiB instance should
// lower InMemory to 1.
func DefaultQueryLimits() QueryLimits {
	return QueryLimits{
		Timeout:        10 * time.Second,
		InMemory:       2,
		Database:       4,
		QueueWait:      time.Second,
		MaxSourceRows:  100_000,
		MaxSourceBytes: 64 << 20,
		JoinEngines:    []string{"sqlite", "ingitdb"},
	}
}

// normalizeQueryLimits fills every unset field of l with its default.
func normalizeQueryLimits(l QueryLimits) QueryLimits {
	def := DefaultQueryLimits()
	if l.Timeout <= 0 {
		l.Timeout = def.Timeout
	}
	if l.InMemory <= 0 {
		l.InMemory = def.InMemory
	}
	if l.Database <= 0 {
		l.Database = def.Database
	}
	if l.QueueWait == 0 {
		l.QueueWait = def.QueueWait
	}
	if l.MaxSourceRows <= 0 {
		l.MaxSourceRows = def.MaxSourceRows
	}
	if l.MaxSourceBytes <= 0 {
		l.MaxSourceBytes = def.MaxSourceBytes
	}
	if len(l.JoinEngines) == 0 {
		l.JoinEngines = def.JoinEngines
	}
	l.JoinEngines = append([]string(nil), l.JoinEngines...)
	return l
}

// WithQueryLimits sets the relational query limits. Unset fields take the
// defaults from DefaultQueryLimits.
func WithQueryLimits(limits QueryLimits) Option {
	return func(s *Server) { s.setQueryLimits(limits) }
}

func (s *Server) setQueryLimits(limits QueryLimits) {
	s.queryLimits = normalizeQueryLimits(limits)
	s.queryGate = newQueryGate(s.queryLimits, realClock{})
}

// queryRoute names the execution route a query is gated under.
type queryRoute string

const (
	routeInMemory queryRoute = "in-memory"
	routeDatabase queryRoute = "database"
)

// clock is the time seam of the gate: tests inject one they fire by hand.
type clock interface {
	After(d time.Duration) <-chan time.Time
}

type realClock struct{}

func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// queryGate is a counting gate with one slot pool per route.
type queryGate struct {
	slots map[queryRoute]chan struct{}
	wait  time.Duration
	clock clock
}

func newQueryGate(l QueryLimits, c clock) *queryGate {
	return &queryGate{
		slots: map[queryRoute]chan struct{}{
			routeInMemory: make(chan struct{}, l.InMemory),
			routeDatabase: make(chan struct{}, l.Database),
		},
		wait:  l.QueueWait,
		clock: c,
	}
}

// noRelease is the release function of a refused acquire: calling it does
// nothing, so `release, ok := g.acquire(...); defer release()` written before
// the ok check does not panic on a refusal.
func noRelease() {}

// acquire takes a slot on route, waiting up to the queue wait for one to free.
// It returns the release function and true, or a no-op release function and
// false when the route is unknown, the context is done, or the wait ran out.
// The release function is safe to call more than once.
func (g *queryGate) acquire(ctx context.Context, route queryRoute) (func(), bool) {
	slots, known := g.slots[route]
	if !known || ctx.Err() != nil {
		return noRelease, false
	}
	select {
	case slots <- struct{}{}:
		return releaseSlot(slots), true
	default:
	}
	if g.wait <= 0 {
		return noRelease, false
	}
	select {
	case slots <- struct{}{}:
		return releaseSlot(slots), true
	case <-g.clock.After(g.wait):
		return noRelease, false
	case <-ctx.Done():
		return noRelease, false
	}
}

func releaseSlot(slots chan struct{}) func() {
	var once sync.Once
	return func() { once.Do(func() { <-slots }) }
}

// maxCacheTTL mirrors the manifest's cache_ttl ceiling.
const maxCacheTTL = 365 * 24 * time.Hour

// cacheFacts is what the cache decision needs to know about one database a
// relational query reads.
type cacheFacts struct {
	// TTL is the database's cache_ttl.
	TTL time.Duration
	// HasAccessPolicies is true when the database has access policies.
	HasAccessPolicies bool
	NoRetention       bool
}

// relationalCacheControl returns the Cache-Control value for a relational
// response. It is public, for the smallest cache_ttl of the databases read,
// only when the server is read-only, auth is off, the method is GET and every
// database has a whole-second TTL above zero (up to a year) and no access
// policies. Any other input, including none, returns no-store.
func relationalCacheControl(readOnly, authEnabled bool, method string, dbs []cacheFacts) string {
	const noStore = "no-store"
	if !readOnly || authEnabled || method != "GET" || len(dbs) == 0 {
		return noStore
	}
	smallest := maxCacheTTL
	for _, db := range dbs {
		if db.NoRetention || db.HasAccessPolicies || db.TTL < time.Second || db.TTL > maxCacheTTL || db.TTL%time.Second != 0 {
			return noStore
		}
		smallest = min(smallest, db.TTL)
	}
	seconds := int(smallest / time.Second)
	return fmt.Sprintf("public, max-age=%d, s-maxage=%d", seconds, seconds)
}
