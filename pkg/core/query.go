package core

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sort"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo/dtql"
	"github.com/dal-go/record"
)

// Query is a structured query in the JSON wire format (see docs/api.md).
// Filters are AND-ed. It translates 1:1 onto dal.StructuredQuery and executes
// natively on the DALgo driver — ovdb does not evaluate queries itself.
type Query struct {
	Collection string    `json:"collection"`
	Parent     string    `json:"parent,omitempty"` // dal-escaped parent key path for scoped subcollection queries
	Where      []Filter  `json:"where,omitempty"`
	OrderBy    []OrderBy `json:"orderBy,omitempty"`
	Limit      int       `json:"limit,omitempty"`
	KeysOnly   bool      `json:"keysOnly,omitempty"`
}

// Filter is one field condition. Op is one of:
// ==, <, <=, >, >=, in, array-contains, array-contains-any.
type Filter struct {
	Field string `json:"field"`
	Op    string `json:"op"`
	Value any    `json:"value"`
}

// OrderBy is one ordering key.
type OrderBy struct {
	Field string `json:"field"`
	Desc  bool   `json:"desc,omitempty"`
}

// Record is one query result.
type Record struct {
	Key  *record.Key
	Data map[string]any
}

// Execute translates the wire query to dal.StructuredQuery and runs it on
// the driver.
func (d *Database) Execute(ctx context.Context, q Query) ([]Record, error) {
	if q.Collection == "" {
		return nil, fmt.Errorf("query: collection is required")
	}
	collectionRef := dal.NewRootCollectionRef(q.Collection, "")
	if q.Parent != "" {
		parentKey, err := ParseKeyPath(q.Parent)
		if err != nil {
			return nil, fmt.Errorf("query: invalid parent key %q: %w", q.Parent, err)
		}
		collectionRef = dal.NewCollectionRef(q.Collection, "", parentKey)
	}
	var builder dal.IQueryBuilder = dal.From(collectionRef).NewQuery()
	for _, f := range q.Where {
		switch f.Op {
		case "==":
			builder = builder.WhereField(f.Field, dal.Equal, f.Value)
		case "<":
			builder = builder.WhereField(f.Field, dal.LessThen, f.Value)
		case "<=":
			builder = builder.WhereField(f.Field, dal.LessOrEqual, f.Value)
		case ">":
			builder = builder.WhereField(f.Field, dal.GreaterThen, f.Value)
		case ">=":
			builder = builder.WhereField(f.Field, dal.GreaterOrEqual, f.Value)
		case "in":
			builder = builder.WhereField(f.Field, dal.In, f.Value)
		case "array-contains":
			builder = builder.WhereArrayContains(f.Field, f.Value)
		case "array-contains-any":
			builder = builder.WhereArrayContainsAny(f.Field, f.Value)
		default:
			return nil, fmt.Errorf("query: unknown filter op %q", f.Op)
		}
	}
	for _, ob := range q.OrderBy {
		if ob.Desc {
			builder = builder.OrderBy(dal.DescendingField(ob.Field))
		} else {
			builder = builder.OrderBy(dal.AscendingField(ob.Field))
		}
	}
	// Keys-only queries without explicit ordering must return IDs in sorted
	// order with the limit applied after sorting (the dalgo end2end contract,
	// natural for document stores). SQL drivers return insertion order, so
	// core sorts and limits itself in that case.
	sortKeysInCore := q.KeysOnly && len(q.OrderBy) == 0
	if q.Limit > 0 && !sortKeysInCore {
		builder = builder.Limit(q.Limit)
	}
	var query dal.StructuredQuery
	if q.KeysOnly {
		query = builder.SelectKeysOnly(reflect.String)
	} else {
		query = builder.SelectIntoRecord(func() record.Record {
			// Canonical factory shape (matches the dalgo end2end suite):
			// an incomplete string-ID key — readers fill the ID.
			return record.NewRecordWithIncompleteKey(q.Collection, reflect.String, map[string]any{})
		})
	}
	records, err := d.executeDalQuery(ctx, query, q.Collection, q.KeysOnly)
	if err != nil {
		return nil, err
	}
	if sortKeysInCore {
		sort.Slice(records, func(i, j int) bool {
			return fmt.Sprintf("%v", records[i].Key.ID) < fmt.Sprintf("%v", records[j].Key.ID)
		})
		if q.Limit > 0 && len(records) > q.Limit {
			records = records[:q.Limit]
		}
	}
	return records, nil
}

// ExecuteDTQL deserializes a DTQL-YAML document (dalgo's native query
// serialization) and passes it straight through to the driver — ovdb's job
// is only to (in the future) authenticate it.
func (d *Database) ExecuteDTQL(ctx context.Context, doc []byte) ([]Record, error) {
	query, err := dtql.Deserialize(doc)
	if err != nil {
		return nil, fmt.Errorf("invalid DTQL document: %w", err)
	}
	collection := ""
	if from := query.From(); from != nil {
		if named, ok := from.Base().(interface{ Name() string }); ok {
			collection = named.Name()
		}
	}
	keysOnly := query.IntoRecord() == nil
	return d.executeDalQuery(ctx, query, collection, keysOnly)
}

func (d *Database) executeDalQuery(ctx context.Context, query dal.StructuredQuery, collection string, keysOnly bool) ([]Record, error) {
	reader, err := d.db.ExecuteQueryToRecordsReader(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("failed to query collection %q: %w", collection, err)
	}
	defer func() { _ = reader.Close() }()
	var records []Record
	for {
		rec, nextErr := reader.Next()
		// dal.ErrNoMoreRecords wraps io.EOF; some drivers (dalgo2sql) return
		// raw io.EOF — checking io.EOF covers both.
		if errors.Is(nextErr, io.EOF) {
			return records, nil
		}
		if nextErr != nil {
			return nil, fmt.Errorf("failed reading query results for %q: %w", collection, nextErr)
		}
		out := Record{Key: rec.Key()}
		data, _ := rec.Data().(map[string]any)
		// Relational drivers (dalgo2sql) return rows without dal keys — the
		// PK lands in the "id" column instead. Rebuild the key from it.
		// Plumbing recordset PK metadata into dalgo2sql's reader is a
		// roadmap upstream improvement.
		if collection != "" && (out.Key == nil || fmt.Sprintf("%v", out.Key.ID) == "") {
			if id, ok := data["id"].(string); ok && id != "" {
				out.Key = record.NewKeyWithID(collection, id)
			}
		}
		if !keysOnly {
			out.Data = d.coerceToSchema(collection, data)
		}
		records = append(records, out)
	}
}
