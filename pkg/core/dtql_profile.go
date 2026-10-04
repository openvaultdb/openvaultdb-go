package core

import (
	"fmt"
	"strings"

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
	// relationalMaxLimit and relationalMaxOffset cap the outermost query only:
	// they bound the answer. A nested read is bounded by the request's read
	// budget (see pkg/joinexec), so a top-N derived source is a valid shape.
	relationalMaxLimit         = 1000
	relationalMaxOffset        = 10000
	relationalMaxSources       = 8
	relationalMaxSubqueryDepth = 4
	// relationalMaxNesting bounds how deep conditions and expressions nest,
	// counted across subqueries. It protects the walk itself; the query guard's
	// own, tighter limit (maxQueryTreeDepth) applies afterwards.
	relationalMaxNesting = 64
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
// The relational rules are checked for every document, single-collection ones
// included, over the whole query tree and the subqueries in it. A document that
// ParseDTQL accepts is therefore refused here when it carries a money
// configuration, the one such shape: a schema-qualified root, a scan root and a
// database on the root are refused by ParseDTQL as well. Names are checked next,
// with the query guard's rules (see validateRelationalNames), so a name that
// ParseDTQL refuses is not an accepted relational document either.
//
// A query is single-collection only when validateDTQL accepts it and it also
// names no database and has no subquery anywhere (validateDTQL checks names
// inside conditions but does not refuse a subquery there, so a subquery in
// WHERE would otherwise hide a second source). Every other query is relational.
func ClassifyDTQL(query dal.StructuredQuery) (Profile, error) {
	walk := &profileWalk{}
	if err := walk.query(query, 0); err != nil {
		return Profile{}, err
	}
	if err := validateRelationalNames(query); err != nil {
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

// pathSegment is one step of a DTQL path: a key, with the index of the array
// element when the step enters one ("joins[0]").
type pathSegment struct {
	key   string
	index int // -1 when the step is not an array element
}

// profileWalk collects sources and enforces the relational rules in one pass
// over the exported dal tree.
//
// It tracks where it is as a stack of path segments, one per level, and joins
// them into text only when a rule refuses. The walk stops at its first refusal,
// so the levels it unwinds through never pop their segments; nothing reads the
// path after a refusal. A successful walk therefore holds one segment per level
// of nesting, not a longer copy of the path at every level.
type profileWalk struct {
	sources []ProfileSource
	path    []pathSegment
	// nest is the number of conditions and expressions currently being walked
	// above this point.
	nest int
}

func (w *profileWalk) push(key string) { w.path = append(w.path, pathSegment{key: key, index: -1}) }

func (w *profileWalk) pushIndex(key string, i int) {
	w.path = append(w.path, pathSegment{key: key, index: i})
}

func (w *profileWalk) pop() { w.path = w.path[:len(w.path)-1] }

// pathText renders the current path as a DTQL path, "$" for the query root.
func (w *profileWalk) pathText() string {
	if len(w.path) == 0 {
		return "$"
	}
	var b strings.Builder
	for i, segment := range w.path {
		if i > 0 {
			b.WriteByte('.')
		}
		b.WriteString(segment.key)
		if segment.index >= 0 {
			fmt.Fprintf(&b, "[%d]", segment.index)
		}
	}
	return b.String()
}

// refuse returns the refusal of rule at the current path.
func (w *profileWalk) refuse(rule, detail string) error {
	return fmt.Errorf("%w: relational profile: %s at %s: %s", ErrInvalidDTQL, rule, w.pathText(), detail)
}

// refuseWith is refuse for a rule that another rule already states: the result
// wraps cause as well as ErrInvalidDTQL, so a caller that tells the two apart
// (a collection name is 400 invalid_key) still can.
func (w *profileWalk) refuseWith(rule string, cause error) error {
	return fmt.Errorf("%w: relational profile: %s at %s: %w", ErrInvalidDTQL, rule, w.pathText(), cause)
}

// enter counts one more condition or expression level and refuses past the
// nesting cap. A successful level ends with w.nest--.
func (w *profileWalk) enter() error {
	w.nest++
	if w.nest > relationalMaxNesting {
		return w.refuse("nesting", fmt.Sprintf("conditions and expressions nest at most %d levels", relationalMaxNesting))
	}
	return nil
}

// query checks one query and everything nested in it. depth is the number of
// subquery levels above it; the path is where the query sits.
func (w *profileWalk) query(query dal.StructuredQuery, depth int) error {
	if query == nil {
		return w.refuse("query-shape", "a query is required")
	}
	if depth > relationalMaxSubqueryDepth {
		return w.refuse("subquery-depth", fmt.Sprintf("subqueries nest at most %d levels", relationalMaxSubqueryDepth))
	}
	if query.StartFrom() != "" || query.StartAfter() != "" {
		return w.refuse("cursor", "cursors are not supported")
	}
	if configured, ok := query.(interface{ Money() *dal.MoneyConfig }); ok && configured.Money() != nil {
		return w.refuse("money", "money arithmetic is not supported")
	}
	// The caps bound the answer, so they apply to the outermost query only; a
	// negative number is refused at every level.
	if limit := query.Limit(); limit < 0 || (depth == 0 && limit > relationalMaxLimit) {
		if depth == 0 {
			return w.refuse("limit", fmt.Sprintf("limit must be 0..%d", relationalMaxLimit))
		}
		return w.refuse("limit", "limit must not be negative")
	}
	if offset := query.Offset(); offset < 0 || (depth == 0 && offset > relationalMaxOffset) {
		if depth == 0 {
			return w.refuse("offset", fmt.Sprintf("offset must be 0..%d", relationalMaxOffset))
		}
		return w.refuse("offset", "offset must not be negative")
	}
	w.push("from")
	if err := w.from(query.From(), depth); err != nil {
		return err
	}
	w.pop()
	w.push("where")
	if err := w.condition(query.Where(), depth); err != nil {
		return err
	}
	w.pop()
	for i, expression := range query.GroupBy() {
		w.pushIndex("groupBy", i)
		if err := w.expression(expression, depth); err != nil {
			return err
		}
		w.pop()
	}
	w.push("having")
	if err := w.condition(query.Having(), depth); err != nil {
		return err
	}
	w.pop()
	for i, order := range query.OrderBy() {
		w.pushIndex("orderBy", i)
		if order == nil {
			return w.refuse("expression-shape", "an ordering expression is required")
		}
		if err := w.expression(order.Expression(), depth); err != nil {
			return err
		}
		w.pop()
	}
	for i, column := range query.Columns() {
		if column.Wildcard != nil && column.Expression == nil {
			continue
		}
		w.pushIndex("columns", i)
		if err := w.expression(column.Expression, depth); err != nil {
			return err
		}
		w.pop()
	}
	return nil
}

// from checks a relation tree: its base source, then each join's relation
// tree and ON conditions.
func (w *profileWalk) from(from dal.FromSource, depth int) error {
	if from == nil || from.Base() == nil {
		return w.refuse("query-shape", "a source is required")
	}
	if err := w.source(from.Base(), depth); err != nil {
		return err
	}
	for i, join := range from.Joins() {
		w.pushIndex("joins", i)
		if join.JoinType() != dal.JoinInner && join.JoinType() != dal.JoinLeft {
			return w.refuse("join-type", fmt.Sprintf("only inner and left joins are supported, not %s", join.JoinType()))
		}
		child := join.From()
		if child == nil {
			child = dal.From(join.RecordsetSource)
		}
		w.push("from")
		if err := w.from(child, depth); err != nil {
			return err
		}
		w.pop()
		for j, on := range join.On() {
			w.pushIndex("on", j)
			if err := w.condition(on, depth); err != nil {
				return err
			}
			w.pop()
		}
		w.pop()
	}
	return nil
}

// source checks one relation: a root collection, or a derived query.
func (w *profileWalk) source(source dal.RecordsetSource, depth int) error {
	switch value := source.(type) {
	case dal.CollectionRef:
		return w.collection(value)
	case dal.CollectionGroupRef:
		return w.refuse("collection-group", "collection groups are not supported")
	case dal.QuerySource:
		w.push("query")
		if err := w.query(value.Query(), depth+1); err != nil {
			return err
		}
		w.pop()
		return nil
	default:
		return w.refuse("source-shape", fmt.Sprintf("unsupported source type %T", source))
	}
}

func (w *profileWalk) collection(ref dal.CollectionRef) error {
	if ref.Parent() != nil {
		return w.refuse("parent-source", "only root collections are supported")
	}
	// The access layer treats a schema-qualified source as an opaque
	// resource, so collection policies would not match it.
	if ref.Schema() != "" {
		return w.refuse("schema", "schema-qualified sources are not supported")
	}
	if ref.ScanLimit() != 0 || len(ref.ScanOrders()) != 0 {
		return w.refuse("scan", "scan bounds are not supported")
	}
	if err := ValidateCollectionName(ref.Name()); err != nil {
		return w.refuseWith("collection-name", err)
	}
	if database := ref.Database(); database != "" {
		if err := manifest.ValidateID(database); err != nil {
			return w.refuse("database-id", err.Error())
		}
	}
	if len(w.sources) >= relationalMaxSources {
		return w.refuse("source-count", fmt.Sprintf("at most %d sources are supported", relationalMaxSources))
	}
	w.sources = append(w.sources, ProfileSource{Database: ref.Database(), Collection: ref.Name(), Alias: ref.Alias()})
	return nil
}

// condition checks a condition tree. A nil condition is an absent clause.
func (w *profileWalk) condition(condition dal.Condition, depth int) error {
	if condition == nil {
		return nil
	}
	if err := w.enter(); err != nil {
		return err
	}
	if err := w.conditionNode(condition, depth); err != nil {
		return err
	}
	w.nest--
	return nil
}

func (w *profileWalk) conditionNode(condition dal.Condition, depth int) error {
	switch value := condition.(type) {
	case dal.Comparison:
		w.push("left")
		if err := w.expression(value.Left, depth); err != nil {
			return err
		}
		w.pop()
		w.push("right")
		if err := w.expression(value.Right, depth); err != nil {
			return err
		}
		w.pop()
		return nil
	case dal.GroupCondition:
		for i, child := range value.Conditions() {
			w.pushIndex(groupKey(value.Operator()), i)
			if child == nil {
				return w.refuse("condition-shape", "a condition is required")
			}
			if err := w.condition(child, depth); err != nil {
				return err
			}
			w.pop()
		}
		return nil
	case dal.IsNullCondition:
		key := "isNull"
		if value.Negated() {
			key = "isNotNull"
		}
		w.push(key)
		if err := w.expression(value.Operand(), depth); err != nil {
			return err
		}
		w.pop()
		return nil
	case dal.ExistsCondition:
		key := "exists"
		if value.Negated() {
			key = "notExists"
		}
		w.push(key)
		w.push("query")
		if err := w.query(value.Query(), depth+1); err != nil {
			return err
		}
		w.pop()
		w.pop()
		return nil
	default:
		return w.refuse("condition-shape", fmt.Sprintf("unsupported condition type %T", condition))
	}
}

func groupKey(operator dal.Operator) string {
	if operator == dal.Or {
		return "or"
	}
	return "and"
}

// expression checks an expression tree. Expressions are never absent.
func (w *profileWalk) expression(expression dal.Expression, depth int) error {
	if err := w.enter(); err != nil {
		return err
	}
	if err := w.expressionNode(expression, depth); err != nil {
		return err
	}
	w.nest--
	return nil
}

func (w *profileWalk) expressionNode(expression dal.Expression, depth int) error {
	switch value := expression.(type) {
	case dal.FieldRef, dal.Constant, dal.Param, dal.Array:
		return nil
	case dal.StarExpression:
		return nil
	case dal.BinaryExpression:
		w.push("binary")
		w.push("left")
		if err := w.expression(value.Left, depth); err != nil {
			return err
		}
		w.pop()
		w.push("right")
		if err := w.expression(value.Right, depth); err != nil {
			return err
		}
		w.pop()
		w.pop()
		return nil
	case dal.AggregateFunc:
		w.push("aggregate")
		for i, arg := range value.FuncArgs() {
			w.pushIndex("args", i)
			if err := w.expression(arg, depth); err != nil {
				return err
			}
			w.pop()
		}
		w.pop()
		return nil
	case dal.QueryExpression:
		w.push("query")
		if err := w.query(value.Query(), depth+1); err != nil {
			return err
		}
		w.pop()
		return nil
	default:
		return w.refuse("expression-shape", fmt.Sprintf("unsupported expression type %T", expression))
	}
}
