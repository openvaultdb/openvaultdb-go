package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sort"
	"time"

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

// ErrInvalidQuery identifies a structurally invalid wire query (mapped to
// HTTP 400).
var ErrInvalidQuery = errors.New("invalid query")

// Target validates the query's collection name and parent path and returns
// the parsed parent key (nil for a root-collection query) and the root
// collection that scopes it: the parent's root when a parent is given.
func (q Query) Target() (parent *record.Key, rootCollection string, err error) {
	if err = ValidateCollectionName(q.Collection); err != nil {
		return nil, "", fmt.Errorf("query: collection: %w", err)
	}
	if q.Parent == "" {
		return nil, q.Collection, nil
	}
	if parent, err = ParseKeyPath(q.Parent); err != nil {
		return nil, "", fmt.Errorf("query: parent: %w", err)
	}
	return parent, RootCollection(parent), nil
}

// Execute translates the wire query to dal.StructuredQuery and runs it on
// the driver. Result keys are full paths from the database root: records of
// a nested collection carry their parent key.
func (d *Database) Execute(ctx context.Context, q Query) ([]Record, error) {
	parentKey, _, err := q.Target()
	if err != nil {
		return nil, err
	}
	collectionRef := dal.NewRootCollectionRef(q.Collection, "")
	if parentKey != nil {
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
			return nil, fmt.Errorf("%w: unknown filter op %q", ErrInvalidQuery, f.Op)
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
	if parentKey != nil {
		// Some drivers (dalgo2ingitdb) return subcollection keys without
		// their parent; re-root them so clients get a key that round-trips
		// through /records.
		for i := range records {
			if k := records[i].Key; k.Parent() == nil {
				records[i].Key = record.NewKeyWithParentAndID(parentKey, k.Collection(), k.ID)
			}
		}
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

// ErrInvalidDTQL identifies invalid or unsupported DTQL query shapes.
var ErrInvalidDTQL = errors.New("invalid or unsupported DTQL query")

// ParseDTQL validates the server's bounded single-collection query profile.
// The returned collection is suitable for checking the token's capabilities.
func ParseDTQL(doc []byte) (dal.StructuredQuery, string, error) {
	query, err := dtql.Deserialize(doc)
	if err != nil {
		return nil, "", fmt.Errorf("%w: %v", ErrInvalidDTQL, err)
	}
	collection, err := validateDTQL(query)
	return query, collection, err
}

func validateDTQL(query dal.StructuredQuery) (string, error) {
	if query == nil || query.From() == nil || len(query.From().Joins()) != 0 {
		return "", fmt.Errorf("%w: one root collection is required", ErrInvalidDTQL)
	}
	source, ok := query.From().Base().(dal.CollectionRef)
	if !ok || source.Parent() != nil || source.Alias() != "" || source.Name() == "" {
		return "", fmt.Errorf("%w: one unaliased root collection is required", ErrInvalidDTQL)
	}
	if len(query.GroupBy()) != 0 || query.Having() != nil || query.StartFrom() != "" || query.StartAfter() != "" {
		return "", fmt.Errorf("%w: aggregation and cursors are not supported", ErrInvalidDTQL)
	}
	if query.Limit() < 0 || query.Limit() > 1000 || query.Offset() < 0 || query.Offset() > 10000 {
		return "", fmt.Errorf("%w: limit must be 0..1000 and offset 0..10000", ErrInvalidDTQL)
	}
	for _, column := range query.Columns() {
		field, ok := column.Expression.(dal.FieldRef)
		if !ok || column.Alias != "" || field.Source() != "" {
			return "", fmt.Errorf("%w: only unaliased field columns are supported", ErrInvalidDTQL)
		}
	}
	if err := ValidateCollectionName(source.Name()); err != nil {
		return "", err
	}
	return source.Name(), nil
}

// ExecuteDTQL runs DTQL through the same secured DALgo handle as record reads.
func (d *Database) ExecuteDTQL(ctx context.Context, doc []byte) ([]Record, error) {
	query, _, err := ParseDTQL(doc)
	if err != nil {
		return nil, err
	}
	return d.ExecuteDTQLQuery(ctx, query)
}

type boundedDTQL struct{ dal.StructuredQuery }

// DTQL describes projection, not a Go record factory. A nil factory from the
// decoder is not a keys-only request; install the factory needed by drivers.
func (q boundedDTQL) IntoRecord() record.Record {
	return record.NewRecordWithIncompleteKey(q.From().Base().Name(), reflect.String, map[string]any{})
}

func (q boundedDTQL) IDKind() reflect.Kind { return reflect.String }

func (q boundedDTQL) String() string { return dal.QueryString(q) }

func (q boundedDTQL) Limit() int {
	if q.StructuredQuery.Limit() == 0 {
		return 1000
	}
	return q.StructuredQuery.Limit()
}

// snapshotDTQL removes the ordinary 1000-row default for a disk-backed
// result capture. Its caller enforces a byte and row bound while streaming.
type snapshotDTQL struct{ boundedDTQL }

func (q snapshotDTQL) Limit() int { return 0 }

// StreamDTQLSnapshot executes one complete query through one DALgo reader.
// Local OVDB writes cannot interleave with the capture. The caller must bound
// the emitted result and persist it before exposing any page token.
func (d *Database) StreamDTQLSnapshot(ctx context.Context, query dal.StructuredQuery, emit func(Record) error) error {
	collection, err := validateDTQL(query)
	if err != nil {
		return err
	}
	if query.Limit() != 0 || query.Offset() != 0 {
		return fmt.Errorf("%w: snapshot query requires limit and offset to be zero", ErrInvalidDTQL)
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	d.mu.Lock()
	defer d.mu.Unlock()
	reader, err := d.db.ExecuteQueryToRecordsReader(ctx, snapshotDTQL{boundedDTQL{query}})
	if err != nil {
		return fmt.Errorf("failed to query collection %q: %w", collection, err)
	}
	defer func() { _ = reader.Close() }()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		rec, nextErr := reader.Next()
		if errors.Is(nextErr, io.EOF) {
			return nil
		}
		if nextErr != nil {
			return fmt.Errorf("failed reading query results for %q: %w", collection, nextErr)
		}
		out := Record{Key: rec.Key()}
		data, _ := rec.Data().(map[string]any)
		if out.Key == nil || fmt.Sprintf("%v", out.Key.ID) == "" {
			if id, ok := data["id"].(string); ok && id != "" {
				out.Key = record.NewKeyWithID(collection, id)
			}
		}
		if out.Key == nil {
			return fmt.Errorf("query result has no record key")
		}
		out.Data = d.coerceToSchema(collection, data)
		if err := emit(out); err != nil {
			return err
		}
	}
}

// ExecuteDTQLQuery executes an already parsed query after validating its shape.
func (d *Database) ExecuteDTQLQuery(ctx context.Context, query dal.StructuredQuery) ([]Record, error) {
	collection, err := validateDTQL(query)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return d.executeDalQuery(ctx, boundedDTQL{query}, collection, false)
}

func (d *Database) executeDalQuery(ctx context.Context, query dal.StructuredQuery, collection string, keysOnly bool) ([]Record, error) {
	return d.executeDalQueryOn(ctx, d.db, query, collection, keysOnly)
}

func (d *Database) executeDalQueryOn(ctx context.Context, db dal.DB, query dal.StructuredQuery, collection string, keysOnly bool) ([]Record, error) {
	reader, err := db.ExecuteQueryToRecordsReader(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("failed to query collection %q: %w", collection, err)
	}
	defer func() { _ = reader.Close() }()
	var records []Record
	var bytesRead int
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
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
		if out.Key == nil {
			return nil, fmt.Errorf("query result has no record key")
		}
		encoded, err := json.Marshal(out.Data)
		if err != nil {
			return nil, err
		}
		bytesRead += len(encoded) + len(out.Key.String())
		if bytesRead > 8<<20 {
			return nil, fmt.Errorf("query result exceeds 8 MiB buffer limit")
		}
		records = append(records, out)
	}
}
