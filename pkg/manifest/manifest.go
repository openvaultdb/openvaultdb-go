// Package manifest defines the OpenVaultDB database manifest format.
//
// A manifest is one YAML file describing a logical database: its id, schema
// mode, storage engine + configuration, and (when required) declared schemas.
package manifest

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dal-go/dalgo/access"
	"github.com/openvaultdb/openvaultdb-go/pkg/license"
	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
	"gopkg.in/yaml.v3"
)

// Manifest is the root of a database manifest file.
type Manifest struct {
	RecordsetLicenses map[string]license.Declaration `yaml:"recordset_licenses,omitempty" json:"recordsetLicenses,omitempty"`
	Database          Database                       `yaml:"database" json:"database"`
	Storage           Storage                        `yaml:"storage" json:"storage"`
	Schemas           *schema.Schemas                `yaml:"schemas,omitempty" json:"schemas,omitempty"`
	// ACL policies belong to this OpenVaultDB mount. Files resolve relative to
	// the manifest directory; underlying engines retain their own policies.
	ACLStore *PolicyStoreConfig       `yaml:"acl_store,omitempty" json:"aclStore,omitempty"`
	ACL      *access.FilePolicyConfig `yaml:"acl,omitempty" json:"acl,omitempty"`
}

// PolicyStoreConfig selects immutable owner generations instead of flat policy files.
type PolicyStoreConfig struct {
	Path string `yaml:"path" json:"path"`
}

// Database identifies the logical database and its schema mode.
type Database struct {
	License    *license.Declaration `yaml:"license,omitempty" json:"license,omitempty"`
	ID         string               `yaml:"id" json:"id"`
	SchemaMode schema.Mode          `yaml:"schema_mode" json:"schemaMode"`
	CacheTTL   string               `yaml:"cache_ttl,omitempty" json:"cacheTtl,omitempty"`
}

// ReadCacheTTL is the database's public read cache duration. Invalid values
// fail manifest validation; returning zero here also keeps unchecked values safe.
func (d Database) ReadCacheTTL() time.Duration {
	ttl, err := time.ParseDuration(d.CacheTTL)
	if err != nil || ttl < 0 || ttl > 365*24*time.Hour || ttl%time.Second != 0 {
		return 0
	}
	return ttl
}

// Storage selects and configures the storage engine.
type Storage struct {
	SQLite    *SQLiteOptions    `yaml:"sqlite,omitempty" json:"sqlite,omitempty"`
	Engine    string            `yaml:"engine" json:"engine"`                 // "sqlite" | "ingitdb" | "firestore" | "postgres" | "mysql"
	Path      string            `yaml:"path,omitempty" json:"path,omitempty"` // unused by firestore/postgres/mysql
	InGitDB   *InGitDBOptions   `yaml:"ingitdb,omitempty" json:"ingitdb,omitempty"`
	Firestore *FirestoreOptions `yaml:"firestore,omitempty" json:"firestore,omitempty"`
	Postgres  *PostgresOptions  `yaml:"postgres,omitempty" json:"postgres,omitempty"`
	MySQL     *MySQLOptions     `yaml:"mysql,omitempty" json:"mysql,omitempty"`
}

// SQLiteOptions optionally selects verified TEXT transport keys and lock wait.
// A nil RecordKeys map retains the legacy id key. A present map must cover
// exactly the mounted canonical collections. BusyTimeout is presence-aware:
// omitted retains the legacy wait, while "0s" explicitly disables lock retries.
type SQLiteOptions struct {
	RecordKeys  map[string]string `yaml:"record_keys,omitempty" json:"recordKeys,omitempty"`
	BusyTimeout *string           `yaml:"busy_timeout,omitempty" json:"busyTimeout,omitempty"`
}

// LockWait returns the configured wait, or the supplied legacy default.
func (o *SQLiteOptions) LockWait(legacy time.Duration) (time.Duration, error) {
	if o == nil || o.BusyTimeout == nil {
		return legacy, nil
	}
	wait, err := time.ParseDuration(*o.BusyTimeout)
	if err != nil || wait < 0 || wait > 5*time.Second || wait%time.Millisecond != 0 {
		return 0, fmt.Errorf("storage.sqlite.busy_timeout must be a whole-millisecond duration from 0s through 5s")
	}
	return wait, nil
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
// the environment variable that holds it. A manifest of the postgres engine
// declares only collection and field names that are plain identifiers (ASCII
// letters, digits and underscores, not starting with a digit) of at most 63
// bytes, or it is refused when it is loaded (see CheckPostgresNames).
type PostgresOptions struct {
	// DSNEnv is the name of the environment variable holding the Postgres DSN
	// (default "OVDB_POSTGRES_DSN"); the DSN is e.g.
	// postgres://user:pass@host:5432/db?sslmode=require. Validate accepts only a
	// variable name (see ValidEnvVarName).
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
	// DSNEnv is the name of the environment variable holding the MySQL DSN
	// (default "OVDB_MYSQL_DSN"); the DSN is in go-sql-driver form, e.g.
	// user:pass@tcp(host:3306)/db?parseTime=true. Validate accepts only a
	// variable name (see ValidEnvVarName).
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
	// TokenEnv is the name of the environment variable holding the GitHub token
	// (default "OVDB_GITHUB_TOKEN") — a PAT or app installation token with
	// contents:write on the repo. Validate accepts only a variable name (see
	// ValidEnvVarName).
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

var envVarNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ValidEnvVarName reports whether name can be the name of an environment
// variable as a manifest writes it: an ASCII letter or an underscore followed by
// ASCII letters, digits and underscores.
func ValidEnvVarName(name string) bool { return envVarNameRe.MatchString(name) }

// maxEchoedIDLen bounds how much of a database id an error message repeats: the
// id of a request or a manifest is checked here, and what fails the check can
// be as large as its source.
const maxEchoedIDLen = 256

// clipID returns id as it may appear in an error message: whole when it is at
// most maxEchoedIDLen bytes, otherwise cut at a character boundary and followed
// by its length.
func clipID(id string) string {
	if len(id) <= maxEchoedIDLen {
		return id
	}
	cut := maxEchoedIDLen
	for cut > 0 && !utf8.RuneStart(id[cut]) {
		cut--
	}
	return fmt.Sprintf("%s...(%d bytes)", id[:cut], len(id))
}

// ValidateID checks a database id against the manifest id constraint
// (also used by the server when creating databases at runtime).
func ValidateID(id string) error {
	if id == "" {
		return fmt.Errorf("database id is required")
	}
	if !dbIDRe.MatchString(id) {
		return fmt.Errorf("database id %q is invalid: must match %s", clipID(id), dbIDRe.String())
	}
	return nil
}

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
	if err := decodeYAML(func() error { return dec.Decode(&m) }); err != nil {
		return nil, parseError(err)
	}
	var extra any
	if err := decodeYAML(func() error { return dec.Decode(&extra) }); err != io.EOF {
		return nil, fmt.Errorf("manifest must contain exactly one YAML document")
	}
	var document map[string]yaml.Node
	if err := decodeYAML(func() error { return unmarshalYAML(b, &document) }); err != nil {
		return nil, parseError(err)
	}
	if database, present := document["database"]; present {
		if err := validateDatabaseLicenseNode(&database, 0); err != nil {
			return nil, err
		}
	}
	if recordsets, present := document["recordset_licenses"]; present && recordsets.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("recordset_licenses must be a mapping")
	}
	if storage, present := document["storage"]; present {
		for i := 0; storage.Kind == yaml.MappingNode && i+1 < len(storage.Content); i += 2 {
			if storage.Content[i].Value != "sqlite" {
				continue
			}
			options := storage.Content[i+1]
			if options.Kind != yaml.MappingNode {
				return nil, fmt.Errorf("storage.sqlite must be an options mapping")
			}
			for j := 0; j+1 < len(options.Content); j += 2 {
				key, value := options.Content[j].Value, options.Content[j+1]
				if key == "busy_timeout" && (value.Kind != yaml.ScalarNode || value.Tag != "!!str") {
					return nil, fmt.Errorf("storage.sqlite.busy_timeout must be a duration string when supplied")
				}
				if key == "record_keys" && value.Kind != yaml.MappingNode {
					return nil, fmt.Errorf("storage.sqlite.record_keys must be a mapping when supplied")
				}
			}
		}
	}
	if acl, present := document["acl"]; present {
		explicitMode := false
		for i := 0; acl.Kind == yaml.MappingNode && i+1 < len(acl.Content); i += 2 {
			if acl.Content[i].Value == "enabled" && acl.Content[i+1].Tag == "!!bool" {
				explicitMode = true
			}
		}
		if !explicitMode {
			return nil, fmt.Errorf("acl requires an explicit boolean enabled setting")
		}
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

// maxParseProblems bounds how many mistakes of a manifest one parse error lists.
const maxParseProblems = 10

// unmarshalYAML decodes the manifest a second time into nodes. It is a variable so
// a test can make that decode fail or panic, which no document does today.
var unmarshalYAML = yaml.Unmarshal

// errDecoderPanic is what decodeYAML returns for a decode that panicked.
var errDecoderPanic = errors.New("the YAML decoder panicked")

// decodeYAML runs one decode of the manifest and turns a panic of the decoder into
// an error, so a manifest the decoder cannot decode is reported and never crashes
// the process. The decoder panics, instead of returning an error, for a mapping that
// holds a merge key and a key it cannot hash.
func decodeYAML(decode func() error) (err error) {
	defer func() {
		if recover() != nil {
			err = errDecoderPanic
		}
	}()
	return decode()
}

var typeErrorLine = regexp.MustCompile(`^line [0-9]+: `)

// scannerErrorText matches the text of an error of the YAML scanner or parser, which
// the decoder builds from the line and a message of its own, never from the document.
var scannerErrorText = regexp.MustCompile(`^yaml: line [0-9]+: `)

// parseError is the error for a manifest that the YAML decoder refused. For a
// value of the wrong type, a field the manifest does not have and a key written
// twice the decoder quotes the start of the value or the whole name, and a manifest
// can hold a connection string or a token there (as the value of storage.postgres,
// for instance). Each such mistake is therefore reported by its line and its kind,
// never by what the line holds. A scanner or parser error, whose text is fixed, is
// wrapped as it is. Any other error of the decoder (a scalar written with a tag it
// does not fit, an anchor that is not defined or that holds itself, a key that
// cannot be hashed) quotes the document too, so it is replaced by one sentence that
// wraps nothing, and so is a decode that panicked. A manifest with no document in it
// is reported as empty.
func parseError(err error) error {
	if errors.Is(err, io.EOF) {
		return errors.New("failed to parse manifest YAML: the manifest is empty")
	}
	var typeErr *yaml.TypeError
	if !errors.As(err, &typeErr) {
		if scannerErrorText.MatchString(err.Error()) {
			return fmt.Errorf("failed to parse manifest YAML: %w", err)
		}
		return errors.New("failed to parse manifest YAML: the document could not be decoded")
	}
	problems := make([]string, 0, maxParseProblems+1)
	for i, text := range typeErr.Errors {
		if i == maxParseProblems {
			problems = append(problems, "and more")
			break
		}
		where, rest := "", text
		if prefix := typeErrorLine.FindString(text); prefix != "" {
			where, rest = prefix, text[len(prefix):]
		}
		kind := "a value of the wrong type"
		switch {
		case strings.HasPrefix(rest, "field "):
			kind = "a field the manifest does not have"
		case strings.HasPrefix(rest, "mapping key "):
			kind = "a key written twice"
		}
		problems = append(problems, where+kind)
	}
	return fmt.Errorf("failed to parse manifest YAML: %s", strings.Join(problems, "; "))
}

// Validate checks the manifest for structural correctness. It does NOT check
// engine/schema-mode compatibility — that is engine capability knowledge and
// is enforced when the database is opened (see pkg/core). The names a
// PostgreSQL database cannot keep whole are the one engine fact it does check
// (see CheckPostgresNames): a manifest that declares one is refused here,
// before any connection is made.
func (m *Manifest) Validate() error {
	if err := m.ValidateLicenses(); err != nil {
		return err
	}
	if m.ACLStore != nil {
		if m.ACL == nil || !m.ACL.Enabled || m.ACLStore.Path == "" || len(m.ACL.Policies) > 0 {
			return fmt.Errorf("acl_store requires enabled acl, a path, and no flat policies")
		}
	}

	if m.Database.ID == "" {
		return fmt.Errorf("database.id is required")
	}
	if !dbIDRe.MatchString(m.Database.ID) {
		return fmt.Errorf("database.id %q is invalid: must match %s", clipID(m.Database.ID), dbIDRe.String())
	}
	if err := m.Database.SchemaMode.Validate(); err != nil {
		return fmt.Errorf("database: %w", err)
	}
	if m.Database.CacheTTL != "" {
		ttl, err := time.ParseDuration(m.Database.CacheTTL)
		if err != nil || ttl < 0 || ttl > 365*24*time.Hour || ttl%time.Second != 0 {
			return fmt.Errorf("database.cache_ttl must be a whole-second duration from 0s through 8760h")
		}
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
	if o := m.Storage.SQLite; o != nil {
		if m.Storage.Engine != "sqlite" {
			return fmt.Errorf("storage.sqlite options require the sqlite engine")
		}
		if _, err := o.LockWait(5 * time.Second); err != nil {
			return err
		}
		if o.RecordKeys != nil && len(o.RecordKeys) == 0 {
			return fmt.Errorf("storage.sqlite.record_keys must be nonempty when supplied")
		}
	}
	if m.Storage.Postgres != nil && m.Storage.Engine != "postgres" {
		return fmt.Errorf("storage.postgres options are only valid with engine 'postgres', got %q", m.Storage.Engine)
	}
	if m.Storage.MySQL != nil && m.Storage.Engine != "mysql" {
		return fmt.Errorf("storage.mysql options are only valid with engine 'mysql', got %q", m.Storage.Engine)
	}
	// The message names the field and does not repeat the value.
	if o := m.Storage.Postgres; o != nil && o.DSNEnv != "" && !ValidEnvVarName(o.DSNEnv) {
		return fmt.Errorf("storage.postgres.dsn_env must be the name of an environment variable (ASCII letters, digits and underscores, not starting with a digit)")
	}
	if o := m.Storage.MySQL; o != nil && o.DSNEnv != "" && !ValidEnvVarName(o.DSNEnv) {
		return fmt.Errorf("storage.mysql.dsn_env must be the name of an environment variable (ASCII letters, digits and underscores, not starting with a digit)")
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
			// The message names the field and does not repeat the value.
			if gh.TokenEnv != "" && !ValidEnvVarName(gh.TokenEnv) {
				return fmt.Errorf("storage.ingitdb.github.token_env must be the name of an environment variable (ASCII letters, digits and underscores, not starting with a digit)")
			}
			if o.Push != "" {
				return fmt.Errorf("storage.ingitdb.push does not apply to the github backend (writes commit directly)")
			}
		}
	}
	if err := m.Schemas.Validate(); err != nil {
		return fmt.Errorf("schemas: %w", err)
	}
	if err := m.CheckPostgresNames(); err != nil {
		return err
	}
	if m.Database.SchemaMode == schema.ModeStrict {
		if m.Schemas == nil || len(m.Schemas.Collections) == 0 {
			return fmt.Errorf("schema_mode 'strict' requires schemas.collections to declare at least one collection")
		}
	}
	return nil
}
