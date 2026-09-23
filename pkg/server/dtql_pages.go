package server

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
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
	db        *core.Database
	queryHash [32]byte
	actorHash [32]byte
	pageSize  int
	id        string
	secret    [32]byte
	expiresAt time.Time
	timer     *time.Timer
}

func newSnapshotIdentity() (string, [32]byte, error) {
	var id, secret [32]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", secret, err
	}
	if _, err := rand.Read(secret[:]); err != nil {
		return "", secret, err
	}
	return base64.RawURLEncoding.EncodeToString(id[:]), secret, nil
}

// A signed offset makes every page token stable and retryable without keeping
// a token map proportional to the number of pages.
func pageToken(snap *querySnapshot, offset int64) string {
	position := strconv.FormatInt(offset, 10)
	mac := hmac.New(sha256.New, snap.secret[:])
	_, _ = mac.Write([]byte(snap.id + "." + position))
	return snap.id + "." + position + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)[:16])
}

func pageOffset(snap *querySnapshot, token string) (int64, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != snap.id {
		return 0, false
	}
	offset, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || offset < 0 || offset > snap.bytes {
		return 0, false
	}
	want := pageToken(snap, offset)
	if subtle.ConstantTimeCompare([]byte(want), []byte(token)) != 1 {
		return 0, false
	}
	return offset, true
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
	id, secret, err := newSnapshotIdentity()
	if err != nil {
		_ = os.Remove(path)
		s.writeInternalError(w, r, "failed to create query snapshot", err)
		return
	}
	snap := &querySnapshot{path: path, bytes: size, db: db, queryHash: queryHash,
		actorHash: actorHash, pageSize: pageSize, id: id, secret: secret, expiresAt: time.Now().Add(snapshotLifetime)}
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
	s.snapshots[id] = snap
	snap.timer = time.AfterFunc(snapshotLifetime, func() { s.expireSnapshot(snap) })
	s.snapshotMu.Unlock()
	s.mu.RUnlock()
	reserved = false
	s.serveSnapshotPage(w, pageToken(snap, 0), db, queryHash, actorHash, pageSize)
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
	if s.snapshots[snap.id] == snap {
		s.removeSnapshotLocked(snap)
	}
}

// A token identifies a fixed byte offset in the immutable spool, so retries
// return the same page. Hold snapshotMu only for lookup and bounded disk read;
// release it before the network write.
func (s *Server) serveSnapshotPage(w http.ResponseWriter, token string, db *core.Database, queryHash, actorHash [32]byte, pageSize int) {
	s.snapshotMu.Lock()
	parts := strings.Split(token, ".")
	var snap *querySnapshot
	if len(parts) == 3 {
		snap = s.snapshots[parts[0]]
	}
	if snap == nil || time.Now().After(snap.expiresAt) {
		if snap != nil {
			s.removeSnapshotLocked(snap)
		}
		s.snapshotMu.Unlock()
		writeError(w, http.StatusGone, "snapshot_expired", "query snapshot expired; restart the query")
		return
	}
	offset, valid := pageOffset(snap, token)
	if !valid {
		s.snapshotMu.Unlock()
		writeError(w, http.StatusGone, "snapshot_expired", "query page token is invalid; restart the query")
		return
	}
	if snap.db != db || snap.actorHash != actorHash {
		s.snapshotMu.Unlock()
		writeError(w, http.StatusForbidden, "forbidden", "query snapshot belongs to another database or credential")
		return
	}
	if snap.queryHash != queryHash || snap.pageSize != pageSize {
		s.snapshotMu.Unlock()
		writeError(w, http.StatusBadRequest, "bad_request", "query or page size does not match the snapshot")
		return
	}
	f, err := os.Open(snap.path)
	if err != nil {
		s.removeSnapshotLocked(snap)
		s.snapshotMu.Unlock()
		writeError(w, http.StatusGone, "snapshot_expired", "query snapshot is unavailable; restart the query")
		return
	}
	if _, err = f.Seek(offset, io.SeekStart); err != nil {
		_ = f.Close()
		s.removeSnapshotLocked(snap)
		s.snapshotMu.Unlock()
		writeError(w, http.StatusGone, "snapshot_expired", "query snapshot is unavailable; restart the query")
		return
	}
	reader := bufio.NewReader(f)
	rows := make([]json.RawMessage, 0, pageSize)
	var pageBytes int
	for len(rows) < pageSize && offset < snap.bytes {
		row, readErr := reader.ReadBytes('\n')
		if readErr != nil {
			_ = f.Close()
			s.removeSnapshotLocked(snap)
			s.snapshotMu.Unlock()
			writeError(w, http.StatusGone, "snapshot_expired", "query snapshot is damaged; restart the query")
			return
		}
		if pageBytes+len(row) > maxPageBytes && len(rows) > 0 {
			break
		}
		rows = append(rows, json.RawMessage(row[:len(row)-1]))
		pageBytes += len(row)
		offset += int64(len(row))
	}
	_ = f.Close()
	response := map[string]any{"records": rows}
	if offset < snap.bytes {
		response["nextPageToken"] = pageToken(snap, offset)
		response["snapshotExpiresAt"] = snap.expiresAt.UTC().Format(time.RFC3339)
	}
	s.snapshotMu.Unlock()
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) removeSnapshotLocked(snap *querySnapshot) {
	delete(s.snapshots, snap.id)
	if snap.timer != nil {
		snap.timer.Stop()
	}
	s.snapshotSlots--
	if err := os.Remove(snap.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		s.logger.Warn("failed to remove query snapshot", "error", fmt.Errorf("remove temporary file: %w", err))
	}
}
