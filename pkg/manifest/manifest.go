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
	Engine    string            `yaml:"engine" json:"engine"`                 // "sqlite" | "ingitdb" | "firestore" | "postgres" | "mysql"
	Path      string            `yaml:"path,omitempty" json:"path,omitempty"` // unused by firestore/postgres/mysql
	InGitDB   *InGitDBOptions   `yaml:"ingitdb,omitempty" json:"ingitdb,omitempty"`
	Firestore *FirestoreOptions `yaml:"firestore,omitempty" json:"firestore,omitempty"`
	Postgres  *PostgresOptions  `yaml:"postgres,omitempty" json:"postgres,omitempty"`
	MySQL     *MySQLOptions     `yaml:"mysql,omitempty" json:"mysql,omitempty"`
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

// PostgresOptions configures the PostgreSQL engine. The connection string
// (which carries credentials) is NEVER stored in the manifest: DSNEnv names
// the environment variable that holds it.
type PostgresOptions struct {
	// DSNEnv is the environment variable holding the Postgres DSN
	// (default "OVDB_POSTGRES_DSN"), e.g.
	// postgres://user:pass@host:5432/db?sslmode=require.
	DSNEnv string `yaml:"dsn_env,omitempty" json:"dsnEnv,omitempty"`
}

// DSNEnvVar returns the env var name holding the DSN, with the default applied.
func (o *PostgresOptions) DSNEnvVar() string {
	if o == nil || o.DSNEnv == "" {
		return "OVDB_POSTGRES_DSN"
	}
	return o.DSNEnv
}

// MySQLOptions configures the MySQL engine. The connection string (which
// carries credentials) is NEVER stored in the manifest: DSNEnv names the
// environment variable that holds it.
type MySQLOptions struct {
	// DSNEnv is the environment variable holding the MySQL DSN
	// (default "OVDB_MYSQL_DSN"), in go-sql-driver form, e.g.
	// user:pass@tcp(host:3306)/db?parseTime=true.
	DSNEnv string `yaml:"dsn_env,omitempty" json:"dsnEnv,omitempty"`
}

// DSNEnvVar returns the env var name holding the DSN, with the default applied.
func (o *MySQLOptions) DSNEnvVar() string {
	if o == nil || o.DSNEnv == "" {
		return "OVDB_MYSQL_DSN"
	}
	return o.DSNEnv
}

// InGitDBOptions configures inGitDB-specific storage behavior. It has two
// backends: the local filesystem working tree (default, driven by
// storage.path) and, when GitHub is set, direct writes to a GitHub repo over
// the API (no local working tree — each write batch is one commit via the
// GitHub Git tree API).
type InGitDBOptions struct {
	// Push controls whether ovdb pushes the local working tree to the git
	// remote after each committed write batch (filesystem backend only):
	//   "none"  (default) — commit locally only, never push;
	//   "sync"  — push before acknowledging the write; a failed push fails
	//             the write request (data is still committed locally);
	//   "async" — trigger a coalesced background push; failures are logged.
	Push string `yaml:"push,omitempty" json:"push,omitempty"`
	// Remote is the git remote to push to (default "origin").
	Remote string `yaml:"remote,omitempty" json:"remote,omitempty"`
	// Branch to push (default: HEAD — the current branch).
	Branch string `yaml:"branch,omitempty" json:"branch,omitempty"`

	// GitHub, when set, selects the GitHub backend: records are written
	// directly to a GitHub repository over the API instead of a local
	// working tree. storage.path is then unused.
	GitHub *InGitDBGitHubOptions `yaml:"github,omitempty" json:"github,omitempty"`
}

// InGitDBGitHubOptions configures the GitHub backend of the inGitDB engine.
// The access token (a credential) is NEVER stored in the manifest: TokenEnv
// names the environment variable that holds it.
type InGitDBGitHubOptions struct {
	// Owner is the GitHub account or org owning the repo (required).
	Owner string `yaml:"owner" json:"owner"`
	// Repo is the repository name (required).
	Repo string `yaml:"repo" json:"repo"`
	// Ref is the branch to read and commit to (default "main").
	Ref string `yaml:"ref,omitempty" json:"ref,omitempty"`
	// TokenEnv is the environment variable holding the GitHub token
	// (default "OVDB_GITHUB_TOKEN") — a PAT or app installation token with
	// contents:write on the repo.
	TokenEnv string `yaml:"token_env,omitempty" json:"tokenEnv,omitempty"`
	// APIBaseURL overrides the GitHub API base (for GitHub Enterprise).
	APIBaseURL string `yaml:"api_base_url,omitempty" json:"apiBaseURL,omitempty"`
}

// GitRef returns the branch with the default applied.
func (o *InGitDBGitHubOptions) GitRef() string {
	if o == nil || o.Ref == "" {
		return "main"
	}
	return o.Ref
}

// TokenEnvVar returns the env var name holding the token, default applied.
func (o *InGitDBGitHubOptions) TokenEnvVar() string {
	if o == nil || o.TokenEnv == "" {
		return "OVDB_GITHUB_TOKEN"
	}
	return o.TokenEnv
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
	// The inGitDB engine's GitHub backend is also pathless (data lives in the
	// remote repo, not a local working tree).
	ingitdbGitHub := m.Storage.Engine == "ingitdb" && m.Storage.InGitDB != nil && m.Storage.InGitDB.GitHub != nil
	pathless := m.Storage.Engine == "firestore" || m.Storage.Engine == "postgres" ||
		m.Storage.Engine == "mysql" || ingitdbGitHub
	if m.Storage.Path == "" && !pathless {
		return fmt.Errorf("storage.path is required")
	}
	if m.Storage.Engine == "firestore" {
		if m.Storage.Firestore == nil || m.Storage.Firestore.Project == "" {
			return fmt.Errorf("storage.firestore.project is required for the firestore engine")
		}
	} else if m.Storage.Firestore != nil {
		return fmt.Errorf("storage.firestore options are only valid with engine 'firestore', got %q", m.Storage.Engine)
	}
	if m.Storage.Postgres != nil && m.Storage.Engine != "postgres" {
		return fmt.Errorf("storage.postgres options are only valid with engine 'postgres', got %q", m.Storage.Engine)
	}
	if m.Storage.MySQL != nil && m.Storage.Engine != "mysql" {
		return fmt.Errorf("storage.mysql options are only valid with engine 'mysql', got %q", m.Storage.Engine)
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
		if gh := o.GitHub; gh != nil {
			if gh.Owner == "" || gh.Repo == "" {
				return fmt.Errorf("storage.ingitdb.github requires both owner and repo")
			}
			if o.Push != "" {
				return fmt.Errorf("storage.ingitdb.push does not apply to the github backend (writes commit directly)")
			}
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
