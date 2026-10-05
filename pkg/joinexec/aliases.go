package joinexec

import (
	"context"

	"github.com/dal-go/dalgo/dal"
)

// The alias of a column in HAVING and ORDER BY.
//
// In a query that aggregates, HAVING and ORDER BY may name a column of the select list
// by its alias: DALgo's aggregation reads such a name as the column it computed, and an
// aggregate or a grouped expression in either clause as the same value. DALgo's join
// evaluation, which runs under every document that reaches it here, checks every field
// of the query against the field lists the sources supply (OJ-14) and knows no alias,
// so a name that is not a field of a source is refused as unavailable: the document
// that was answered while no list was supplied would be a 400.
//
// resolveAliases closes that. Before DALgo sees the document, every unqualified name in
// HAVING and ORDER BY of a query that aggregates (dal.HasAggregation, DALgo's own test)
// that a column carries as its alias is replaced by the expression of that column,
// wherever the query stands: the document itself, a derived source, a scalar subquery or
// an EXISTS test. The column and the expression are one value to the aggregation, which
// computes it from the same group, so the answer is the one it gave for the alias.
//
// A name is replaced only where DALgo reads it as the column: not in the argument of an
// aggregate, where DALgo reads a field of a source, and not in WHERE, ON, GROUP BY or the
// columns of any query (checkScopes treats them the same way). A query that names no alias
// is returned as it is, so a document that never needed the rewrite reaches DALgo as it was
// built. A query inside an aggregate's argument is not looked at: DALgo refuses the argument
// of an aggregate that is not a field, a constant or arithmetic over them.
//
// ORDER BY of a query that does not aggregate is the one clause where DALgo reads a name
// as a field of a source and a SQL database that runs the whole document reads it as the
// column. A name that a column carries as the alias of a field of a source is replaced by
// that field here too, at every level of the document, so the answer is sorted the way the
// database sorts it, by the field the column selects, and no executor (DALgo, or the one of
// a mount that is handed the document whole) is given a name it reads as another field or
// does not know. Only an ORDER BY expression that is the bare name is the column: inside
// arithmetic the database reads a field of a source (order by b*1 is the table's own b, not
// the alias), so a name there is left alone, for checkScopes to look at as the field it is.
// The alias of a column that is not a field has nothing to be replaced by: checkScopes
// refuses it. Nothing here runs on the database route, where the SQL database reads the
// alias itself.
//
// Nor is a name replaced when the replacement would be read as another column. DALgo's
// aggregation reads an unqualified field of an expression first as the column of the select
// list that is named so, and as a field only when none is. A column that selects the
// unqualified field n under the alias a, beside a column that is named n, would be ordered
// by the other column: the name a is left as it is, and DALgo refuses it as unavailable
// (replaceableAliases says which names are replaced).

// replaceableAliases lists the columns of a select list that carry an alias and that
// resolveAliases may replace the alias of, by alias, each with the expression it
// computes. DALgo's aggregation names a column by its alias, and only by an alias a column
// carries: the result name of a scalar subquery is not one (a scalar subquery cannot be a
// column of a query that aggregates).
//
// An alias is left out when what it stands for would be read as another column: its
// expression holds, outside an aggregate, an unqualified field that a column of the select
// list is named after, by its alias or, with none, by the field it selects. DALgo reads
// such a field as that column (aggregationColumnName names the columns), so the
// replacement of the alias would order or filter by the other column. The one exception is
// a column that is a field named as its own alias: the replacement is the name itself.
func replaceableAliases(columns []dal.Column) map[string]dal.Expression {
	named := map[string]bool{}
	for _, column := range columns {
		if name := selectedName(column); name != "" {
			named[name] = true
		}
	}
	aliases := map[string]dal.Expression{}
	for _, column := range columns {
		if column.Alias == "" || column.Expression == nil || (readsAColumn(column.Expression, named) && !isNamedAs(column)) {
			continue
		}
		aliases[column.Alias] = column.Expression
	}
	return aliases
}

// selectedName is the name DALgo's aggregation gives the column in its answer, and so the
// name an unqualified field of an expression is read as when it is the same: the alias, or
// the name of the field the column selects. A column that is neither has no name a field
// can be (a wildcard selects none, and an expression is named by its text).
func selectedName(column dal.Column) string {
	if column.Alias != "" {
		return column.Alias
	}
	if field, ok := column.Expression.(dal.FieldRef); ok {
		return field.Name()
	}
	return ""
}

// readsAColumn reports whether expression holds, outside an aggregate, an unqualified field
// that is named like one of the columns.
func readsAColumn(expression dal.Expression, named map[string]bool) bool {
	switch value := expression.(type) {
	case dal.FieldRef:
		return value.Source() == "" && named[value.Name()]
	case dal.BinaryExpression:
		return readsAColumn(value.Left, named) || readsAColumn(value.Right, named)
	}
	return false
}

// isNamedAs reports whether the column is an unqualified field named as its own alias.
func isNamedAs(column dal.Column) bool {
	field, ok := column.Expression.(dal.FieldRef)
	return ok && field.Source() == "" && field.Name() == column.Alias
}

// resolveAliases returns query with the aliases of its select lists resolved in HAVING
// and ORDER BY (of a query that does not aggregate: the alias of a field in ORDER BY), at
// every level of the document, or query itself when it names none.
// Execute walks and checks the document before it gets here, so every node is of a type
// the walk accepts.
func resolveAliases(query dal.StructuredQuery) dal.StructuredQuery {
	resolved, _ := resolveQuery(query)
	return resolved
}

// resolveQuery resolves the aliases of query and of every query inside it, and
// reports whether it built a query to say so.
func resolveQuery(query dal.StructuredQuery) (dal.StructuredQuery, bool) {
	from, fromChanged := resolveFrom(query.From())
	where, whereChanged := resolveCondition(query.Where())
	groupBy, groupChanged := mapEach(query.GroupBy(), resolveExpression)
	columns, columnsChanged := mapEach(query.Columns(), resolveColumn)
	having, havingChanged := resolveCondition(query.Having())
	orderBy, orderChanged := mapEach(query.OrderBy(), resolveOrder)
	if dal.HasAggregation(query) {
		aliases := replaceableAliases(columns)
		var haveAliasChanged, orderAliasChanged bool
		having, haveAliasChanged = replaceInCondition(having, aliases)
		orderBy, orderAliasChanged = mapEach(orderBy, func(order dal.OrderExpression) (dal.OrderExpression, bool) {
			expression, changed := replaceInExpression(order.Expression(), aliases)
			return reorder(order, expression), changed
		})
		havingChanged = havingChanged || haveAliasChanged
		orderChanged = orderChanged || orderAliasChanged
	} else if fields := namesOfColumns(columns).fields; len(fields) > 0 {
		var orderAliasChanged bool
		orderBy, orderAliasChanged = mapEach(orderBy, func(order dal.OrderExpression) (dal.OrderExpression, bool) {
			// Only the bare name is the column: inside arithmetic a SQL database reads a field
			// of a source (order by b*1 is not order by b), and so does this.
			field, bare := order.Expression().(dal.FieldRef)
			if !bare {
				return order, false
			}
			expression, changed := replaceInExpression(field, fields)
			return reorder(order, expression), changed
		})
		orderChanged = orderChanged || orderAliasChanged
	}
	if !fromChanged && !whereChanged && !groupChanged && !columnsChanged && !havingChanged && !orderChanged {
		return query, false
	}
	return aliasResolvedQuery{StructuredQuery: query, from: from, where: where, groupBy: groupBy, having: having, orderBy: orderBy, columns: columns}, true
}

// mapEach applies rewrite to every item and returns the items, as a new slice when
// any item changed and as the slice it was given when none did.
func mapEach[T any](items []T, rewrite func(T) (T, bool)) ([]T, bool) {
	var out []T
	for i, item := range items {
		next, changed := rewrite(item)
		if !changed {
			continue
		}
		if out == nil {
			out = append([]T(nil), items...)
		}
		out[i] = next
	}
	if out == nil {
		return items, false
	}
	return out, true
}

func resolveColumn(column dal.Column) (dal.Column, bool) {
	if column.Expression == nil {
		return column, false
	}
	expression, changed := resolveExpression(column.Expression)
	column.Expression = expression
	return column, changed
}

func resolveOrder(order dal.OrderExpression) (dal.OrderExpression, bool) {
	expression, changed := resolveExpression(order.Expression())
	return reorder(order, expression), changed
}

// reorder is order over expression, in the direction order has.
func reorder(order dal.OrderExpression, expression dal.Expression) dal.OrderExpression {
	if order.Descending() {
		return dal.Descending(expression)
	}
	return dal.Ascending(expression)
}

// resolveFrom resolves the queries a relation tree holds: derived sources, in its base
// and in its joins, and the EXISTS tests and subqueries of the ON conditions.
func resolveFrom(from dal.FromSource) (dal.FromSource, bool) {
	base, baseChanged := resolveSource(from.Base())
	joins, joinsChanged := mapEach(from.Joins(), resolveJoin)
	if !baseChanged && !joinsChanged {
		return from, false
	}
	rebuilt := dal.From(base)
	for _, join := range joins {
		rebuilt = rebuilt.Join(join)
	}
	return rebuilt, true
}

func resolveSource(source dal.RecordsetSource) (dal.RecordsetSource, bool) {
	derived, ok := source.(dal.QuerySource)
	if !ok {
		return source, false
	}
	inner, changed := resolveQuery(derived.Query())
	if !changed {
		return source, false
	}
	return dal.NewQuerySource(inner, derived.Alias()), true
}

// resolveJoin keeps the kind of the join, its hints and the form it was built in: a
// join of one source, or of a relation tree.
func resolveJoin(join dal.JoinedSource) (dal.JoinedSource, bool) {
	on, onChanged := mapEach(join.On(), resolveCondition)
	var (
		rebuilt dal.JoinedSource
		changed = onChanged
	)
	if tree := join.From(); tree != nil {
		child, childChanged := resolveFrom(tree)
		changed = changed || childChanged
		rebuilt = dal.NewNestedJoinedSource(child, join.JoinType(), on...)
	} else {
		source, sourceChanged := resolveSource(join.RecordsetSource)
		changed = changed || sourceChanged
		rebuilt = dal.NewJoinedSource(source, join.JoinType(), on...)
	}
	if !changed {
		return join, false
	}
	if algorithms := join.Algorithms(); algorithms != nil {
		rebuilt = rebuilt.WithAlgorithms(algorithms...)
	}
	return rebuilt, true
}

// resolveCondition resolves the queries a condition holds.
func resolveCondition(condition dal.Condition) (dal.Condition, bool) {
	switch value := condition.(type) {
	case dal.Comparison:
		left, leftChanged := resolveExpression(value.Left)
		right, rightChanged := resolveExpression(value.Right)
		if leftChanged || rightChanged {
			return dal.NewComparison(left, value.Operator, right), true
		}
	case dal.GroupCondition:
		children, changed := mapEach(value.Conditions(), resolveCondition)
		if changed {
			return dal.NewGroupCondition(value.Operator(), children...), true
		}
	case dal.IsNullCondition:
		operand, changed := resolveExpression(value.Operand())
		if changed {
			return isNull(value, operand), true
		}
	case dal.ExistsCondition:
		inner, changed := resolveQuery(value.Query())
		if changed {
			if value.Negated() {
				return dal.NewNotExistsCondition(inner), true
			}
			return dal.NewExistsCondition(inner), true
		}
	}
	return condition, false
}

// isNull is the null test that test is, over operand.
func isNull(test dal.IsNullCondition, operand dal.Expression) dal.Condition {
	if test.Negated() {
		return dal.NewIsNotNullCondition(operand)
	}
	return dal.NewIsNullCondition(operand)
}

// resolveExpression resolves the queries an expression holds.
func resolveExpression(expression dal.Expression) (dal.Expression, bool) {
	switch value := expression.(type) {
	case dal.BinaryExpression:
		left, leftChanged := resolveExpression(value.Left)
		right, rightChanged := resolveExpression(value.Right)
		if leftChanged || rightChanged {
			return dal.Binary(left, value.Operator, right), true
		}
	case dal.QueryExpression:
		inner, changed := resolveQuery(value.Query())
		if changed {
			return dal.NewQueryExpression(inner, value.As()), true
		}
	}
	return expression, false
}

// replaceInCondition replaces the aliases that a condition of HAVING names. A test that
// DALgo's HAVING does not take (EXISTS) is left to be refused.
func replaceInCondition(condition dal.Condition, aliases map[string]dal.Expression) (dal.Condition, bool) {
	switch value := condition.(type) {
	case dal.Comparison:
		left, leftChanged := replaceInExpression(value.Left, aliases)
		right, rightChanged := replaceInExpression(value.Right, aliases)
		if leftChanged || rightChanged {
			return dal.NewComparison(left, value.Operator, right), true
		}
	case dal.GroupCondition:
		children, changed := mapEach(value.Conditions(), func(child dal.Condition) (dal.Condition, bool) {
			return replaceInCondition(child, aliases)
		})
		if changed {
			return dal.NewGroupCondition(value.Operator(), children...), true
		}
	case dal.IsNullCondition:
		operand, changed := replaceInExpression(value.Operand(), aliases)
		if changed {
			return isNull(value, operand), true
		}
	}
	return condition, false
}

// replaceInExpression replaces an unqualified name that is an alias, at the top of the
// expression or in its arithmetic. It does not look inside an aggregate: the argument
// of one is read per row, from a source.
func replaceInExpression(expression dal.Expression, aliases map[string]dal.Expression) (dal.Expression, bool) {
	switch value := expression.(type) {
	case dal.FieldRef:
		if replacement, alias := aliases[value.Name()]; alias && value.Source() == "" {
			return replacement, true
		}
	case dal.BinaryExpression:
		left, leftChanged := replaceInExpression(value.Left, aliases)
		right, rightChanged := replaceInExpression(value.Right, aliases)
		if leftChanged || rightChanged {
			return dal.Binary(left, value.Operator, right), true
		}
	}
	return expression, false
}

// aliasResolvedQuery is a query with the clauses that resolveQuery rebuilt. The rest of
// it (the paging, the cursors, the record it reads into) is the query it wraps. Like the
// wrapper DALgo gives a foreign query, it hands itself, not the query it wraps, to an
// executor, and prints the clauses it holds.
type aliasResolvedQuery struct {
	dal.StructuredQuery
	from    dal.FromSource
	where   dal.Condition
	groupBy []dal.Expression
	having  dal.Condition
	orderBy []dal.OrderExpression
	columns []dal.Column
}

func (q aliasResolvedQuery) From() dal.FromSource           { return q.from }
func (q aliasResolvedQuery) Where() dal.Condition           { return q.where }
func (q aliasResolvedQuery) GroupBy() []dal.Expression      { return q.groupBy }
func (q aliasResolvedQuery) Having() dal.Condition          { return q.having }
func (q aliasResolvedQuery) OrderBy() []dal.OrderExpression { return q.orderBy }
func (q aliasResolvedQuery) Columns() []dal.Column          { return q.columns }
func (q aliasResolvedQuery) String() string                 { return dal.QueryString(q) }

// Money hands on the money configuration the wrapped query declares, which DALgo's
// federated executor reads from a query by type assertion: a wrapper that did not have the
// method would hide it, and the executor would run a money document as plain numbers. A
// query that declares none declares none here.
func (q aliasResolvedQuery) Money() *dal.MoneyConfig {
	if declarative, ok := q.StructuredQuery.(interface{ Money() *dal.MoneyConfig }); ok {
		return declarative.Money()
	}
	return nil
}

func (q aliasResolvedQuery) GetRecordsReader(ctx context.Context, executor dal.QueryExecutor) (dal.RecordsReader, error) {
	return executor.ExecuteQueryToRecordsReader(ctx, q)
}

func (q aliasResolvedQuery) GetRecordsetReader(ctx context.Context, executor dal.QueryExecutor) (dal.RecordsetReader, error) {
	return executor.ExecuteQueryToRecordsetReader(ctx, q)
}
