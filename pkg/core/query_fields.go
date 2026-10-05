package core

import (
	"fmt"
	"slices"
	"strings"

	"github.com/dal-go/dalgo/dal"

	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
)

// guardFields refuses, before any adapter call, a structured query on a mount
// that reaches a database server (serverEngines) and names something its tables
// cannot have: a source qualifier that no source of the query has, and a field that
// is no column of the collection it belongs to. The adapter of such a mount refuses
// both itself, but as plain errors it has no type for, which the server can only
// answer as a failure of its own, with a log record: a mistake of the caller that
// anyone who may read could make at will. Here it is a 400 invalid_dtql whose
// sentence repeats a bounded name and nothing the server said.
//
// A qualifier names a source by its identity, the alias or else the collection's
// name, as the adapter's compiler resolves it (an aliased source hides the name of its
// collection), and any source of an enclosing query as well. The columns of a
// collection are known only where the mount provisioned them from what the
// manifest declares: a strict mount, whose table holds the key column "id" and the
// declared fields, each under the name the server folds it to. A derived source,
// whose output is its query's, checks no field: the qualifier is still checked, and
// so is a qualifier on a mount whose tables may hold more than it declares, which
// checks no field either (no PostgreSQL mount is one: it is strict only, see
// pkg/mount/postgres.go, so no mount of this repository reaches that branch, which is
// kept as a defence). An unqualified field is checked in a query of one source. It may
// name a column the query gives an alias only where the compiler reads an alias: in
// HAVING and in ORDER BY, outside the argument of an aggregate, by the spelling the
// column wrote. Anywhere else (a column, WHERE, GROUP BY, a scan order, an aggregate's
// argument) such a name is the name of a column, and one the table lacks is refused. In
// a query with a join the compiler refuses an unqualified field as it is, and it is not
// looked at here. The key pseudo-field of DALgo (dal.DocumentID) has no column and is the
// adapter's to refuse. A source alias longer than the server holds in a name is refused
// as well, which the compiler could only fail with an error of its own.
//
// It does nothing while the mount is not cleared for queries, so that a mount that
// read the preview switch off answers every query as it always did (501), and it
// does nothing on any engine that is not a server: those answer a name that their
// tables do not have in their own ways, which clients already see.
func (d *Database) guardFields(query dal.StructuredQuery) error {
	if !serverEngines[d.queryEngine()] || !d.CanQuery() {
		return nil
	}
	return fieldGuard{db: d}.query(query, nil, 0)
}

// maxServerNameBytes is the most bytes PostgreSQL holds in a name (NAMEDATALEN is 64, and
// a name ends at 63): the compiler of the adapter refuses a source alias over it, with a
// plain error. It is the limit of the one server engine that is cleared for queries.
const maxServerNameBytes = 63

// nameLimits holds, for each engine whose server cuts a name that is too long without
// saying so, the most bytes it keeps. PostgreSQL keeps 63 of an identifier, so a name of
// 64 bytes or more would address the table or the column named by its first 63: on such
// an engine every route refuses a collection or a field that long before the driver is
// reached (see Database.nameTooLong). Any engine not listed has no limit here.
var nameLimits = map[string]int{"postgres": maxServerNameBytes}

// nameLimit is the most bytes of a name the mount's server keeps, or 0 when the engine
// has no limit.
func (d *Database) nameLimit() int { return nameLimits[d.queryEngine()] }

// nameTooLong reports whether name is longer than the server of the mount keeps in a
// name.
func (d *Database) nameTooLong(name string) bool {
	limit := d.nameLimit()
	return limit > 0 && len(name) > limit
}

// errCollectionNameTooLong is the refusal of a collection name over the limit of the
// engine. It wraps ErrInvalidKey (HTTP 400 invalid_key, as every collection name outside
// the name rule is) and gives the limit and not the name, which can be as long as the
// request allows.
func (d *Database) errCollectionNameTooLong() error {
	return fmt.Errorf("%w: a collection name is longer than %d bytes, the most the database holds in a name", ErrInvalidKey, d.nameLimit())
}

// errFieldNameTooLong is the refusal of a field name over the limit of the engine, as
// the kind of request says: ErrInvalidFieldName for a write (HTTP 400 bad_request) and
// ErrInvalidDTQL for a query (HTTP 400 invalid_dtql). It gives the limit and not the name.
func (d *Database) errFieldNameTooLong(kind error) error {
	return fmt.Errorf("%w: a field name is longer than %d bytes, the most the database holds in a name", kind, d.nameLimit())
}

// errSourceAliasTooLong is the refusal of a source alias over maxServerNameBytes. It gives
// the limit and not the alias, which can be as long as the request allows.
var errSourceAliasTooLong = fmt.Errorf("%w: a source alias is longer than %d bytes, the most the database holds in a name", ErrInvalidDTQL, maxServerNameBytes)

// fieldSource is one source a field of a query can be qualified with.
type fieldSource struct {
	// identity is what a qualifier names: the alias, else the name of the collection.
	identity string
	// collection is the declared collection the source reads, empty for a derived
	// source.
	collection string
	// columns holds the columns of the table, folded as the server folds a name, or is
	// nil when they are not known.
	columns map[string]struct{}
}

// fieldScope lists the sources a field can be qualified with: those of the query
// being walked, after those of every enclosing query. The innermost is the last, so
// that it hides an enclosing source of the same identity.
type fieldScope []fieldSource

func (s fieldScope) find(identity string) (fieldSource, bool) {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i].identity == identity {
			return s[i], true
		}
	}
	return fieldSource{}, false
}

// fieldGuard walks a query tree and checks every field and qualifier of it, as
// Database.guardFields says.
type fieldGuard struct{ db *Database }

// fold is the name of a column as the server holds it.
func (g fieldGuard) fold(name string) string {
	if g.db.foldsIdentifiers {
		return strings.ToLower(name)
	}
	return name
}

// sourceOf describes one source of a query. Whether the source is declared, and plain,
// is the source guard's to refuse (guardSources, which runs first): a source it did not
// refuse is a root collection of this database, or a derived query.
func (g fieldGuard) sourceOf(source dal.RecordsetSource) fieldSource {
	identity := source.Alias()
	if identity == "" {
		identity = source.Name()
	}
	described := fieldSource{identity: identity}
	ref, ok := source.(dal.CollectionRef)
	if !ok {
		return described
	}
	described.collection = ref.Name()
	if g.db.Manifest.Database.SchemaMode != schema.ModeStrict {
		return described
	}
	collection := g.db.schemaCollection(ref.Name())
	if collection == nil {
		return described
	}
	described.columns = map[string]struct{}{g.fold("id"): {}}
	for name := range collection.Fields {
		described.columns[g.fold(name)] = struct{}{}
	}
	return described
}

func (g fieldGuard) query(query dal.StructuredQuery, outer fieldScope, depth int) error {
	if depth > maxQueryTreeDepth {
		return errSourcesTooDeep
	}
	sources, joins, err := flattenFrom(query.From(), depth)
	if err != nil {
		return err
	}
	scope := slices.Clone(outer)
	for _, source := range sources {
		if ref, ok := source.(dal.CollectionRef); ok && len(g.fold(ref.Alias())) > maxServerNameBytes {
			return errSourceAliasTooLong
		}
		scope = append(scope, g.sourceOf(source))
	}
	w := fieldWalk{fieldGuard: g, scope: scope, aliases: map[string]struct{}{}}
	if len(sources) == 1 {
		w.single = &scope[len(scope)-1]
	}
	for _, column := range query.Columns() {
		if column.Alias != "" {
			w.aliases[column.Alias] = struct{}{}
		}
	}
	for _, source := range sources {
		switch s := source.(type) {
		case dal.CollectionRef:
			for _, order := range s.ScanOrders() {
				if err := w.expression(order.Expression(), depth+1); err != nil {
					return err
				}
			}
		case dal.QuerySource:
			// A derived source is walked with the sources of this query in scope: a scan
			// order may qualify a field with a sibling, and a name that is more than the
			// compiler resolves is never refused here.
			if err := g.query(s.Query(), scope, depth+1); err != nil {
				return err
			}
		}
	}
	for _, join := range joins {
		for _, on := range join.On() {
			if err := w.condition(on, depth+1); err != nil {
				return err
			}
		}
	}
	for _, column := range query.Columns() {
		if column.Wildcard != nil && column.Wildcard.Source != "" {
			if _, ok := scope.find(column.Wildcard.Source); !ok {
				return errNoSuchSource(column.Wildcard.Source)
			}
		}
		if err := w.expression(column.Expression, depth); err != nil {
			return err
		}
	}
	if err := w.condition(query.Where(), depth); err != nil {
		return err
	}
	for _, group := range query.GroupBy() {
		if err := w.expression(group, depth); err != nil {
			return err
		}
	}
	if err := w.reading(true).condition(query.Having(), depth); err != nil {
		return err
	}
	for _, order := range query.OrderBy() {
		if err := w.reading(true).expression(order.Expression(), depth); err != nil {
			return err
		}
	}
	return nil
}

// fieldWalk is the walk of one query of the tree.
type fieldWalk struct {
	fieldGuard
	scope fieldScope
	// single is the one source of a query without a join, which an unqualified field
	// belongs to, and nil for a query with more.
	single *fieldSource
	// aliases are the aliases of the columns of the query, as the columns wrote them: the
	// compiler matches an alias by the exact spelling, so a name that differs by case
	// from every alias is a column.
	aliases map[string]struct{}
	// readsAliases is set while the walk is where the compiler replaces an alias by the
	// expression of its column: in HAVING and in ORDER BY, and not inside the argument of
	// an aggregate.
	readsAliases bool
}

// reading is the walk w with readsAliases set to on.
func (w fieldWalk) reading(on bool) fieldWalk {
	w.readsAliases = on
	return w
}

func errNoSuchSource(qualifier string) error {
	return fmt.Errorf("%w: no source of the query is named %q", ErrInvalidDTQL, clipName(qualifier))
}

func (w fieldWalk) field(field dal.FieldRef) error {
	if field.IsID() && field.Name() == dal.DocumentID().Name() {
		return nil
	}
	if w.db.nameTooLong(field.Name()) {
		return w.db.errFieldNameTooLong(ErrInvalidDTQL)
	}
	source := w.single
	if qualifier := field.Source(); qualifier != "" {
		found, ok := w.scope.find(qualifier)
		if !ok {
			return errNoSuchSource(qualifier)
		}
		source = &found
	} else if source != nil && w.readsAliases {
		if _, alias := w.aliases[field.Name()]; alias {
			return nil
		}
	}
	if source == nil || source.columns == nil {
		return nil
	}
	if _, ok := source.columns[w.fold(field.Name())]; !ok {
		return fmt.Errorf("%w: collection %q has no field %q", ErrInvalidDTQL, clipName(source.collection), clipName(field.Name()))
	}
	return nil
}

func (w fieldWalk) condition(condition dal.Condition, depth int) error {
	if depth > maxQueryTreeDepth {
		return errSourcesTooDeep
	}
	switch c := condition.(type) {
	case dal.Comparison:
		if err := w.expression(c.Left, depth+1); err != nil {
			return err
		}
		return w.expression(c.Right, depth+1)
	case dal.GroupCondition:
		for _, child := range c.Conditions() {
			if err := w.condition(child, depth+1); err != nil {
				return err
			}
		}
	case dal.IsNullCondition:
		return w.expression(c.Operand(), depth+1)
	case dal.ExistsCondition:
		return w.query(c.Query(), w.scope, depth+1)
	}
	return nil
}

func (w fieldWalk) expression(expression dal.Expression, depth int) error {
	if depth > maxQueryTreeDepth {
		return errSourcesTooDeep
	}
	switch e := expression.(type) {
	case dal.FieldRef:
		return w.field(e)
	case dal.BinaryExpression:
		if err := w.expression(e.Left, depth+1); err != nil {
			return err
		}
		return w.expression(e.Right, depth+1)
	case dal.AggregateFunc:
		// The compiler reads the argument of an aggregate per row, from the source: an alias is
		// not replaced there.
		inside := w.reading(false)
		for _, arg := range e.FuncArgs() {
			if err := inside.expression(arg, depth+1); err != nil {
				return err
			}
		}
	case dal.QueryExpression:
		return w.query(e.Query(), w.scope, depth+1)
	}
	return nil
}
