package dalgo2ovdb

import (
	"context"
	"fmt"

	"github.com/dal-go/dalgo/dal"
)

// executeQuery implements dal.QueryExecutor.ExecuteQueryToRecordsReader by
// mapping a dal.StructuredQuery straight onto OVDB's list endpoint — a clean
// fit, since "list every record in a collection" is exactly what that route
// does.
//
// GAP: OVDB's REST API has no filter/sort query params (and DTQL, the
// query-language name in the docs, has no implementation to push down to
// either — see the investment plan). So Where/GroupBy/Having/OrderBy are
// accepted syntactically (StructuredQuery exposes them) but NOT applied —
// every record in the collection is fetched and returned unfiltered/unsorted.
// Only Offset/Limit are applied, client-side, after the fact. Only
// SelectIntoRecord-style queries are supported (SelectColumns/SelectKeysOnly
// are not); only dal.StructuredQuery is supported (dal.TextQuery is not,
// mirroring dalgo2firestore).
func executeQuery(ctx context.Context, rest *restClient, query dal.Query) (dal.RecordsReader, error) {
	sq, ok := query.(dal.StructuredQuery)
	if !ok {
		return nil, fmt.Errorf("dalgo2ovdb: only dal.StructuredQuery is supported (got %T)", query)
	}
	from := sq.From()
	if from == nil || from.Base() == nil {
		return nil, fmt.Errorf("dalgo2ovdb: query has no From() collection")
	}
	collection := from.Base().Name()

	rows, err := rest.list(ctx, collection)
	if err != nil {
		return nil, err
	}

	records := make([]dal.Record, 0, len(rows))
	for _, row := range rows {
		id, _ := idAsString(row["id"])

		rec := sq.IntoRecord()
		if rec == nil {
			rec = dal.NewRecordWithData(dal.NewKeyWithID(collection, id), map[string]any{})
		} else {
			rec.Key().ID = id
		}
		if err := unmarshalInto(row, rec.Data()); err != nil {
			return nil, err
		}
		records = append(records, rec)
	}

	records = applyOffsetLimit(records, sq.Offset(), sq.Limit())
	return dal.NewRecordsReader(records), nil
}

// applyOffsetLimit is the client-side stand-in for server-side paging OVDB's
// list endpoint doesn't support.
func applyOffsetLimit(records []dal.Record, offset, limit int) []dal.Record {
	if offset > 0 {
		if offset >= len(records) {
			return nil
		}
		records = records[offset:]
	}
	if limit > 0 && limit < len(records) {
		records = records[:limit]
	}
	return records
}
