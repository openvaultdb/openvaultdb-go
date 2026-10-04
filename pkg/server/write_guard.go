package server

import (
	"maps"
	"slices"

	api "github.com/openvaultdb/openvaultdb-go/pkg/authorizationapi"
	"github.com/openvaultdb/openvaultdb-go/pkg/core"
)

// guardOperation refuses an operation of the authorization API before it
// reaches the coordinator that reads or writes the adapter by key (protected
// execution, inspection, evidence and sampling): a table the database does not
// declare when its adapter builds SQL (core.Database.GuardCollection, an error
// wrapping core.ErrNotFound), and any field name that is not plain
// (core.ErrInvalidFieldName): the operation's columns, the top-level keys of an
// insert or set payload and the path of every change of an update. Both errors
// go through writeMappedError. The operation must have been normalized, which
// derives its table.
func guardOperation(db *core.Database, op api.Operation) error {
	if err := db.GuardCollection(op.Resource.Table); err != nil {
		return err
	}
	for _, column := range op.Resource.Columns {
		if err := core.ValidateFieldPath(column); err != nil {
			return err
		}
	}
	if m := op.Mutation; m != nil {
		if err := core.ValidateFieldNames(slices.Sorted(maps.Keys(m.Data))); err != nil {
			return err
		}
		for _, change := range m.Changes {
			if err := core.ValidateFieldPath(change.Path); err != nil {
				return err
			}
		}
	}
	return nil
}
