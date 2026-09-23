// Package server exposes mounted OpenVaultDB databases over the minimal
// HTTP API documented in docs/api.md.
package server

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dal-go/dalgo/access"
	"github.com/openvaultdb/openvaultdb-go/pkg/auth"
	"github.com/openvaultdb/openvaultdb-go/pkg/core"
)

// Server serves one or more mounted databases.
type Server struct {
	version string

	mu  sync.RWMutex // guards dbs and inflight — databases can be mounted at runtime
	dbs map[string]*core.Database
	// inflight counts requests using each mounted database, so Unmount can
	// drain them before closing it.
	inflight   map[*core.Database]*sync.WaitGroup
	inflightMu sync.Mutex // guards inflight writes made under s.mu.RLock

	createMu sync.Mutex // serializes runtime database creation end-to-end

	authCfg             *auth.Config  // nil = auth disabled (local-dev default)
	corsCfg             *CORSConfig   // nil = CORS disabled (no headers added)
	dataDir             string        // "" = runtime database creation disabled
	readOnly            bool          // reject all routes that mutate server or database state
	readCacheTTL        time.Duration // cache lifetime for successful public GET read/query responses; 0 = no cache header
	principalResolver   PrincipalResolver
	accessAuthorization OwnerAuthorization
	explainResolver     MembershipResolver
	accessInstance      string
	logger              *slog.Logger // internal errors; defaults to slog.Default()
	snapshotMu          sync.Mutex
	snapshots           map[string]*querySnapshot // random snapshot id -> disk spool
	snapshotSlots       int                       // includes captures still being built
	snapshotDir         string
	snapshotDirErr      error
	snapshotKey         [32]byte
}

// Option configures the Server.
type Option func(*Server)

// PrincipalResolver maps an authenticated actor to current internal identity
// and memberships. Implementations are trusted server configuration; callers
// cannot supply roles, groups, or policy variables in a DTQL request.
type PrincipalResolver func(context.Context, *auth.Principal) (access.Principal, error)

func WithPrincipalResolver(resolve PrincipalResolver) Option {
	return func(s *Server) { s.principalResolver = resolve }
}

// WithLogger sets the logger internal (HTTP 500) errors are reported to.
// A nil logger keeps the default, slog.Default().
func WithLogger(logger *slog.Logger) Option {
	return func(s *Server) {
		if logger != nil {
			s.logger = logger
		}
	}
}

// WithAuth enables authentication: the connect flow endpoints are served and
// every data/admin request must carry the owner token or a scoped app token.
func WithAuth(cfg *auth.Config) Option {
	return func(s *Server) { s.authCfg = cfg }
}

// WithCORS configures CORS header injection for browser clients.
// A nil cfg disables CORS entirely (zero behavior change, the default).
func WithCORS(cfg *CORSConfig) Option {
	return func(s *Server) { s.corsCfg = cfg }
}

// WithDataDir enables runtime database creation (POST /v1/databases):
// each created database gets a manifest YAML plus an inGitDB data directory
// under dir, so a restart rescan (mount.Dir) remounts them.
func WithDataDir(dir string) Option {
	return func(s *Server) { s.dataDir = dir }
}

// WithReadOnly makes the entire server read-only. It rejects record, database,
// and token mutations before authentication or a route handler can cause a
// side effect. Owner credentials do not bypass this setting.
func WithReadOnly(readOnly bool) Option {
	return func(s *Server) { s.readOnly = readOnly }
}

// WithReadCacheTTL makes successful unauthenticated GET /read and GET /query
// responses cacheable for ttl when WithReadOnly is also enabled. A non-positive
// ttl disables the cache header. Protected databases and authentication-enabled
// servers always remain uncacheable because their response can vary by caller.
func WithReadCacheTTL(ttl time.Duration) Option {
	return func(s *Server) { s.readCacheTTL = ttl }
}

// New creates a Server over mounted databases keyed by database id.
func New(version string, dbs map[string]*core.Database, opts ...Option) *Server {
	if dbs == nil {
		dbs = map[string]*core.Database{}
	}
	s := &Server{version: version, dbs: dbs, inflight: map[*core.Database]*sync.WaitGroup{}, accessInstance: "local", logger: slog.Default()}
	for _, opt := range opts {
		opt(s)
	}
	s.snapshotDir, s.snapshotDirErr = prepareSnapshotDir()
	if _, err := rand.Read(s.snapshotKey[:]); err != nil {
		s.snapshotDirErr = fmt.Errorf("query snapshot token key: %w", err)
	}
	return s
}

// ErrDatabaseMounted is returned by Mount when a database with the same id is
// already mounted.
var ErrDatabaseMounted = errors.New("database already mounted")

// ErrDatabaseNotMounted is returned by Unmount for an unknown database id.
var ErrDatabaseNotMounted = errors.New("database not mounted")

// Mount starts serving db at runtime. It fails with ErrDatabaseMounted when
// its id is taken; the server owns db once Mount succeeds.
func (s *Server) Mount(db *core.Database) error {
	if db == nil {
		return errors.New("mount: nil database")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, taken := s.dbs[db.ID()]; taken {
		return fmt.Errorf("%w: %s", ErrDatabaseMounted, db.ID())
	}
	s.dbs[db.ID()] = db
	return nil
}

// Unmount stops serving database id: new requests get 404 at once, requests
// already using it run to completion, then the database is closed, releasing
// its engine resources (e.g. the SQLite file handle). The Close error, if
// any, is returned; the database is unmounted either way. It waits for
// in-flight requests without limit; see UnmountContext.
func (s *Server) Unmount(id string) error {
	return s.UnmountContext(context.Background(), id)
}

// UnmountContext is Unmount with a bound on the wait for in-flight requests.
// If ctx ends first, the database stays unrouted, requests in flight keep
// running, Close runs in the background once they finish (its error is
// dropped), and ctx.Err() is returned.
func (s *Server) UnmountContext(ctx context.Context, id string) error {
	s.mu.Lock()
	db, ok := s.dbs[id]
	if !ok {
		s.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrDatabaseNotMounted, id)
	}
	delete(s.dbs, id)
	wg := s.inflight[db]
	delete(s.inflight, db)
	s.mu.Unlock()
	s.expireSnapshotsForDB(db)
	// Safe: no lease can be added after the db left the map (acquire holds
	// s.mu.RLock across lookup and Add).
	drained := make(chan struct{})
	closeErr := make(chan error, 1)
	go func() {
		if wg != nil {
			wg.Wait()
		}
		close(drained)
		closeErr <- db.Close()
	}()
	select {
	case <-drained:
		return <-closeErr
	case <-ctx.Done():
		select {
		case <-drained: // drained concurrently: finish synchronously
			return <-closeErr
		default:
			return ctx.Err()
		}
	}
}

// leases records the in-flight counts a request holds; released when the
// request's handler returns.
type leases struct {
	mu   sync.Mutex
	done []func()
}

type leasesKey struct{}

func (l *leases) release() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, done := range l.done {
		done()
	}
	l.done = nil
}

// acquire returns the mounted database id and, when r carries a lease holder
// (every request routed by Handler does), counts r as in flight on it.
func (s *Server) acquire(r *http.Request, id string) *core.Database {
	s.mu.RLock()
	defer s.mu.RUnlock()
	db := s.dbs[id]
	if db == nil {
		return nil
	}
	if l, ok := r.Context().Value(leasesKey{}).(*leases); ok {
		wg := s.inflightFor(db)
		wg.Add(1)
		l.mu.Lock()
		l.done = append(l.done, wg.Done)
		l.mu.Unlock()
	}
	return db
}

// inflightFor returns db's in-flight counter, creating it on first use.
// Callers hold s.mu.RLock; inflightMu serializes concurrent readers, and
// Unmount's exclusive s.mu.Lock excludes them all.
func (s *Server) inflightFor(db *core.Database) *sync.WaitGroup {
	s.inflightMu.Lock()
	defer s.inflightMu.Unlock()
	wg := s.inflight[db]
	if wg == nil {
		wg = &sync.WaitGroup{}
		s.inflight[db] = wg
	}
	return wg
}

// Handler builds the HTTP handler. Middleware order (outermost first):
//  1. CORS (when --cors is set) — short-circuits preflight OPTIONS before auth
//  2. Auth (when --auth is set) — Layer-1 token validation
//  3. Per-handler capability checks — Layer-2
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openvaultdb", s.handleWellKnown)
	mux.HandleFunc("GET /v1/status", s.handleStatus)
	mux.HandleFunc("GET /v1/databases", s.handleDatabases)
	mux.HandleFunc("POST /v1/databases", s.handleDatabaseCreate)
	mux.HandleFunc("GET /v1/databases/{db}", s.handleDatabase)
	mux.HandleFunc("GET /v1/databases/{db}/inferred-schema", s.handleInferredSchema)
	mux.HandleFunc("GET /v1/databases/{db}/read", s.handleRead)
	mux.HandleFunc("/v1/databases/{db}/records/", s.handleRecord) // GET/HEAD/PUT/POST/PATCH/DELETE
	mux.HandleFunc("POST /v1/databases/{db}/batch", s.handleBatch)
	mux.HandleFunc("POST /v1/databases/{db}/query", s.handleQuery)
	mux.HandleFunc("GET /v1/databases/{db}/query", s.handleQuery)
	mux.HandleFunc("POST /v1/databases/{db}/dtql", s.handleDTQL)
	mux.HandleFunc("POST /v1/databases/{db}/access/evaluate", s.handleAccessEvaluate)
	mux.HandleFunc("POST /v1/databases/{db}/access/evidence", s.handleAccessEvidence)
	mux.HandleFunc("GET /v1/databases/{db}/access/layers", s.handleAccessLayers)
	mux.HandleFunc("GET /v1/databases/{db}/access/policies", s.handleAccessPolicies)
	if s.authCfg != nil {
		mux.HandleFunc("GET /authorize", s.handleAuthorizeGet)
		mux.HandleFunc("POST /authorize", s.handleAuthorizePost)
		mux.HandleFunc("POST /token", s.handleToken)
		mux.HandleFunc("POST /v1/tokens", s.handleTokensCreate)
		mux.HandleFunc("GET /v1/tokens", s.handleTokensList)
		mux.HandleFunc("DELETE /v1/tokens/{id}", s.handleTokensRevoke)
	}

	var h http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.principalResolver != nil {
			actor := auth.FromRequest(r)
			if actor != nil {
				principal, err := s.principalResolver(r.Context(), actor)
				if err == nil && principal.Subject != nil {
					err = principal.Subject.Validate()
				}
				if err == nil && principal.Actor != nil {
					err = principal.Actor.Validate()
				}
				if err != nil {
					writeError(w, http.StatusForbidden, "ACCESS_DENIED", "principal resolution failed")
					return
				}
				r = r.WithContext(access.WithPrincipal(r.Context(), principal))
			}
		}
		held := &leases{}
		defer held.release()
		mux.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), leasesKey{}, held)))
	})
	if s.authCfg != nil {
		h = s.authCfg.Middleware(h)
	}
	if s.readOnly {
		next := h
		h = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if isMutation(r) {
				writeError(w, http.StatusForbidden, "read_only", "server is read-only")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
	// Default the URL-query read forms to no-store before authentication or
	// database lookup. A successful public read may replace this with its
	// configured TTL in cacheReadResponse.
	next := h
	h = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isReadCacheEndpoint(r) {
			w.Header().Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
	if s.corsCfg != nil {
		h = corsMiddleware(s.corsCfg, h)
	}
	return h
}

func isReadCacheEndpoint(r *http.Request) bool {
	return (r.Method == http.MethodGet || r.Method == http.MethodHead) &&
		strings.HasPrefix(r.URL.Path, "/v1/databases/") &&
		(strings.HasSuffix(r.URL.Path, "/read") || strings.HasSuffix(r.URL.Path, "/query"))
}

// isMutation identifies every mounted route that persists data or changes
// server/token state. POST access inspection endpoints deliberately remain
// available: they only evaluate or inspect existing state.
func isMutation(r *http.Request) bool {
	switch r.Method {
	case http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	case http.MethodPost:
		path := r.URL.Path
		return path == "/v1/databases" ||
			path == "/authorize" || path == "/token" || path == "/v1/tokens" ||
			strings.Contains(path, "/records/") || strings.HasSuffix(path, "/batch")
	default:
		return false
	}
}

// authorize enforces a capability for the request (Layer 2). Always true
// when auth is disabled. Writes the 403 response when denied.
func (s *Server) authorize(w http.ResponseWriter, r *http.Request, databaseID, action, collection string) bool {
	if s.authCfg == nil {
		return true
	}
	if auth.FromRequest(r).Allows(databaseID, action, collection) {
		return true
	}
	writeError(w, http.StatusForbidden, "forbidden",
		"token does not grant "+action+" on database "+databaseID)
	return false
}

// isOwner reports whether the request is made with the owner token (or auth
// is disabled, in which case every caller has owner-level access).
func (s *Server) isOwner(r *http.Request) bool {
	if s.authCfg == nil {
		return true
	}
	p := auth.FromRequest(r)
	return p != nil && p.Owner
}

func (s *Server) db(w http.ResponseWriter, r *http.Request) *core.Database {
	id := r.PathValue("db")
	db := s.acquire(r, id)
	if db == nil {
		writeError(w, http.StatusNotFound, "not_found", "database not found: "+id)
		return nil
	}
	return db
}

// getDB returns the mounted database with the given id, or nil.
func (s *Server) getDB(id string) *core.Database {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.dbs[id]
}

func (s *Server) databaseIDs() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ids := make([]string, 0, len(s.dbs))
	for id := range s.dbs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	status := map[string]any{
		"name":    "OpenVaultDB",
		"version": s.version,
	}
	// The database list is owner-level information once auth is on.
	if s.isOwner(r) {
		status["databases"] = s.databaseIDs()
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) handleDatabases(w http.ResponseWriter, r *http.Request) {
	if !s.isOwner(r) {
		writeError(w, http.StatusForbidden, "forbidden", "owner token required")
		return
	}
	type dbInfo struct {
		ID         string `json:"id"`
		Engine     string `json:"engine"`
		SchemaMode string `json:"schemaMode"`
	}
	ids := s.databaseIDs()
	infos := make([]dbInfo, 0, len(ids))
	for _, id := range ids {
		db := s.getDB(id)
		if db == nil {
			continue
		}
		infos = append(infos, dbInfo{
			ID:         id,
			Engine:     db.Manifest.Storage.Engine,
			SchemaMode: string(db.Manifest.Database.SchemaMode),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"databases": infos})
}

func (s *Server) handleDatabase(w http.ResponseWriter, r *http.Request) {
	db := s.db(w, r)
	if db == nil {
		return
	}
	if !s.authorize(w, r, db.ID(), auth.CapCollectionsRead, "") {
		return
	}
	if db.HasAccessPolicies() {
		writeError(w, 422, "authorization_unsupported", "filtered schema discovery is unavailable for this profile")
		return
	}
	collections, err := db.Collections(r.Context())
	if err != nil {
		s.writeMappedError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":          db.ID(),
		"engine":      db.Manifest.Storage.Engine,
		"schemaMode":  string(db.Manifest.Database.SchemaMode),
		"collections": collections,
	})
}

func (s *Server) handleInferredSchema(w http.ResponseWriter, r *http.Request) {
	db := s.db(w, r)
	if db == nil {
		return
	}
	if !s.authorize(w, r, db.ID(), auth.CapSchemaRead, "") {
		return
	}
	if db.HasAccessPolicies() {
		writeError(w, 422, "authorization_unsupported", "filtered schema discovery is unavailable for this profile")
		return
	}
	snapshot := db.InferredSnapshot()
	if snapshot == nil {
		writeError(w, http.StatusNotFound, "not_found",
			"database "+db.ID()+" is strict; no inferred schema catalogue is maintained")
		return
	}
	writeJSON(w, http.StatusOK, snapshot)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
