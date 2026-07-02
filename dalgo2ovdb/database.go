package dalgo2ovdb

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo/recordset"
)

// DB is a dalgo2ovdb dal.DB backed by one scoped connect-flow Conn (one vault
// + one namespace on one ovdb-server). Construct it via Connect/ExchangeCode,
// or NewDB directly if you already hold a Conn (e.g. a persisted/refreshed
// token — see Track A's token-persistence gap noted in connect.go).
type DB struct {
	// ConcurrencyAvailable: ovdb-server is an ordinary net/http server; it
	// tolerates multiple concurrent client connections fine (the
	// per-vault/namespace serialization, if any, is inGitDB's problem, on the
	// server side of this HTTP boundary, not this client's).
	dal.ConcurrencyAvailable
	rest *restClient
}

var _ dal.DB = (*DB)(nil)

// NewDB wraps an already-established Conn (e.g. from Connect, or a token
// loaded from storage) as a dal.DB. httpClient may be nil to get a default
// client with a 15s timeout.
func NewDB(httpClient *http.Client, conn Conn) *DB {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 15 * time.Second}
	}
	return &DB{rest: &restClient{http: httpClient, conn: conn}}
}

func (db *DB) ID() string { return db.rest.conn.VaultID + "/" + db.rest.conn.NamespaceID }

func (db *DB) Adapter() dal.Adapter { return dal.NewAdapter("dalgo2ovdb", "0.0.1-poc") }

func (db *DB) Schema() dal.Schema { return nil }

// Get implements dal.Getter. OVDB's REST API has no single-record GET route
// (only the collection-wide list, see restClient.list), so Get fetches the
// whole collection and filters client-side by id — correct, but O(collection
// size) per call. Fine for a PoC; a real integration would want OVDB to grow a
// GET .../records/{id} route.
func (db *DB) Get(ctx context.Context, record dal.Record) error {
	key := record.Key()
	id, ok := idAsString(key.ID)
	if !ok {
		err := fmt.Errorf("dalgo2ovdb: Get requires a non-empty record key ID")
		record.SetError(err)
		return err
	}
	recs, err := db.rest.list(ctx, key.Collection())
	if err != nil {
		record.SetError(err)
		return err
	}
	for _, r := range recs {
		if rowID, _ := idAsString(r["id"]); rowID == id {
			record.SetError(nil)
			if err := unmarshalInto(r, record.Data()); err != nil {
				record.SetError(err)
				return err
			}
			return nil
		}
	}
	err = notFoundErr(key.Collection(), id)
	record.SetError(err)
	return err
}

// Exists implements dal.Getter.
func (db *DB) Exists(ctx context.Context, key *dal.Key) (bool, error) {
	id, ok := idAsString(key.ID)
	if !ok {
		return false, fmt.Errorf("dalgo2ovdb: Exists requires a non-empty key ID")
	}
	recs, err := db.rest.list(ctx, key.Collection())
	if err != nil {
		return false, err
	}
	for _, r := range recs {
		if rowID, _ := idAsString(r["id"]); rowID == id {
			return true, nil
		}
	}
	return false, nil
}

// GetMulti implements dal.MultiGetter. It lists each distinct collection at
// most once (not once per key) and matches every record against it, so N
// records over 1 collection costs exactly 1 HTTP call.
func (db *DB) GetMulti(ctx context.Context, records []dal.Record) error {
	listed := map[string][]map[string]any{}
	for _, record := range records {
		key := record.Key()
		col := key.Collection()
		rows, ok := listed[col]
		if !ok {
			var err error
			rows, err = db.rest.list(ctx, col)
			if err != nil {
				return err
			}
			listed[col] = rows
		}
		id, ok := idAsString(key.ID)
		if !ok {
			record.SetError(fmt.Errorf("dalgo2ovdb: GetMulti requires a non-empty record key ID"))
			continue
		}
		found := false
		for _, r := range rows {
			if rowID, _ := idAsString(r["id"]); rowID == id {
				record.SetError(nil)
				if err := unmarshalInto(r, record.Data()); err != nil {
					record.SetError(err)
				}
				found = true
				break
			}
		}
		if !found {
			record.SetError(notFoundErr(col, id))
		}
	}
	return nil
}

// ExecuteQueryToRecordsReader implements dal.QueryExecutor.
func (db *DB) ExecuteQueryToRecordsReader(ctx context.Context, query dal.Query) (dal.RecordsReader, error) {
	return executeQuery(ctx, db.rest, query)
}

// ExecuteQueryToRecordsetReader implements dal.QueryExecutor. Not supported by
// this PoC (dalgo2firestore doesn't support it either — see its
// query_executor.go).
func (db *DB) ExecuteQueryToRecordsetReader(_ context.Context, _ dal.Query, _ ...recordset.Option) (dal.RecordsetReader, error) {
	return nil, fmt.Errorf("%w: recordset reader is not implemented by dalgo2ovdb", dal.ErrNotSupported)
}

// RunReadonlyTransaction implements dal.ReadTransactionCoordinator. OVDB has
// no transaction API at all, so this is just the read methods above wrapped
// to satisfy the dal.ReadTransaction shape — there is no isolation guarantee
// beyond what a sequence of independent HTTP GETs gives you.
func (db *DB) RunReadonlyTransaction(ctx context.Context, f dal.ROTxWorker, opts ...dal.TransactionOption) error {
	tx := &transaction{DB: db, options: dal.NewTransactionOptions(append(opts, dal.TxWithReadonly())...)}
	return f(ctx, tx)
}

// RunReadwriteTransaction implements dal.ReadwriteTransactionCoordinator.
//
// GAP (flagged in the investment plan before this was written, confirmed
// here): OVDB has no server-side multi-record transaction. Every
// Setter/Deleter/Inserter/Updater call made through tx inside f executes
// immediately over HTTP, one record at a time, with NO atomicity and NO
// rollback — if f fails after 2 of 3 writes, the first 2 stick. Any
// consistency across multiple records is best-effort/last-write-wins, exactly
// as the plan anticipated. This is the sharpest divergence from Firestore
// found in this PoC; a module that genuinely needs atomic multi-record writes
// cannot get them from dalgo2ovdb today.
func (db *DB) RunReadwriteTransaction(ctx context.Context, f dal.RWTxWorker, opts ...dal.TransactionOption) error {
	tx := &transaction{DB: db, options: dal.NewTransactionOptions(opts...)}
	return f(ctx, tx)
}
