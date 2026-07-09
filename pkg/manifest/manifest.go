// Package manifest defines the OpenVaultDB database manifest format.
//
// A manifest is one YAML file describing a logical database: its id, schema
// mode, storage engine + configuration, and (when required) declared schemas.
package manifest

import (
	"bytes"
	"fmt"
	"os"
	"regexp"

	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
	"gopkg.in/yaml.v3"
)

// Manifest is the root of a database manifest file.
type Manifest struct {
	Database Database        `yaml:"database" json:"database"`
	Storage  Storage         `yaml:"storage" json:"storage"`
	Schemas  *schema.Schemas `yaml:"schemas,omitempty" json:"schemas,omitempty"`
}

// Database identifies the logical database and its schema mode.
type Database struct {
	ID         string      `yaml:"id" json:"id"`
	SchemaMode schema.Mode `yaml:"schema_mode" json:"schemaMode"`
}

// Storage selects and configures the storage engine.
type Storage struct {
	Engine    string            `yaml:"engine" json:"engine"`                 // "sqlite" | "ingitdb" | "firestore"
	Path      string            `yaml:"path,omitempty" json:"path,omitempty"` // unused by firestore
	InGitDB   *InGitDBOptions   `yaml:"ingitdb,omitempty" json:"ingitdb,omitempty"`
	Firestore *FirestoreOptions `yaml:"firestore,omitempty" json:"firestore,omitempty"`
}

// FirestoreOptions configures the Firestore engine. Credentials come from
// Application Default Credentials (or FIRESTORE_EMULATOR_HOST) — manifests
// never carry secrets.
type FirestoreOptions struct {
	// Project is the GCP project id (required).
	Project string `yaml:"project" json:"project"`
	// Database is the Firestore database id (default "(default)").
	Database string `yaml:"database,omitempty" json:"database,omitempty"`
}

// InGitDBOptions configures inGitDB-specific storage behavior.
type InGitDBOptions struct {
	// Push controls whether ovdb pushes to the git remote after each
	// committed write batch:
	//   "none"  (default) — commit locally only, never push;
	//   "sync"  — push before acknowledging the write; a failed push fails
	//             the write request (data is still committed locally);
	//   "async" — trigger a coalesced background push; failures are logged.
	Push string `yaml:"push,omitempty" json:"push,omitempty"`
	// Remote is the git remote to push to (default "origin").
	Remote string `yaml:"remote,omitempty" json:"remote,omitempty"`
	// Branch to push (default: HEAD — the current branch).
	Branch string `yaml:"branch,omitempty" json:"branch,omitempty"`
}

// PushMode returns the configured push mode with defaults applied.
func (o *InGitDBOptions) PushMode() string {
	if o == nil || o.Push == "" {
		return "none"
	}
	return o.Push
}

// PushRemote returns the remote with the default applied.
func (o *InGitDBOptions) PushRemote() string {
	if o == nil || o.Remote == "" {
		return "origin"
	}
	return o.Remote
}

// PushBranch returns the branch ref to push (default HEAD).
func (o *InGitDBOptions) PushBranch() string {
	if o == nil || o.Branch == "" {
		return "HEAD"
	}
	return o.Branch
}

var dbIDRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]*$`)

// Load reads and validates a manifest from a YAML file.
func Load(path string) (*Manifest, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read manifest: %w", err)
	}
	return Parse(b)
}

// Parse parses and validates manifest YAML.
func Parse(b []byte) (*Manifest, error) {
	var m Manifest
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("failed to parse manifest YAML: %w", err)
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

// Validate checks the manifest for structural correctness. It does NOT check
// engine/schema-mode compatibility — that is engine capability knowledge and
// is enforced when the database is opened (see pkg/core).
func (m *Manifest) Validate() error {
	if m.Database.ID == "" {
		return fmt.Errorf("database.id is required")
	}
	if !dbIDRe.MatchString(m.Database.ID) {
		return fmt.Errorf("database.id %q is invalid: must match %s", m.Database.ID, dbIDRe.String())
	}
	if err := m.Database.SchemaMode.Validate(); err != nil {
		return fmt.Errorf("database: %w", err)
	}
	if m.Storage.Engine == "" {
		return fmt.Errorf("storage.engine is required")
	}
	if m.Storage.Path == "" && m.Storage.Engine != "firestore" {
		return fmt.Errorf("storage.path is required")
	}
	if m.Storage.Engine == "firestore" {
		if m.Storage.Firestore == nil || m.Storage.Firestore.Project == "" {
			return fmt.Errorf("storage.firestore.project is required for the firestore engine")
		}
	} else if m.Storage.Firestore != nil {
		return fmt.Errorf("storage.firestore options are only valid with engine 'firestore', got %q", m.Storage.Engine)
	}
	if o := m.Storage.InGitDB; o != nil {
		if m.Storage.Engine != "ingitdb" {
			return fmt.Errorf("storage.ingitdb options are only valid with engine 'ingitdb', got %q", m.Storage.Engine)
		}
		switch o.Push {
		case "", "none", "sync", "async":
		default:
			return fmt.Errorf("storage.ingitdb.push must be one of: none, sync, async; got %q", o.Push)
		}
	}
	if err := m.Schemas.Validate(); err != nil {
		return fmt.Errorf("schemas: %w", err)
	}
	if m.Database.SchemaMode == schema.ModeStrict {
		if m.Schemas == nil || len(m.Schemas.Collections) == 0 {
			return fmt.Errorf("schema_mode 'strict' requires schemas.collections to declare at least one collection")
		}
	}
	return nil
}
