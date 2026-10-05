package core

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/record"
)

type missingServingKeyRecord struct{ record.Record }

func (missingServingKeyRecord) Key() *record.Key { return nil }

type missingServingKeyReader struct {
	done   bool
	closed *int
}

func (r *missingServingKeyReader) Next() (record.Record, error) {
	if r.done {
		return nil, io.EOF
	}
	r.done = true
	return missingServingKeyRecord{record.NewRecordWithData(record.NewKeyWithID("customers", "native"), map[string]any{"id": "native", "name": "Ada"})}, nil
}
func (*missingServingKeyReader) Cursor() (string, error) { return "", nil }
func (r *missingServingKeyReader) Close() error          { *r.closed++; return nil }

type verifiedServingKeyDB struct {
	scriptedQueryDB
	closed int
}

func (*verifiedServingKeyDB) SQLiteRecordKeys() (map[string]string, time.Duration) {
	return map[string]string{"customers": "helper"}, 0
}
func (d *verifiedServingKeyDB) ExecuteQueryToRecordsReader(context.Context, dal.Query) (dal.RecordsReader, error) {
	return &missingServingKeyReader{closed: &d.closed}, nil
}
func TestConfiguredServingKeyNeverFallsBackToNativeID(t *testing.T) {
	driver := &verifiedServingKeyDB{}
	db := openScripted(t, "sqlite", driver)
	parsed, _, err := ParseDTQL([]byte(guardDTQL))
	if err != nil {
		t.Fatal(err)
	}
	_, ordinary := db.ExecuteDTQLQuery(context.Background(), parsed)
	_, structured := db.Execute(context.Background(), Query{Collection: "customers"})
	_, keys := db.Execute(context.Background(), Query{Collection: "customers", KeysOnly: true})
	snapshot := db.StreamDTQLSnapshot(context.Background(), parsed, func(Record) error { t.Fatal("emitted native fallback key"); return nil })
	for name, err := range map[string]error{"ordinary": ordinary, "structured": structured, "keys": keys, "snapshot": snapshot} {
		if err == nil || !strings.Contains(err.Error(), "no record key") {
			t.Fatalf("%s substituted native ID: %v", name, err)
		}
	}
	if driver.closed != 4 {
		t.Fatalf("failed readers not closed: %d", driver.closed)
	}
}
