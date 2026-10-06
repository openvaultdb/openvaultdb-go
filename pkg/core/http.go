package core

import (
	"fmt"
	"github.com/dal-go/dalgo/dal"
)

// ErrHTTPOperationUnsupported identifies a safe, pre-read refusal of a live
// resource capability. Adapter details and source values are never reflected.
var ErrHTTPOperationUnsupported = fmt.Errorf("%w: HTTP source operation unsupported", dal.ErrNotSupported)

// ReadOnlyHTTP reports the fixed source capability captured by Open. Such a
// source supports bounded queries, but not keyed reads, writes or DDL.
func (d *Database) ReadOnlyHTTP() bool { return d.readOnlyHTTP }
