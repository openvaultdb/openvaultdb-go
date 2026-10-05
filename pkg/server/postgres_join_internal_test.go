package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/record"

	"github.com/openvaultdb/openvaultdb-go/pkg/joinexec"
)

// pgJoinBackend is the adapter of a PostgreSQL mount for a join, behind dal.NewDB as every
// adapter is: it runs a join whose sources name no database as one statement, and, like the
// compiler of the PostgreSQL adapter, declines one whose sources do (DALgo then reads each
// table whole and joins the rows in the transaction). decline makes it decline every join,
// as it does for what it cannot write as one statement. It keeps what it was asked to read.
type pgJoinBackend struct {
	dal.Backend
	decline error
	read    func(dal.StructuredQuery) (dal.RecordsReader, error)
	fields  func(dal.RecordsetSource) ([]string, error)

	mu     sync.Mutex
	handed []dal.StructuredQuery
}

func (b *pgJoinBackend) ID() string { return "pg" }

func (b *pgJoinBackend) CanExecuteJoin(_ context.Context, q dal.StructuredQuery) error {
	if pgJoinNamedDatabase(q.From()) != "" {
		return fmt.Errorf("join_plan: PostgreSQL cannot natively compile query: %w", dal.ErrNotSupported)
	}
	return b.decline
}

func (b *pgJoinBackend) answer(query dal.Query) (dal.RecordsReader, error) {
	structured := query.(dal.StructuredQuery)
	b.mu.Lock()
	b.handed = append(b.handed, structured)
	b.mu.Unlock()
	if b.read == nil {
		return &pgJoinReader{}, nil
	}
	return b.read(structured)
}

func (b *pgJoinBackend) reads() []dal.StructuredQuery {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]dal.StructuredQuery(nil), b.handed...)
}

func (b *pgJoinBackend) ExecuteQueryToRecordsReader(_ context.Context, query dal.Query) (dal.RecordsReader, error) {
	return b.answer(query)
}

func (b *pgJoinBackend) JoinFields(_ context.Context, source dal.RecordsetSource) ([]string, error) {
	if b.fields == nil {
		return nil, nil
	}
	return b.fields(source)
}

func (b *pgJoinBackend) RunReadonlyTransaction(ctx context.Context, worker dal.ROTxWorker, _ ...dal.TransactionOption) error {
	return worker(ctx, &pgJoinTx{backend: b})
}

type pgJoinTx struct {
	dal.ReadTransaction
	backend *pgJoinBackend
}

func (t *pgJoinTx) ExecuteQueryToRecordsReader(_ context.Context, query dal.Query) (dal.RecordsReader, error) {
	return t.backend.answer(query)
}

func (t *pgJoinTx) JoinFields(ctx context.Context, source dal.RecordsetSource) ([]string, error) {
	return t.backend.JoinFields(ctx, source)
}

// pgJoinReader reads the rows it holds, then fails with err or ends as a read ends.
type pgJoinReader struct {
	rows []map[string]any
	next int
	err  error
}

func (r *pgJoinReader) Next() (record.Record, error) {
	if r.next < len(r.rows) {
		row := r.rows[r.next]
		r.next++
		return record.NewRecordWithData(record.NewKeyWithID("t", fmt.Sprint(row["id"])), row), nil
	}
	if r.err != nil {
		return nil, r.err
	}
	return nil, dal.ErrNoMoreRecords
}
func (r *pgJoinReader) Cursor() (string, error) { return "", nil }
func (r *pgJoinReader) Close() error            { return nil }

// pgJoinNamedDatabase returns the first database a source of the relation tree names.
func pgJoinNamedDatabase(from dal.FromSource) string {
	if ref, ok := from.Base().(dal.CollectionRef); ok && ref.Database() != "" {
		return ref.Database()
	}
	for _, join := range from.Joins() {
		if tree := join.From(); tree != nil {
			if name := pgJoinNamedDatabase(tree); name != "" {
				return name
			}
		} else if ref, ok := join.RecordsetSource.(dal.CollectionRef); ok && ref.Database() != "" {
			return ref.Database()
		}
	}
	return ""
}

func pgJoinRows(count int) *pgJoinReader {
	rows := make([]map[string]any, count)
	for i := range rows {
		rows[i] = map[string]any{"id": fmt.Sprint(i), "customer_id": "0", "name": "n"}
	}
	return &pgJoinReader{rows: rows}
}

// pgJoinDocument is a join of two collections of the PostgreSQL mount that selects a column
// of each, in the form /v1/dtql takes it: every source names its database.
const pgJoinDocument = previewPGSameDatabaseJoin + `columns:
  - {field: id, source: o}
  - {field: name, source: c}
`

// TestAJoinOfOnePostgresDatabaseIsRunByTheMountAsOneStatement: a join of two collections of a
// PostgreSQL mount, as /v1/dtql takes it with the database named on each source, reaches the
// adapter once, as the whole join and without the database names that would make the
// compiler decline it, and the answer says it ran in the database. Read table by table it
// would reach the adapter once for each, whole, and be bound by DALgo's 10,000 rows.
func TestAJoinOfOnePostgresDatabaseIsRunByTheMountAsOneStatement(t *testing.T) {
	backend := &pgJoinBackend{read: func(dal.StructuredQuery) (dal.RecordsReader, error) {
		return &pgJoinReader{rows: []map[string]any{{"id": "o1", "name": "Ada"}}}, nil
	}}
	host, logs := previewPGServerWith(t, dal.NewDB(backend))
	resp := relFakeDo(t, host, http.MethodPost, "/v1/dtql", "", pgJoinDocument, nil)
	if resp.status != http.StatusOK {
		t.Fatalf("status %d: %s", resp.status, resp.raw)
	}
	if route, _ := resp.body["execution"].(map[string]any)["route"].(string); route != joinexec.RouteDatabase {
		t.Errorf("route = %q, want %q", route, joinexec.RouteDatabase)
	}
	handed := backend.reads()
	if len(handed) != 1 {
		t.Fatalf("the adapter was reached %d times, want once with the whole join", len(handed))
	}
	if name := pgJoinNamedDatabase(handed[0].From()); name != "" {
		t.Errorf("the adapter was handed a source that names the database %q", name)
	}
	if len(handed[0].From().Joins()) != 1 {
		t.Errorf("the adapter was handed %d joins, want the one of the document", len(handed[0].From().Joins()))
	}
	if logs.Len() != 0 {
		t.Errorf("a request that was answered was logged: %s", logs)
	}
}

// TestWhatDalgoRefusesInAJoinThePostgresAdapterDeclinesIsARefusalAndNotAFailure: a join that
// the adapter declines for what it cannot write as one statement is read table by table and
// joined by DALgo, in the transaction, and its refusals are answered as the relational route
// answers them anywhere else: the bound of its scan is 422 query_budget_exceeded, a field
// the tables do not give and an alias used twice are 400 invalid_dtql, and a scan that
// fails is the 500 of any failure of the server, built, with none of the driver's text in the
// answer or in the log.
func TestWhatDalgoRefusesInAJoinThePostgresAdapterDeclinesIsARefusalAndNotAFailure(t *testing.T) {
	decline := fmt.Errorf("join_plan: PostgreSQL cannot natively compile query: %w", dal.ErrNotSupported)
	t.Run("the bound of the scan", func(t *testing.T) {
		backend := &pgJoinBackend{decline: decline, read: func(dal.StructuredQuery) (dal.RecordsReader, error) { return pgJoinRows(10001), nil }}
		host, logs := previewPGServerWith(t, dal.NewDB(backend))
		resp := relFakeDo(t, host, http.MethodPost, "/v1/dtql", "", pgJoinDocument, nil)
		if resp.status != http.StatusUnprocessableEntity || resp.code() != "query_budget_exceeded" {
			t.Fatalf("status %d: %s", resp.status, resp.raw)
		}
		budget, _ := resp.errorDetail()["budget"].(map[string]any)
		if budget["name"] != joinexec.BudgetJoinScan || budget["route"] != joinexec.RouteDatabase {
			t.Errorf("budget = %v, want the scan bound of the database route", budget)
		}
		if logs.Len() != 0 {
			t.Errorf("a refusal the caller can act on was logged: %s", logs)
		}
	})
	t.Run("a field the tables do not give", func(t *testing.T) {
		backend := &pgJoinBackend{decline: decline, fields: func(source dal.RecordsetSource) ([]string, error) {
			return []string{"id", "customer_id"}, nil // the catalog holds no column called name
		}}
		host, logs := previewPGServerWith(t, dal.NewDB(backend))
		resp := relFakeDo(t, host, http.MethodPost, "/v1/dtql", "", pgJoinDocument, nil)
		if resp.status != http.StatusBadRequest || resp.code() != "invalid_dtql" {
			t.Fatalf("status %d: %s", resp.status, resp.raw)
		}
		if message := resp.errorDetail()["message"].(string); !strings.Contains(message, `field "name" is unavailable in "c"`) {
			t.Errorf("message = %q, want the field DALgo could not find", message)
		}
		if logs.Len() != 0 {
			t.Errorf("a refusal the caller can act on was logged: %s", logs)
		}
	})
	t.Run("an alias used twice", func(t *testing.T) {
		host, logs := previewPGServerWith(t, dal.NewDB(&pgJoinBackend{}))
		body := strings.Replace(previewPGSameDatabaseJoin, "alias: c", "alias: o", 1) + "columns:\n  - {field: id, source: o}\n"
		resp := relFakeDo(t, host, http.MethodPost, "/v1/dtql", "", body, nil)
		if resp.status != http.StatusBadRequest || resp.code() != "invalid_dtql" {
			t.Fatalf("status %d: %s", resp.status, resp.raw)
		}
		if logs.Len() != 0 {
			t.Errorf("a refusal the caller can act on was logged: %s", logs)
		}
	})
	t.Run("a scan that fails", func(t *testing.T) {
		backend := &pgJoinBackend{decline: decline, read: func(dal.StructuredQuery) (dal.RecordsReader, error) {
			return &pgJoinReader{err: errors.New("connection to server " + previewPGMarker + " lost")}, nil
		}}
		host, logs := previewPGServerWith(t, dal.NewDB(backend))
		resp := relFakeDo(t, host, http.MethodPost, "/v1/dtql", "", pgJoinDocument, nil)
		if resp.status != http.StatusInternalServerError || resp.code() != "internal" {
			t.Fatalf("status %d: %s", resp.status, resp.raw)
		}
		if strings.Contains(resp.raw, previewPGMarker) || strings.Contains(logs.String(), previewPGMarker) {
			t.Errorf("text of the driver is in the answer %s or in the log %s", resp.raw, logs)
		}
		if !strings.Contains(logs.String(), "the database server could not run the query") {
			t.Errorf("the log does not say what failed: %s", logs)
		}
	})
}
