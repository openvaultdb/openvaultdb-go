package server

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/dal-go/dalgo/access"
	az "github.com/dal-go/dalgo/dtql/authorization"

	api "github.com/openvaultdb/openvaultdb-go/pkg/authorizationapi"
	"github.com/openvaultdb/openvaultdb-go/pkg/core"
)

// guardOperation refuses an operation of the authorization API before it
// reaches the coordinator that reads or writes the adapter by key (protected
// execution, inspection and evidence), and returns the protected operation it
// builds for the coordinator. The checks run in one order, and each depends on
// the request alone until the last: any field name that is not plain
// (core.ErrInvalidFieldName, see guardOperationFields), then whether the
// protected session supports the operation (an error wrapping
// errProtectedUnsupported: an execution class other than dtql, an action or a
// change it cannot carry), and last a table that is a declared collection named
// by its canonical name when the adapter builds SQL
// (core.Database.GuardCanonicalCollection, an error wrapping core.ErrNotFound).
// The table comes last so that a table the database does not declare gets the
// answer a declared table gets for the same operation. The caller answers the
// error. The operation must have been normalized, which derives its table.
func guardOperation(db *core.Database, op api.Operation) (access.ProtectedOperation, error) {
	if err := guardOperationFields(db, op); err != nil {
		return access.ProtectedOperation{}, err
	}
	internal, err := protectedOperation(op)
	if err != nil {
		if !errors.Is(err, errProtectedUnsupported) {
			err = fmt.Errorf("%w: %v", errProtectedUnsupported, err)
		}
		return access.ProtectedOperation{}, err
	}
	return internal, db.GuardCanonicalCollection(op.Resource.Table)
}

// guardOperationFields refuses an operation that carries a field name that is
// not plain (core.ErrInvalidFieldName): the operation's columns, the top-level
// keys of an insert or set payload (which on an engine that builds SQL must not
// be the record's key column in a case other than "id": core.Database.ValidateDataKeys,
// core.ErrKeyColumnCase) and the path of every change of an update, which on such
// an engine must not name the record's key column either
// (core.Database.ValidateUpdatePath, core.ErrKeyUpdate). The rule depends
// on the engine and not on the table, so it holds for a table the database does
// not declare as it does for a declared one.
func guardOperationFields(db *core.Database, op api.Operation) error {
	for _, column := range op.Resource.Columns {
		if err := db.ValidateFieldPath(column); err != nil {
			return err
		}
	}
	if m := op.Mutation; m != nil {
		if err := db.ValidateDataKeys(m.Data); err != nil {
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
// only the rest here.) An operation the protected session does not support is
// 422 authorization_unsupported with a message that names nothing of the request.
// Any other refusal is mapped as usual (a field name that is not plain, or that
// names the key column, is a 400).
func (s *Server) refuseOperation(w http.ResponseWriter, r *http.Request, mode az.Mode, op api.Operation, err error) {
	switch {
	case errors.Is(err, core.ErrNotFound):
		writeUnavailableOperation(w, mode, op)
	case errors.Is(err, errProtectedUnsupported):
		writeError(w, 422, "authorization_unsupported", errProtectedUnsupported.Error())
	default:
		s.writeMappedError(w, r, err)
	}
}
