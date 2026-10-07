package core

import (
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/record"
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

// clipKey returns the path of key as it may appear in an error message: the
// text record.Key.String gives, with the collection name and the id of every
// segment clipped as clipName clips a name. It does not validate the key.
func clipKey(key *record.Key) string {
	var segments []string
	for k := key; k != nil; k = k.Parent() {
		segments = append(segments, clipName(record.EscapeID(fmt.Sprint(k.ID))), clipName(k.Collection()))
	}
	slices.Reverse(segments)
	return strings.Join(segments, "/")
}

// errSourcesTooDeep refuses a query nested deeper than the query guard allows.
var errSourcesTooDeep = fmt.Errorf("%w: query nesting is too deep", ErrInvalidDTQL)

// guardSources refuses, before any adapter call, a structured query that reads
// a collection this database does not declare when its adapter builds SQL
// (sqlite, postgres, mysql, and any engine not known to be a document engine).
// It is the set of declared collections GuardCollection applies to key reads
// and writes, applied to every collection the query reads: the root source,
// every joined source at any depth, every derived source and every subquery
// (EXISTS, scalar and the ones in a scan order), wherever they sit. A query
// names a declared collection by its canonical name only (checkSourceCollection),
// where a key may carry any of its spellings. A source qualified by a
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
//
// Once every source is declared, a mount that reaches a database server has the
// fields and qualifiers of the query checked against what its tables hold
// (guardFields), so that a name the tables do not have is a refusal and not a failure
// of the server.
func (d *Database) guardSources(query dal.StructuredQuery) error {
	if d.isDocumentEngine() {
		return nil
	}
	if err := (sourceGuard{visit: d.checkSourceCollection}).query(query, 0); err != nil {
		return err
	}
	return d.guardFields(query)
}

// guardProtectedSources refuses, before any adapter call and before any
// collection name is looked at, a structured query that is not a plain read of
// one collection on a database with access policies (ErrProtectedSingleSource):
// one with a join, a derived source, a subquery anywhere, a scan bound, or a
// source that is not a plain collection (singleSourceRead). DALgo's access layer
// authorises the base and first-level join sources of a query and not what is
// nested deeper, and this profile does not authorise part of a query, so such a
// database serves one plain collection per query. The decision depends on the
// shape of the query alone, so the answer is the same whichever collections the
// query names, declared or not. It is the first source check of the entry points
// that take one query of the single-collection profile (ExecuteDTQLQuery,
// StreamDTQLSnapshot and SelectAccessSample) and of the join source's executor
// (guardStructured, after the names are checked).
func (d *Database) guardProtectedSources(query dal.StructuredQuery) error {
	if d.HasAccessPolicies() && !singleSourceRead(query) {
		return ErrProtectedSingleSource
	}
	return nil
}

// guardSource is guardSources for one source, for the call that reads the
// schema of a table (JoinFields) rather than a query.
func (d *Database) guardSource(source dal.RecordsetSource) error {
	if d.isDocumentEngine() {
		return nil
	}
	return sourceGuard{visit: d.checkSourceCollection}.source(source, 0)
}

// QueryCollections lists the root collections a query reads, once each and in
// document order: the root source, every joined source at any depth, every
// derived source and every subquery, wherever it sits (the walk of the source
// guard, whatever the engine). A caller that scopes a capability to a
// collection uses it to authorise every read a query makes, not only the one
// the route names. A source that a parent record, a schema or another database
// qualifies is not named by one root collection of this database, and a shape
// the walk does not know cannot be read: both are refused with ErrInvalidDTQL
// and nothing is listed. The cost is linear in the size of the query.
func QueryCollections(query dal.StructuredQuery) ([]string, error) {
	var names []string
	listed := map[string]struct{}{}
	walk := sourceGuard{visit: func(ref dal.CollectionRef) error {
		if ref.Parent() != nil || ref.Schema() != "" || ref.Database() != "" {
			return fmt.Errorf("%w: only plain root collections are supported as sources", ErrInvalidDTQL)
		}
		if _, seen := listed[ref.Name()]; !seen {
			listed[ref.Name()] = struct{}{}
			names = append(names, ref.Name())
		}
		return nil
	}}
	if err := walk.query(query, 0); err != nil {
		return nil, err
	}
	return names, nil
}

// sourceGuard walks a query tree and applies visit to every collection source
// it reaches, before it walks the scan orders of that source. Its depth
// counting is that of the query guard's name walk (nameWalker) except that it
// counts a nested join tree one level per nesting, where the name walk
// flattens such a tree. It is therefore never laxer than the name walk: it
// refuses a deeper tree, or a cyclic one, instead of recursing without bound.
type sourceGuard struct {
	visit func(dal.CollectionRef) error
}

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
	if err := g.visit(ref); err != nil {
		return err
	}
	for _, order := range ref.ScanOrders() {
		if err := g.order(order, depth+1); err != nil {
			return err
		}
	}
	return nil
}

// checkSourceCollection is the check of guardSources on one collection source:
// it is refused unless it is a declared collection of this database, named
// plainly (no parent record, no schema, no other database) and by its canonical
// name. A query gives the adapter the name as written, and the adapter quotes
// it as one identifier, so a spelling of the collection that is not its
// canonical name (the SQL-quoted key of a SQLite manifest, with its quote
// characters) would address the table of that literal name, which is a different
// table. The canonical name is the only spelling a query may use.
func (d *Database) checkSourceCollection(ref dal.CollectionRef) error {
	if ref.Parent() != nil {
		return fmt.Errorf("%w: collection %q cannot be nested under a record on this database", ErrNotFound, clipName(ref.Name()))
	}
	if named := ref.Database(); named != "" && (d.Manifest == nil || named != d.Manifest.Database.ID) {
		return fmt.Errorf("%w: collection %q of database %q is not declared by this database", ErrNotFound, clipName(ref.Name()), clipName(named))
	}
	if d.nativePostgres {
		if ref.Schema() == "" {
			return errUndeclared(ref.Path())
		}
		if err := validateNativeIdentifier(ref.Schema()); err != nil {
			return err
		}
		if err := validateNativeIdentifier(ref.Name()); err != nil {
			return err
		}
		id, ok := d.ResolveNativePostgresCollection(ref.Schema(), ref.Name())
		if !ok {
			return errUndeclared(ref.Path())
		}
		return d.GuardCanonicalCollection(id)
	}
	if ref.Schema() != "" {
		return errUndeclared(ref.Path())
	}
	return d.GuardCanonicalCollection(ref.Name())
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
