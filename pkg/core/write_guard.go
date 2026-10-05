package core

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"unicode"

	"github.com/dal-go/record"
)

// ErrInvalidFieldName identifies a write that carries a field name that is not
// a plain name (mapped to HTTP 400 bad_request). See ValidateFieldName.
var ErrInvalidFieldName = errors.New("invalid field name")

// ErrKeyUpdate identifies an update that names the record's key column, on an
// engine whose adapter builds SQL: the key of a record is not a field an update
// changes (mapped to HTTP 400 bad_request, as ErrInvalidFieldName is). See
// Database.ValidateUpdatePath.
var ErrKeyUpdate = fmt.Errorf("%w: an update cannot name the record's key column", ErrInvalidFieldName)

// keyColumn is the primary-key column of every collection a SQL mount declares
// (see ensureCollection): it holds the record key's ID. SQL engines compare
// column names without regard to case (SQLite and MySQL always do, and
// PostgreSQL folds a name it is given unquoted), so every spelling of it is the
// same column.
const keyColumn = "id"

// ErrEmptyWrite identifies a write that names nothing to change, on an engine
// whose adapter builds SQL and cannot carry it out: an update with no operation,
// and a set that names no field but the record's id for a record that exists
// (mapped to HTTP 400 bad_request). A set of no field for a record that does not
// exist inserts a record that holds only its id, and is not refused.
var ErrEmptyWrite = errors.New("empty write")

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
// documents (see documentEngines). The class is recorded when the database
// opens, beside the declared set, so a later change of the live manifest's
// engine changes no rule. It is false for an engine nobody classified and for a
// database not built by Open: those are held to the SQL rules.
func (d *Database) isDocumentEngine() bool { return d.documentEngine }

// GuardKey refuses, before any adapter call, a key the adapter of a SQL engine
// cannot address safely: a key with a parent, because a SQL mount has no
// subcollections (dalgo2sql maps such a key to a recordset named after the whole
// path, <leaf>_<parent>, that no mount registers and that is not the root
// collection the capability is checked on), and a collection the database does
// not declare (see GuardCollection). Document engines address nested keys natively, as a
// subcollection of the parent record, so they are not refused here and the
// capability stays scoped by the root collection. A nil key names nothing and is
// left to the caller. The error wraps ErrNotFound (HTTP 404 not_found).
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
// opened is the allow-list (see collectionNames: every spelling of a declared
// collection is accepted and has one canonical name). Names match exactly, case
// included. Document engines keep their own rule: any collection that passes
// ValidateCollectionName. The error wraps ErrNotFound.
func (d *Database) GuardCollection(name string) error {
	if d.isDocumentEngine() {
		return nil
	}
	if _, ok := d.names.canonical[name]; ok {
		return nil
	}
	return fmt.Errorf("%w: collection %q is not declared by this database", ErrNotFound, name)
}

// GuardCanonicalCollection refuses, before any adapter call, a collection that
// is not named by its canonical name, when the database's adapter builds SQL (see
// GuardCollection): a name the database does not declare, and a spelling of a
// declared collection that is not its canonical name (the SQL-quoted key of a
// SQLite manifest). It is the rule of the routes that give the adapter, or the
// coordinator that reads it by key, the collection as the caller wrote it: a
// spelling that is not the canonical name would address the table of that
// literal name, which is a different table. Document engines keep their own rule
// and are not refused. The error wraps ErrNotFound and names the collection,
// clipped, as GuardCollection does.
func (d *Database) GuardCanonicalCollection(name string) error {
	if d.isDocumentEngine() {
		return nil
	}
	if canonical, declared := d.CanonicalCollection(name); declared && canonical == name {
		return nil
	}
	return errUndeclared(name)
}

// SQLiteLogicalName returns the public identifier inside a SQL-quoted schema
// key of a SQLite manifest, where a doubled quote stands for one literal quote.
// It is the one definition the declared set (newCollectionNames) and the mount
// (the recordsets it registers with the driver) share.
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
// a segment that is blank (empty, or only white space) or carries a control
// character is refused: update.ByFieldPath panics on a blank segment inside the
// transaction. A path with no segment at all is refused too. The error wraps
// ErrInvalidFieldName.
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

// ValidateUpdatePath checks the path of an update: ValidateFieldPath, and on an
// engine whose adapter builds SQL (or one nobody classified) the path must not
// name the record's key column, in any spelling of its case, as a field or as the
// head of a nested path. The error wraps ErrKeyUpdate, which wraps
// ErrInvalidFieldName. A document engine keeps the key outside the record's
// fields and is not refused here.
func (d *Database) ValidateUpdatePath(path []string) error {
	if err := d.ValidateFieldPath(path); err != nil {
		return err
	}
	if !d.isDocumentEngine() && strings.EqualFold(strings.SplitN(path[0], ".", 2)[0], keyColumn) {
		return fmt.Errorf("%w (%q)", ErrKeyUpdate, keyColumn)
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
// fieldName or fieldPath is not plain or, on an engine that builds SQL, names
// the record's key column (ValidateUpdatePath).
func (d *Database) validateUpdate(u UpdateOp) error {
	if u.FieldName == "" && len(u.FieldPath) == 0 {
		return fmt.Errorf("%w: update names no field (fieldName or fieldPath)", ErrInvalidFieldName)
	}
	if u.FieldName != "" {
		if err := d.ValidateUpdatePath([]string{u.FieldName}); err != nil {
			return err
		}
	}
	if len(u.FieldPath) > 0 {
		return d.ValidateUpdatePath(u.FieldPath)
	}
	return nil
}

// guardWrite refuses a whole batch, before any adapter call (the validation
// that follows reads the adapter by key), when any op names an undeclared
// collection on a SQL engine (GuardKey) or carries a field name that is not
// plain on any engine: the top-level keys of its data, the fieldName and the
// first path segment of its updates, delete-field included, and the later
// segments as ValidateFieldPath says; an update that names no field is refused
// too, and on an engine that builds SQL an update with no operation at all
// (ErrEmptyWrite) and an update that names the record's key column (ErrKeyUpdate,
// see ValidateUpdatePath). Ops are checked in order, and an op's collection
// before its fields. So within one op a collection the database does not declare
// is a 404 whatever its body carries, and in a batch the first op that fails
// decides the refusal, whichever rule it breaks. An op without a key carries
// nothing to the adapter; the validation that follows refuses it.
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
		if op.Op == "update" && len(op.Updates) == 0 && !d.isDocumentEngine() {
			return fmt.Errorf("op %d (update): %w: the update carries no operation", i, ErrEmptyWrite)
		}
		for j, u := range op.Updates {
			if err := d.validateUpdate(u); err != nil {
				return fmt.Errorf("op %d (%s) update %d: %w", i, op.Op, j, err)
			}
		}
	}
	return nil
}

// setsNoColumn reports whether data, written to a record that exists, would
// leave a SQL adapter nothing to update: it holds no field but "id", the
// primary-key column of every collection a SQL mount declares. It is false on a
// document engine, which rewrites the document.
func (d *Database) setsNoColumn(data map[string]any) bool {
	if d.isDocumentEngine() {
		return false
	}
	for name := range data {
		if name != "id" {
			return false
		}
	}
	return true
}
