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
// scope error that says to qualify the field.
//
// An unqualified field of an ON condition is held to the same rule on every plan. DALgo's
// recursive plan binds it to its carrier, but its streaming plan, which runs a flat join of
// two sources that each name their database, reads it from the row of the first source with
// no check at all, so a name that only the second source carries compares as null and the
// join answers no row, with no error. The answer must not depend on the plan DALgo picks, so
// the field is a field of the first source or is refused, and the rule is the one the
// aggregation's clauses follow: the refusal says to qualify the field. (The alternative,
// refusing every unqualified ON field of a query whose other source carries it, refuses
// the same documents.) WHERE is not held to it: DALgo evaluates it on the joined row and
// binds a name there to its carrier on both plans. A name that no source carries is left
// to DALgo, which refuses it as unavailable.
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
// aggregate, DALgo reads a field of a source, whatever the select list calls its
// columns, so the name is looked at like any other; so is an alias that resolveAliases
// does not replace.
//
// Two more things DALgo reads as a column, and the walk refuses them whatever the sources
// supply, because nothing of the sources can say what the name was meant for. In the
// columns of a query that aggregates, DALgo reads an unqualified field first as the
// earlier column that is named so (an alias, or the name a column selects a field under),
// and as a field of a source only when none is: so a column field that an earlier column
// carries by name is refused, with the way out, a rename of the alias or a qualified field.
// And in ORDER BY of a query that does not aggregate, a SQL database that runs the whole
// document reads a name that a column carries as its alias as that column, while DALgo, and
// the executor of a mount that is handed the document whole, read a field of a source.
// The name of a column that selects a field of a source is replaced by that field before
// either sees the document (resolveAliases), which is the order the SQL database gives;
// the name of a column that is an expression has no field to be replaced by, and is
// refused with the way out, to order by the fields of the expression.
//
// The names of the ORDER BY of a query that does not aggregate that are left are fields
// of a source. When every source of the query supplies its list and none carries the name,
// it is refused as an unknown field, one source or several, before anything is read: an
// executor handed the document whole (an inGitDB mount) ignores a field it does not know,
// and answers the document unsorted. A field that names its source (c.nope) is looked at in
// the list of that source alone, and refused when that list does not carry it. A source
// that supplies no list (a partial or schemaless database, a database with access policies,
// a derived source) cannot say a name is unknown, and the name is left to the mount.
//
// A column's alias is read as the column only where it is the whole ORDER BY expression, as
// a SQL database reads it: inside arithmetic (b*1) a name is a field of a source, and the
// source that carries a field of that name wins over a column that is called so. The
// alias of a column that is not a field is not refused there for what it is: it is a name
// that a source carries or does not.

// fieldSupplier answers the field list of a source, the way the executors DALgo
// reads through do (dal.JoinFieldsProvider): nil when the source has no list.
type fieldSupplier func(ctx context.Context, source dal.RecordsetSource) ([]string, error)

// readKind says how the clause that holds an unqualified field has it read.
type readKind int

const (
	// readAnywhere is a field DALgo binds to its carrier: WHERE, GROUP BY and the columns
	// of a query that does not aggregate, and the argument of an aggregate.
	readAnywhere readKind = iota
	// readAggregated is a field of GROUP BY, HAVING, ORDER BY or the columns of a query
	// that aggregates, which DALgo's aggregation reads from the first source of the query.
	readAggregated
	// readOn is a field of an ON condition, which DALgo's streaming plan reads from the
	// first source of the query.
	readOn
	// readOrdered is a field of the ORDER BY of a query that does not aggregate: a field
	// of a source, which every source of the query that supplies a list is asked for.
	readOrdered
)

// keyField is the pseudo-field the document engines (inGitDB) order by the record key
// with. No list of fields carries it, and the mount that is handed the document sorts by it.
const keyField = "$id"

// scopeRef is a field of a clause, with where it stands.
type scopeRef struct {
	name string
	path string
	kind readKind
	// qualifier is the source the field names, of a field that names one.
	qualifier string
	// inExpression is true for a name inside the arithmetic of an ORDER BY, where the alias
	// of a column is not read as the column.
	inExpression bool
}

// scopeLevel is one query of the document.
type scopeLevel struct {
	sources []dal.RecordsetSource
	refs    []scopeRef
	// qualified are the fields of an ORDER BY of a query that does not aggregate that name
	// their source, which the list of that source alone says are known or not.
	qualified []scopeRef
	// refusals are the refusals that no field list is needed for, in document order.
	refusals []*dal.QueryValidationError
	// ordered is true when a ref is a field of an ORDER BY, which is looked at even for a
	// query of one source.
	ordered bool
}

// checkScopes refuses the first unqualified field of a query, in document order, that
// DALgo cannot bind to the source the document means. A query of several sources is
// refused for a field that a source of that query supplies no field list for, that two of
// the lists carry, that DALgo's aggregation or its streaming plan would read from the first
// source when only another carries it, and (ORDER BY) that none carries. A query of one
// source is refused for an ORDER BY name its list does not carry, as is a field of an ORDER BY
// that names its source and whose list does not carry it. A name that stands for
// a column where DALgo or a mount would read a field of a source is refused whatever the
// sources supply (see the comment at the top of this file). The refusal is a scope error
// (*dal.QueryValidationError), which the server answers as a 400 invalid_dtql, and nothing
// has been read when it is returned. An error of fields is returned as it is. A field a
// source qualifies, and one that is the alias of a column where DALgo reads it as the
// column, are not looked at.
func checkScopes(ctx context.Context, query dal.StructuredQuery, fields fieldSupplier) error {
	scope := &scopeWalk{}
	scope.query(query, "")
	for _, level := range scope.levels {
		if len(level.refusals) > 0 {
			return level.refusals[0]
		}
		if err := level.check(ctx, fields); err != nil {
			return err
		}
	}
	return nil
}

// check refuses the first field of the level that the lists of its sources show is
// not bound to a source: an unqualified field first, and then a field of an ORDER BY that
// names a source its list does not carry. A query of one source is looked at for its
// ORDER BY names only.
func (l *scopeLevel) check(ctx context.Context, fields fieldSupplier) error {
	lists := &levelLists{ctx: ctx, fields: fields, sources: l.sources, answers: map[int]listAnswer{}}
	if err := l.checkUnqualified(lists); err != nil {
		return err
	}
	return l.checkQualified(lists)
}

func (l *scopeLevel) checkUnqualified(lists *levelLists) error {
	several := len(l.sources) >= 2
	if len(l.refs) == 0 || !several && !l.ordered {
		return nil
	}
	var all [][]string
	for i := range l.sources {
		list, supplied, err := lists.of(i)
		if err != nil {
			return err
		}
		if !supplied {
			if !several {
				// The one source cannot say a name is unknown: the mount decides.
				return nil
			}
			first := l.refs[0]
			return &dal.QueryValidationError{
				Category: "scope",
				Path:     first.path,
				Message: fmt.Sprintf("cannot tell which source carries the unqualified field %s, because a source of the query has no field list: qualify the field with its source",
					clip(first.name)),
			}
		}
		all = append(all, list)
	}
	for _, ref := range l.refs {
		if !several && ref.kind != readOrdered {
			continue
		}
		if err := ref.check(all); err != nil {
			return err
		}
	}
	return nil
}

// checkQualified refuses the first field of an ORDER BY that names a source whose list does
// not carry it. The source is the one whose alias, or collection with no alias, the field
// names; a name that is no source of the level belongs to a query around it, and is left to
// DALgo. The key pseudo-field is not refused.
func (l *scopeLevel) checkQualified(lists *levelLists) error {
	for _, ref := range l.qualified {
		if ref.name == keyField {
			continue
		}
		i := slices.IndexFunc(l.sources, func(source dal.RecordsetSource) bool { return sourceAlias(source) == ref.qualifier })
		if i < 0 {
			continue
		}
		list, supplied, err := lists.of(i)
		if err != nil {
			return err
		}
		if supplied && !slices.Contains(list, ref.name) {
			return &dal.QueryValidationError{
				Category: "shape",
				Path:     ref.path,
				Message:  fmt.Sprintf("unknown field %q in ORDER BY: the source %s does not carry it", clip(ref.name), clip(ref.qualifier)),
			}
		}
	}
	return nil
}

// levelLists asks the sources of a level for their field lists, once each.
type levelLists struct {
	ctx     context.Context
	fields  fieldSupplier
	sources []dal.RecordsetSource
	answers map[int]listAnswer
}

type listAnswer struct {
	list     []string
	supplied bool
}

// of returns the list of the source at i and whether it supplies one.
func (s *levelLists) of(i int) ([]string, bool, error) {
	if answer, asked := s.answers[i]; asked {
		return answer.list, answer.supplied, nil
	}
	list, supplied, err := suppliedFields(s.ctx, s.sources[i], s.fields)
	if err != nil {
		return nil, false, err
	}
	s.answers[i] = listAnswer{list, supplied}
	return list, supplied, nil
}

// sourceAlias is the name a field qualifies a source by: its alias, or its own name when
// it has none, as DALgo names it.
func sourceAlias(source dal.RecordsetSource) string {
	if source.Alias() != "" {
		return source.Alias()
	}
	return source.Name()
}

// check refuses the field when the lists say DALgo cannot read it from the source it
// means. The lists are those of the sources of its level, in order.
func (r scopeRef) check(lists [][]string) error {
	switch count := carriers(lists, r.name); {
	case count > 1:
		return &dal.QueryValidationError{
			Category: "scope",
			Path:     r.path,
			Message:  fmt.Sprintf("ambiguous unqualified field %s", clip(r.name)),
		}
	case count == 1 && (r.kind == readAggregated || r.kind == readOn) && !slices.Contains(lists[0], r.name):
		where := "a query that aggregates"
		if r.kind == readOn {
			where = "a join condition"
		}
		return &dal.QueryValidationError{
			Category: "scope",
			Path:     r.path,
			Message: fmt.Sprintf("cannot read the unqualified field %s from a source other than the first in %s: qualify the field with its source",
				clip(r.name), where),
		}
	case count == 0 && r.kind == readOrdered && r.name != keyField:
		alias := "no column has it as its alias"
		if r.inExpression {
			alias = "the alias of a column is read only where it is the whole ORDER BY expression, not inside arithmetic"
		}
		return &dal.QueryValidationError{
			Category: "shape",
			Path:     r.path,
			Message:  fmt.Sprintf("unknown field %q in ORDER BY: no source of the query carries it, and %s", clip(r.name), alias),
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
	columns := query.Columns()
	flow.from(query.From(), path+"from")
	flow.condition(query.Where(), path+"where")
	// From here on, an aggregating query reads a field from the first source: the clauses
	// below are the aggregation's, and WHERE and the conditions of ON above are not.
	aggregated := dal.HasAggregation(query)
	if aggregated {
		flow.kind = readAggregated
	}
	for i, group := range query.GroupBy() {
		flow.expression(group, fmt.Sprintf("%sgroupBy[%d]", path, i))
	}
	// HAVING and ORDER BY of a query that aggregates read the alias of a column as the
	// column.
	if aggregated {
		flow.aliases = replaceableAliases(columns)
	}
	flow.condition(query.Having(), path+"having")
	// ORDER BY of a query that does not aggregate reads a field of a source, wherever a
	// column of the select list is named so.
	if !aggregated {
		flow.kind = readOrdered
		flow.names = namesOfColumns(columns)
	}
	for i, order := range query.OrderBy() {
		flow.expression(order.Expression(), fmt.Sprintf("%sorderBy[%d]", path, i))
	}
	flow.aliases, flow.names = nil, columnNames{}
	flow.kind = readAnywhere
	if aggregated {
		flow.kind = readAggregated
		flow.earlier = map[string]bool{}
	}
	for i, column := range columns {
		flow.expression(column.Expression, fmt.Sprintf("%scolumns[%d]", path, i))
		if aggregated {
			flow.earlier[outputName(column)] = !flow.isOwnName(column)
		}
	}
}

// isOwnName reports whether the column is a field that is named as itself, by its alias or
// by none: what a later unqualified reference to the name reads is the same field. The
// field is unqualified, or it names the one source of the query, which an unqualified
// field of that name is read from too.
func (f *scopeFlow) isOwnName(column dal.Column) bool {
	field, ok := column.Expression.(dal.FieldRef)
	if !ok || field.Name() != outputName(column) {
		return false
	}
	return field.Source() == "" || len(f.level.sources) == 1 && field.Source() == sourceAlias(f.level.sources[0])
}

// columnNames says what a name stands for when a column of a select list carries it.
type columnNames struct {
	// fields are the aliases of the columns that select a field of a source, with the
	// field each selects.
	fields map[string]dal.Expression
	// computed are the aliases of the columns that are an expression of any other kind,
	// and the result names of a scalar subquery.
	computed map[string]bool
}

// namesOfColumns reads the names the columns of a select list give their values.
func namesOfColumns(columns []dal.Column) columnNames {
	names := columnNames{fields: map[string]dal.Expression{}, computed: map[string]bool{}}
	for _, column := range columns {
		if column.Expression == nil {
			continue
		}
		if field, ok := column.Expression.(dal.FieldRef); ok {
			if column.Alias != "" {
				names.fields[column.Alias] = field
			}
			continue
		}
		name := column.Alias
		if scalar, ok := column.Expression.(dal.QueryExpression); ok && name == "" {
			name = scalar.As()
		}
		if name != "" {
			names.computed[name] = true
		}
	}
	return names
}

// scopeFlow walks the clauses of one level, adding its sources and unqualified
// fields to it and handing every query inside a clause to the walk.
type scopeFlow struct {
	walk  *scopeWalk
	level *scopeLevel
	// kind is how the clause in hand has an unqualified field read.
	kind readKind
	// aliases are the names a field may carry without naming a field of a source: the
	// aliases of the columns that resolveAliases replaces, while HAVING and ORDER BY of
	// a query that aggregates are walked, and none otherwise.
	aliases map[string]dal.Expression
	// names are what the columns of the select list call their values, while the ORDER BY
	// of a query that does not aggregate is walked, and none otherwise.
	names columnNames
	// earlier is, while the columns of a query that aggregates are walked, the names of the
	// columns before the one in hand that DALgo's aggregation reads a field of that name
	// as: true for every name but one that is the same field. It is nil otherwise.
	earlier map[string]bool
	// inExpression is true while the operands of arithmetic are walked: the alias of a column
	// is read as the column only where it is the whole expression of an ORDER BY.
	inExpression bool
}

// refuse adds a refusal that needs no field list to the level.
func (f *scopeFlow) refuse(category, path, format string, args ...any) {
	f.level.refusals = append(f.level.refusals, &dal.QueryValidationError{Category: category, Path: path, Message: fmt.Sprintf(format, args...)})
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
		kind := f.kind
		f.kind = readOn
		for j, on := range join.On() {
			f.condition(on, fmt.Sprintf("%s.on[%d]", joinAt, j))
		}
		f.kind = kind
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
		if value.Source() == "" {
			f.field(value.Name(), path)
		} else if f.kind == readOrdered {
			f.level.qualified = append(f.level.qualified, scopeRef{name: value.Name(), path: path, kind: readOrdered, qualifier: value.Source()})
		}
	case dal.BinaryExpression:
		inside := f.inExpression
		f.inExpression = true
		f.expression(value.Left, path+".left")
		f.expression(value.Right, path+".right")
		f.inExpression = inside
	case dal.AggregateFunc:
		// An argument is read per row, from a source: an alias is not a field there, and
		// neither is the name of an earlier column.
		aliases, earlier := f.aliases, f.earlier
		f.aliases, f.earlier = nil, nil
		for i, arg := range value.FuncArgs() {
			f.expression(arg, fmt.Sprintf("%s.args[%d]", path, i))
		}
		f.aliases, f.earlier = aliases, earlier
	case dal.QueryExpression:
		f.walk.query(value.Query(), path+".query.")
	}
}

// field adds an unqualified field of the clause in hand to the level, or refuses it when
// the name is read as something else than a field of a source.
func (f *scopeFlow) field(name, path string) {
	if _, alias := f.aliases[name]; alias {
		return
	}
	if !f.inExpression {
		if _, alias := f.names.fields[name]; alias {
			// resolveAliases replaces it by the field its column selects, which is looked at
			// in the column.
			return
		}
		if f.names.computed[name] {
			f.refuse("scope", path, "cannot order by %s, the alias of a column that is not a field, when the document is not run whole by a SQL database: order by the fields of its expression", clip(name))
			return
		}
	}
	if f.earlier[name] {
		f.refuse("scope", path, "the unqualified field %s is also the name of an earlier column of a query that aggregates, and would be read as that column: rename the alias or qualify the field with its source", clip(name))
		return
	}
	f.level.refs = append(f.level.refs, scopeRef{name: name, path: path, kind: f.kind, inExpression: f.inExpression})
	if f.kind == readOrdered {
		f.level.ordered = true
	}
}
