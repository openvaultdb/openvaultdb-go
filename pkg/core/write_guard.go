package core

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"unicode"

	"github.com/dal-go/record"

	"github.com/openvaultdb/openvaultdb-go/pkg/manifest"
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

// isDocumentEngine reports whether the mount's adapter addresses records as
// documents (see documentEngines). It is false for an engine nobody classified
// and for a database without a manifest: those are held to the SQL rules.
func (d *Database) isDocumentEngine() bool { return documentEngines[d.queryEngine()] }

// GuardKey refuses, before any adapter call, a key the adapter of a SQL engine
// cannot address safely: a key with a parent, because a SQL mount has no
// subcollections (dalgo2sql maps such a key to a recordset named
// <leaf>_<parent> that no mount registers, and its delete statement names only
// the leaf table, so the capability checked on the root collection would not be
// the table written), and a collection the database does not declare (see
// GuardCollection). Document engines address nested keys natively and are not
// refused here. A nil key names nothing and is left to the caller. The error
// wraps ErrNotFound (HTTP 404 not_found).
func (d *Database) GuardKey(key *record.Key) error {
	if key == nil {
		return nil
	}
	if key.Parent() != nil && !d.isDocumentEngine() {
		return fmt.Errorf("%w: collection %q cannot be nested under a record on this database", ErrNotFound, key.Collection())
	}
	return d.GuardCollection(key.Collection())
}

// GuardCollection refuses, before any adapter call, a collection the database
// does not declare when its adapter builds SQL (sqlite, postgres, mysql, and
// any engine not known to be a document engine). dalgo2sql writes the
// collection name into the text of key reads and writes, and
// ValidateCollectionName is only a path-safety rule that accepts quotes,
// spaces and semicolons, so the set of collections the mount declared when it
// opened is the allow-list. Names match exactly, case included. Document
// engines keep their own rule: any collection that passes
// ValidateCollectionName. The error wraps ErrNotFound.
func (d *Database) GuardCollection(name string) error {
	if d.isDocumentEngine() {
		return nil
	}
	if _, ok := d.declared[name]; ok {
		return nil
	}
	return fmt.Errorf("%w: collection %q is not declared by this database", ErrNotFound, name)
}

// declaredCollections is the allow-list of a mount, fixed when the database
// opens: every key of the manifest's schemas, and on SQLite the public name of
// a key that is a quoted SQL identifier ("Order Details" with the quotes), the
// name the mount registers with the driver for it too. It is built from the
// manifest as the mount saw it, not read from the live manifest afterwards:
// an embedder may rename the manifest's keys once mounted (openvaultdb/cloud
// publishes the public names and rewrites key reads to the quoted ones), and
// the name the driver was given must stay declared.
func declaredCollections(m *manifest.Manifest) map[string]struct{} {
	if m.Schemas == nil {
		return nil
	}
	declared := make(map[string]struct{}, len(m.Schemas.Collections))
	for name := range m.Schemas.Collections {
		declared[name] = struct{}{}
		if m.Storage.Engine != "sqlite" {
			continue
		}
		if logical, ok := SQLiteLogicalName(name); ok {
			declared[logical] = struct{}{}
		}
	}
	return declared
}

// SQLiteLogicalName returns the public identifier inside a SQL-quoted schema
// key of a SQLite manifest, where a doubled quote stands for one literal quote.
// It is the one definition the guard (declaredCollections) and the mount (the
// recordsets it registers with the driver) share.
func SQLiteLogicalName(name string) (string, bool) {
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

// ValidateFieldPath checks the path of an update from a write body. The first
// segment is a field of the record, so it must be a plain name
// (ValidateFieldName) on every engine. The segments after it are map keys: on
// a SQL engine (or one nobody classified) they are held to the same rule, to
// fail closed; on a document engine they never reach SQL and are data
// (Sneat's linkage writes ["related", ext, collection, "id@spaceID"]), so only
// a segment that is empty, blank or carries a control character is refused (an
// empty one panics in update.ByFieldPath inside the transaction). An empty
// path is refused. The error wraps ErrInvalidFieldName.
func (d *Database) ValidateFieldPath(path []string) error {
	if len(path) == 0 {
		return fmt.Errorf("%w: field path is empty", ErrInvalidFieldName)
	}
	if err := ValidateFieldNames(path[:1]); err != nil {
		return err
	}
	check := ValidateFieldName
	if d.isDocumentEngine() {
		check = validateMapKey
	}
	for _, segment := range path[1:] {
		if err := check(segment); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidFieldName, err)
		}
	}
	return nil
}

// validateMapKey is the rule for a key inside a document: anything but an
// empty or blank key or one with a control character.
func validateMapKey(key string) error {
	if strings.TrimSpace(key) == "" {
		return errors.New("map key is empty or blank")
	}
	if strings.IndexFunc(key, unicode.IsControl) >= 0 {
		return fmt.Errorf("map key %q has a control character", key)
	}
	return nil
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

// validateUpdate refuses an update that names no field, and one whose
// fieldName or fieldPath is not plain (ValidateFieldPath for the path).
func (d *Database) validateUpdate(u UpdateOp) error {
	if u.FieldName == "" && len(u.FieldPath) == 0 {
		return fmt.Errorf("%w: update names no field (fieldName or fieldPath)", ErrInvalidFieldName)
	}
	if u.FieldName != "" {
		if err := ValidateFieldNames([]string{u.FieldName}); err != nil {
			return err
		}
	}
	if len(u.FieldPath) > 0 {
		return d.ValidateFieldPath(u.FieldPath)
	}
	return nil
}

// guardWrite refuses a whole batch, before any adapter call (the validation
// that follows reads the adapter by key), when any op names an undeclared
// collection on a SQL engine (GuardKey) or carries a field name that is not
// plain on any engine: the top-level keys of its data, the fieldName and the
// first path segment of its updates, delete-field included, and the later
// segments as ValidateFieldPath says; an update that names no field is refused
// too (it used to reach the adapter for a key read and then fail as a 500). An
// op's collection is checked before its fields, so a request for a collection
// the database does not declare is a 404 whatever its body carries. An op without
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
			if err := d.validateUpdate(u); err != nil {
				return fmt.Errorf("op %d (%s) update %d: %w", i, op.Op, j, err)
			}
		}
	}
	return nil
}
