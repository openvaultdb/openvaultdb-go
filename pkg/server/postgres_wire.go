package server

import (
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/dal-go/dalgo/dal"
	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
)

const maxJavaScriptSafeInteger int64 = 1<<53 - 1

// nativePostgresJSONSafeIntegers returns a copy of a PostgreSQL relational row
// with unsafe integer values represented as decimal strings. JSON/JSONB raw
// documents remain byte-for-byte intact; their nested number semantics are
// not advertised as safe for JavaScript's Number parser.
func nativePostgresJSONSafeIntegers(fields map[string]any) map[string]any {
	if fields == nil {
		return nil
	}
	out := make(map[string]any, len(fields))
	for name, field := range fields {
		out[name] = nativePostgresJSONSafeValue(field)
	}
	return out
}

func nativePostgresJSONSafeValue(value any) any {
	switch n := value.(type) {
	case int64:
		if n < -maxJavaScriptSafeInteger || n > maxJavaScriptSafeInteger {
			return strconv.FormatInt(n, 10)
		}
	case uint64:
		if n > uint64(maxJavaScriptSafeInteger) {
			return strconv.FormatUint(n, 10)
		}
	case json.RawMessage, []byte:
		// Raw JSON is kept byte-for-byte so its number lexemes and shape
		// remain intact. Binary values are base64-encoded by encoding/json.
		return value
	case map[string]any:
		// A nested object may be a JSON/JSONB document. Keep its numeric
		// values intact just as we do for json.RawMessage.
		return value
	case []any:
		out := make([]any, len(n))
		for i, item := range n {
			out[i] = nativePostgresJSONSafeValue(item)
		}
		return out
	}
	// Driver array values can be typed slices (for example []int64), not
	// []any. Preserve their normal JSON shape while checking each element.
	v := reflect.ValueOf(value)
	if !v.IsValid() || (v.Kind() != reflect.Slice && v.Kind() != reflect.Array) {
		return value
	}
	if v.Type().Elem().Kind() == reflect.Uint8 {
		return value
	}
	out := make([]any, v.Len())
	for i := range out {
		out[i] = nativePostgresJSONSafeValue(v.Index(i).Interface())
	}
	return out
}

// nativePostgresJSONValues applies the native scalar wire mapping using the
// discovered field metadata. PostgreSQL's driver exposes temporal fields as
// time.Time, whose default JSON encoding adds an offset even to DATE and
// timestamp-without-time-zone values. Use the source type to retain that
// distinction in the response.
func nativePostgresJSONValues(fields map[string]any, nativeTypes map[string]string) map[string]any {
	out := nativePostgresJSONSafeIntegers(fields)
	for name, value := range out {
		nativeType := strings.ToLower(strings.TrimSpace(nativeTypes[name]))
		switch nativeType {
		case "json", "jsonb":
			switch raw := value.(type) {
			case string:
				if json.Valid([]byte(raw)) {
					out[name] = json.RawMessage(raw)
				}
			case []byte:
				if json.Valid(raw) {
					out[name] = json.RawMessage(append([]byte(nil), raw...))
				}
			}
			continue
		}
		t, ok := value.(time.Time)
		if !ok {
			continue
		}
		switch nativeType {
		case "date":
			out[name] = t.Format("2006-01-02")
		case "time without time zone":
			out[name] = t.Format("15:04:05.999999999")
		case "time with time zone":
			out[name] = t.Format("15:04:05.999999999Z07:00")
		case "timestamp without time zone":
			out[name] = t.Format("2006-01-02T15:04:05.999999999")
		case "timestamp with time zone":
			out[name] = t.UTC().Format(time.RFC3339Nano)
		}
	}
	return out
}

func nativePostgresJSONValue(value any, nativeTypes map[string]string) any {
	if fields, ok := nativePostgresStringMap(value); ok {
		return nativePostgresJSONValues(fields, nativeTypes)
	}
	return nativePostgresJSONSafeValue(value)
}

func nativePostgresStringMap(value any) (map[string]any, bool) {
	if fields, ok := value.(map[string]any); ok {
		return fields, true
	}
	reflected := reflect.ValueOf(value)
	if !reflected.IsValid() || reflected.Kind() != reflect.Map || reflected.Type().Key().Kind() != reflect.String {
		return nil, false
	}
	fields := make(map[string]any, reflected.Len())
	iterator := reflected.MapRange()
	for iterator.Next() {
		fields[iterator.Key().String()] = iterator.Value().Interface()
	}
	return fields, true
}

func nativePostgresFieldTypes(collection *schema.Collection) map[string]string {
	if collection == nil {
		return nil
	}
	types := make(map[string]string, len(collection.Fields))
	for name, field := range collection.Fields {
		types[name] = field.NativeType
	}
	return types
}

// nativePostgresProjectionTypes follows direct field projections through
// relational aliases, joins, and derived sources. joinexec returns a row map
// keyed by output name, so the wire encoder needs this result-name mapping
// rather than the source collection's physical field names.
func nativePostgresProjectionTypes(query dal.StructuredQuery, endpoint *core.Database, databases map[string]*core.Database) map[string]string {
	if query == nil || query.From() == nil {
		return nil
	}
	type projectionSource struct {
		name   string
		fields map[string]string
	}
	var orderedSources []projectionSource
	sources := map[string]map[string]string{}
	var addSource func(dal.RecordsetSource)
	var addFrom func(dal.FromSource)
	addSource = func(source dal.RecordsetSource) {
		if source == nil {
			return
		}
		var fields map[string]string
		switch typed := source.(type) {
		case dal.CollectionRef:
			fields = nativePostgresCollectionRefTypes(typed, endpoint, databases)
		case *dal.CollectionRef:
			if typed != nil {
				fields = nativePostgresCollectionRefTypes(*typed, endpoint, databases)
			}
		case dal.QuerySource:
			fields = nativePostgresProjectionTypes(typed.Query(), endpoint, databases)
		case *dal.QuerySource:
			if typed != nil {
				fields = nativePostgresProjectionTypes(typed.Query(), endpoint, databases)
			}
		case dal.JoinedSource:
			if nested := typed.From(); nested != nil {
				addFrom(nested)
				return
			}
		}
		if fields != nil {
			name := source.Alias()
			if name == "" {
				name = source.Name()
			}
			sources[name] = fields
			orderedSources = append(orderedSources, projectionSource{name: name, fields: fields})
		}
	}
	addFrom = func(from dal.FromSource) {
		if from == nil {
			return
		}
		addSource(from.Base())
		for _, joined := range from.Joins() {
			if nested := joined.From(); nested != nil {
				addFrom(nested)
			} else {
				addSource(joined.RecordsetSource)
			}
		}
	}
	addFrom(query.From())
	if len(sources) == 0 {
		return nil
	}

	result := make(map[string]string)
	put := func(name, nativeType string) {
		if name == "" {
			return
		}
		// DALgo merges joined row maps from left to right, with each later
		// source replacing an earlier value under the same field name. SQL
		// record readers likewise assign duplicate column labels in scan order.
		// Keep that established rightmost-wins contract for wildcard outputs.
		result[name] = nativeType
	}
	fieldType := func(field dal.FieldRef) string {
		if field.Source() != "" {
			return sources[field.Source()][field.Name()]
		}
		if len(sources) == 1 {
			return orderedSources[0].fields[field.Name()]
		}
		var matched string
		for _, source := range orderedSources {
			if candidate := source.fields[field.Name()]; candidate != "" {
				if matched != "" && matched != candidate {
					return ""
				}
				matched = candidate
			}
		}
		return matched
	}
	var expressionType func(dal.Expression) string
	expressionType = func(expression dal.Expression) string {
		switch typed := expression.(type) {
		case dal.FieldRef:
			return fieldType(typed)
		case dal.QueryExpression:
			fields := nativePostgresProjectionTypes(typed.Query(), endpoint, databases)
			if len(fields) == 1 {
				for _, nativeType := range fields {
					return nativeType
				}
			}
			if typed.As() != "" {
				return fields[typed.As()]
			}
		case dal.AggregateFunc:
			// MIN and MAX preserve the PostgreSQL source type. Other aggregates
			// may widen or change it, so only their exact integer values are
			// normalized by nativePostgresJSONSafeIntegers.
			if len(typed.FuncArgs()) == 1 && (strings.EqualFold(typed.FuncName(), "min") || strings.EqualFold(typed.FuncName(), "max")) {
				return expressionType(typed.FuncArgs()[0])
			}
		}
		return ""
	}
	columns := query.Columns()
	if len(columns) == 0 {
		for _, source := range orderedSources {
			for name, nativeType := range source.fields {
				put(name, nativeType)
			}
		}
		return result
	}
	for _, column := range columns {
		if column.Wildcard != nil {
			if column.Wildcard.Source != "" {
				for name, nativeType := range sources[column.Wildcard.Source] {
					if !column.Wildcard.Excludes(name) {
						put(name, nativeType)
					}
				}
				continue
			}
			for _, source := range orderedSources {
				for name, nativeType := range source.fields {
					if !column.Wildcard.Excludes(name) {
						put(name, nativeType)
					}
				}
			}
			continue
		}
		name := column.Alias
		if name == "" {
			name = nativePostgresExpressionName(column.Expression)
		}
		put(name, expressionType(column.Expression))
	}
	return result
}

func nativePostgresExpressionName(expression dal.Expression) string {
	switch typed := expression.(type) {
	case dal.FieldRef:
		return typed.Name()
	case dal.QueryExpression:
		return typed.As()
	case dal.AggregateFunc:
		return typed.String()
	default:
		return ""
	}
}

func nativePostgresCollectionRefTypes(ref dal.CollectionRef, endpoint *core.Database, databases map[string]*core.Database) map[string]string {
	db := endpoint
	if ref.Database() != "" {
		db = databases[ref.Database()]
	}
	if db == nil || db.Manifest == nil || db.Manifest.Schemas == nil {
		return nil
	}
	var collection *schema.Collection
	if db.NativePostgresReadOnly() && ref.Schema() != "" {
		collectionID, err := schema.NativePostgresCollectionID(ref.Schema(), ref.Name())
		if err != nil {
			return nil
		}
		collection = db.Manifest.Schemas.Collection(collectionID)
	} else if ref.Schema() == "" {
		collection = db.Manifest.Schemas.Collection(ref.Name())
	}
	return nativePostgresFieldTypes(collection)
}
