// Package mount loads database manifests and opens the corresponding DALgo
// drivers, producing ready-to-serve core.Database instances. Storage access
// inside ovdb is dalgo-native: each engine is simply a dal.DB driver plus a
// schema-mode capability declaration.
package mount

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/dal-go/dalgo/dal"
	"github.com/ingitdb/dalgo2ingitdb"
	"github.com/ingitdb/ingitdb-go/ingitdb/validator"

	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/manifest"
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
		if db, modes, err = openInGitDB(storagePath); err != nil {
			return nil, fmt.Errorf("%s: %w", manifestPath, err)
		}
		cataloguePath = filepath.Join(storagePath, ".ovdb", "inferred-schema.json")
	default:
		return nil, fmt.Errorf("%s: unknown storage engine %q (supported: sqlite, ingitdb)",
			manifestPath, m.Storage.Engine)
	}

	d, err := core.Open(m, db, modes, cataloguePath)
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
func openInGitDB(dir string) (dal.DB, []schema.Mode, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, nil, fmt.Errorf("failed to create inGitDB directory %s: %w", dir, err)
	}
	db, err := dalgo2ingitdb.NewDatabase(dir, validator.NewCollectionsReader())
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
