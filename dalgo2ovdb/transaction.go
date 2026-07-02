package dalgo2ovdb

import (
	"context"
	"fmt"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo/update"
)

// transaction implements dal.ReadwriteTransaction. It embeds *DB to inherit
// Get/Exists/GetMulti/ExecuteQueryToRecordsReader/ExecuteQueryToRecordsetReader
// verbatim (reads are identical whether or not they're "inside" a
// transaction, since OVDB has none), and adds the write methods below. See
// DB.RunReadwriteTransaction for why "transaction" is a documentation fiction
// here: there is no atomicity, no isolation, no rollback.
type transaction struct {
	*DB
	options dal.TransactionOptions
}

var _ dal.ReadwriteTransaction = (*transaction)(nil)
var _ dal.ReadTransaction = (*transaction)(nil)

// ID implements dal.ReadwriteTransaction. OVDB mints no transaction id (there
// is no transaction), so this is always empty.
func (tx *transaction) ID() string { return "" }

func (tx *transaction) Options() dal.TransactionOptions { return tx.options }

// Insert implements dal.Inserter: POST create. If the DALgo key has no ID,
// OVDB's native server-side id generation (store.CreateRecord's
// uuid.NewString() fallback) is used, and the minted id is written back onto
// record.Key() — the WithAdapterGeneratedID contract's "adapter has a native
// mechanism" case.
func (tx *transaction) Insert(ctx context.Context, record dal.Record, opts ...dal.InsertOption) error {
	options := dal.NewInsertOptions(opts...)
	if gen := options.IDGenerator(); gen != nil {
		if err := gen(ctx, record); err != nil {
			return fmt.Errorf("dalgo2ovdb: generate record id: %w", err)
		}
	}
	key := record.Key()
	// dal.Record.Data() panics unless SetError has been called at least once
	// (see dal/record.go) -- a record freshly built via NewRecordWithData for
	// a write has never had SetError called. Mark it "no error" before reading
	// Data(), mirroring the convention dal.NewRecordWithIncompleteKey uses
	// internally.
	record.SetError(nil)
	body, err := recordToBody(record.Data())
	if err != nil {
		return err
	}
	if id, ok := idAsString(key.ID); ok {
		body["id"] = id
	}
	created, err := tx.rest.create(ctx, key.Collection(), body)
	if err != nil {
		return err
	}
	if _, ok := idAsString(key.ID); !ok {
		if newID, ok := idAsString(created["id"]); ok {
			key.ID = newID
		}
	}
	return nil
}

// InsertMulti implements dal.MultiInserter. GAP: OVDB has no batch/bulk
// endpoint, so this is N sequential single inserts with no atomicity — a
// failure partway through leaves the earlier inserts committed.
func (tx *transaction) InsertMulti(ctx context.Context, records []dal.Record, opts ...dal.InsertOption) error {
	for _, r := range records {
		if err := tx.Insert(ctx, r, opts...); err != nil {
			return err
		}
	}
	return nil
}

// Set implements dal.Setter.
//
// GAP: OVDB has no upsert/full-replace route. Set emulates DALgo's
// "create-or-replace" contract with an existence probe (Exists) followed by
// POST create (if absent) or PATCH (if present). The PATCH branch is a field
// MERGE, not a replace: a field present in the stored record but absent from
// the new record's Data() survives instead of being cleared, which is a real
// semantic gap versus DALgo's Set().
func (tx *transaction) Set(ctx context.Context, record dal.Record) error {
	key := record.Key()
	id, ok := idAsString(key.ID)
	if !ok {
		return fmt.Errorf("dalgo2ovdb: Set requires a non-empty record key ID (OVDB has no upsert route to generate one)")
	}
	record.SetError(nil) // see Insert's comment on dal.Record.Data()'s panic contract
	body, err := recordToBody(record.Data())
	if err != nil {
		return err
	}
	exists, err := tx.Exists(ctx, key)
	if err != nil {
		return err
	}
	if exists {
		_, err = tx.rest.patch(ctx, key.Collection(), id, body)
		return err
	}
	body["id"] = id
	_, err = tx.rest.create(ctx, key.Collection(), body)
	return err
}

// SetMulti implements dal.MultiSetter. Sequential, no atomicity (see Set).
func (tx *transaction) SetMulti(ctx context.Context, records []dal.Record) error {
	for _, r := range records {
		if err := tx.Set(ctx, r); err != nil {
			return err
		}
	}
	return nil
}

// Delete implements dal.Deleter: DELETE by id. A clean 1:1 fit onto OVDB's
// REST API, unlike Get/Set above.
func (tx *transaction) Delete(ctx context.Context, key *dal.Key) error {
	id, ok := idAsString(key.ID)
	if !ok {
		return fmt.Errorf("dalgo2ovdb: Delete requires a non-empty key ID")
	}
	return tx.rest.delete(ctx, key.Collection(), id)
}

// DeleteMulti implements dal.MultiDeleter. Sequential, no atomicity.
func (tx *transaction) DeleteMulti(ctx context.Context, keys []*dal.Key) error {
	for _, k := range keys {
		if err := tx.Delete(ctx, k); err != nil {
			return err
		}
	}
	return nil
}

// Update, UpdateRecord, and UpdateMulti implement dal.Updater/dal.MultiUpdater.
//
// NOT IMPLEMENTED in this PoC. Translating DALgo's field-level update.Update
// operations (Set/Increment/append-to-array/...) onto OVDB's raw-JSON
// PATCH-merge endpoint is a real, nontrivial gap — PATCH accepts a flat JSON
// object merge, not a vocabulary of field operations — and was out of scope
// for proving the Get/Set/Delete/Insert/Query round trip. A module that calls
// Update() against dalgo2ovdb today gets a clear error, not silent data loss.
func (tx *transaction) Update(_ context.Context, _ *dal.Key, _ []update.Update, _ ...dal.Precondition) error {
	return fmt.Errorf("dalgo2ovdb: Update() is not implemented in this PoC (see transaction.go doc comment)")
}

func (tx *transaction) UpdateRecord(ctx context.Context, record dal.Record, updates []update.Update, preconditions ...dal.Precondition) error {
	return tx.Update(ctx, record.Key(), updates, preconditions...)
}

func (tx *transaction) UpdateMulti(_ context.Context, _ []*dal.Key, _ []update.Update, _ ...dal.Precondition) error {
	return fmt.Errorf("dalgo2ovdb: UpdateMulti() is not implemented in this PoC (see transaction.go doc comment)")
}
