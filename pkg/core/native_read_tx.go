package core

import (
	"context"
	"fmt"
	"sync"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo/recordset"
)

// nativeReadTxDB is the driver of a native PostgreSQL mount as the database holds
// it: every query that is not already run inside a transaction (ReadTx) runs in an
// explicit read-only transaction of its own (BEGIN READ ONLY), which stays open
// while the reader is read and is committed when the reader is closed. The session
// default (default_transaction_read_only, set by the mount) covers an implicit
// transaction too, but a function a view calls can change a session default; it
// cannot make a transaction that began READ ONLY writable.
type nativeReadTxDB struct{ dal.DB }

func (d nativeReadTxDB) ExecuteQueryToRecordsReader(ctx context.Context, query dal.Query) (dal.RecordsReader, error) {
	reader, hold, err := openInReadTx(ctx, d.DB, func(ctx context.Context, tx dal.ReadTransaction) (dal.RecordsReader, error) {
		return tx.ExecuteQueryToRecordsReader(ctx, query)
	})
	if err != nil {
		return nil, err
	}
	return readTxRecordsReader{RecordsReader: reader, hold: hold}, nil
}

func (d nativeReadTxDB) ExecuteQueryToRecordsetReader(ctx context.Context, query dal.Query, options ...recordset.Option) (dal.RecordsetReader, error) {
	reader, hold, err := openInReadTx(ctx, d.DB, func(ctx context.Context, tx dal.ReadTransaction) (dal.RecordsetReader, error) {
		return tx.ExecuteQueryToRecordsetReader(ctx, query, options...)
	})
	if err != nil {
		return nil, err
	}
	return readTxRecordsetReader{RecordsetReader: reader, hold: hold}, nil
}

// readTxHold keeps the transaction of one reader open until finish is called.
type readTxHold struct {
	release chan struct{}
	done    chan error
	once    sync.Once
	err     error
}

// finish lets the transaction end and returns its error (a failed commit).
func (h *readTxHold) finish() error {
	h.once.Do(func() {
		close(h.release)
		h.err = <-h.done
	})
	return h.err
}

// openInReadTx opens a reader inside a read-only transaction that a goroutine
// keeps open until the hold is finished or ctx ends. A reader that could not be
// opened ends the transaction before the error is returned.
func openInReadTx[R any](ctx context.Context, db dal.DB, open func(context.Context, dal.ReadTransaction) (R, error)) (R, *readTxHold, error) {
	type opened struct {
		reader R
		err    error
	}
	ready := make(chan opened, 1)
	hold := &readTxHold{release: make(chan struct{}), done: make(chan error, 1)}
	go func() {
		handed := false
		var err error
		defer func() {
			if recovered := recover(); recovered != nil {
				err = fmt.Errorf("the read transaction failed: %v", recovered)
			}
			if !handed {
				ready <- opened{err: err}
			}
			hold.done <- err
		}()
		err = db.RunReadonlyTransaction(ctx, func(ctx context.Context, tx dal.ReadTransaction) error {
			reader, openErr := open(ctx, tx)
			handed = true
			ready <- opened{reader: reader, err: openErr}
			if openErr != nil {
				return openErr
			}
			select {
			case <-hold.release:
			case <-ctx.Done():
			}
			return nil
		})
	}()
	got := <-ready
	if got.err != nil {
		<-hold.done
		var zero R
		return zero, nil, got.err
	}
	return got.reader, hold, nil
}

// readTxRecordsReader closes the reader and then ends its transaction.
type readTxRecordsReader struct {
	dal.RecordsReader
	hold *readTxHold
}

func (r readTxRecordsReader) Close() error {
	closeErr := r.RecordsReader.Close()
	txErr := r.hold.finish()
	if closeErr != nil {
		return closeErr
	}
	return txErr
}

// readTxRecordsetReader closes the reader and then ends its transaction.
type readTxRecordsetReader struct {
	dal.RecordsetReader
	hold *readTxHold
}

func (r readTxRecordsetReader) Close() error {
	closeErr := r.RecordsetReader.Close()
	txErr := r.hold.finish()
	if closeErr != nil {
		return closeErr
	}
	return txErr
}

// driver is the driver the database was opened with, beneath the read-transaction
// wrapper of a native PostgreSQL mount. Every lookup of an optional driver
// interface (dal.As: schema reading, DDL, foreign keys) goes through it, because
// the wrapper adds only the two query methods and hides everything else the
// driver implements.
func (d *Database) driver() dal.DB {
	if wrapped, ok := d.db.(nativeReadTxDB); ok {
		return wrapped.DB
	}
	return d.db
}
