package mount

import (
	"fmt"
	"os"
	"sort"

	"github.com/dal-go/dalgo/dal"
	dalgo2github "github.com/ingitdb/dalgo2ingitdb4github"
	"github.com/ingitdb/ingitdb-go/ingitdb"

	"github.com/openvaultdb/openvaultdb-go/pkg/manifest"
	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
)

// openInGitDBGitHub opens the GitHub backend of the inGitDB engine: records
// are written directly to a GitHub repository over the API (no local working
// tree), one commit per write batch via the GitHub Git tree API. The access
// token comes from the environment variable named by
// storage.ingitdb.github.token_env (default OVDB_GITHUB_TOKEN); manifests
// never carry secrets (see docs/threat-model.md).
//
// The collection layout is described by an in-memory inGitDB Definition built
// from the manifest's declared schemas, so this backend supports strict and
// partial modes. Schemaless (collections discovered on write) needs the
// GitHub driver to grow the definition on the fly — a follow-up.
func openInGitDBGitHub(m *manifest.Manifest) (dal.DB, []schema.Mode, error) {
	gh := m.Storage.InGitDB.GitHub
	tokenEnv := gh.TokenEnvVar()
	token := os.Getenv(tokenEnv)
	if token == "" {
		return nil, nil, fmt.Errorf("GitHub token not set: expected a contents:write token in $%s", tokenEnv)
	}
	if m.Schemas == nil || len(m.Schemas.Collections) == 0 {
		return nil, nil, fmt.Errorf("the inGitDB github backend requires declared schemas.collections (strict or partial mode)")
	}

	def := buildInGitDBDefinition(m.Schemas)
	cfg := dalgo2github.Config{
		Owner:      gh.Owner,
		Repo:       gh.Repo,
		Ref:        gh.GitRef(),
		Token:      token,
		APIBaseURL: gh.APIBaseURL,
	}
	db, err := dalgo2github.NewBatchingGitHubDB(cfg, def, "ovdb: write")
	if err != nil {
		return nil, nil, fmt.Errorf("failed to open GitHub-backed inGitDB %s/%s@%s: %w",
			gh.Owner, gh.Repo, gh.GitRef(), err)
	}
	return db, []schema.Mode{schema.ModeStrict, schema.ModePartial}, nil
}

// buildInGitDBDefinition constructs an in-memory inGitDB Definition from the
// manifest's declared collections: one root collection per entry, stored as
// one YAML file per record under "<collection>/". Field types populate the
// column definitions so the driver encodes/decodes faithfully.
func buildInGitDBDefinition(schemas *schema.Schemas) *ingitdb.Definition {
	def := &ingitdb.Definition{Collections: map[string]*ingitdb.CollectionDef{}}
	for name, col := range schemas.Collections {
		columns := make(map[string]*ingitdb.ColumnDef, len(col.Fields))
		order := make([]string, 0, len(col.Fields))
		for field := range col.Fields {
			order = append(order, field)
		}
		sort.Strings(order)
		for _, field := range order {
			columns[field] = &ingitdb.ColumnDef{
				Type:     ingitdbColumnType(col.Fields[field].Type),
				Required: col.Fields[field].Required,
			}
		}
		def.Collections[name] = &ingitdb.CollectionDef{
			ID:      name,
			DirPath: name,
			RecordFile: &ingitdb.RecordFileDef{
				RecordType: ingitdb.SingleRecord,
				Format:     ingitdb.RecordFormatYAML,
				Name:       "{key}.yaml",
			},
			Columns:      columns,
			ColumnsOrder: order,
		}
	}
	return def
}

func ingitdbColumnType(t schema.FieldType) ingitdb.ColumnType {
	switch t {
	case schema.TypeString:
		return ingitdb.ColumnTypeString
	case schema.TypeNumber:
		return ingitdb.ColumnTypeFloat
	case schema.TypeInteger:
		return ingitdb.ColumnTypeInt
	case schema.TypeBoolean:
		return ingitdb.ColumnTypeBool
	default: // number/array/object/any — stored as-is in YAML
		return ingitdb.ColumnTypeAny
	}
}
