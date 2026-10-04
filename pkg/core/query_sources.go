package core

import (
	"fmt"
	"unicode/utf8"

	"github.com/dal-go/dalgo/dal"
)

// maxEchoedNameLen bounds how much of a caller-supplied collection name or
// database id an error message repeats. Collection names have no length rule of
// their own (ValidateSegment is a path-safety rule), so a request can carry a
// name as large as its body.
const maxEchoedNameLen = 256

// clipName returns name as it may appear in an error message: whole when it is
// at most maxEchoedNameLen bytes, otherwise cut at a character boundary and
// followed by its length.
func clipName(name string) string {
	if len(name) <= maxEchoedNameLen {
		return name
	}
	cut := maxEchoedNameLen
	for cut > 0 && !utf8.RuneStart(name[cut]) {
		cut--
	}
	return fmt.Sprintf("%s...(%d bytes)", name[:cut], len(name))
}

// errSourcesTooDeep refuses a query nested deeper than the query guard allows.
var errSourcesTooDeep = fmt.Errorf("%w: query nesting is too deep", ErrInvalidDTQL)

// guardSources refuses, before any adapter call, a structured query that reads
// a collection this database does not declare when its adapter builds SQL
// (sqlite, postgres, mysql, and any engine not known to be a document engine).
// It is the same allow-list GuardCollection applies to key reads and writes,
// applied to every collection the query reads: the root source, every joined
// source at any depth, every derived source and every subquery (EXISTS, scalar
// and the ones in a scan order), wherever they sit. A source qualified by a
// parent record or a schema, or naming another database, is not a collection
// of this one and is refused too, because the adapter ignores or rewrites such
// a qualifier. A shape the walk does not know is refused with ErrInvalidDTQL
// (fail closed). The refusal of an undeclared source wraps ErrNotFound and says
// what the refusal of an undeclared key says.
//
// Every entry point that hands a structured query to a driver calls it after
// validating the names and before the engine guard, so an undeclared source is
// a 404 whatever the engine can query. Document engines (ingitdb, firestore)
// are not walked: they keep the collection rule of ValidateCollectionName.
func (d *Database) guardSources(query dal.StructuredQuery) error {
	if d.isDocumentEngine() {
		return nil
	}
	return sourceGuard{d: d}.query(query, 0)
}

// guardSource is guardSources for one source, for the call that reads the
// schema of a table (JoinFields) rather than a query.
func (d *Database) guardSource(source dal.RecordsetSource) error {
	if d.isDocumentEngine() {
		return nil
	}
	return sourceGuard{d: d}.source(source, 0)
}

// sourceGuard walks a query tree and checks every collection source against
// the database's declared collections. Its depth counting is that of the query
// guard's name walk (nameWalker), so it accepts every tree that walk accepts
// and refuses a deeper one, or a cyclic one, instead of recursing without bound.
type sourceGuard struct{ d *Database }

func (g sourceGuard) query(query dal.StructuredQuery, depth int) error {
	if query == nil {
		return fmt.Errorf("%w: a query is required", ErrInvalidDTQL)
	}
	if err := g.from(query.From(), depth); err != nil {
		return err
	}
	if err := g.condition(query.Where(), depth); err != nil {
		return err
	}
	for _, column := range query.Columns() {
		if err := g.expression(column.Expression, depth); err != nil {
			return err
		}
	}
	for _, group := range query.GroupBy() {
		if err := g.expression(group, depth); err != nil {
			return err
		}
	}
	if err := g.condition(query.Having(), depth); err != nil {
		return err
	}
	for _, order := range query.OrderBy() {
		if err := g.order(order, depth); err != nil {
			return err
		}
	}
	return nil
}

// from checks a relation tree the way an adapter reads it: the base source,
// then for each join its relation tree (or, for a join without one, its
// source) and its ON conditions.
func (g sourceGuard) from(from dal.FromSource, depth int) error {
	if depth > maxQueryTreeDepth {
		return errSourcesTooDeep
	}
	if from == nil || from.Base() == nil {
		return fmt.Errorf("%w: a source is required", ErrInvalidDTQL)
	}
	if err := g.source(from.Base(), depth); err != nil {
		return err
	}
	for _, join := range from.Joins() {
		child := join.From()
		if child == nil {
			if join.RecordsetSource == nil {
				return fmt.Errorf("%w: a source is required", ErrInvalidDTQL)
			}
			child = dal.From(join.RecordsetSource)
		}
		if err := g.from(child, depth+1); err != nil {
			return err
		}
		for _, on := range join.On() {
			if err := g.condition(on, depth+1); err != nil {
				return err
			}
		}
	}
	return nil
}

// source checks one relation: a root collection or a derived query. Any other
// source type (a collection group, a pointer, a type nobody has heard of) is
// refused.
func (g sourceGuard) source(source dal.RecordsetSource, depth int) error {
	switch value := source.(type) {
	case dal.CollectionRef:
		return g.collection(value, depth)
	case dal.QuerySource:
		return g.query(value.Query(), depth+1)
	default:
		return fmt.Errorf("%w: unsupported source %T", ErrInvalidDTQL, source)
	}
}

func (g sourceGuard) collection(ref dal.CollectionRef, depth int) error {
	if ref.Parent() != nil {
		return fmt.Errorf("%w: collection %q cannot be nested under a record on this database", ErrNotFound, clipName(ref.Name()))
	}
	if ref.Schema() != "" {
		return errUndeclared(ref.Path())
	}
	if named := ref.Database(); named != "" && (g.d.Manifest == nil || named != g.d.Manifest.Database.ID) {
		return fmt.Errorf("%w: collection %q of database %q is not declared by this database", ErrNotFound, clipName(ref.Name()), clipName(named))
	}
	if g.d.GuardCollection(ref.Name()) != nil {
		return errUndeclared(ref.Name())
	}
	for _, order := range ref.ScanOrders() {
		if err := g.order(order, depth+1); err != nil {
			return err
		}
	}
	return nil
}

// errUndeclared is the refusal of a collection the database does not declare,
// as GuardCollection words it, with the name clipped.
func errUndeclared(name string) error {
	return fmt.Errorf("%w: collection %q is not declared by this database", ErrNotFound, clipName(name))
}

func (g sourceGuard) order(order dal.OrderExpression, depth int) error {
	if order == nil {
		return fmt.Errorf("%w: an ordering expression is required", ErrInvalidDTQL)
	}
	return g.expression(order.Expression(), depth)
}

func (g sourceGuard) condition(condition dal.Condition, depth int) error {
	if depth > maxQueryTreeDepth {
		return errSourcesTooDeep
	}
	switch value := condition.(type) {
	case nil:
		return nil
	case dal.Comparison:
		if err := g.expression(value.Left, depth+1); err != nil {
			return err
		}
		return g.expression(value.Right, depth+1)
	case dal.GroupCondition:
		for _, child := range value.Conditions() {
			if err := g.condition(child, depth+1); err != nil {
				return err
			}
		}
		return nil
	case dal.IsNullCondition:
		return g.expression(value.Operand(), depth+1)
	case dal.ExistsCondition:
		return g.query(value.Query(), depth+1)
	default:
		return fmt.Errorf("%w: unsupported condition %T", ErrInvalidDTQL, condition)
	}
}

func (g sourceGuard) expression(expression dal.Expression, depth int) error {
	if depth > maxQueryTreeDepth {
		return errSourcesTooDeep
	}
	switch value := expression.(type) {
	case nil:
		return nil
	case dal.FieldRef, dal.Constant, dal.Array, dal.Param, dal.StarExpression:
		return nil
	case dal.BinaryExpression:
		if err := g.expression(value.Left, depth+1); err != nil {
			return err
		}
		return g.expression(value.Right, depth+1)
	case dal.AggregateFunc:
		for _, arg := range value.FuncArgs() {
			if err := g.expression(arg, depth+1); err != nil {
				return err
			}
		}
		return nil
	case dal.QueryExpression:
		return g.query(value.Query(), depth+1)
	default:
		return fmt.Errorf("%w: unsupported expression %T", ErrInvalidDTQL, expression)
	}
}
