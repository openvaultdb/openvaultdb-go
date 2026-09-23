package server

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/dal-go/dalgo/dal"
	"github.com/openvaultdb/openvaultdb-go/pkg/core"
)

const (
	maxSnapshotSlots = 2 // with maxSnapshotBytes: at most 1 GiB globally
	maxSnapshotBytes = 512 << 20
	maxSnapshotRows  = 1_000_000
	maxPageBytes     = 7 << 20
	snapshotLifetime = 5 * time.Minute
)

// querySnapshot contains no live reader or transaction. Its complete result
// was captured into a private temporary file before the first page was sent.
type querySnapshot struct {
	path      string
	bytes     int64
	offset    int64
	db        *core.Database
	queryHash [32]byte
	actorHash [32]byte
	pageSize  int
	token     string
	expiresAt time.Time
	timer     *time.Timer
}

func snapshotToken() (string, error) {
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(secret[:]), nil
}

func (s *Server) handlePagedDTQL(w http.ResponseWriter, r *http.Request, db *core.Database, query dal.StructuredQuery, doc []byte) {
	w.Header().Set("Cache-Control", "no-store")
	if db.HasAccessPolicies() {
		writeError(w, http.StatusUnprocessableEntity, "snapshot_unsupported", "consistent result paging is unavailable for policy-protected databases")
		return
	}
	pageSize, err := strconv.Atoi(r.Header.Get("OVDB-Page-Size"))
	if err != nil || pageSize < 1 || pageSize > 1000 {
		writeError(w, http.StatusBadRequest, "bad_request", "OVDB-Page-Size must be 1..1000")
		return
	}
	if query.Limit() != 0 || query.Offset() != 0 {
		writeError(w, http.StatusBadRequest, "invalid_dtql", "paged DTQL requires limit and offset to be zero")
		return
	}
	queryHash := sha256.Sum256(doc)
	actorHash := sha256.Sum256([]byte(r.Header.Get("Authorization")))
	if token := r.Header.Get("OVDB-Page-Token"); token != "" {
		s.serveSnapshotPage(w, token, db, queryHash, actorHash, pageSize)
		return
	}
	if !s.reserveSnapshotSlot() {
		writeError(w, http.StatusServiceUnavailable, "snapshot_capacity", "too many active query snapshots")
		return
	}
	reserved := true
	defer func() {
		if reserved {
			s.releaseSnapshotSlot()
		}
	}()
	if s.snapshotDirErr != nil {
		s.writeInternalError(w, r, "query snapshot storage is unavailable", s.snapshotDirErr)
		return
	}
	path, size, err := spoolDTQL(r.Context(), db, query, s.snapshotDir)
	if err != nil {
		var bound *snapshotBoundError
		if errors.As(err, &bound) {
			writeError(w, http.StatusRequestEntityTooLarge, "snapshot_too_large", bound.Error())
		} else {
			s.writeMappedError(w, r, err)
		}
		return
	}
	token, err := snapshotToken()
	if err != nil {
		_ = os.Remove(path)
		s.writeInternalError(w, r, "failed to create query snapshot", err)
		return
	}
	snap := &querySnapshot{path: path, bytes: size, db: db, queryHash: queryHash,
		actorHash: actorHash, pageSize: pageSize, token: token, expiresAt: time.Now().Add(snapshotLifetime)}
	s.mu.RLock()
	if s.dbs[db.ID()] != db {
		s.mu.RUnlock()
		_ = os.Remove(path)
		writeError(w, http.StatusGone, "snapshot_expired", "database was unmounted during snapshot capture")
		return
	}
	s.snapshotMu.Lock()
	if s.snapshots == nil {
		s.snapshots = make(map[string]*querySnapshot)
	}
	s.snapshots[token] = snap
	snap.timer = time.AfterFunc(snapshotLifetime, func() { s.expireSnapshot(snap) })
	s.snapshotMu.Unlock()
	s.mu.RUnlock()
	reserved = false
	s.serveSnapshotPage(w, token, db, queryHash, actorHash, pageSize)
}

type snapshotBoundError string

func (e *snapshotBoundError) Error() string { return string(*e) }

func spoolDTQL(ctx context.Context, db *core.Database, query dal.StructuredQuery, dir string) (path string, size int64, err error) {
	f, err := os.CreateTemp(dir, "snapshot-*")
	if err != nil {
		return "", 0, err
	}
	defer func() {
		if closeErr := f.Close(); err == nil && closeErr != nil {
			err = closeErr
		}
		if err != nil {
			_ = os.Remove(f.Name())
		}
	}()
	w := bufio.NewWriterSize(f, 64<<10)
	rows := 0
	err = db.StreamDTQLSnapshot(ctx, query, func(rec core.Record) error {
		row, marshalErr := json.Marshal(struct {
			Key  string         `json:"key"`
			Data map[string]any `json:"data,omitempty"`
		}{Key: rec.Key.String(), Data: rec.Data})
		if marshalErr != nil {
			return marshalErr
		}
		if len(row) > maxPageBytes {
			bound := snapshotBoundError("one query row exceeds the page size limit")
			return &bound
		}
		if rows >= maxSnapshotRows || size+int64(len(row))+1 > maxSnapshotBytes {
			bound := snapshotBoundError("query snapshot exceeds its row or disk size limit")
			return &bound
		}
		if _, writeErr := w.Write(row); writeErr != nil {
			return writeErr
		}
		if writeErr := w.WriteByte('\n'); writeErr != nil {
			return writeErr
		}
		size += int64(len(row)) + 1
		rows++
		return nil
	})
	if err != nil {
		return "", 0, err
	}
	if err = w.Flush(); err != nil {
		return "", 0, err
	}
	return f.Name(), size, nil
}

// prepareSnapshotDir uses a private stable directory so a new server can reap
// files left by a crash. Active captures last at most one minute and live
// snapshots at most five; the extra minute avoids touching an active file.
func prepareSnapshotDir() (string, error) {
	dir := filepath.Join(os.TempDir(), "ovdb-query-snapshots-"+strconv.Itoa(os.Getuid()))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		return "", fmt.Errorf("query snapshot directory is not private")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	cutoff := time.Now().Add(-snapshotLifetime - time.Minute)
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "snapshot-") || !entry.Type().IsRegular() {
			continue
		}
		fileInfo, statErr := entry.Info()
		if statErr == nil && fileInfo.ModTime().Before(cutoff) {
			_ = os.Remove(filepath.Join(dir, entry.Name()))
		}
	}
	return dir, nil
}

// CloseSnapshots removes all materialized query results. Call after stopping
// HTTP serving and draining in-flight requests during shutdown.
func (s *Server) CloseSnapshots() {
	s.snapshotMu.Lock()
	defer s.snapshotMu.Unlock()
	for _, snap := range s.snapshots {
		s.removeSnapshotLocked(snap)
	}
}

func (s *Server) expireSnapshotsForDB(db *core.Database) {
	s.snapshotMu.Lock()
	defer s.snapshotMu.Unlock()
	for _, snap := range s.snapshots {
		if snap.db == db {
			s.removeSnapshotLocked(snap)
		}
	}
}

func (s *Server) reserveSnapshotSlot() bool {
	s.snapshotMu.Lock()
	defer s.snapshotMu.Unlock()
	if s.snapshotSlots >= maxSnapshotSlots {
		return false
	}
	s.snapshotSlots++
	return true
}

func (s *Server) releaseSnapshotSlot() {
	s.snapshotMu.Lock()
	s.snapshotSlots--
	s.snapshotMu.Unlock()
}

func (s *Server) expireSnapshot(snap *querySnapshot) {
	s.snapshotMu.Lock()
	defer s.snapshotMu.Unlock()
	if s.snapshots[snap.token] == snap {
		s.removeSnapshotLocked(snap)
	}
}

// Caller holds snapshotMu, including during the short disk read. The token is
// single-use: a retry after an uncertain response returns explicit 410 and
// the caller can restart the query instead of mixing two result sets.
func (s *Server) serveSnapshotPage(w http.ResponseWriter, token string, db *core.Database, queryHash, actorHash [32]byte, pageSize int) {
	s.snapshotMu.Lock()
	defer s.snapshotMu.Unlock()
	snap := s.snapshots[token]
	if snap == nil || time.Now().After(snap.expiresAt) {
		if snap != nil {
			s.removeSnapshotLocked(snap)
		}
		writeError(w, http.StatusGone, "snapshot_expired", "query snapshot expired; restart the query")
		return
	}
	if snap.db != db || snap.actorHash != actorHash {
		writeError(w, http.StatusForbidden, "forbidden", "query snapshot belongs to another database or credential")
		return
	}
	if snap.queryHash != queryHash || snap.pageSize != pageSize {
		writeError(w, http.StatusBadRequest, "bad_request", "query or page size does not match the snapshot")
		return
	}
	f, err := os.Open(snap.path)
	if err != nil {
		s.removeSnapshotLocked(snap)
		writeError(w, http.StatusGone, "snapshot_expired", "query snapshot is unavailable; restart the query")
		return
	}
	defer func() { _ = f.Close() }()
	if _, err = f.Seek(snap.offset, io.SeekStart); err != nil {
		s.removeSnapshotLocked(snap)
		writeError(w, http.StatusGone, "snapshot_expired", "query snapshot is unavailable; restart the query")
		return
	}
	reader := bufio.NewReader(f)
	rows := make([]json.RawMessage, 0, pageSize)
	var pageBytes int
	for len(rows) < pageSize && snap.offset < snap.bytes {
		row, readErr := reader.ReadBytes('\n')
		if readErr != nil {
			s.removeSnapshotLocked(snap)
			writeError(w, http.StatusGone, "snapshot_expired", "query snapshot is damaged; restart the query")
			return
		}
		if pageBytes+len(row) > maxPageBytes && len(rows) > 0 {
			break
		}
		rows = append(rows, json.RawMessage(row[:len(row)-1]))
		pageBytes += len(row)
		snap.offset += int64(len(row))
	}
	response := map[string]any{"records": rows}
	if snap.offset < snap.bytes {
		next, tokenErr := snapshotToken()
		if tokenErr != nil {
			s.removeSnapshotLocked(snap)
			writeError(w, http.StatusInternalServerError, "internal", "failed to continue query snapshot")
			return
		}
		delete(s.snapshots, snap.token)
		snap.token = next
		s.snapshots[next] = snap
		response["nextPageToken"] = next
		response["snapshotExpiresAt"] = snap.expiresAt.UTC().Format(time.RFC3339)
	} else {
		s.removeSnapshotLocked(snap)
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) removeSnapshotLocked(snap *querySnapshot) {
	delete(s.snapshots, snap.token)
	if snap.timer != nil {
		snap.timer.Stop()
	}
	s.snapshotSlots--
	if err := os.Remove(snap.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		s.logger.Warn("failed to remove query snapshot", "error", fmt.Errorf("remove temporary file: %w", err))
	}
}
