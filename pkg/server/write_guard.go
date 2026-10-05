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
// execution, inspection, evidence and sampling): any field name that is not
// plain (core.ErrInvalidFieldName, see core.Database.ValidateFieldPath for the
// later segments of a path), checked as guardOperationFields says, and then a
// table that is not a declared collection named by its canonical name when the
// adapter builds SQL (core.Database.GuardCanonicalCollection, an error wrapping
// core.ErrNotFound). The fields are checked first, so a table the database does
// not declare gets the answer a declared table gets for the same fields. The
// caller answers the error with refuseOperation. The operation must have been
// normalized, which derives its table.
func guardOperation(db *core.Database, op api.Operation) error {
	if err := guardOperationFields(db, op); err != nil {
		return err
	}
	return db.GuardCanonicalCollection(op.Resource.Table)
}

// guardOperationFields refuses an operation that carries a field name that is
// not plain (core.ErrInvalidFieldName): the operation's columns, the top-level
// keys of an insert or set payload and the path of every change of an update,
// which on an engine that builds SQL must not name the record's key column
// either (core.Database.ValidateUpdatePath, core.ErrKeyUpdate). The rule depends
// on the engine and not on the table, so it holds for a table the database does
// not declare as it does for a declared one.
func guardOperationFields(db *core.Database, op api.Operation) error {
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
			if err := db.ValidateUpdatePath(change.Path); err != nil {
				return err
			}
		}
	}
	return nil
}

// refuseOperation answers a refusal of guardOperation or guardOperationFields.
// Every caller needs the coordinator of a mount with access policies, and such a
// mount hides which tables it declares (it refuses schema discovery). A table the
// database does not declare is therefore answered as the protected PATCH and the
// evidence route answer a hidden record: 404 resource_unavailable, redacted, with
// no message that names the table. (Inspection and sampling answer such a table
// with the redacted deny they give a declared table the policy hides, and refuse
// only the field names here.) Any other refusal is mapped as usual (a field name
// that is not plain, or that names the key column, is a 400).
func (s *Server) refuseOperation(w http.ResponseWriter, r *http.Request, mode az.Mode, op api.Operation, err error) {
	if errors.Is(err, core.ErrNotFound) {
		writeUnavailableOperation(w, mode, op)
		return
	}
	s.writeMappedError(w, r, err)
}
