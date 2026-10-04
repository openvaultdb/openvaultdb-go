package core

import (
	"fmt"

	"github.com/dal-go/dalgo/dal"
	"github.com/openvaultdb/openvaultdb-go/pkg/manifest"
)

// ProfileKind names the DTQL profile a query belongs to.
type ProfileKind string

const (
	// ProfileSingleCollection is today's profile: one plain root collection
	// of the database the request is addressed to, accepted by validateDTQL.
	ProfileSingleCollection ProfileKind = "single-collection"
	// ProfileRelational is the bounded relational profile: joins, GROUP BY,
	// HAVING, aggregates, column aliases and subqueries over root collections.
	ProfileRelational ProfileKind = "relational"
)

// Relational profile bounds.
const (
	relationalMaxLimit         = 1000
	relationalMaxOffset        = 10000
	relationalMaxSources       = 8
	relationalMaxSubqueryDepth = 4
)

// ProfileSource is one collection read a query makes. A query that reads the
// same collection twice lists it twice, because each reference is a read.
type ProfileSource struct {
	// Database is the mounted database id the source names, or empty when the
	// source does not name one.
	Database string
	// Collection is the root collection name.
	Collection string
	// Alias is the alias the query gives the source, or empty.
	Alias string
}

// Profile is the classification of a DTQL query.
type Profile struct {
	Kind ProfileKind
	// Sources lists every collection read in the query, in document order,
	// including reads inside derived sources and subquery expressions.
	Sources []ProfileSource
	// HasSubquery reports a derived source, scalar subquery, EXISTS predicate
	// or query-valued comparison operand anywhere in the query.
	HasSubquery bool
	// HasAggregation reports GROUP BY, HAVING or an aggregate function in the
	// query's own columns or ordering.
	HasAggregation bool
}

// ClassifyDTQL decides which profile query belongs to and refuses everything
// outside both. It is a security boundary: a shape it does not recognise is
// refused with ErrInvalidDTQL, never passed through.
//
// A query is single-collection only when validateDTQL accepts it and it also
// names no database and has no subquery anywhere (validateDTQL does not look
// at conditions, so a subquery in WHERE would otherwise hide a second source).
// Every other query must satisfy the relational rules, which are checked over
// the whole query tree, subqueries included.
func ClassifyDTQL(query dal.StructuredQuery) (Profile, error) {
	walk := &profileWalk{}
	if err := walk.query(query, "", 0); err != nil {
		return Profile{}, err
	}
	hasSubquery := dal.HasSubquery(query)
	if !hasSubquery && len(walk.sources) == 1 && walk.sources[0].Database == "" {
		if collection, err := validateDTQL(query); err == nil {
			return Profile{
				Kind:    ProfileSingleCollection,
				Sources: []ProfileSource{{Collection: collection}},
			}, nil
		}
	}
	return Profile{
		Kind:           ProfileRelational,
		Sources:        walk.sources,
		HasSubquery:    hasSubquery,
		HasAggregation: dal.HasAggregation(query),
	}, nil
}

// profileWalk collects sources and enforces the relational rules in one pass
// over the exported dal tree.
type profileWalk struct {
	sources []ProfileSource
}

func relationalRefusal(rule, path, detail string) error {
	if path == "" {
		path = "$"
	}
	return fmt.Errorf("%w: relational profile: %s at %s: %s", ErrInvalidDTQL, rule, path, detail)
}

func joinPath(path, suffix string) string {
	if path == "" {
		return suffix
	}
	return path + "." + suffix
}

func indexPath(path string, i int) string { return fmt.Sprintf("%s[%d]", path, i) }

// query checks one query and everything nested in it. depth is the number of
// subquery levels above it, path the DTQL path of the query ("" for the root).
func (w *profileWalk) query(query dal.StructuredQuery, path string, depth int) error {
	if query == nil {
		return relationalRefusal("query-shape", path, "a query is required")
	}
	if depth > relationalMaxSubqueryDepth {
		return relationalRefusal("subquery-depth", path, fmt.Sprintf("subqueries nest at most %d levels", relationalMaxSubqueryDepth))
	}
	if query.StartFrom() != "" || query.StartAfter() != "" {
		return relationalRefusal("cursor", path, "cursors are not supported")
	}
	if configured, ok := query.(interface{ Money() *dal.MoneyConfig }); ok && configured.Money() != nil {
		return relationalRefusal("money", path, "money arithmetic is not supported")
	}
	if limit := query.Limit(); limit < 0 || limit > relationalMaxLimit {
		return relationalRefusal("limit", path, fmt.Sprintf("limit must be 0..%d", relationalMaxLimit))
	}
	if offset := query.Offset(); offset < 0 || offset > relationalMaxOffset {
		return relationalRefusal("offset", path, fmt.Sprintf("offset must be 0..%d", relationalMaxOffset))
	}
	if err := w.from(query.From(), joinPath(path, "from"), depth); err != nil {
		return err
	}
	if err := w.condition(query.Where(), joinPath(path, "where"), depth); err != nil {
		return err
	}
	for i, expression := range query.GroupBy() {
		if err := w.expression(expression, indexPath(joinPath(path, "groupBy"), i), depth); err != nil {
			return err
		}
	}
	if err := w.condition(query.Having(), joinPath(path, "having"), depth); err != nil {
		return err
	}
	for i, order := range query.OrderBy() {
		orderPath := indexPath(joinPath(path, "orderBy"), i)
		if order == nil {
			return relationalRefusal("expression-shape", orderPath, "an ordering expression is required")
		}
		if err := w.expression(order.Expression(), orderPath, depth); err != nil {
			return err
		}
	}
	for i, column := range query.Columns() {
		if column.Wildcard != nil && column.Expression == nil {
			continue
		}
		if err := w.expression(column.Expression, indexPath(joinPath(path, "columns"), i), depth); err != nil {
			return err
		}
	}
	return nil
}

// from checks a relation tree: its base source, then each join's relation
// tree and ON conditions.
func (w *profileWalk) from(from dal.FromSource, path string, depth int) error {
	if from == nil || from.Base() == nil {
		return relationalRefusal("query-shape", path, "a source is required")
	}
	if err := w.source(from.Base(), path, depth); err != nil {
		return err
	}
	for i, join := range from.Joins() {
		joinAt := indexPath(joinPath(path, "joins"), i)
		if join.JoinType() != dal.JoinInner && join.JoinType() != dal.JoinLeft {
			return relationalRefusal("join-type", joinAt, fmt.Sprintf("only inner and left joins are supported, not %s", join.JoinType()))
		}
		child := join.From()
		if child == nil {
			child = dal.From(join.RecordsetSource)
		}
		if err := w.from(child, joinPath(joinAt, "from"), depth); err != nil {
			return err
		}
		for j, on := range join.On() {
			if err := w.condition(on, indexPath(joinPath(joinAt, "on"), j), depth); err != nil {
				return err
			}
		}
	}
	return nil
}

// source checks one relation: a root collection, or a derived query.
func (w *profileWalk) source(source dal.RecordsetSource, path string, depth int) error {
	switch value := source.(type) {
	case dal.CollectionRef:
		return w.collection(value, path)
	case dal.CollectionGroupRef:
		return relationalRefusal("collection-group", path, "collection groups are not supported")
	case dal.QuerySource:
		return w.query(value.Query(), joinPath(path, "query"), depth+1)
	default:
		return relationalRefusal("source-shape", path, fmt.Sprintf("unsupported source type %T", source))
	}
}

func (w *profileWalk) collection(ref dal.CollectionRef, path string) error {
	if ref.Parent() != nil {
		return relationalRefusal("parent-source", path, "only root collections are supported")
	}
	// The access layer treats a schema-qualified source as an opaque
	// resource, so collection policies would not match it.
	if ref.Schema() != "" {
		return relationalRefusal("schema", path, "schema-qualified sources are not supported")
	}
	if ref.ScanLimit() != 0 || len(ref.ScanOrders()) != 0 {
		return relationalRefusal("scan", path, "scan bounds are not supported")
	}
	if err := ValidateCollectionName(ref.Name()); err != nil {
		return relationalRefusal("collection-name", path, err.Error())
	}
	if database := ref.Database(); database != "" {
		if err := manifest.ValidateID(database); err != nil {
			return relationalRefusal("database-id", path, err.Error())
		}
	}
	if len(w.sources) >= relationalMaxSources {
		return relationalRefusal("source-count", path, fmt.Sprintf("at most %d sources are supported", relationalMaxSources))
	}
	w.sources = append(w.sources, ProfileSource{Database: ref.Database(), Collection: ref.Name(), Alias: ref.Alias()})
	return nil
}

// condition checks a condition tree. A nil condition is an absent clause.
func (w *profileWalk) condition(condition dal.Condition, path string, depth int) error {
	switch value := condition.(type) {
	case nil:
		return nil
	case dal.Comparison:
		if err := w.expression(value.Left, joinPath(path, "left"), depth); err != nil {
			return err
		}
		return w.expression(value.Right, joinPath(path, "right"), depth)
	case dal.GroupCondition:
		for i, child := range value.Conditions() {
			childPath := indexPath(joinPath(path, groupKey(value.Operator())), i)
			if child == nil {
				return relationalRefusal("condition-shape", childPath, "a condition is required")
			}
			if err := w.condition(child, childPath, depth); err != nil {
				return err
			}
		}
		return nil
	case dal.IsNullCondition:
		key := "isNull"
		if value.Negated() {
			key = "isNotNull"
		}
		return w.expression(value.Operand(), joinPath(path, key), depth)
	case dal.ExistsCondition:
		key := "exists"
		if value.Negated() {
			key = "notExists"
		}
		return w.query(value.Query(), joinPath(joinPath(path, key), "query"), depth+1)
	default:
		return relationalRefusal("condition-shape", path, fmt.Sprintf("unsupported condition type %T", condition))
	}
}

func groupKey(operator dal.Operator) string {
	if operator == dal.Or {
		return "or"
	}
	return "and"
}

// expression checks an expression tree. Expressions are never absent.
func (w *profileWalk) expression(expression dal.Expression, path string, depth int) error {
	switch value := expression.(type) {
	case dal.FieldRef, dal.Constant, dal.Param, dal.Array:
		return nil
	case dal.StarExpression:
		return nil
	case dal.BinaryExpression:
		if err := w.expression(value.Left, joinPath(joinPath(path, "binary"), "left"), depth); err != nil {
			return err
		}
		return w.expression(value.Right, joinPath(joinPath(path, "binary"), "right"), depth)
	case dal.AggregateFunc:
		for i, arg := range value.FuncArgs() {
			if err := w.expression(arg, indexPath(joinPath(joinPath(path, "aggregate"), "args"), i), depth); err != nil {
				return err
			}
		}
		return nil
	case dal.QueryExpression:
		return w.query(value.Query(), joinPath(path, "query"), depth+1)
	default:
		return relationalRefusal("expression-shape", path, fmt.Sprintf("unsupported expression type %T", expression))
	}
}
