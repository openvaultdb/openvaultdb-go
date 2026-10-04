package joinexec

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/dal-go/dalgo/dal"
)

// The walk reads a document the way the profile classifier does, over the same
// node types and no others, and reports what the executor needs to know
// without trusting the profile it was given:
//
//   - every collection the document reads, in the order the classifier lists
//     them, with whether the read carries a scan clause;
//   - whether any subquery or null test appears;
//   - whether every name in the document is a plain name.
//
// A node of a type it does not know is refused: whatever such a node holds is
// out of sight of the authorisation, so the document does not run.

// maxWalkDepth bounds how deep subqueries and join trees nest. The profile
// admits four levels of subquery; the walk allows more, so it never refuses
// what the profile accepted, and it stops a query graph that refers to itself.
const maxWalkDepth = 16

// maxNameLen is the longest field name, qualifier or alias.
const maxNameLen = 256

// maxEchoLen is the most bytes of a refused name an error message repeats. A
// name can be as long as the request body, and an error is not the place to
// send it back.
const maxEchoLen = 64

// The name rules mirror pkg/core (ValidateFieldName, ValidateCollectionName and
// the identifier rule of validateDTQLFields). They are repeated here so that
// this package does not depend on pkg/core, which imports it to declare that a
// database is a Source. A test in pkg/core (joinexec_names_drift_test.go) runs
// one table of documents through both and fails when they answer differently,
// so a change to one rule without the other is caught. A name is a plain name
// when it matches, so a character nobody thought of is refused.
//
// Two differences are known and pinned by that test. A field qualifier may name
// a source of any query of the document, where pkg/core scopes it to its own
// query and the queries around it; the name is a validated collection name or
// alias either way. And pkg/core's name check refuses a null test, which the
// relational profile accepts and the walk checks.
var (
	fieldNameRe  = regexp.MustCompile(`^(\$[\p{L}_]|[\p{L}\p{Nd}_])[\p{L}\p{Nd}_-]*(\.(\$[\p{L}_]|[\p{L}\p{Nd}_])[\p{L}\p{Nd}_-]*)*$`)
	identifierRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// walkedSource is one collection read.
type walkedSource struct {
	database   string
	collection string
	// scan is true when the read carries a scan limit or scan order.
	scan bool
}

// document is what the walk found.
type document struct {
	sources     []walkedSource
	hasSubquery bool
	// hasNull is true when any condition tests for null (IS NULL, IS NOT NULL).
	// The SQL adapters cannot compile such a test, so a document with one is
	// never sent to an engine whole.
	hasNull bool
}

// anyScan reports whether any source carries a scan limit or scan order.
func (d document) anyScan() bool {
	for _, source := range d.sources {
		if source.scan {
			return true
		}
	}
	return false
}

type walker struct {
	doc document
	// scope holds the collection names and aliases of every source, which a
	// field qualifier may name.
	scope       map[string]bool
	fields      []string
	identifiers []string
	qualifiers  []string
}

// inspect walks query and checks its names. The returned document is empty
// when it returns an error.
func inspect(query dal.StructuredQuery) (document, error) {
	w := &walker{scope: map[string]bool{}}
	if err := w.query(query, "$", 0); err != nil {
		return document{}, err
	}
	if err := w.checkNames(); err != nil {
		return document{}, err
	}
	return w.doc, nil
}

// checkProfile reports whether profile describes doc: the same collections as
// many times over (in any order) and the same answer about subqueries. The
// profile is a claim: the execution authorises and routes by what the walk
// found, and refuses a profile that says less than the document reads, so a
// classifier that misses a source is caught here rather than trusted.
func checkProfile(doc document, profile Profile) error {
	if profile.HasSubquery != doc.hasSubquery {
		return fmt.Errorf("%w: the profile does not match the query: subqueries", ErrInvalidDocument)
	}
	counts := map[ProfileSource]int{}
	for _, s := range doc.sources {
		counts[ProfileSource{Database: s.database, Collection: s.collection}]++
	}
	for _, s := range profile.Sources {
		counts[s]--
	}
	for _, n := range counts {
		if n != 0 {
			return fmt.Errorf("%w: the profile does not match the query: sources", ErrInvalidDocument)
		}
	}
	return nil
}

func refuse(path, format string, args ...any) error {
	return fmt.Errorf("%w: %s: %s", ErrInvalidDocument, path, fmt.Sprintf(format, args...))
}

func (w *walker) query(query dal.StructuredQuery, path string, depth int) error {
	if query == nil {
		return refuse(path, "a query is required")
	}
	if depth > maxWalkDepth {
		return refuse(path, "subqueries nest too deep")
	}
	if err := w.from(query.From(), path+".from", depth); err != nil {
		return err
	}
	if err := w.condition(query.Where(), path+".where", depth); err != nil {
		return err
	}
	for i, expression := range query.GroupBy() {
		if err := w.expression(expression, fmt.Sprintf("%s.groupBy[%d]", path, i), depth); err != nil {
			return err
		}
	}
	if err := w.condition(query.Having(), path+".having", depth); err != nil {
		return err
	}
	for i, order := range query.OrderBy() {
		orderPath := fmt.Sprintf("%s.orderBy[%d]", path, i)
		if order == nil {
			return refuse(orderPath, "an ordering expression is required")
		}
		if err := w.expression(order.Expression(), orderPath, depth); err != nil {
			return err
		}
	}
	for i, column := range query.Columns() {
		if err := w.column(column, fmt.Sprintf("%s.columns[%d]", path, i), depth); err != nil {
			return err
		}
	}
	return nil
}

func (w *walker) column(column dal.Column, path string, depth int) error {
	if column.Alias != "" {
		w.identifiers = append(w.identifiers, column.Alias)
	}
	if column.Wildcard != nil {
		if column.Wildcard.Source != "" {
			w.qualifiers = append(w.qualifiers, column.Wildcard.Source)
		}
		w.fields = append(w.fields, column.Wildcard.Exclude...)
		if column.Expression == nil {
			return nil
		}
	}
	if column.Expression == nil {
		return refuse(path, "a column needs an expression")
	}
	return w.expression(column.Expression, path, depth)
}

// from walks a relation tree: its base source, then each join's relation tree
// and ON conditions.
func (w *walker) from(from dal.FromSource, path string, depth int) error {
	if depth > maxWalkDepth {
		return refuse(path, "relations nest too deep")
	}
	if from == nil || from.Base() == nil {
		return refuse(path, "a source is required")
	}
	if err := w.source(from.Base(), path, depth); err != nil {
		return err
	}
	for i, join := range from.Joins() {
		joinAt := fmt.Sprintf("%s.joins[%d]", path, i)
		child := join.From()
		if child == nil {
			if join.RecordsetSource == nil {
				return refuse(joinAt, "a join needs a source")
			}
			child = dal.From(join.RecordsetSource)
		}
		if err := w.from(child, joinAt+".from", depth+1); err != nil {
			return err
		}
		for j, on := range join.On() {
			if err := w.condition(on, fmt.Sprintf("%s.on[%d]", joinAt, j), depth); err != nil {
				return err
			}
		}
	}
	return nil
}

func (w *walker) source(source dal.RecordsetSource, path string, depth int) error {
	switch value := source.(type) {
	case dal.CollectionRef:
		return w.collection(value, path, depth)
	case dal.QuerySource:
		w.doc.hasSubquery = true
		w.identifiers = append(w.identifiers, value.Alias())
		w.scope[value.Alias()] = true
		return w.query(value.Query(), path+".query", depth+1)
	default:
		return refuse(path, "unsupported source %T", source)
	}
}

func (w *walker) collection(ref dal.CollectionRef, path string, depth int) error {
	if ref.Parent() != nil || ref.Schema() != "" {
		return refuse(path, "only plain root collections are supported")
	}
	if !isCollectionName(ref.Name()) {
		return refuse(path, "collection name %q is not a plain collection name", clip(ref.Name()))
	}
	w.doc.sources = append(w.doc.sources, walkedSource{
		database:   ref.Database(),
		collection: ref.Name(),
		scan:       ref.ScanLimit() != 0 || len(ref.ScanOrders()) != 0,
	})
	w.scope[ref.Name()] = true
	if ref.Alias() != "" {
		w.identifiers = append(w.identifiers, ref.Alias())
		w.scope[ref.Alias()] = true
	}
	for i, order := range ref.ScanOrders() {
		orderPath := fmt.Sprintf("%s.scan[%d]", path, i)
		if order == nil {
			return refuse(orderPath, "an ordering expression is required")
		}
		if err := w.expression(order.Expression(), orderPath, depth); err != nil {
			return err
		}
	}
	return nil
}

// condition walks a condition tree. A nil condition is an absent clause.
func (w *walker) condition(condition dal.Condition, path string, depth int) error {
	switch value := condition.(type) {
	case nil:
		return nil
	case dal.Comparison:
		if err := w.expression(value.Left, path+".left", depth); err != nil {
			return err
		}
		return w.expression(value.Right, path+".right", depth)
	case dal.GroupCondition:
		for i, child := range value.Conditions() {
			childPath := fmt.Sprintf("%s[%d]", path, i)
			if child == nil {
				return refuse(childPath, "a condition is required")
			}
			if err := w.condition(child, childPath, depth); err != nil {
				return err
			}
		}
		return nil
	case dal.IsNullCondition:
		w.doc.hasNull = true
		return w.expression(value.Operand(), path+".isNull", depth)
	case dal.ExistsCondition:
		w.doc.hasSubquery = true
		return w.query(value.Query(), path+".exists", depth+1)
	default:
		return refuse(path, "unsupported condition %T", condition)
	}
}

// expression walks an expression tree. Expressions are never absent.
func (w *walker) expression(expression dal.Expression, path string, depth int) error {
	switch value := expression.(type) {
	case dal.FieldRef:
		w.fields = append(w.fields, value.Name())
		if value.Source() != "" {
			w.qualifiers = append(w.qualifiers, value.Source())
		}
		return nil
	case dal.Constant, dal.Array:
		return nil
	case dal.Param:
		if !dal.ValidParamName(value.Name) {
			return refuse(path, "parameter name %q is not valid", clip(value.Name))
		}
		return nil
	case dal.StarExpression:
		return nil
	case dal.BinaryExpression:
		if err := w.expression(value.Left, path+".left", depth); err != nil {
			return err
		}
		return w.expression(value.Right, path+".right", depth)
	case dal.AggregateFunc:
		for i, arg := range value.FuncArgs() {
			if err := w.expression(arg, fmt.Sprintf("%s.args[%d]", path, i), depth); err != nil {
				return err
			}
		}
		return nil
	case dal.QueryExpression:
		w.doc.hasSubquery = true
		return w.query(value.Query(), path+".query", depth+1)
	default:
		return refuse(path, "unsupported expression %T", expression)
	}
}

// checkNames refuses a name that is not plain. Field names and aliases are
// checked once the whole document is known, so that a qualifier may name any
// source of it.
func (w *walker) checkNames() error {
	for _, name := range w.fields {
		if len(name) > maxNameLen || !fieldNameRe.MatchString(name) || strings.Contains(name, "--") {
			return refuse("$", "field name %q is not a plain field name", clip(name))
		}
	}
	for _, name := range w.identifiers {
		if !isIdentifier(name) {
			return refuse("$", "%q is not a plain identifier", clip(name))
		}
	}
	for _, name := range w.qualifiers {
		if !w.scope[name] && !isIdentifier(name) {
			return refuse("$", "qualifier %q is not a plain identifier or a source of the document", clip(name))
		}
	}
	return nil
}

func isIdentifier(name string) bool {
	return len(name) <= maxNameLen && identifierRe.MatchString(name)
}

// isCollectionName applies the rule of pkg/core's ValidateCollectionName, which
// is ValidateSegment: the name is not empty, has no control character, and none
// of its '/'- or '\'-separated components is "." or "..". The collection name is
// the one name that becomes a path on a file-backed engine such as inGitDB, so
// it is checked here too, even though the classifier checks it before the
// document gets this far.
func isCollectionName(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	parts := strings.FieldsFunc(name, func(r rune) bool { return r == '/' || r == '\\' })
	if len(parts) == 0 {
		return false
	}
	for _, part := range parts {
		if part == "." || part == ".." {
			return false
		}
	}
	return true
}

// clip shortens name to maxEchoLen bytes, on a character boundary, for an
// error message.
func clip(name string) string {
	if len(name) <= maxEchoLen {
		return name
	}
	cut := maxEchoLen
	for cut > 0 && !utf8.RuneStart(name[cut]) {
		cut--
	}
	return name[:cut] + "..."
}
