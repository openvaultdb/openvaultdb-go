package server

import (
	"fmt"
	"maps"
	"net/http"

	"github.com/dal-go/dalgo/dal"
	"github.com/openvaultdb/openvaultdb-go/pkg/core"
)

// BoundedImmutable is the closed profile for immutable, keyed SQLite lookup pages.
const BoundedImmutable = "bounded-immutable/1"

// ReadProfile is trusted per-instance configuration from checked runtime inventory.
// PublishedQuery controls discovery independently of guarded candidate execution.
type ReadProfile struct {
	Kind               string
	AllowOrdinaryQuery bool
	PublishedQuery     bool
}

// WithDatabaseReadProfiles configures immutable lookup policy per database. The
// map is copied. NewChecked reports invalid configuration before publication;
// New retains its signature and serves configuration_error on invalid options.
func WithDatabaseReadProfiles(profiles map[string]ReadProfile) Option {
	frozen := maps.Clone(profiles)
	return func(s *Server) { s.readProfiles = maps.Clone(frozen) }
}

// NewChecked is New with startup validation of optional read profiles.
func NewChecked(version string, dbs map[string]*core.Database, opts ...Option) (*Server, error) {
	s := New(version, dbs, opts...)
	if s.readProfileErr != nil {
		s.CloseSnapshots()
		return nil, s.readProfileErr
	}
	return s, nil
}

func (s *Server) validateReadProfiles() error {
	s.readProfilesByDB = make(map[*core.Database]ReadProfile)
	s.retiringProfileDBs = make(map[*core.Database]bool)
	s.readProfileRemounts = make(map[string]ReadProfile)
	for id, profile := range s.readProfiles {
		if profile.Kind != BoundedImmutable {
			return fmt.Errorf("database %q has unsupported read profile", id)
		}
		if profile.PublishedQuery && !profile.AllowOrdinaryQuery {
			return fmt.Errorf("database %q publishes a disabled ordinary query", id)
		}
		db := s.dbs[id]
		if db == nil || !s.readOnly || !db.HasImmutableSQLiteKeys() {
			return fmt.Errorf("database %q read profile requires a read-only SQLite instance with complete verified keys and zero lock wait", id)
		}
		if existing, ok := s.readProfilesByDB[db]; ok && existing != profile {
			return fmt.Errorf("database %q has conflicting read profiles across mount aliases", id)
		}
		if existing, ok := s.readProfileRemounts[db.ID()]; ok && existing != profile {
			return fmt.Errorf("database %q has conflicting read profiles for remount identity", id)
		}
		s.readProfilesByDB[db] = profile
		s.readProfileRemounts[db.ID()] = profile
	}
	for id, profile := range s.readProfiles {
		if remount, ok := s.readProfileRemounts[id]; ok && remount != profile {
			return fmt.Errorf("database %q has conflicting mount and remount profiles", id)
		}
	}
	return nil
}

// Profiles bind to the verified instance, including every server mount alias.
// Retain bindings while an unmounted instance drains its leased requests.
func (s *Server) databaseReadProfile(db *core.Database) (ReadProfile, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	profile, ok := s.readProfilesByDB[db]
	return profile, ok
}

func (s *Server) boundedImmutable(db *core.Database) bool {
	_, ok := s.databaseReadProfile(db)
	return ok
}

func (s *Server) advertisesOrdinaryQuery(db *core.Database) bool {
	if db == nil || !db.CanQuery() || s.readProfileErr != nil {
		return false
	}
	profile, configured := s.databaseReadProfile(db)
	return !configured || (profile.AllowOrdinaryQuery && profile.PublishedQuery)
}

// stageReadProfile participates in the same chain used by relational execution
// and discovery, using resolved databases from the existing classifier.
func stageReadProfile(s *Server, id string, db *core.Database) error {
	if s.boundedImmutable(db) {
		return fmt.Errorf("database %q permits only ordinary immutable lookup pages", id)
	}
	return nil
}

type servingOrderQuery struct {
	dal.StructuredQuery
	key string
}

func (q servingOrderQuery) OrderBy() []dal.OrderExpression {
	return []dal.OrderExpression{dal.AscendingField(q.key)}
}
func (q servingOrderQuery) String() string {
	return fmt.Sprintf("%s ORDER BY serving key %q ASC", q.StructuredQuery.String(), q.key)
}

func (s *Server) prepareImmutableQuery(w http.ResponseWriter, r *http.Request, db *core.Database, query dal.StructuredQuery, collection string) (dal.StructuredQuery, bool) {
	profile, configured := s.databaseReadProfile(db)
	if !configured {
		return query, true
	}
	if !profile.AllowOrdinaryQuery {
		writeError(w, http.StatusUnprocessableEntity, "query_unsupported", "ordinary queries are disabled")
		return nil, false
	}
	if len(query.OrderBy()) > 0 {
		writeError(w, http.StatusBadRequest, "ordering_unsupported", "immutable lookup pages use server-owned serving-key order")
		return nil, false
	}
	for _, header := range pagingHeaders {
		if r.Header.Get(header) != "" {
			writeError(w, http.StatusUnprocessableEntity, "snapshot_unsupported", "immutable lookups use explicit ordinary limit and offset pages")
			return nil, false
		}
	}
	if !readableCollection(db, collection) {
		writeError(w, http.StatusNotFound, "not_found", "collection not found")
		return nil, false
	}
	key, ok := db.ServingKey(collection)
	if !ok {
		writeError(w, http.StatusInternalServerError, "configuration_error", "immutable serving key unavailable")
		return nil, false
	}
	return servingOrderQuery{StructuredQuery: query, key: key}, true
}
