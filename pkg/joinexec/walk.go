package joinexec

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/dal-go/dalgo/dal"
	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
)

// The walk reads a document the way the profile classifier does, over the same
// node types and no others, and reports what the executor needs to know
// without trusting the profile it was given:
//
//   - every collection the document reads, in the order the classifier lists
//     them, with whether the read carries a scan clause;
//   - whether any subquery or null test appears;
//   - whether every name in the document is a plain name;
//   - whether the columns of every query have output names that differ;
//   - whether conditions and expressions nest within a bound.
//
// A node of a type it does not know is refused: whatever such a node holds is
// out of sight of the authorisation, so the document does not run.

// maxWalkDepth bounds how deep subqueries and join trees nest, counted together:
// a subquery is one level and so is each level of a join tree. The classifier
// admits four levels of subquery and eight sources (relationalMaxSubqueryDepth
// and relationalMaxSources in pkg/core), so the deepest document it accepts
// nests 4 + 7 = 11 levels; the walk allows 16, so it never refuses what the
// classifier accepted, and it stops a query graph that refers to itself. The
// drift test in pkg/core builds the deepest document the classifier accepts from
// those two constants and requires the walk to accept it, so a raised limit of
// the classifier that this bound would refuse fails there.
const maxWalkDepth = 16

// maxWalkNesting bounds how many conditions and expressions sit one inside the
// next, counted across subqueries. It is the bound of the profile walk of
// pkg/core (relationalMaxNesting), counted the same way, and the drift test in
// pkg/core compares the two. A level is one condition or one expression: a
// comparison is a level and so is each of its operands. The bound keeps the
// recursion of the walk, and the length of the paths it builds, in proportion
// whatever the depth of the document.
const maxWalkNesting = 64

// arithmeticOperators and aggregateFunctions are the operators and aggregate
// names the classifier of pkg/core accepts in any position (validateRelationalNames
// and core.AggregateFunctions, the five functions of the profile); the drift test
// in pkg/core compares them.
var (
	arithmeticOperators = map[dal.ArithmeticOperator]bool{dal.Add: true, dal.Subtract: true, dal.Multiply: true, dal.Divide: true}
	aggregateFunctions  = map[string]bool{"COUNT": true, "SUM": true, "AVG": true, "MIN": true, "MAX": true}
)

// maxNameLen is the longest field name, qualifier or alias.
const maxNameLen = 256

// maxEchoLen is the most bytes of a refused name an error message repeats. A
// name can be as long as the request body, and an error is not the place to
// send it back.
const maxEchoLen = 64

// The name rules mirror pkg/core: ValidateFieldName (the strict field-name
// rule), ValidateCollectionName and the identifier rule of the name check the
// profile classifier runs. They are repeated here so that this package depends
// on DALgo alone, and so that the test in pkg/core that compares the two
// (joinexec_names_drift_test.go) can import this package. That test runs one
// table of documents through both and fails when they answer differently, so a
// change to one rule without the other is caught. A name is a plain name when it
// matches, so a character nobody thought of is refused.
//
// Four differences between the walk's name rules and the classifier's are known
// and pinned by that test. The classifier applies a wider quoted-name rule to the
// field names of a relational document (a column named "zip code" is a name
// there); the walk applies the strict rule on every route, so Execute refuses a
// document with a field name that only the wider rule accepts. A field
// qualifier may name a source of any query of the document, where the classifier
// scopes it to its own query and the queries around it; the name is a validated
// collection name or alias either way. The walk refuses a query whose columns
// carry one output name twice, which the classifier's name check does not
// compare. And the walk refuses a parameter wherever it sits, because nothing
// binds a parameter once a document reaches Execute, where the classifier takes
// a parameter of a valid name as an operand. The walk also refuses an arithmetic
// operator outside + - * / and an aggregate name outside the classifier's five,
// as the classifier does, so those are not differences. What only the classifier
// checks is listed on Execute.
var (
	fieldNameRe  = regexp.MustCompile(`^(\$[\p{L}_]|[\p{L}\p{Nd}_])[\p{L}\p{M}\p{Nd}_-]*(\.(\$[\p{L}_]|[\p{L}\p{Nd}_])[\p{L}\p{M}\p{Nd}_-]*)*$`)
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
	// nest is the number of conditions and expressions being walked above the
	// node in hand, across subqueries.
	nest int
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
		return fmt.Errorf("%w: %w: subqueries", ErrInvalidDocument, ErrProfileMismatch)
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
			return fmt.Errorf("%w: %w: sources", ErrInvalidDocument, ErrProfileMismatch)
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
	names := map[string]bool{}
	for i, column := range query.Columns() {
		columnPath := fmt.Sprintf("%s.columns[%d]", path, i)
		if err := w.column(column, columnPath, depth); err != nil {
			return err
		}
		// A row is keyed by the output name of its columns, so two columns with one
		// name would lose one of them (a database that runs the document keeps the
		// later column) or fail (DALgo's join refuses the document): the document
		// is refused on every route before anything is read.
		if name := outputName(column); name != "" {
			if names[name] {
				return refuse(columnPath, "duplicate output name %q", clip(name))
			}
			names[name] = true
		}
	}
	return nil
}

// outputName is the name a column has in the result: its alias, else the field
// name of a field, the result name of a scalar subquery, or the text of an
// aggregate, as DALgo names them. A wildcard stands for the fields of its source
// and a column of any other kind has no name to compare; both return "".
func outputName(column dal.Column) string {
	if column.Wildcard != nil {
		return ""
	}
	if column.Alias != "" {
		return column.Alias
	}
	switch value := column.Expression.(type) {
	case dal.FieldRef:
		return value.Name()
	case dal.QueryExpression:
		return value.As()
	case dal.AggregateFunc:
		return value.String()
	}
	return ""
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
	if ref.Parent() != nil {
		return refuse(path, "only plain root collections are supported")
	}
	collection := ref.Name()
	if ref.Schema() != "" {
		var err error
		collection, err = schema.NativePostgresCollectionID(ref.Schema(), ref.Name())
		if err != nil {
			return refuse(path, "schema-qualified relation name is outside the PostgreSQL identifier contract")
		}
	} else if !isCollectionName(ref.Name()) {
		return refuse(path, "collection name %q is not a plain collection name", clip(ref.Name()))
	}
	w.doc.sources = append(w.doc.sources, walkedSource{
		database:   ref.Database(),
		collection: collection,
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

// enter counts one more condition or expression level and refuses past the
// bound. A level that is walked without a refusal ends with w.nest--; the walk
// stops at its first refusal, so the levels it unwinds through are not counted
// back.
func (w *walker) enter(path string) error {
	w.nest++
	if w.nest > maxWalkNesting {
		return refuse(path, "conditions and expressions nest more than %d levels", maxWalkNesting)
	}
	return nil
}

// condition walks a condition tree. A nil condition is an absent clause.
func (w *walker) condition(condition dal.Condition, path string, depth int) error {
	if condition == nil {
		return nil
	}
	if err := w.enter(path); err != nil {
		return err
	}
	if err := w.conditionNode(condition, path, depth); err != nil {
		return err
	}
	w.nest--
	return nil
}

func (w *walker) conditionNode(condition dal.Condition, path string, depth int) error {
	switch value := condition.(type) {
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
	if err := w.enter(path); err != nil {
		return err
	}
	if err := w.expressionNode(expression, path, depth); err != nil {
		return err
	}
	w.nest--
	return nil
}

func (w *walker) expressionNode(expression dal.Expression, path string, depth int) error {
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
		// A parameter is bound before a document runs (a JSON body binds its
		// parameters, a YAML body binds none), and DALgo's join evaluates no
		// parameter: it answers one as a refusal that depends on the route and on
		// where the parameter sits. One that reaches this point was never bound.
		return refuse(path, "parameter %q is not bound: bind it in a JSON body or give the value", clip(value.Name))
	case dal.StarExpression:
		return nil
	case dal.BinaryExpression:
		// DALgo reads any text as an arithmetic operator and checks it only where
		// it runs an aggregation. The text is not echoed.
		if !arithmeticOperators[value.Operator] {
			return refuse(path, "an arithmetic operator must be one of + - * /")
		}
		if err := w.expression(value.Left, path+".left", depth); err != nil {
			return err
		}
		return w.expression(value.Right, path+".right", depth)
	case dal.AggregateFunc:
		// The same: DALgo validates an aggregate's name only where it aggregates. A
		// name with a byte of 0x80 or above is refused, as core.IsAggregateFunction
		// refuses it: the two fold the case of a name in opposite directions, and
		// Go's folding does not agree with itself outside ASCII.
		if !isASCII(value.FuncName()) || !aggregateFunctions[strings.ToUpper(value.FuncName())] {
			return refuse(path, "an aggregate function must be one of COUNT, SUM, AVG, MIN, MAX")
		}
		for i, arg := range value.FuncArgs() {
			if err := w.expression(arg, fmt.Sprintf("%s.args[%d]", path, i), depth); err != nil {
				return err
			}
		}
		return nil
	case dal.QueryExpression:
		w.doc.hasSubquery = true
		// The result name becomes a key of the row and a column name.
		if as := value.As(); as != "" {
			w.identifiers = append(w.identifiers, as)
		}
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

// isASCII reports whether every byte of s is below 0x80.
func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}
