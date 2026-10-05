package joinexec

import (
	"context"
	"fmt"

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
// source when any source of that query supplies no field list, and leaves the
// decision to DALgo when every source supplies one.
//
// A query here is one level of the document: the root query, a derived source, a
// scalar subquery or an EXISTS test, each with its own sources. A field is read
// against the sources of the query whose clause holds it; a subquery inside a
// clause is a level of its own.

// fieldSupplier answers the field list of a source, the way the executors DALgo
// reads through do (dal.JoinFieldsProvider): nil when the source has no list.
type fieldSupplier func(ctx context.Context, source dal.RecordsetSource) ([]string, error)

// scopeRef is an unqualified field of a clause, with where it stands.
type scopeRef struct {
	name string
	path string
}

// scopeLevel is one query of the document.
type scopeLevel struct {
	sources []dal.RecordsetSource
	refs    []scopeRef
}

// checkScopes refuses the first unqualified field of a query of several sources,
// in document order, that DALgo cannot bind because a source of that query
// supplies no field list. The refusal is a scope error (*dal.QueryValidationError),
// which the server answers as a 400 invalid_dtql, and nothing has been read when it
// is returned. An error of fields is returned as it is. A query with one source,
// and a field a source qualifies or the select list names as an alias, are not
// looked at.
func checkScopes(ctx context.Context, query dal.StructuredQuery, fields fieldSupplier) error {
	scope := &scopeWalk{}
	scope.query(query, "")
	for _, level := range scope.levels {
		if len(level.sources) < 2 || len(level.refs) == 0 {
			continue
		}
		for _, source := range level.sources {
			supplied, err := suppliedFields(ctx, source, fields)
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
		}
	}
	return nil
}

// suppliedFields reports whether source supplies a field list. A derived source
// supplies none, and is not asked: DALgo evaluates it by running its query.
func suppliedFields(ctx context.Context, source dal.RecordsetSource, fields fieldSupplier) (bool, error) {
	if _, derived := source.(dal.QuerySource); derived {
		return false, nil
	}
	list, err := fields(ctx, source)
	return list != nil, err
}

// scopeWalk collects the levels of a document in document order: a level is
// listed before the levels inside it.
type scopeWalk struct {
	levels []*scopeLevel
}

func (w *scopeWalk) query(query dal.StructuredQuery, path string) {
	level := &scopeLevel{}
	w.levels = append(w.levels, level)
	aliases := map[string]bool{}
	for _, column := range query.Columns() {
		if column.Alias != "" {
			aliases[column.Alias] = true
		}
		if as, ok := column.Expression.(dal.QueryExpression); ok && as.As() != "" {
			aliases[as.As()] = true
		}
	}
	flow := &scopeFlow{walk: w, level: level, aliases: aliases}
	flow.from(query.From(), path+"from")
	flow.condition(query.Where(), path+"where")
	for i, group := range query.GroupBy() {
		flow.expression(group, fmt.Sprintf("%sgroupBy[%d]", path, i))
	}
	flow.condition(query.Having(), path+"having")
	for i, order := range query.OrderBy() {
		flow.expression(order.Expression(), fmt.Sprintf("%sorderBy[%d]", path, i))
	}
	for i, column := range query.Columns() {
		flow.expression(column.Expression, fmt.Sprintf("%scolumns[%d]", path, i))
	}
}

// scopeFlow walks the clauses of one level, adding its sources and unqualified
// fields to it and handing every query inside a clause to the walk.
type scopeFlow struct {
	walk    *scopeWalk
	level   *scopeLevel
	aliases map[string]bool
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
		if value.Source() == "" && !f.aliases[value.Name()] {
			f.level.refs = append(f.level.refs, scopeRef{name: value.Name(), path: path})
		}
	case dal.BinaryExpression:
		f.expression(value.Left, path+".left")
		f.expression(value.Right, path+".right")
	case dal.AggregateFunc:
		for i, arg := range value.FuncArgs() {
			f.expression(arg, fmt.Sprintf("%s.args[%d]", path, i))
		}
	case dal.QueryExpression:
		f.walk.query(value.Query(), path+".query.")
	}
}
