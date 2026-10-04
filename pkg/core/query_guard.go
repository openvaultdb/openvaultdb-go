package core

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

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
// clears it here. PostgreSQL and MySQL are deliberately absent: they open
// dalgo2sql with no structured-query dialect, so a query would reach its
// legacy text emitter, which writes values and field names into SQL text.
// Re-add them only together with the reviewed compiler (plan task OV-01).
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
func (d *Database) CanQuery() bool { return queryEngines[d.queryEngine()] }

const maxFieldNameLen = 256

// fieldNameRe is the field-name rule for /query and /dtql. A name is one or
// more dot-separated segments (nested fields). A segment starts with a letter,
// a digit or an underscore, or with "$" immediately followed by a letter or
// underscore (the key pseudo-field $id of the document engines; "$1" is
// refused because it reads as a positional parameter), and continues with
// letters, digits, underscore or hyphen. Letters and digits are Unicode,
// because firestore field names may be non-ASCII; hyphens and digit-leading
// segments (numeric map keys such as byYear.2024) are legal there too.
// Everything else is refused on every engine: quotes, backticks, spaces and
// other whitespace, semicolons, slashes, brackets, "#", backslash, control
// characters. The comment marker "--" is refused explicitly because hyphens
// are allowed. It is an allow-list, so a character nobody thought of is
// refused too.
var fieldNameRe = regexp.MustCompile(`^(\$[\p{L}_]|[\p{L}\p{Nd}_])[\p{L}\p{Nd}_-]*(\.(\$[\p{L}_]|[\p{L}\p{Nd}_])[\p{L}\p{Nd}_-]*)*$`)

// ValidateFieldName checks one field name from a request body.
func ValidateFieldName(name string) error {
	if len(name) > maxFieldNameLen {
		return fmt.Errorf("field name exceeds %d bytes", maxFieldNameLen)
	}
	if !fieldNameRe.MatchString(name) || strings.Contains(name, "--") {
		return fmt.Errorf("field name %q is not a plain field name (letters, digits, underscore and hyphen, an optional leading $ before a letter per segment, dot-separated for nested fields)", name)
	}
	return nil
}

// validateFields checks every field name a wire query carries.
func (q Query) validateFields() error {
	for _, f := range q.Where {
		if err := ValidateFieldName(f.Field); err != nil {
			return fmt.Errorf("%w: where: %v", ErrInvalidQuery, err)
		}
	}
	for _, ob := range q.OrderBy {
		if err := ValidateFieldName(ob.Field); err != nil {
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

// validateDTQLFields walks a parsed DTQL query and refuses a name that is not
// plain: its sources (collections, aliases, scan orders, joins with their ON
// conditions, derived queries) and every expression position (columns,
// where, orderBy, groupBy, having, and nested subqueries). Expression,
// condition or source shapes it does not know are refused too: fail closed.
func validateDTQLFields(query dal.StructuredQuery, depth int) error {
	return validateQuery(query, depth, nil)
}

func validateQuery(query dal.StructuredQuery, depth int, outer sourceScope) error {
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
	scope, err := validateSources(sources, depth, outer)
	if err != nil {
		return err
	}
	for _, join := range joins {
		for _, on := range join.On() {
			if err := validateCondition(on, depth+1, scope); err != nil {
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
				if err := validateField(excluded); err != nil {
					return err
				}
			}
		}
		if err := validateExpression(column.Expression, depth, scope); err != nil {
			return err
		}
	}
	if err := validateCondition(query.Where(), depth, scope); err != nil {
		return err
	}
	if err := validateCondition(query.Having(), depth, scope); err != nil {
		return err
	}
	for _, group := range query.GroupBy() {
		if err := validateExpression(group, depth, scope); err != nil {
			return err
		}
	}
	for _, order := range query.OrderBy() {
		if err := validateExpression(order.Expression(), depth, scope); err != nil {
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

// validateSources checks every source of one query and returns the scope its
// expressions run in. Names and aliases are validated for all sources before
// any scan order or derived query is walked, because those may qualify a
// field with a sibling source's name.
func validateSources(sources []dal.RecordsetSource, depth int, outer sourceScope) (sourceScope, error) {
	scope := slices.Clone(outer)
	for _, source := range sources {
		switch s := source.(type) {
		case dal.CollectionRef:
			if s.Parent() != nil || s.Schema() != "" || s.Database() != "" {
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
				if err := validateExpression(order.Expression(), depth+1, scope); err != nil {
					return nil, err
				}
			}
		case dal.QuerySource:
			if err := validateQuery(s.Query(), depth+1, scope); err != nil {
				return nil, err
			}
		}
	}
	return scope, nil
}

func validateField(name string) error {
	if err := ValidateFieldName(name); err != nil {
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
	if len(name) > maxFieldNameLen || !identifierRe.MatchString(name) {
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

func validateCondition(condition dal.Condition, depth int, scope sourceScope) error {
	if depth > maxQueryTreeDepth {
		return fmt.Errorf("%w: query nesting is too deep", ErrInvalidDTQL)
	}
	switch c := condition.(type) {
	case nil:
		return nil
	case dal.Comparison:
		if err := validateExpression(c.Left, depth+1, scope); err != nil {
			return err
		}
		return validateExpression(c.Right, depth+1, scope)
	case dal.GroupCondition:
		for _, child := range c.Conditions() {
			if err := validateCondition(child, depth+1, scope); err != nil {
				return err
			}
		}
		return nil
	case dal.ExistsCondition:
		return validateQuery(c.Query(), depth+1, scope)
	default:
		return fmt.Errorf("%w: unsupported condition %T", ErrInvalidDTQL, condition)
	}
}

func validateExpression(expression dal.Expression, depth int, scope sourceScope) error {
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
		return validateField(e.Name())
	case dal.Constant, dal.Array:
		return nil
	case dal.Param:
		if !dal.ValidParamName(e.Name) {
			return fmt.Errorf("%w: invalid parameter name %q", ErrInvalidDTQL, e.Name)
		}
		return nil
	case dal.BinaryExpression:
		if err := validateExpression(e.Left, depth+1, scope); err != nil {
			return err
		}
		return validateExpression(e.Right, depth+1, scope)
	case dal.QueryExpression:
		return validateQuery(e.Query(), depth+1, scope)
	case dal.StarExpression:
		return nil
	case dal.AggregateFunc:
		for _, arg := range e.FuncArgs() {
			if err := validateExpression(arg, depth+1, scope); err != nil {
				return err
			}
		}
		return nil
	default:
		return fmt.Errorf("%w: unsupported expression %T", ErrInvalidDTQL, expression)
	}
}
