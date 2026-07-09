package mount

import (
	"context"
	"fmt"

	"cloud.google.com/go/firestore"
	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo2firestore"

	"github.com/openvaultdb/openvaultdb-go/pkg/manifest"
	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
)

// openFirestore opens a Firestore database through the dal-go
// dalgo2firestore driver. Credentials come from Application Default
// Credentials, or from a local emulator when FIRESTORE_EMULATOR_HOST is set —
// manifests never carry secrets (see docs/threat-model.md).
//
// Firestore is schemaless natively; strict and partial modes are enforced by
// ovdb core above the driver (collections are implicit — there is no DDL to
// provision).
func openFirestore(o *manifest.FirestoreOptions) (dal.DB, []schema.Mode, error) {
	ctx := context.Background()
	var client *firestore.Client
	var err error
	if o.Database != "" {
		client, err = firestore.NewClientWithDatabase(ctx, o.Project, o.Database)
	} else {
		client, err = firestore.NewClient(ctx, o.Project)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("failed to open Firestore client for project %s: %w", o.Project, err)
	}
	db := dalgo2firestore.NewDatabase(o.Project, client)
	return db, []schema.Mode{schema.ModeStrict, schema.ModePartial, schema.ModeSchemaless}, nil
}
