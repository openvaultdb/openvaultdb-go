package joinexec

import (
	"context"
	"fmt"
	"slices"

	"github.com/dal-go/dalgo/dal"
)

// Which source an unqualified field belongs to.
//
// DALgo's recursive evaluation binds an unqualified field of a query to the one
// source that carries it, and refuses it as ambiguous when two carry it, only when
// every source of the query supplied the list of its fields (JoinFields). When one
// source supplied none, it cannot tell, and it binds the field to the first source
// of the query without saying so: a query of several sources then reads a field of
// the wrong source and answers with its values. The check below closes that: before
// DALgo reads anything, it refuses an unqualified field of a query of more than one
// source when any source of that query supplies no field list.
//
// When every source supplies one, DALgo decides, but not on every route: its
// streaming plan of a flat join of two sources binds an unqualified field to the first
// source with no ambiguity check at all. The check therefore refuses, in DALgo's own
// words, a name that two of the lists carry, and leaves DALgo what it decides on every
// route: a name that one list carries is bound to that source, and one that none
// carries is refused as unavailable. The DTQL parser keeps an unqualified field out of
// a join with no subquery as well, but Execute takes any query and does not depend on
// it.
//
// That binding is not what an aggregation reads. DALgo's check of the fields binds a name
// to the one list that carries it, and then its aggregation evaluates the expressions of
// GROUP BY and the arguments of the aggregates (and compares what HAVING, ORDER BY and the
// columns name with them by their text) against the first source of the query and against
// no other: a name that only another source carries is read as null there, or as the key
// of that name that a record of the first source holds outside its list. So a query of
// several sources that aggregates (dal.HasAggregation, DALgo's own test) is held to one
// rule more: an unqualified field in its GROUP BY, HAVING, ORDER BY or columns, whatever
// the clause reads it for, is a field of the first source or is refused, with the same
// scope error that says to qualify the field. WHERE and ON are not aggregated, DALgo
// evaluates them on the joined row and binds a name there to its carrier, so they are not
// held to it. A name that no source carries is left to DALgo, which refuses it as
// unavailable.
//
// A query here is one level of the document: the root query, a derived source, a
// scalar subquery or an EXISTS test, each with its own sources. A field is read
// against the sources of the query whose clause holds it; a subquery inside a
// clause is a level of its own.
//
// A name that a column of the select list carries as its alias is not a field of a
// source where DALgo reads it as the column: in HAVING and ORDER BY of a query that
// aggregates, outside the argument of an aggregate, and when resolveAliases replaces
// it (replaceableAliases says which names it does) by the column before DALgo sees it.
// Everywhere else, the WHERE, ON, GROUP BY and column expressions, the argument of an
// aggregate, and the ORDER BY of a query that does not aggregate, DALgo reads a field
// of a source, whatever the select list calls its columns, so the name is looked at
// like any other; so is an alias that resolveAliases does not replace.

// fieldSupplier answers the field list of a source, the way the executors DALgo
// reads through do (dal.JoinFieldsProvider): nil when the source has no list.
type fieldSupplier func(ctx context.Context, source dal.RecordsetSource) ([]string, error)

// scopeRef is an unqualified field of a clause, with where it stands.
type scopeRef struct {
	name string
	path string
	// aggregated is true for a field of GROUP BY, HAVING, ORDER BY or the columns of a
	// query that aggregates, which DALgo's aggregation reads from the first source of
	// the query.
	aggregated bool
}

// scopeLevel is one query of the document.
type scopeLevel struct {
	sources []dal.RecordsetSource
	refs    []scopeRef
}

// checkScopes refuses the first unqualified field of a query of several sources,
// in document order, that DALgo cannot bind because a source of that query
// supplies no field list, that two of the lists carry, or that DALgo's aggregation
// would read from the first source when only another carries it. The refusal is a
// scope error (*dal.QueryValidationError), which the server answers as a 400
// invalid_dtql, and nothing has been read when it is returned. An error of fields
// is returned as it is. A query with one source, and a field a source qualifies or
// that is the alias of a column where DALgo reads it as the column, are not looked
// at.
func checkScopes(ctx context.Context, query dal.StructuredQuery, fields fieldSupplier) error {
	scope := &scopeWalk{}
	scope.query(query, "")
	for _, level := range scope.levels {
		if len(level.sources) < 2 || len(level.refs) == 0 {
			continue
		}
		var lists [][]string
		for _, source := range level.sources {
			list, supplied, err := suppliedFields(ctx, source, fields)
			if err != nil {
				return err
			}
			if !supplied {
				first := level.refs[0]
				return &dal.QueryValidationError{
					Category: "scope",
					Path:     first.path,
					Message: fmt.Sprintf("cannot tell which source carries the unqualified field %s, because a source of the query has no field list: qualify the field with its source",
						clip(first.name)),
				}
			}
			lists = append(lists, list)
		}
		for _, ref := range level.refs {
			switch count := carriers(lists, ref.name); {
			case count > 1:
				return &dal.QueryValidationError{
					Category: "scope",
					Path:     ref.path,
					Message:  fmt.Sprintf("ambiguous unqualified field %s", clip(ref.name)),
				}
			case count == 1 && ref.aggregated && !slices.Contains(lists[0], ref.name):
				return &dal.QueryValidationError{
					Category: "scope",
					Path:     ref.path,
					Message: fmt.Sprintf("cannot read the unqualified field %s from a source other than the first in a query that aggregates: qualify the field with its source",
						clip(ref.name)),
				}
			}
		}
	}
	return nil
}

// carriers counts the lists that hold name.
func carriers(lists [][]string, name string) int {
	count := 0
	for _, list := range lists {
		if slices.Contains(list, name) {
			count++
		}
	}
	return count
}

// suppliedFields returns the field list of source and whether it supplies one. A
// derived source supplies none, and is not asked: DALgo evaluates it by running its
// query.
func suppliedFields(ctx context.Context, source dal.RecordsetSource, fields fieldSupplier) ([]string, bool, error) {
	if _, derived := source.(dal.QuerySource); derived {
		return nil, false, nil
	}
	list, err := fields(ctx, source)
	return list, list != nil, err
}

// scopeWalk collects the levels of a document in document order: a level is
// listed before the levels inside it.
type scopeWalk struct {
	levels []*scopeLevel
}

func (w *scopeWalk) query(query dal.StructuredQuery, path string) {
	level := &scopeLevel{}
	w.levels = append(w.levels, level)
	flow := &scopeFlow{walk: w, level: level}
	flow.from(query.From(), path+"from")
	flow.condition(query.Where(), path+"where")
	// From here on, an aggregating query reads a field from the first source: the clauses
	// below are the aggregation's, and WHERE and ON above are not.
	flow.aggregated = dal.HasAggregation(query)
	for i, group := range query.GroupBy() {
		flow.expression(group, fmt.Sprintf("%sgroupBy[%d]", path, i))
	}
	// HAVING and ORDER BY of a query that aggregates read the alias of a column as the
	// column.
	if flow.aggregated {
		flow.aliases = replaceableAliases(query.Columns())
	}
	flow.condition(query.Having(), path+"having")
	for i, order := range query.OrderBy() {
		flow.expression(order.Expression(), fmt.Sprintf("%sorderBy[%d]", path, i))
	}
	flow.aliases = nil
	for i, column := range query.Columns() {
		flow.expression(column.Expression, fmt.Sprintf("%scolumns[%d]", path, i))
	}
}

// scopeFlow walks the clauses of one level, adding its sources and unqualified
// fields to it and handing every query inside a clause to the walk.
type scopeFlow struct {
	walk  *scopeWalk
	level *scopeLevel
	// aliases are the names a field may carry without naming a field of a source: the
	// aliases of the columns that resolveAliases replaces, while HAVING and ORDER BY of
	// a query that aggregates are walked, and none otherwise.
	aliases map[string]dal.Expression
	// aggregated is set once the walk is past WHERE and ON of a query that aggregates: the
	// fields it finds from then on are read by DALgo's aggregation.
	aggregated bool
}

// from walks a relation tree: its base source, then each join's relation tree and
// ON conditions. The sources of a join tree are the sources of the level.
func (f *scopeFlow) from(from dal.FromSource, path string) {
	f.source(from.Base(), path)
	for i, join := range from.Joins() {
		joinAt := fmt.Sprintf("%s.joins[%d]", path, i)
		child := join.From()
		if child == nil {
			child = dal.From(join.RecordsetSource)
		}
		f.from(child, joinAt+".from")
		for j, on := range join.On() {
			f.condition(on, fmt.Sprintf("%s.on[%d]", joinAt, j))
		}
	}
}

func (f *scopeFlow) source(source dal.RecordsetSource, path string) {
	f.level.sources = append(f.level.sources, source)
	if derived, ok := source.(dal.QuerySource); ok {
		f.walk.query(derived.Query(), path+".query.")
	}
}

func (f *scopeFlow) condition(condition dal.Condition, path string) {
	switch value := condition.(type) {
	case dal.Comparison:
		f.expression(value.Left, path+".left")
		f.expression(value.Right, path+".right")
	case dal.GroupCondition:
		for i, child := range value.Conditions() {
			f.condition(child, fmt.Sprintf("%s.conditions[%d]", path, i))
		}
	case dal.IsNullCondition:
		f.expression(value.Operand(), path+".operand")
	case dal.ExistsCondition:
		f.walk.query(value.Query(), path+".query.")
	}
}

func (f *scopeFlow) expression(expression dal.Expression, path string) {
	switch value := expression.(type) {
	case dal.FieldRef:
		if _, alias := f.aliases[value.Name()]; value.Source() == "" && !alias {
			f.level.refs = append(f.level.refs, scopeRef{name: value.Name(), path: path, aggregated: f.aggregated})
		}
	case dal.BinaryExpression:
		f.expression(value.Left, path+".left")
		f.expression(value.Right, path+".right")
	case dal.AggregateFunc:
		// An argument is read per row, from a source: an alias is not a field there.
		aliases := f.aliases
		f.aliases = nil
		for i, arg := range value.FuncArgs() {
			f.expression(arg, fmt.Sprintf("%s.args[%d]", path, i))
		}
		f.aliases = aliases
	case dal.QueryExpression:
		f.walk.query(value.Query(), path+".query.")
	}
}
