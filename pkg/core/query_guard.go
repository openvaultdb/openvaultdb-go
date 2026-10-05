package core

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/dal-go/dalgo/dal"
)

// ErrQueryUnsupported identifies a structured query refused because the
// mounted storage engine cannot run structured queries safely yet (mapped to
// HTTP 501 query_unsupported). Use errors.As with *QueryUnsupportedError for
// the engine name.
var ErrQueryUnsupported = errors.New("structured queries are not supported on this engine")

// QueryUnsupportedError is the typed refusal of a structured query on an
// engine that is not yet cleared for queries.
type QueryUnsupportedError struct {
	// Engine is the storage engine of the mount, as written in its manifest.
	Engine string
}

func (e *QueryUnsupportedError) Error() string {
	return fmt.Sprintf("the %q storage engine is not yet supported for queries: structured queries (/query, /dtql) are refused on it; key reads and writes still work", e.Engine)
}

// Is makes errors.Is(err, ErrQueryUnsupported) true.
func (e *QueryUnsupportedError) Is(target error) bool { return target == ErrQueryUnsupported }

// queryEngines lists the storage engines whose driver compiles a structured
// query with bound arguments. It is an allow-list so that an engine added
// later, or a mount with an unrecognised engine, is refused until someone
// clears it here. MySQL is deliberately absent: its mount opens dalgo2sql with
// no structured-query dialect, so a query would reach the legacy text emitter,
// which writes values and field names into SQL text. PostgreSQL is absent too,
// and is cleared by the preview switch alone (see engineCleared): the
// PostgreSQL adapter forces the typed dialect, which binds every value and
// quotes every name, and the switch stays until the security review of the whole
// path is done.
var queryEngines = map[string]bool{
	"sqlite":    true,
	"ingitdb":   true,
	"firestore": true,
}

// guardQuery returns a *QueryUnsupportedError unless the mounted engine is
// cleared for structured queries. Every path that hands a structured query to
// the driver calls it first.
func (d *Database) guardQuery() error {
	if !d.CanQuery() {
		return &QueryUnsupportedError{Engine: d.queryEngine()}
	}
	return nil
}

func (d *Database) queryEngine() string {
	if d.Manifest == nil {
		return ""
	}
	return d.Manifest.Storage.Engine
}

// CanQuery reports whether structured queries (/query, /dtql) are allowed on
// this mount. It is the same allow-list guardQuery enforces, so database
// metadata can advertise exactly what the guard will accept.
func (d *Database) CanQuery() bool { return engineCleared(d.queryEngine(), d.previewPostgres) }

// EngineCanQuery reports whether the storage engine, as a manifest writes it, is
// cleared for structured queries whatever the environment says: the allow-list
// CanQuery asks, for a caller that holds an engine name and no database. It is
// false for PostgreSQL, which the preview switch clears for the mounts that read it
// (see Database.CanQuery).
func EngineCanQuery(engine string) bool { return engineCleared(engine, false) }

const maxFieldNameLen = 256

// fieldRule selects how strictly a field name is checked.
type fieldRule int

const (
	// strictNames is the plain-name rule (fieldNameRe), for every engine that
	// is not listed in quotedNameEngines.
	strictNames fieldRule = iota
	// quotedNames is the rule for an engine whose read path never puts a field
	// name into SQL text unquoted (validateQuotedFieldName).
	quotedNames
)

func (r fieldRule) validate(name string) error {
	if r == quotedNames {
		return validateQuotedFieldName(name)
	}
	return ValidateFieldName(name)
}

// quotedNameEngines lists the storage engines whose read path never puts a
// field name into SQL text unquoted: sqlite through dalgo2sql's quoting
// compiler, ingitdb builds no SQL at all. They take the wider quoted-name rule;
// every other engine keeps the strict one. The list is kept apart from
// queryEngines on purpose: an engine cleared for queries later keeps the strict
// rule until someone clears it here too, together with a test that runs a real
// query against it. Firestore is cleared for queries and holds the strict rule:
// its client rejects some of the characters the quoted rule accepts in a field
// path, so it joins this list when its own name rules are tested.
var quotedNameEngines = map[string]bool{
	"sqlite":  true,
	"ingitdb": true,
}

// fieldRule is the field-name rule of this mount's engine.
func (d *Database) fieldRule() fieldRule {
	if quotedNameEngines[d.queryEngine()] {
		return quotedNames
	}
	return strictNames
}

// fieldNameRe is the strict field-name rule for /query and /dtql, used for
// every engine that is not in quotedNameEngines. A name is one or
// more dot-separated segments (nested fields). A segment starts with a letter,
// a digit or an underscore, or with "$" immediately followed by a letter or
// underscore (the key pseudo-field $id of the document engines; "$1" is
// refused because it reads as a positional parameter), and continues with
// letters, combining marks, digits, underscore or hyphen. Letters, marks and
// digits are Unicode, because firestore field names may be non-ASCII: a
// combining mark (category M) is part of a letter in scripts such as
// Devanagari and Thai and in decomposed Latin text, so it may continue a
// segment, but it never starts one. Hyphens and digit-leading segments
// (numeric map keys such as byYear.2024) are legal there too.
// Everything else is refused: quotes, backticks, spaces and
// other whitespace, semicolons, slashes, brackets, "#", backslash, control
// characters. The comment marker "--" is refused explicitly because hyphens
// are allowed. It is an allow-list, so a character nobody thought of is
// refused too.
var fieldNameRe = regexp.MustCompile(`^(\$[\p{L}_]|[\p{L}\p{Nd}_])[\p{L}\p{M}\p{Nd}_-]*(\.(\$[\p{L}_]|[\p{L}\p{Nd}_])[\p{L}\p{M}\p{Nd}_-]*)*$`)

// ValidateFieldName checks one field name from a request body against the
// strict rule. The rule a mount applies depends on its engine.
func ValidateFieldName(name string) error {
	if len(name) > maxFieldNameLen {
		return fmt.Errorf("field name exceeds %d bytes", maxFieldNameLen)
	}
	if !fieldNameRe.MatchString(name) || strings.Contains(name, "--") {
		return fmt.Errorf("field name %q is not a plain field name (letters, digits, underscore and hyphen, an optional leading $ before a letter per segment, dot-separated for nested fields)", name)
	}
	return nil
}

// validateQuotedFieldName is the field-name rule for the engines of
// quotedNameEngines, which quote every name they write into a statement, so a
// column named "zip code" can be selected, filtered and ordered. A name is at
// most 256 bytes of valid UTF-8 and has dot-separated, non-empty segments. It
// holds no control character (NUL included), no quote character (", ' or `),
// no backslash and no semicolon, and no segment starts or ends with a space.
// Everything else, whitespace inside a segment, SQL punctuation and comment
// markers among it, is a name.
func validateQuotedFieldName(name string) error {
	if len(name) > maxFieldNameLen {
		return fmt.Errorf("field name exceeds %d bytes", maxFieldNameLen)
	}
	refuse := func() error {
		return fmt.Errorf("field name %q is not acceptable (not empty and valid UTF-8, no control character, quote, backslash or semicolon, dot-separated non-empty segments that neither start nor end with a space)", name)
	}
	if !utf8.ValidString(name) {
		return refuse()
	}
	for _, r := range name {
		if unicode.IsControl(r) || r == '"' || r == '\'' || r == '`' || r == '\\' || r == ';' {
			return refuse()
		}
	}
	for _, segment := range strings.Split(name, ".") {
		if segment == "" {
			return refuse()
		}
		first, _ := utf8.DecodeRuneInString(segment)
		last, _ := utf8.DecodeLastRuneInString(segment)
		if unicode.IsSpace(first) || unicode.IsSpace(last) {
			return refuse()
		}
	}
	return nil
}

// validateFields checks every field name a wire query carries.
func (q Query) validateFields(rule fieldRule) error {
	for _, f := range q.Where {
		if err := rule.validate(f.Field); err != nil {
			return fmt.Errorf("%w: where: %v", ErrInvalidQuery, err)
		}
	}
	for _, ob := range q.OrderBy {
		if err := rule.validate(ob.Field); err != nil {
			return fmt.Errorf("%w: orderBy: %v", ErrInvalidQuery, err)
		}
	}
	return nil
}

const maxQueryTreeDepth = 16

// sourceScope lists the source names and aliases a field qualifier may name:
// those of the query being walked and of every enclosing query. Every entry
// has been validated (a collection name by ValidateCollectionName, an alias
// as an identifier) before it is added.
type sourceScope []string

func (s sourceScope) has(name string) bool { return slices.Contains(s, name) }

// nameWalker walks a parsed DTQL query and refuses a name that is not plain:
// its sources (collections, aliases, scan orders, joins with their ON
// conditions, derived queries) and every expression position (columns,
// where, orderBy, groupBy, having, and nested subqueries). Expression,
// condition or source shapes it does not know are refused too: fail closed.
//
// The zero value is the single-collection variant, used by validateDTQL for
// the one-collection profile /dtql serves today.
type nameWalker struct {
	// names is the field-name rule; the zero value is the strict one.
	names fieldRule
	// relational selects the relational profile's variant (ClassifyDTQL). It
	// differs in two places: a source may name a database (the profile walk
	// validates the id), and an IS NULL test is walked. The single-collection
	// variant refuses both, because its adapters name no other database and
	// implement no null test.
	relational bool
	// refuseMembership marks engines whose query adapter cannot evaluate DALgo's
	// In and NotIn comparisons. It is set only by the mounted engine's guard.
	refuseMembership bool
}

// validateDTQLFields applies the single-collection variant of the name walk
// with the strict field-name rule.
func validateDTQLFields(query dal.StructuredQuery, depth int) error {
	return validateDTQLFieldsWith(query, depth, strictNames)
}

// validateDTQLFieldsWith is validateDTQLFields with the given field-name rule.
func validateDTQLFieldsWith(query dal.StructuredQuery, depth int, names fieldRule) error {
	return nameWalker{names: names}.query(query, depth, nil)
}

// validateRelationalNames applies the relational variant of the name walk to a
// whole query. ClassifyDTQL calls it, so that a relational document is held to
// the names rule ParseDTQL holds a single-collection one to. Like ParseDTQL it
// knows no engine and applies the widest rule; the Database that reads the
// query applies the rule of its own engine (checkRelationalNames).
func validateRelationalNames(query dal.StructuredQuery) error {
	return validateRelationalNamesFor(query, quotedNames)
}

// validateRelationalNamesFor is validateRelationalNames with the field-name
// rule of an engine.
func validateRelationalNamesFor(query dal.StructuredQuery, names fieldRule) error {
	return nameWalker{names: names, relational: true}.query(query, 0, nil)
}

// checkRelationalNames runs the relational name walk over a whole query with
// the field-name rule of this database's engine, as validateDTQLFor does for the
// single-collection profile. The join source's guard calls it before every
// read, so an engine outside quotedNameEngines is held to the strict rule even
// when an operator lists it as a join engine.
func (d *Database) checkRelationalNames(query dal.StructuredQuery) error {
	return (nameWalker{
		names:            d.fieldRule(),
		relational:       true,
		refuseMembership: d.queryEngine() == "ingitdb",
	}).query(query, 0, nil)
}

// guardQueryConditions refuses membership comparisons an engine cannot run,
// before the query reaches its adapter. Single-collection DTQL has already been
// shape-validated; wire queries use this narrower walk so parent collection
// reads retain their existing source rules.
func (d *Database) guardQueryConditions(query dal.StructuredQuery) error {
	if d.queryEngine() != "ingitdb" || query == nil {
		return nil
	}
	w := nameWalker{names: d.fieldRule(), relational: true, refuseMembership: true}
	return w.condition(query.Where(), 0, nil)
}

func (w nameWalker) query(query dal.StructuredQuery, depth int, outer sourceScope) error {
	if depth > maxQueryTreeDepth {
		return fmt.Errorf("%w: query nesting is too deep", ErrInvalidDTQL)
	}
	if query == nil {
		return fmt.Errorf("%w: a query is required", ErrInvalidDTQL)
	}
	sources, joins, err := flattenFrom(query.From(), depth)
	if err != nil {
		return err
	}
	scope, err := w.sources(sources, depth, outer)
	if err != nil {
		return err
	}
	for _, join := range joins {
		for _, on := range join.On() {
			if err := w.condition(on, depth+1, scope); err != nil {
				return err
			}
		}
	}
	for _, column := range query.Columns() {
		if column.Alias != "" {
			if err := validateIdentifier(column.Alias); err != nil {
				return err
			}
		}
		if column.Wildcard != nil {
			if column.Wildcard.Source != "" {
				if err := validateQualifier(column.Wildcard.Source, scope); err != nil {
					return err
				}
			}
			for _, excluded := range column.Wildcard.Exclude {
				if err := w.field(excluded); err != nil {
					return err
				}
				// DALgo reads an exclude that holds * as a case-insensitive mask, not as the
				// name of a field. ? has no meaning to DALgo at present and is refused with
				// it, so that it stays free to mean one.
				if strings.ContainsAny(excluded, "*?") {
					return fmt.Errorf("%w: a wildcard exclude names one field: it holds no * (a mask) and no ? (reserved)", ErrInvalidDTQL)
				}
			}
		}
		if err := w.expression(column.Expression, depth, scope); err != nil {
			return err
		}
	}
	if err := w.condition(query.Where(), depth, scope); err != nil {
		return err
	}
	if err := w.condition(query.Having(), depth, scope); err != nil {
		return err
	}
	for _, group := range query.GroupBy() {
		if err := w.expression(group, depth, scope); err != nil {
			return err
		}
	}
	for _, order := range query.OrderBy() {
		if err := w.expression(order.Expression(), depth, scope); err != nil {
			return err
		}
	}
	return nil
}

// flattenFrom lists every source of a from clause (the base, each join's
// source, and the sources of joined relation trees) and every join of the
// tree, refusing a missing clause or a tree nested deeper than the limit.
func flattenFrom(from dal.FromSource, depth int) ([]dal.RecordsetSource, []dal.JoinedSource, error) {
	if depth > maxQueryTreeDepth {
		return nil, nil, fmt.Errorf("%w: query nesting is too deep", ErrInvalidDTQL)
	}
	if from == nil {
		return nil, nil, fmt.Errorf("%w: a from clause is required", ErrInvalidDTQL)
	}
	sources := []dal.RecordsetSource{from.Base()}
	var joins []dal.JoinedSource
	for _, join := range from.Joins() {
		joins = append(joins, join)
		child := join.From()
		if child == nil {
			sources = append(sources, join.RecordsetSource)
			continue
		}
		childSources, childJoins, err := flattenFrom(child, depth+1)
		if err != nil {
			return nil, nil, err
		}
		sources = append(sources, childSources...)
		joins = append(joins, childJoins...)
	}
	return sources, joins, nil
}

// sources checks every source of one query and returns the scope its
// expressions run in. Names and aliases are validated for all sources before
// any scan order or derived query is walked, because those may qualify a
// field with a sibling source's name.
func (w nameWalker) sources(sources []dal.RecordsetSource, depth int, outer sourceScope) (sourceScope, error) {
	scope := slices.Clone(outer)
	for _, source := range sources {
		switch s := source.(type) {
		case dal.CollectionRef:
			if s.Parent() != nil || s.Schema() != "" || (s.Database() != "" && !w.relational) {
				return nil, fmt.Errorf("%w: only plain root collections are supported as sources", ErrInvalidDTQL)
			}
			if err := ValidateCollectionName(s.Name()); err != nil {
				return nil, fmt.Errorf("%w: %v", ErrInvalidDTQL, err)
			}
			scope = append(scope, s.Name())
			if s.Alias() != "" {
				if err := validateIdentifier(s.Alias()); err != nil {
					return nil, err
				}
				scope = append(scope, s.Alias())
			}
		case dal.QuerySource:
			if err := validateIdentifier(s.Alias()); err != nil {
				return nil, err
			}
			scope = append(scope, s.Alias())
		default:
			return nil, fmt.Errorf("%w: unsupported source %T", ErrInvalidDTQL, source)
		}
	}
	for _, source := range sources {
		switch s := source.(type) {
		case dal.CollectionRef:
			for _, order := range s.ScanOrders() {
				if err := w.expression(order.Expression(), depth+1, scope); err != nil {
					return nil, err
				}
			}
		case dal.QuerySource:
			if err := w.query(s.Query(), depth+1, scope); err != nil {
				return nil, err
			}
		}
	}
	return scope, nil
}

func (w nameWalker) field(name string) error {
	if err := w.names.validate(name); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidDTQL, err)
	}
	return nil
}

// identifierRe is the rule for an alias, and for a source qualifier that does
// not name a source in scope: a plain ASCII identifier. It is narrower than a
// fieldNameRe segment on purpose: aliases are written by the caller, not read
// from a schema.
var identifierRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// validateIdentifier checks an alias or source qualifier: a single plain
// identifier segment, no dots.
func validateIdentifier(name string) error {
	// An over-long name is not echoed: the text is the caller's and can be as
	// large as the request body. ValidateFieldName does the same.
	if len(name) > maxFieldNameLen {
		return fmt.Errorf("%w: an alias or qualifier exceeds %d bytes", ErrInvalidDTQL, maxFieldNameLen)
	}
	if !identifierRe.MatchString(name) {
		return fmt.Errorf("%w: %q is not a plain identifier", ErrInvalidDTQL, name)
	}
	return nil
}

// validateQualifier checks the source qualifier of a field or wildcard. It may
// name a source of the query or of an enclosing query, even when that
// collection name is not an ASCII identifier ("Order Details", "order-items");
// any other qualifier must be a plain identifier.
func validateQualifier(name string, scope sourceScope) error {
	if scope.has(name) {
		return nil
	}
	return validateIdentifier(name)
}

func (w nameWalker) condition(condition dal.Condition, depth int, scope sourceScope) error {
	if depth > maxQueryTreeDepth {
		return fmt.Errorf("%w: query nesting is too deep", ErrInvalidDTQL)
	}
	switch c := condition.(type) {
	case nil:
		return nil
	case dal.Comparison:
		if w.refuseMembership && dal.IsGroupOperator(c.Operator) {
			return ErrQueryNotRunnable
		}
		if err := w.expression(c.Left, depth+1, scope); err != nil {
			return err
		}
		return w.expression(c.Right, depth+1, scope)
	case dal.GroupCondition:
		for _, child := range c.Conditions() {
			if err := w.condition(child, depth+1, scope); err != nil {
				return err
			}
		}
		return nil
	case dal.IsNullCondition:
		if !w.relational {
			return fmt.Errorf("%w: unsupported condition %T", ErrInvalidDTQL, condition)
		}
		return w.expression(c.Operand(), depth+1, scope)
	case dal.ExistsCondition:
		return w.query(c.Query(), depth+1, scope)
	default:
		return fmt.Errorf("%w: unsupported condition %T", ErrInvalidDTQL, condition)
	}
}

func (w nameWalker) expression(expression dal.Expression, depth int, scope sourceScope) error {
	if depth > maxQueryTreeDepth {
		return fmt.Errorf("%w: query nesting is too deep", ErrInvalidDTQL)
	}
	switch e := expression.(type) {
	case nil:
		return nil
	case dal.FieldRef:
		if e.Source() != "" {
			if err := validateQualifier(e.Source(), scope); err != nil {
				return err
			}
		}
		return w.field(e.Name())
	case dal.Constant, dal.Array:
		return nil
	case dal.Param:
		if !dal.ValidParamName(e.Name) {
			return fmt.Errorf("%w: invalid parameter name %q", ErrInvalidDTQL, e.Name)
		}
		return nil
	case dal.BinaryExpression:
		// dalgo reads any text as an arithmetic operator and checks it only in
		// an aggregate position, so it is checked here. The text is not echoed.
		switch e.Operator {
		case dal.Add, dal.Subtract, dal.Multiply, dal.Divide:
		default:
			return fmt.Errorf("%w: an arithmetic operator must be one of + - * /", ErrInvalidDTQL)
		}
		if err := w.expression(e.Left, depth+1, scope); err != nil {
			return err
		}
		return w.expression(e.Right, depth+1, scope)
	case dal.QueryExpression:
		if as := e.As(); as != "" {
			if err := validateIdentifier(as); err != nil {
				return err
			}
		}
		return w.query(e.Query(), depth+1, scope)
	case dal.StarExpression:
		return nil
	case dal.AggregateFunc:
		// The names are the profile's (AggregateFunctions), in any position. DALgo
		// validates the name only where it runs an aggregation, so a name in a where,
		// scan or ON operand would otherwise reach an adapter.
		if !IsAggregateFunction(e.FuncName()) {
			return fmt.Errorf("%w: %s", ErrInvalidDTQL, unsupportedAggregateText(e.FuncName()))
		}
		for _, arg := range e.FuncArgs() {
			if err := w.expression(arg, depth+1, scope); err != nil {
				return err
			}
		}
		return nil
	default:
		return fmt.Errorf("%w: unsupported expression %T", ErrInvalidDTQL, expression)
	}
}
