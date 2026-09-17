// Package mount loads database manifests and opens the corresponding DALgo
// drivers, producing ready-to-serve core.Database instances. Storage access
// inside ovdb is dalgo-native: each engine is simply a dal.DB driver plus a
// schema-mode capability declaration.
package mount

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/dal-go/dalgo/access"
	"github.com/dal-go/dalgo/dal"
	"github.com/ingitdb/dalgo2ingitdb"
	"github.com/ingitdb/ingitdb-go/ingitdb/validator"

	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/manifest"
	"github.com/openvaultdb/openvaultdb-go/pkg/policystore"
	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
)

// Options tunes how FileWithOptions mounts a database. The zero value is the
// behaviour of File.
type Options struct {
	// CatalogueDir, when set, is where the inferred-schema catalogue lives,
	// as <CatalogueDir>/<database id>.inferred.json, for every engine —
	// instead of the default next to (or inside) the user's storage
	// (<file>.inferred.json, <folder>/.ovdb/inferred-schema.json).
	CatalogueDir string
	// SkipGitIdentity disables stamping a repo-local git identity on a local
	// inGitDB folder (see ensureGitIdentity), so mounting leaves .git/config
	// untouched. Commits then rely on the host's own git identity.
	SkipGitIdentity bool
}

// File mounts one database from a manifest file. Relative storage paths are
// resolved against the manifest file's directory.
func File(manifestPath string) (*core.Database, error) {
	return FileWithOptions(manifestPath, Options{})
}

// FileWithOptions is File with mount Options. With CatalogueDir set outside
// the storage and SkipGitIdentity true, mounting an existing local inGitDB
// folder or SQLite file adds or changes nothing in it (writes made later
// through the database still land in the storage, as they must).
func FileWithOptions(manifestPath string, opts Options) (*core.Database, error) {
	m, err := manifest.Load(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", manifestPath, err)
	}
	baseDir := filepath.Dir(manifestPath)
	var policies []access.Policy
	var controller *policystore.Controller
	if m.ACL != nil {
		if m.ACL.Enabled && m.Storage.Engine != "sqlite" && m.Storage.Engine != "ingitdb" {
			return nil, fmt.Errorf("%s: file ACL currently supports SQLite and local InGitDB mounts", manifestPath)
		}
		if m.ACL.Enabled && m.Storage.InGitDB != nil && m.Storage.InGitDB.GitHub != nil {
			return nil, fmt.Errorf("%s: file ACL does not yet support the GitHub InGitDB adapter", manifestPath)
		}
		config := *m.ACL
		if config.Database != "" && config.Database != m.Database.ID {
			return nil, fmt.Errorf("%s: ACL database must match mounted database", manifestPath)
		}
		config.Database = m.Database.ID
		if m.ACLStore != nil {
			root := m.ACLStore.Path
			if !filepath.IsAbs(root) {
				root = filepath.Join(baseDir, root)
			}
			store, openErr := policystore.Open(root, policystore.Owner{Enabled: true, Database: config.Database, Realm: config.Realm})
			if openErr != nil {
				return nil, openErr
			}
			controller, err = policystore.NewController(context.Background(), store)
			if err == nil {
				policies, err = controller.Policies(context.Background())
			}
		} else {
			policies, err = access.LoadPolicyFiles(baseDir, config)
		}
		if err != nil {
			return nil, fmt.Errorf("%s: load OpenVaultDB policies: %w", manifestPath, err)
		}
	}
	storagePath := m.Storage.Path
	if !filepath.IsAbs(storagePath) {
		storagePath = filepath.Join(baseDir, storagePath)
	}

	var db dal.DB
	var modes []schema.Mode
	var cataloguePath string
	var closeClient func() error // engine resources the dal.DB does not own
	switch m.Storage.Engine {
	case "sqlite":
		if db, modes, err = openSQLite(storagePath, m); err != nil {
			return nil, fmt.Errorf("%s: %w", manifestPath, err)
		}
		cataloguePath = storagePath + ".inferred.json"
	case "ingitdb":
		if m.Storage.InGitDB != nil && m.Storage.InGitDB.GitHub != nil {
			if db, modes, err = openInGitDBGitHub(m); err != nil {
				return nil, fmt.Errorf("%s: %w", manifestPath, err)
			}
			cataloguePath = filepath.Join(baseDir, m.Database.ID+".inferred.json")
		} else {
			options := []dalgo2ingitdb.DatabaseOption{dalgo2ingitdb.WithProtectedProfile()}
			if len(policies) > 0 {
				options = append(options, dalgo2ingitdb.WithStoredOnlyReads())
			}
			if db, modes, err = openInGitDB(storagePath, !opts.SkipGitIdentity, options...); err != nil {
				return nil, fmt.Errorf("%s: %w", manifestPath, err)
			}
			cataloguePath = filepath.Join(storagePath, ".ovdb", "inferred-schema.json")
		}
	case "firestore":
		if db, modes, closeClient, err = openFirestore(m.Storage.Firestore); err != nil {
			return nil, fmt.Errorf("%s: %w", manifestPath, err)
		}
		// Firestore has no local data directory; the inferred catalogue
		// lives next to the manifest.
		cataloguePath = filepath.Join(baseDir, m.Database.ID+".inferred.json")
	case "postgres":
		if db, modes, err = openPostgres(m); err != nil {
			return nil, fmt.Errorf("%s: %w", manifestPath, err)
		}
		cataloguePath = filepath.Join(baseDir, m.Database.ID+".inferred.json")
	case "mysql":
		if db, modes, err = openMySQL(m); err != nil {
			return nil, fmt.Errorf("%s: %w", manifestPath, err)
		}
		cataloguePath = filepath.Join(baseDir, m.Database.ID+".inferred.json")
	default:
		return nil, fmt.Errorf("%s: unknown storage engine %q (supported: sqlite, ingitdb, firestore, postgres, mysql)",
			manifestPath, m.Storage.Engine)
	}

	if opts.CatalogueDir != "" {
		cataloguePath = filepath.Join(opts.CatalogueDir, m.Database.ID+".inferred.json")
	}

	var d *core.Database
	if controller != nil {
		d, err = core.OpenWithPolicyController(m, db, modes, cataloguePath, controller)
	} else {
		d, err = core.Open(m, db, modes, cataloguePath, policies...)
	}
	if err != nil {
		// core does not take ownership on failure: release the driver.
		if closer, ok := db.(io.Closer); ok {
			_ = closer.Close()
		}
		if closeClient != nil {
			_ = closeClient()
		}
		return nil, fmt.Errorf("%s: %w", manifestPath, err)
	}
	d.OnClose(closeClient)
	if m.Storage.Engine == "ingitdb" {
		if hook, stop := newGitPushHook(storagePath, m.Storage.InGitDB); hook != nil {
			d.SetAfterWrite(hook)
			d.OnClose(stop)
		}
	}
	return d, nil
}

// openInGitDB opens (creating if needed) an inGitDB directory through the
// published dalgo2ingitdb driver. inGitDB is the reference engine and
// supports all schema modes: schemaless works because core auto-creates
// collection definitions on first write via the driver's ddl.SchemaModifier.
func openInGitDB(dir string, gitIdentity bool, options ...dalgo2ingitdb.DatabaseOption) (dal.DB, []schema.Mode, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, nil, fmt.Errorf("failed to create inGitDB directory %s: %w", dir, err)
	}
	// Best-effort: make dir self-sufficient for the commits dalgo2ingitdb
	// makes on every write, regardless of how dir came to be a git
	// repository or whether this is a fresh create or a remount. See
	// ensureGitIdentity for why this must live here rather than only at
	// creation time. Skipped when connecting a user's folder must not
	// change it (Options.SkipGitIdentity).
	if gitIdentity {
		ensureGitIdentity(dir)
	}
	db, err := dalgo2ingitdb.NewDatabase(dir, validator.NewCollectionsReader(), options...)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to open inGitDB at %s: %w", dir, err)
	}
	return db, []schema.Mode{schema.ModeStrict, schema.ModePartial, schema.ModeSchemaless}, nil
}

// Dir mounts every *.yaml / *.yml manifest found directly in dir,
// keyed by database id. It fails on the first manifest that does not mount;
// see DirReport for a scan that tolerates broken manifests.
func Dir(dir string) (map[string]*core.Database, error) {
	paths, err := manifestPaths(dir)
	if err != nil {
		return nil, err
	}
	dbs := map[string]*core.Database{}
	for _, path := range paths {
		db, err := mountUnique(path, dbs, Options{})
		if err != nil {
			closeAll(dbs)
			return nil, err
		}
		dbs[db.ID()] = db
	}
	return dbs, nil
}

// DirReport mounts every *.yaml / *.yml manifest found directly in dir, like
// Dir, but one manifest that fails to mount does not stop the others. It
// returns the mounted databases keyed by database id and the failures keyed by
// manifest path. The error result is non-nil only when dir itself cannot be
// read.
func DirReport(dir string) (map[string]*core.Database, map[string]error, error) {
	return DirReportWithOptions(dir, Options{})
}

// DirReportWithOptions is DirReport mounting each manifest with
// FileWithOptions, e.g. so a registry scan never writes into user storage.
// Catalogues in Options.CatalogueDir are per database id and cannot collide
// (duplicate ids are rejected).
func DirReportWithOptions(dir string, opts Options) (map[string]*core.Database, map[string]error, error) {
	paths, err := manifestPaths(dir)
	if err != nil {
		return nil, nil, err
	}
	dbs := map[string]*core.Database{}
	failures := map[string]error{}
	for _, path := range paths {
		db, err := mountUnique(path, dbs, opts)
		if err != nil {
			failures[path] = err
			continue
		}
		dbs[db.ID()] = db
	}
	return dbs, failures, nil
}

func manifestPaths(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("failed to read manifests directory %s: %w", dir, err)
	}
	var paths []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasSuffix(name, ".yaml") || strings.HasSuffix(name, ".yml") {
			paths = append(paths, filepath.Join(dir, name))
		}
	}
	sort.Strings(paths)
	return paths, nil
}

// mountUnique mounts path and rejects (closing it again) a database whose id
// is already in dbs.
func mountUnique(path string, dbs map[string]*core.Database, opts Options) (*core.Database, error) {
	db, err := FileWithOptions(path, opts)
	if err != nil {
		return nil, err
	}
	if _, dup := dbs[db.ID()]; dup {
		_ = db.Close()
		return nil, fmt.Errorf("%s: duplicate database id %q", path, db.ID())
	}
	return db, nil
}

func closeAll(dbs map[string]*core.Database) {
	for _, db := range dbs {
		_ = db.Close()
	}
}
