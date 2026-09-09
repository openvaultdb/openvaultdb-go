// Package mount loads database manifests and opens the corresponding DALgo
// drivers, producing ready-to-serve core.Database instances. Storage access
// inside ovdb is dalgo-native: each engine is simply a dal.DB driver plus a
// schema-mode capability declaration.
package mount

import (
	"context"
	"fmt"
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

// File mounts one database from a manifest file. Relative storage paths are
// resolved against the manifest file's directory.
func File(manifestPath string) (*core.Database, error) {
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
			var options []dalgo2ingitdb.DatabaseOption
			if len(policies) > 0 {
				options = append(options, dalgo2ingitdb.WithStoredOnlyReads())
			}
			if db, modes, err = openInGitDB(storagePath, options...); err != nil {
				return nil, fmt.Errorf("%s: %w", manifestPath, err)
			}
			cataloguePath = filepath.Join(storagePath, ".ovdb", "inferred-schema.json")
		}
	case "firestore":
		if db, modes, err = openFirestore(m.Storage.Firestore); err != nil {
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

	var d *core.Database
	if controller != nil {
		d, err = core.OpenWithPolicyController(m, db, modes, cataloguePath, controller)
	} else {
		d, err = core.Open(m, db, modes, cataloguePath, policies...)
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", manifestPath, err)
	}
	if m.Storage.Engine == "ingitdb" {
		if hook := newGitPushHook(storagePath, m.Storage.InGitDB); hook != nil {
			d.SetAfterWrite(hook)
		}
	}
	return d, nil
}

// openInGitDB opens (creating if needed) an inGitDB directory through the
// published dalgo2ingitdb driver. inGitDB is the reference engine and
// supports all schema modes: schemaless works because core auto-creates
// collection definitions on first write via the driver's ddl.SchemaModifier.
func openInGitDB(dir string, options ...dalgo2ingitdb.DatabaseOption) (dal.DB, []schema.Mode, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, nil, fmt.Errorf("failed to create inGitDB directory %s: %w", dir, err)
	}
	// Best-effort: make dir self-sufficient for the commits dalgo2ingitdb
	// makes on every write, regardless of how dir came to be a git
	// repository or whether this is a fresh create or a remount. See
	// ensureGitIdentity for why this must live here rather than only at
	// creation time.
	ensureGitIdentity(dir)
	db, err := dalgo2ingitdb.NewDatabase(dir, validator.NewCollectionsReader(), options...)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to open inGitDB at %s: %w", dir, err)
	}
	return db, []schema.Mode{schema.ModeStrict, schema.ModePartial, schema.ModeSchemaless}, nil
}

// Dir mounts every *.yaml / *.yml manifest found directly in dir,
// keyed by database id.
func Dir(dir string) (map[string]*core.Database, error) {
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
	dbs := map[string]*core.Database{}
	for _, path := range paths {
		db, err := File(path)
		if err != nil {
			return nil, err
		}
		if _, dup := dbs[db.ID()]; dup {
			return nil, fmt.Errorf("%s: duplicate database id %q", path, db.ID())
		}
		dbs[db.ID()] = db
	}
	return dbs, nil
}
