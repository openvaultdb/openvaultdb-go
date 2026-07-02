// Command write-fixture scaffolds the contacts collection (see ../../scaffold.go)
// at a given directory and writes one contactus-shaped record into it via
// dalgo2ingitdb, WITHOUT running git at all. It exists to produce a fixture
// for ../../scripts/ts-read-check.mjs, which proves @ingitdb/client-github's
// real (unmodified) collection-loading code can read the exact on-disk
// layout dalgo2ingitdb writes — the format-compatibility half of the
// write(Go)/read(browser) thesis this PoC exists to test. See the PoC's PR
// description for why this couldn't also be proven end-to-end against a real
// github.com repo in this session.
//
// Usage:
//
//	go run ./cmd/write-fixture <output-dir>
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/dal-go/dalgo/dal"
	"github.com/ingitdb/dalgo2ingitdb"
	"github.com/ingitdb/ingitdb-go/ingitdb/validator"

	poc "github.com/openvaultdb/openvaultdb-go/dalgo2ingitdb-github-poc"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: write-fixture <output-dir>")
		os.Exit(2)
	}
	root := os.Args[1]
	if err := os.MkdirAll(root, 0o755); err != nil {
		fatal(err)
	}
	if err := poc.ScaffoldContactsCollection(root); err != nil {
		fatal(err)
	}
	db, err := dalgo2ingitdb.NewDatabase(root, validator.NewCollectionsReader())
	if err != nil {
		fatal(err)
	}
	ctx := context.Background()
	key := dal.NewKeyWithID(poc.ContactsCollectionID, "contact-jane-doe")
	person := map[string]any{
		"firstName": "Jane",
		"lastName":  "Doe",
		"email":     "jane@example.com",
		"roles":     []any{"owner", "driver"},
	}
	err = db.RunReadwriteTransaction(ctx, func(ctx context.Context, tx dal.ReadwriteTransaction) error {
		return tx.Set(ctx, dal.NewRecordWithData(key, person))
	})
	if err != nil {
		fatal(err)
	}
	fmt.Println("fixture written to", root)
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "write-fixture:", err)
	os.Exit(1)
}
