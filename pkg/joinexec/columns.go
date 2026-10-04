package joinexec

import (
	"fmt"
	"sort"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/record"
)

// orderedColumns lists the column names of a result in the order the query
// selects them, so a client can render the table without guessing the order of
// a JSON object.
//
// Rows are objects, so only the select list has an order. Each selected column
// is named the way DALgo names it (its alias, else the field it reads, else the
// expression's own name), and where the rows show a different spelling for an
// unaliased expression (DALgo names it one way when it projects a joined row
// and another when it aggregates) the spelling the rows carry wins. A wildcard
// stands for the columns that nothing else in the select list names, sorted,
// at the position it has. A key the select list does not explain is appended,
// sorted, so no field a row carries is left out. The result is never nil.
func orderedColumns(query dal.StructuredQuery, records []record.Record) []string {
	keys := map[string]bool{}
	for _, rec := range records {
		if data, ok := rec.Data().(map[string]any); ok {
			for key := range data {
				keys[key] = true
			}
		}
	}
	sorted := make([]string, 0, len(keys))
	for key := range keys {
		sorted = append(sorted, key)
	}
	sort.Strings(sorted)

	selected := query.Columns()
	named := make(map[string]bool, len(selected))
	for i, column := range selected {
		if column.Wildcard == nil {
			named[columnName(query, column, i, keys)] = true
		}
	}

	columns := make([]string, 0, len(sorted))
	seen := map[string]bool{}
	add := func(name string) {
		if !seen[name] {
			seen[name] = true
			columns = append(columns, name)
		}
	}
	for i, column := range selected {
		if column.Wildcard == nil {
			add(columnName(query, column, i, keys))
			continue
		}
		for _, name := range sorted {
			if !named[name] && !column.Wildcard.Excludes(name) {
				add(name)
			}
		}
	}
	for _, name := range sorted {
		add(name)
	}
	return columns
}

// columnName is the name DALgo gives a selected column that is not a wildcard.
func columnName(query dal.StructuredQuery, column dal.Column, index int, keys map[string]bool) string {
	if column.Alias != "" {
		return column.Alias
	}
	if field, ok := column.Expression.(dal.FieldRef); ok {
		return field.Name()
	}
	aggregated := column.Expression.String()
	projected := fmt.Sprintf("column_%d", index)
	if scalar, ok := column.Expression.(dal.QueryExpression); ok && scalar.As() != "" {
		projected = scalar.As()
	}
	candidates := []string{projected, aggregated}
	if dal.HasAggregation(query) {
		candidates = []string{aggregated, projected}
	}
	for _, name := range candidates {
		if keys[name] {
			return name
		}
	}
	return candidates[0]
}
