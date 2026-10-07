package core

import (
	"fmt"
	"github.com/dal-go/dalgo/dal"
)

// ErrHTTPOperationUnsupported identifies a safe, pre-read refusal of a live
// resource capability. Adapter details and source values are never reflected.
var ErrHTTPOperationUnsupported = fmt.Errorf("%w: HTTP source operation unsupported", dal.ErrNotSupported)

// ErrNativePostgresOperationUnsupported identifies a keyed or mutating
// operation that the native PostgreSQL catalog mount does not expose.
var ErrNativePostgresOperationUnsupported = fmt.Errorf("%w: native PostgreSQL mount supports structured reads only", dal.ErrNotSupported)

// ReadOnlyHTTP reports the fixed source capability captured by Open. Such a
// source supports bounded queries, but not keyed reads, writes or DDL.
func (d *Database) ReadOnlyHTTP() bool { return d.readOnlyHTTP }

// HTTPProfile is captured when the mount opens; editing exported Manifest
// metadata cannot change the source profile used by server admission guards.
func (d *Database) HTTPProfile() string { return d.httpProfile }

// ReadOnly reports mounts that expose neither keyed record reads nor writes.
func (d *Database) ReadOnly() bool { return d.readOnlyHTTP || d.nativePostgres }

func (d *Database) readOnlyOperationError() error {
	if d.nativePostgres {
		return ErrNativePostgresOperationUnsupported
	}
	return ErrHTTPOperationUnsupported
}
