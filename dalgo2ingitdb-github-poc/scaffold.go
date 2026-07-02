// Package ingitdbgithubpoc proves the OVDB MVP's real integration path: a
// dal.DB backed by github.com/ingitdb/dalgo2ingitdb (the LOCAL-filesystem +
// local-git adapter ovdb-server itself already uses internally) writing a
// contactus-shaped record into a git working tree, committing it, and pushing
// it to a remote — with the on-disk layout (.ingitdb/root-collections.yaml,
// <collection>/.collection/definition.yaml, <collection>/$records/*.json)
// matching what @ingitdb/client-github (the browser/TS reader) expects, per
// its own source (packages/client-github/src/collection/collection.ts).
//
// This supersedes the earlier dalgo2ovdb-over-REST PoC (kept as
// ../dalgo2ovdb/, a working reference, not the path forward): the real MVP
// has no ovdb-server REST hop in the write or read path at all — sneat-go
// writes with dalgo2ingitdb straight into the user's cloned GitHub repo and
// pushes; the browser reads straight from GitHub's API with ingitdb-ts.
package ingitdbgithubpoc

import (
	"fmt"
	"os"
	"path/filepath"
)

// ContactsCollectionID is the dalgo collection id (== root-collections.yaml
// key == ingitdb.Definition.Collections map key, see dalgo2ingitdb's
// resolveCollection) used for the contactus-shaped record in this PoC.
const ContactsCollectionID = "contacts"

// contactsDefinitionYAML mirrors ovdb-server's internal/store/store.go
// tasksCollectionDefinition constant (same schema shape, same record_file
// convention: one JSON file per record under $records/, keyed by {key}),
// with contactus-shaped columns (firstName, lastName, email, roles) instead
// of the todo-demo's (title, done, createdAt). "roles" is a bare array of
// strings, declared as the ingitdb "any" column type — there is no dedicated
// array column type in ingitdb-go's ColumnType enum
// (ingitdb-go/ingitdb/column_type.go).
const contactsDefinitionYAML = `titles:
  en: Contacts
record_file:
  name: "{key}.json"
  type: "map[string]any"
  format: json
columns:
  firstName:
    type: string
    required: true
  lastName:
    type: string
  email:
    type: string
  roles:
    type: any
columns_order:
  - firstName
  - lastName
  - email
  - roles
`

// ScaffoldContactsCollection creates the minimal on-disk ingitdb project
// layout dalgo2ingitdb.NewDatabase (and, independently, @ingitdb/client-github)
// need: a root .ingitdb/root-collections.yaml mapping the "contacts"
// collection id to its (here, top-level) directory, that directory's
// .collection/definition.yaml schema, and an empty $records/ directory. root
// must already exist (e.g. a freshly `git init`-ed working tree).
func ScaffoldContactsCollection(root string) error {
	colDir := filepath.Join(root, ContactsCollectionID)
	recordsDir := filepath.Join(colDir, "$records")
	if err := os.MkdirAll(recordsDir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", recordsDir, err)
	}
	schemaDir := filepath.Join(colDir, ".collection")
	if err := os.MkdirAll(schemaDir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", schemaDir, err)
	}
	schemaPath := filepath.Join(schemaDir, "definition.yaml")
	if err := os.WriteFile(schemaPath, []byte(contactsDefinitionYAML), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", schemaPath, err)
	}

	ingitdbDir := filepath.Join(root, ".ingitdb")
	if err := os.MkdirAll(ingitdbDir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", ingitdbDir, err)
	}
	rootCollections := fmt.Sprintf("%s: %s\n", ContactsCollectionID, ContactsCollectionID)
	rootCollectionsPath := filepath.Join(ingitdbDir, "root-collections.yaml")
	if err := os.WriteFile(rootCollectionsPath, []byte(rootCollections), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", rootCollectionsPath, err)
	}
	return nil
}
