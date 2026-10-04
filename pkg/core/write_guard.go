package core

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/dal-go/record"
)

// ErrInvalidFieldName identifies a write that carries a field name that is not
// a plain name (mapped to HTTP 400 bad_request). See ValidateFieldName.
var ErrInvalidFieldName = errors.New("invalid field name")

// documentEngines lists the storage engines whose adapters address records as
// documents (a file path, a Firestore document) and never write a collection
// or field name into SQL text. It is an allow-list, like queryEngines: an
// engine added later, or a mount with an unrecognised engine, is held to the
// stricter SQL rule until someone clears it here.
var documentEngines = map[string]bool{
	"ingitdb":   true,
	"firestore": true,
}

// GuardKey refuses, before any adapter call, a key that names a collection the
// database does not declare (see GuardCollection). Every collection of a nested
// key is checked, not only the leaf: the adapter of a SQL engine receives the
// whole chain. The error wraps ErrNotFound (HTTP 404 not_found).
func (d *Database) GuardKey(key *record.Key) error {
	for cur := key; cur != nil; cur = cur.Parent() {
		if err := d.GuardCollection(cur.Collection()); err != nil {
			return err
		}
	}
	return nil
}

// GuardCollection refuses, before any adapter call, a collection the database
// does not declare when its adapter builds SQL (sqlite, postgres, mysql, and
// any engine not known to be a document engine). dalgo2sql writes the
// collection name into the text of key reads and writes, and
// ValidateCollectionName is only a path-safety rule that accepts quotes,
// spaces and semicolons, so the declaration in the manifest's schemas is the
// allow-list. Document engines keep their own rule: any collection that passes
// ValidateCollectionName. The error wraps ErrNotFound.
func (d *Database) GuardCollection(name string) error {
	if documentEngines[d.queryEngine()] || d.declares(name) {
		return nil
	}
	return fmt.Errorf("%w: collection %q is not declared by this database", ErrNotFound, name)
}

// declares reports whether the manifest's schemas declare the collection. A
// SQLite manifest may key a collection by its SQL-quoted storage identifier
// ("Order Details" with the quotes); the mount registers the public name
// (Order Details) for it too, so that name is declared as well.
func (d *Database) declares(name string) bool {
	if d.Manifest == nil || d.Manifest.Schemas == nil {
		return false
	}
	if _, ok := d.Manifest.Schemas.Collections[name]; ok {
		return true
	}
	if d.Manifest.Storage.Engine != "sqlite" {
		return false
	}
	for declared := range d.Manifest.Schemas.Collections {
		if logical, ok := sqliteLogicalName(declared); ok && logical == name {
			return true
		}
	}
	return false
}

// sqliteLogicalName returns the public identifier inside a SQL-quoted schema
// key, where a doubled quote stands for one literal quote. It mirrors
// sqliteLogicalRecordsetName in pkg/mount, which registers the same name with
// the driver; the two must agree.
func sqliteLogicalName(name string) (string, bool) {
	if len(name) < 2 || name[0] != '"' || name[len(name)-1] != '"' {
		return "", false
	}
	quoted := name[1 : len(name)-1]
	var logical strings.Builder
	for i := 0; i < len(quoted); i++ {
		if quoted[i] == '"' {
			if i+1 >= len(quoted) || quoted[i+1] != '"' {
				return "", false
			}
			i++
		}
		logical.WriteByte(quoted[i])
	}
	if logical.Len() == 0 {
		return "", false
	}
	return logical.String(), true
}

// ValidateFieldPath checks every segment of a field path from a write body with
// ValidateFieldName. An empty path is refused. The error wraps
// ErrInvalidFieldName.
func ValidateFieldPath(path []string) error {
	if len(path) == 0 {
		return fmt.Errorf("%w: field path is empty", ErrInvalidFieldName)
	}
	return ValidateFieldNames(path)
}

// ValidateFieldNames checks every name with ValidateFieldName, in order; no
// names is fine. The error wraps ErrInvalidFieldName.
func ValidateFieldNames(names []string) error {
	for _, name := range names {
		if err := ValidateFieldName(name); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidFieldName, err)
		}
	}
	return nil
}

// fieldNames lists every field name an update op carries.
func (u UpdateOp) fieldNames() []string {
	if u.FieldName == "" {
		return u.FieldPath
	}
	return append([]string{u.FieldName}, u.FieldPath...)
}

// guardWrite refuses a whole batch, before any adapter call (the validation
// that follows reads the adapter by key), when any op names an undeclared
// collection on a SQL engine (GuardKey) or carries a field name that is not
// plain on any engine: the top-level keys of its data and every name of its
// updates, delete-field included; an update that names no field is refused too
// (it used to reach the adapter for a key read and then fail as a 500). An op's
// collection is checked before its fields, so a request for a collection the
// database does not declare is a 404 whatever its body carries. An op without
// a key carries nothing to the adapter; the validation that follows refuses it.
func (d *Database) guardWrite(ops []Op) error {
	for i, op := range ops {
		if op.Key != nil {
			if err := d.GuardKey(op.Key); err != nil {
				return fmt.Errorf("op %d (%s): %w", i, op.Op, err)
			}
		}
		if err := ValidateFieldNames(slices.Sorted(maps.Keys(op.Data))); err != nil {
			return fmt.Errorf("op %d (%s) data: %w", i, op.Op, err)
		}
		for j, u := range op.Updates {
			if err := ValidateFieldPath(u.fieldNames()); err != nil {
				return fmt.Errorf("op %d (%s) update %d: %w", i, op.Op, j, err)
			}
		}
	}
	return nil
}
