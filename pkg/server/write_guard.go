package server

import (
	"errors"
	"maps"
	"net/http"
	"slices"

	az "github.com/dal-go/dalgo/dtql/authorization"

	api "github.com/openvaultdb/openvaultdb-go/pkg/authorizationapi"
	"github.com/openvaultdb/openvaultdb-go/pkg/core"
)

// guardOperation refuses an operation of the authorization API before it
// reaches the coordinator that reads or writes the adapter by key (protected
// execution, inspection, evidence and sampling): a table the database does not
// declare when its adapter builds SQL (core.Database.GuardCollection, an error
// wrapping core.ErrNotFound), and any field name that is not plain
// (core.ErrInvalidFieldName, see core.Database.ValidateFieldPath for the later
// segments of a path): the operation's columns, the top-level keys of an insert
// or set payload and the path of every change of an update. The caller answers
// the error with refuseOperation. The operation must have been normalized,
// which derives its table.
func guardOperation(db *core.Database, op api.Operation) error {
	if err := db.GuardCollection(op.Resource.Table); err != nil {
		return err
	}
	for _, column := range op.Resource.Columns {
		if err := db.ValidateFieldPath(column); err != nil {
			return err
		}
	}
	if m := op.Mutation; m != nil {
		if err := core.ValidateFieldNames(slices.Sorted(maps.Keys(m.Data))); err != nil {
			return err
		}
		for _, change := range m.Changes {
			if err := db.ValidateFieldPath(change.Path); err != nil {
				return err
			}
		}
	}
	return nil
}

// refuseOperation answers a refusal of guardOperation. Every caller needs the
// coordinator of a mount with access policies, and such a mount hides which
// tables it declares (it refuses schema discovery). A table the database does
// not declare is therefore answered as GET answers a hidden record: 404
// resource_unavailable, redacted, with no message that names the table. Any
// other refusal is mapped as usual (a field name that is not plain is a 400).
func (s *Server) refuseOperation(w http.ResponseWriter, r *http.Request, mode az.Mode, op api.Operation, err error) {
	if errors.Is(err, core.ErrNotFound) {
		writeUnavailableOperation(w, mode, op)
		return
	}
	s.writeMappedError(w, r, err)
}
