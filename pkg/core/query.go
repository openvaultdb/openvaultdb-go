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

// ResultBufferBytes is the most bytes of JSON (of the rows and of their keys) that the
// server buffers for the result of one structured read of one collection. It is the same
// number as joinexec.MaxResultBytes, the bound of a relational answer; a test of the
// server holds the two together.
const ResultBufferBytes = 8 << 20

// ErrResultTooLarge identifies a structured read whose result is larger than the buffer
// of the server (ResultBufferBytes). It is a request the caller can change, not a fault
// of the server: the HTTP server answers it `422 query_budget_exceeded`, with a hint that
// says to narrow or to page the read, and does not log it as an error. It states no
// figure: the result is not read to its end.
var ErrResultTooLarge = errors.New("the result is larger than the response buffer of the server")

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
	var records []Record
	err := d.StreamQuery(ctx, q, func(rec Record) error { records = append(records, rec); return nil })
	if err != nil {
		return nil, err
	}
	return records, nil
}

// StreamQuery executes the structured collection query used by the legacy
// /query endpoint. It emits one record at a time; the only buffered case is a
// keys-only query without ordering, whose existing API contract sorts keys
// before applying its limit.
func (d *Database) StreamQuery(ctx context.Context, q Query, emit func(Record) error) error {
	if emit == nil {
		return fmt.Errorf("%w: a stream callback is required", ErrInvalidQuery)
	}
	if d.nativePostgres {
		return ErrNativePostgresCollectionQueryUnsupported
	}
	parentKey, _, err := q.Target()
	if err != nil {
		return err
	}
	if err = q.validateFields(d.fieldRule()); err != nil {
		return err
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
			return fmt.Errorf("%w: unknown filter op %q", ErrInvalidQuery, f.Op)
		}
	}
	for _, ob := range q.OrderBy {
		if ob.Desc {
			builder = builder.OrderBy(dal.DescendingField(ob.Field))
		} else {
			builder = builder.OrderBy(dal.AscendingField(ob.Field))
		}
	}
	sortKeys := q.KeysOnly && len(q.OrderBy) == 0
	if q.Limit > 0 && !sortKeys {
		builder = builder.Limit(q.Limit)
	}
	var query dal.StructuredQuery
	if q.KeysOnly {
		query = builder.SelectKeysOnly(reflect.String)
	} else {
		query = builder.SelectIntoRecord(func() record.Record {
			return record.NewRecordWithIncompleteKey(q.Collection, reflect.String, map[string]any{})
		})
	}
	if err = d.guardSources(query); err != nil {
		return err
	}
	if !sortKeys {
		return d.executeDalQueryStreamOn(ctx, ctx, d.db, query, q.Collection, q.KeysOnly, func(rec Record) error {
			if parentKey != nil && rec.Key.Parent() == nil {
				rec.Key = record.NewKeyWithParentAndID(parentKey, rec.Key.Collection(), rec.Key.ID)
			}
			return emit(rec)
		})
	}
	var keys []Record
	err = d.executeDalQueryStreamOn(ctx, ctx, d.db, query, q.Collection, true, func(rec Record) error { keys = append(keys, rec); return nil })
	if err != nil {
		return err
	}
	sort.Slice(keys, func(i, j int) bool { return fmt.Sprintf("%v", keys[i].Key.ID) < fmt.Sprintf("%v", keys[j].Key.ID) })
	if q.Limit > 0 && len(keys) > q.Limit {
		keys = keys[:q.Limit]
	}
	for _, rec := range keys {
		if parentKey != nil && rec.Key.Parent() == nil {
			rec.Key = record.NewKeyWithParentAndID(parentKey, rec.Key.Collection(), rec.Key.ID)
		}
		if err := emit(rec); err != nil {
			return err
		}
	}
	return nil
}

// ErrInvalidDTQL identifies invalid or unsupported DTQL query shapes.
var ErrInvalidDTQL = errors.New("invalid or unsupported DTQL query")

// ParseDTQL validates the server's bounded single-collection query profile.
// The returned collection is suitable for checking the token's capabilities.
// It knows no engine, so it checks field names with the widest rule (the one of
// quotedNameEngines); the Database that runs the query checks them again with
// the rule of its own engine, before any adapter is reached.
func ParseDTQL(doc []byte) (dal.StructuredQuery, string, error) {
	query, err := DeserializeDTQL(doc)
	if err != nil {
		return nil, "", err
	}
	collection, err := validateDTQL(query)
	return query, collection, err
}

func validateDTQL(query dal.StructuredQuery) (string, error) {
	return validateDTQLFor(query, quotedNames)
}

// validateDTQLFor is validateDTQL with the field-name rule of an engine.
func validateDTQLFor(query dal.StructuredQuery, names fieldRule) (string, error) {
	if query == nil || query.From() == nil || len(query.From().Joins()) != 0 {
		return "", fmt.Errorf("%w: one root collection is required", ErrInvalidDTQL)
	}
	source, ok := query.From().Base().(dal.CollectionRef)
	if !ok || source.Parent() != nil || source.Alias() != "" || source.Name() == "" {
		return "", fmt.Errorf("%w: one unaliased root collection is required", ErrInvalidDTQL)
	}
	if source.Schema() != "" || source.Database() != "" || source.ScanLimit() != 0 || len(source.ScanOrders()) != 0 {
		return "", fmt.Errorf("%w: schema, database and scan are not supported on the root collection", ErrInvalidDTQL)
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
	if err := validateDTQLFieldsWith(query, 0, names); err != nil {
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

// The time budgets of a structured read of one collection through ExecuteDTQLQuery and of
// the complete capture of StreamDTQLSnapshot. They are variables only so that a test can
// make a budget end at once; nothing else changes them.
var (
	dtqlBudget     = 10 * time.Second
	snapshotBudget = 60 * time.Second
)

// snapshotDTQL removes the ordinary 1000-row default for a disk-backed
// result capture. Its caller enforces a byte and row bound while streaming.
type snapshotDTQL struct{ boundedDTQL }

func (q snapshotDTQL) Limit() int { return 0 }

// StreamDTQLSnapshot executes one complete query through one DALgo reader.
// Local OVDB writes cannot interleave with the capture. The caller must bound
// the emitted result and persist it before exposing any page token.
func (d *Database) StreamDTQLSnapshot(ctx context.Context, query dal.StructuredQuery, emit func(Record) error) error {
	collection, err := validateDTQLFor(query, d.fieldRule())
	if err != nil {
		return err
	}
	if err := d.checkRelationalNames(query); err != nil {
		return err
	}
	if query.Limit() != 0 || query.Offset() != 0 {
		return fmt.Errorf("%w: snapshot query requires limit and offset to be zero", ErrInvalidDTQL)
	}
	if err := d.guardProtectedSources(query); err != nil {
		return err
	}
	if err := d.guardSources(query); err != nil {
		return err
	}
	if err := d.guardQuery(); err != nil {
		return err
	}
	// A failure is decided on the request's own context (request), not on the budget the
	// capture runs under: a connection that does not answer ends on the budget, and is
	// still the failure of the database, not of the request.
	request := ctx
	ctx, cancel := context.WithTimeout(ctx, snapshotBudget)
	defer cancel()
	d.mu.Lock()
	defer d.mu.Unlock()
	reader, err := d.db.ExecuteQueryToRecordsReader(ctx, snapshotDTQL{boundedDTQL{query}})
	if err != nil {
		return d.queryError(request, fmt.Sprintf("failed to query collection %q", clipName(collection)), err)
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
			return d.queryError(request, fmt.Sprintf("failed reading query results for %q", clipName(collection)), nextErr)
		}
		out := Record{Key: rec.Key()}
		data, _ := rec.Data().(map[string]any)
		if out.Key == nil || out.Key.ID == nil || fmt.Sprintf("%v", out.Key.ID) == "" {
			if id, ok := data["id"].(string); ok && id != "" && !d.hasServingKey(collection) {
				out.Key = record.NewKeyWithID(collection, id)
			}
		}
		if out.Key == nil || out.Key.ID == nil || fmt.Sprint(out.Key.ID) == "" {
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
	var records []Record
	err := d.StreamDTQLQuery(ctx, query, func(rec Record) error {
		records = append(records, rec)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return records, nil
}

// executeDalQueryOn reads under ctx and answers a failure of the adapter as queryError does
// for request. The two are one context unless the server gave the read a budget of its own:
// a connection that does not answer ends on that budget, and is the failure of the database
// whenever the request itself is still alive.
func (d *Database) executeDalQueryOn(ctx, request context.Context, db dal.DB, query dal.StructuredQuery, collection string, keysOnly bool) ([]Record, error) {
	var records []Record
	err := d.executeDalQueryStreamOn(ctx, request, db, query, collection, keysOnly, func(rec Record) error {
		records = append(records, rec)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return records, nil
}

// StreamDTQLQuery executes an ordinary single-collection DTQL query and emits
// rows as DALgo reads them. It preserves the ordinary limit, offset, timeout,
// field coercion, and result byte bound; callers must treat callback or close
// errors as a failed result.
func (d *Database) StreamDTQLQuery(ctx context.Context, query dal.StructuredQuery, emit func(Record) error) error {
	if emit == nil {
		return fmt.Errorf("%w: a stream callback is required", ErrInvalidDTQL)
	}
	collection, err := validateDTQLFor(query, d.fieldRule())
	if err != nil {
		return err
	}
	if err = d.guardProtectedSources(query); err != nil {
		return err
	}
	if err = d.guardSources(query); err != nil {
		return err
	}
	request := ctx
	ctx, cancel := context.WithTimeout(ctx, dtqlBudget)
	defer cancel()
	return d.executeDalQueryStreamOn(ctx, request, d.db, boundedDTQL{query}, collection, false, emit)
}

func (d *Database) executeDalQueryStreamOn(ctx, request context.Context, db dal.DB, query dal.StructuredQuery, collection string, keysOnly bool, emit func(Record) error) (retErr error) {
	if err := d.guardQueryConditions(query); err != nil {
		return err
	}
	// Single choke point for every structured read: no engine that is not
	// cleared for queries (see queryEngines) is ever handed one.
	if err := d.guardQuery(); err != nil {
		return err
	}
	reader, err := db.ExecuteQueryToRecordsReader(ctx, query)
	if reader != nil {
		defer func() {
			if closeErr := reader.Close(); retErr == nil && closeErr != nil {
				retErr = d.queryError(request, fmt.Sprintf("failed closing query results for %q", clipName(collection)), closeErr)
			}
		}()
	}
	if err != nil {
		return d.queryError(request, fmt.Sprintf("failed to query collection %q", clipName(collection)), err)
	}
	if reader == nil {
		return fmt.Errorf("query returned no records reader")
	}
	var bytesRead int
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		rec, nextErr := reader.Next()
		// dal.ErrNoMoreRecords wraps io.EOF; some drivers (dalgo2sql) return
		// raw io.EOF — checking io.EOF covers both.
		if errors.Is(nextErr, io.EOF) {
			return nil
		}
		if nextErr != nil {
			return d.queryError(request, fmt.Sprintf("failed reading query results for %q", clipName(collection)), nextErr)
		}
		out := Record{Key: rec.Key()}
		data, _ := rec.Data().(map[string]any)
		// Relational drivers (dalgo2sql) return rows without dal keys — the
		// PK lands in the "id" column instead. Rebuild the key from it.
		// Plumbing recordset PK metadata into dalgo2sql's reader is a
		// roadmap upstream improvement.
		if collection != "" && (out.Key == nil || out.Key.ID == nil || fmt.Sprintf("%v", out.Key.ID) == "") {
			if id, ok := data["id"].(string); ok && id != "" && !d.hasServingKey(collection) {
				out.Key = record.NewKeyWithID(collection, id)
			}
		}
		if !keysOnly {
			out.Data = d.coerceToSchema(collection, data)
		}
		if out.Key == nil || out.Key.ID == nil || fmt.Sprint(out.Key.ID) == "" {
			return fmt.Errorf("query result has no record key")
		}
		encoded, err := json.Marshal(out.Data)
		if err != nil {
			return err
		}
		bytesRead += len(encoded) + len(out.Key.String())
		if bytesRead > ResultBufferBytes {
			return ErrResultTooLarge
		}
		if err := emit(out); err != nil {
			return err
		}
	}
}

func (d *Database) hasServingKey(collection string) bool {
	_, ok := d.ServingKey(collection)
	return ok
}
