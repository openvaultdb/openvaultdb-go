package core

import (
	"errors"
	"fmt"
	"regexp"
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
// more dot-separated segments (nested fields); a segment is a letter or
// underscore, optionally preceded by one "$" (the key pseudo-field $id of the
// document engines), followed by letters, digits, underscore or hyphen.
// Letters and digits are Unicode, because firestore field names may be
// non-ASCII; hyphens are legal there too. Everything else is refused on every
// engine: quotes, backticks, spaces and other whitespace, semicolons, slashes,
// brackets, "#", backslash, control characters. The comment marker "--" is
// refused explicitly because hyphens are allowed. It is an allow-list, so a
// character nobody thought of is refused too.
var fieldNameRe = regexp.MustCompile(`^\$?[\p{L}_][\p{L}\p{Nd}_-]*(\.\$?[\p{L}_][\p{L}\p{Nd}_-]*)*$`)

// ValidateFieldName checks one field name from a request body.
func ValidateFieldName(name string) error {
	if len(name) > maxFieldNameLen {
		return fmt.Errorf("field name exceeds %d bytes", maxFieldNameLen)
	}
	if !fieldNameRe.MatchString(name) || strings.Contains(name, "--") {
		return fmt.Errorf("field name %q is not a plain field name (letters, digits, underscore and hyphen, an optional leading $ per segment, dot-separated for nested fields)", name)
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

// validateDTQLFields walks every expression position of a parsed DTQL query
// (columns, where, orderBy, groupBy, having, and nested subqueries) and
// refuses a field name that is not a plain identifier. Expression or condition
// shapes it does not know are refused too: fail closed.
func validateDTQLFields(query dal.StructuredQuery, depth int) error {
	if depth > maxQueryTreeDepth {
		return fmt.Errorf("%w: query nesting is too deep", ErrInvalidDTQL)
	}
	for _, column := range query.Columns() {
		if column.Alias != "" {
			if err := validateIdentifier(column.Alias); err != nil {
				return err
			}
		}
		if column.Wildcard != nil {
			if column.Wildcard.Source != "" {
				if err := validateIdentifier(column.Wildcard.Source); err != nil {
					return err
				}
			}
			for _, excluded := range column.Wildcard.Exclude {
				if err := validateField(excluded); err != nil {
					return err
				}
			}
		}
		if err := validateExpression(column.Expression, depth); err != nil {
			return err
		}
	}
	if err := validateCondition(query.Where(), depth); err != nil {
		return err
	}
	if err := validateCondition(query.Having(), depth); err != nil {
		return err
	}
	for _, group := range query.GroupBy() {
		if err := validateExpression(group, depth); err != nil {
			return err
		}
	}
	for _, order := range query.OrderBy() {
		if err := validateExpression(order.Expression(), depth); err != nil {
			return err
		}
	}
	return nil
}

func validateField(name string) error {
	if err := ValidateFieldName(name); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidDTQL, err)
	}
	return nil
}

// identifierRe is one undotted segment of fieldNameRe: the rule for an alias
// or a source qualifier.
var identifierRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// validateIdentifier checks an alias or source qualifier: a single plain
// identifier segment, no dots.
func validateIdentifier(name string) error {
	if len(name) > maxFieldNameLen || !identifierRe.MatchString(name) {
		return fmt.Errorf("%w: %q is not a plain identifier", ErrInvalidDTQL, name)
	}
	return nil
}

func validateCondition(condition dal.Condition, depth int) error {
	if depth > maxQueryTreeDepth {
		return fmt.Errorf("%w: query nesting is too deep", ErrInvalidDTQL)
	}
	switch c := condition.(type) {
	case nil:
		return nil
	case dal.Comparison:
		if err := validateExpression(c.Left, depth+1); err != nil {
			return err
		}
		return validateExpression(c.Right, depth+1)
	case dal.GroupCondition:
		for _, child := range c.Conditions() {
			if err := validateCondition(child, depth+1); err != nil {
				return err
			}
		}
		return nil
	case dal.ExistsCondition:
		return validateDTQLFields(c.Query(), depth+1)
	default:
		return fmt.Errorf("%w: unsupported condition %T", ErrInvalidDTQL, condition)
	}
}

func validateExpression(expression dal.Expression, depth int) error {
	if depth > maxQueryTreeDepth {
		return fmt.Errorf("%w: query nesting is too deep", ErrInvalidDTQL)
	}
	switch e := expression.(type) {
	case nil:
		return nil
	case dal.FieldRef:
		if e.Source() != "" {
			if err := validateIdentifier(e.Source()); err != nil {
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
		if err := validateExpression(e.Left, depth+1); err != nil {
			return err
		}
		return validateExpression(e.Right, depth+1)
	case dal.QueryExpression:
		return validateDTQLFields(e.Query(), depth+1)
	case dal.StarExpression:
		return nil
	case dal.AggregateFunc:
		for _, arg := range e.FuncArgs() {
			if err := validateExpression(arg, depth+1); err != nil {
				return err
			}
		}
		return nil
	default:
		return fmt.Errorf("%w: unsupported expression %T", ErrInvalidDTQL, expression)
	}
}
