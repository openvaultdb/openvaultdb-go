package core

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"

	"github.com/dal-go/record"

	"github.com/openvaultdb/openvaultdb-go/pkg/manifest"
	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
)

// collectionNames is the set of collections a mount declared when it opened,
// and the one name each of them is given to the adapter by.
//
// A collection can be written in more than one way. A SQLite manifest may key a
// collection by its SQL-quoted identifier ("Order Details" with the quotes);
// callers send that form, or the public name inside the quotes (Order Details),
// for the same table. The public name is the collection's canonical name, the
// only form the adapter is given: a name is quoted by the adapter when it is
// written into a statement, so a name that carries quote characters of its own
// would address a table of that literal name.
//
// The set is built from the manifest as the mount saw it, not read from the live
// manifest afterwards: an embedder may rename the manifest's keys once mounted.
// The zero value declares nothing.
type collectionNames struct {
	canonical map[string]string   // every declared spelling to its canonical name
	spellings map[string][]string // canonical name to its spellings, canonical first
}

// ErrCollectionNamesConflict identifies a manifest whose collection names cannot
// each designate one table: a name that is a spelling of one declared collection
// and the canonical name of another, or two keys that are one table and declare
// different fields. Opening a database refuses such a manifest.
var ErrCollectionNamesConflict = errors.New("conflicting collection names")

// newCollectionNames declares every key of the manifest's schemas, and on SQLite
// the public name of a key that is a quoted SQL identifier (SQLiteLogicalName),
// the name the mount registers with the driver for it. It refuses a manifest in
// which one name would designate two tables, or one table would have two
// declarations (ErrCollectionNamesConflict).
func newCollectionNames(m *manifest.Manifest) (collectionNames, error) {
	if m.Schemas == nil {
		return collectionNames{}, nil
	}
	keys := make([]string, 0, len(m.Schemas.Collections))
	for key := range m.Schemas.Collections {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	names := collectionNames{
		canonical: make(map[string]string, len(keys)),
		spellings: make(map[string][]string, len(keys)),
	}
	declaredBy := make(map[string]string, len(keys)) // canonical name to the first key that declares it
	for _, key := range keys {
		canonical := key
		if m.Storage.Engine == "sqlite" {
			if logical, ok := SQLiteLogicalName(key); ok {
				canonical = logical
			}
		}
		if first, ok := declaredBy[canonical]; !ok {
			declaredBy[canonical] = key
		} else if !maps.Equal(m.Schemas.Collections[first].Fields, m.Schemas.Collections[key].Fields) {
			return collectionNames{}, fmt.Errorf("%w: %q and %q are one table (%q) and declare different fields", ErrCollectionNamesConflict, first, key, canonical)
		}
		for _, spelling := range []string{canonical, key} {
			if other, ok := names.canonical[spelling]; ok && other != canonical {
				return collectionNames{}, fmt.Errorf("%w: %q is a spelling of both %q and %q", ErrCollectionNamesConflict, spelling, other, canonical)
			}
			names.canonical[spelling] = canonical
			if !slices.Contains(names.spellings[canonical], spelling) {
				names.spellings[canonical] = append(names.spellings[canonical], spelling)
			}
		}
	}
	return names, nil
}

// CanonicalCollection returns the name the adapter is given for a collection a
// caller named, and whether the database declares that name: a key of its
// schemas, or on SQLite the public name of a key that is a quoted SQL
// identifier. Every spelling of one collection has one canonical name. A name
// the database does not declare returns "" and false.
func (d *Database) CanonicalCollection(name string) (string, bool) {
	canonical, ok := d.names.canonical[name]
	return canonical, ok
}

// CollectionSpellings returns every spelling of the collection that name
// designates, its canonical name first: a grant that scopes a capability to the
// collection holds under any of them. A name the database does not declare is
// the only spelling of itself. The caller owns the result.
func (d *Database) CollectionSpellings(name string) []string {
	if canonical, ok := d.names.canonical[name]; ok {
		return slices.Clone(d.names.spellings[canonical])
	}
	return []string{name}
}

// schemaCollection returns the declared schema of the collection that name
// designates, found under the name itself or under any other spelling of it (the
// live manifest's keys are the quoted form until an embedder renames them).
func (d *Database) schemaCollection(name string) *schema.Collection {
	if col := d.Manifest.Schemas.Collection(name); col != nil {
		return col
	}
	for _, spelling := range d.CollectionSpellings(name) {
		if col := d.Manifest.Schemas.Collection(spelling); col != nil {
			return col
		}
	}
	return nil
}

// adapterKey returns the key the adapter is given for key: the same key, with
// its collection under the canonical name. Only a key without a parent is
// renamed: the guard refuses a nested key on an engine that builds SQL, and a
// document engine addresses it by its whole path.
func (d *Database) adapterKey(key *record.Key) *record.Key {
	if key == nil || key.Parent() != nil {
		return key
	}
	canonical, ok := d.names.canonical[key.Collection()]
	if !ok || canonical == key.Collection() {
		return key
	}
	renamed := record.NewKeyWithID(canonical, key.ID)
	renamed.IDKind = key.IDKind
	return renamed
}

// adapterOps returns ops with every key as the adapter is given it (adapterKey),
// so the batch validation sees one record where a batch addresses the same row
// in two spellings. The caller's slice is not changed.
func (d *Database) adapterOps(ops []Op) []Op {
	out := slices.Clone(ops)
	for i := range out {
		out[i].Key = d.adapterKey(out[i].Key)
	}
	return out
}
