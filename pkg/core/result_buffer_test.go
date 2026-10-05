package core

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/record"
)

// A structured read whose result is larger than the buffer of the server (8 MiB of the
// JSON of the rows and of their keys) is refused with ErrResultTooLarge, which the HTTP
// server answers 422 query_budget_exceeded with a hint to narrow or page the read: it is
// a request a client can change, not a fault of the server. A result of exactly the
// buffer is read.

// bufferRows is a reader of count rows, each holding one record of a name of pad bytes.
type bufferRows struct {
	pad   int
	count int
}

func (r *bufferRows) Next() (record.Record, error) {
	if r.count == 0 {
		return nil, io.EOF
	}
	r.count--
	key := record.NewKeyWithID("customers", "c1")
	return record.NewRecordWithData(key, map[string]any{"name": strings.Repeat("x", r.pad)}), nil
}
func (r *bufferRows) Cursor() (string, error) { return "", nil }
func (r *bufferRows) Close() error            { return nil }

// bufferRowOverhead is what a row counts beside the pad of its name in the buffer: the JSON of its data and its key.
const bufferRowOverhead = len(`{"name":""}`) + len("customers/c1")

func TestAResultOfExactlyTheBufferIsReadAndOneByteMoreIsRefused(t *testing.T) {
	if ResultBufferBytes != 8<<20 {
		t.Fatalf("ResultBufferBytes = %d, want 8 MiB", ResultBufferBytes)
	}
	ctx := context.Background()
	for _, engine := range []string{"sqlite", "postgres"} {
		setPreview(t, true, "1")
		parsed, _, err := ParseDTQL([]byte(guardDTQL))
		if err != nil {
			t.Fatal(err)
		}
		routes := map[string]func(*Database) error{
			"Execute":          func(d *Database) error { _, err := d.Execute(ctx, Query{Collection: "customers"}); return err },
			"ExecuteDTQLQuery": func(d *Database) error { _, err := d.ExecuteDTQLQuery(ctx, parsed); return err },
		}
		for name, run := range routes {
			t.Run(engine+"/"+name+"/at the bound", func(t *testing.T) {
				fake := &scriptedQueryDB{reader: &bufferRows{pad: ResultBufferBytes - bufferRowOverhead, count: 1}}
				if err := run(openScripted(t, engine, fake)); err != nil {
					t.Fatalf("a result of exactly the buffer: %v", err)
				}
			})
			t.Run(engine+"/"+name+"/one byte over", func(t *testing.T) {
				fake := &scriptedQueryDB{reader: &bufferRows{pad: ResultBufferBytes - bufferRowOverhead + 1, count: 1}}
				err := run(openScripted(t, engine, fake))
				if !errors.Is(err, ErrResultTooLarge) {
					t.Fatalf("a result one byte over the buffer: %v, want ErrResultTooLarge", err)
				}
			})
			t.Run(engine+"/"+name+"/many rows", func(t *testing.T) {
				// Rows of half the buffer each: two make exactly the buffer, and the third pushes it over.
				fake := &scriptedQueryDB{reader: &bufferRows{pad: ResultBufferBytes/2 - bufferRowOverhead, count: 3}}
				if err := run(openScripted(t, engine, fake)); !errors.Is(err, ErrResultTooLarge) {
					t.Fatalf("three rows of half the buffer: %v, want ErrResultTooLarge", err)
				}
			})
		}
	}
}

// The refusal is not an internal error, a query the adapter cannot run, or any other
// error the server maps: it is its own, and it says the buffer and not a number the
// result reached.
func TestTheRefusalOfAResultOverTheBufferIsItsOwnError(t *testing.T) {
	err := ErrResultTooLarge
	for _, other := range []error{ErrInvalidQuery, ErrInvalidDTQL, ErrQueryNotRunnable, ErrQueryDoesNotFit, ErrNotFound, ErrDatabaseUnreachable, dal.ErrNotSupported} {
		if errors.Is(err, other) {
			t.Errorf("ErrResultTooLarge matches %v", other)
		}
	}
	if strings.ContainsAny(err.Error(), "0123456789") {
		t.Errorf("the message states a figure: %q", err)
	}
}
