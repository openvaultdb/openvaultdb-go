package server_test

import (
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/mount"
	"github.com/openvaultdb/openvaultdb-go/pkg/server/servertest"
)

// The demo datasets, loaded into a real PostgreSQL server, read through the
// HTTP API collection by collection. It runs only when OVDB_TEST_DEMODB_POSTGRES_DSN
// is a postgres:// URL of a server that holds one database per dataset, each with a
// schema of the dataset's name (the demodb.dev layout); the database name of the
// URL is replaced by each dataset's. OVDB_TEST_DEMODB_DATASETS lists the datasets,
// comma-separated (default: all six). It is not part of the CI PostgreSQL job,
// which has no demo data: the job proves the same rules on its own fixtures.
const (
	demoDBDSNEnv      = "OVDB_TEST_DEMODB_POSTGRES_DSN"
	demoDBDatasetsEnv = "OVDB_TEST_DEMODB_DATASETS"
)

func TestNativePostgresDemoDatasets_EveryCollectionAnswers(t *testing.T) {
	base := os.Getenv(demoDBDSNEnv)
	if base == "" {
		t.Skipf("%s is not set: no PostgreSQL server with the demo datasets", demoDBDSNEnv)
	}
	datasets := []string{"chinook", "northwind", "pubs", "sakila", "adventureworks", "employees"}
	if listed := os.Getenv(demoDBDatasetsEnv); listed != "" {
		datasets = strings.Split(listed, ",")
	}
	for _, dataset := range datasets {
		t.Run(dataset, func(t *testing.T) {
			u, err := url.Parse(base)
			if err != nil {
				t.Fatal(err)
			}
			u.Path = "/" + dataset
			envName := "OVDB_TEST_DEMODB_" + strings.ToUpper(dataset) + "_DSN"
			t.Setenv(envName, u.String())
			path := filepath.Join(t.TempDir(), dataset+".yaml")
			id := dataset + "-postgresql"
			manifest := "database: {id: " + id + ", schema_mode: strict}\nstorage: {engine: postgres, postgres: {dsn_env: " + envName + ", read_only: true}}\n"
			if err := os.WriteFile(path, []byte(manifest), 0o600); err != nil {
				t.Fatal(err)
			}
			db, err := mount.FileWithOptions(path, mount.Options{
				AllowInsecureNativePostgresTransport: u.Query().Get("sslmode") == "disable",
				// The role the public demo connects as must be read-only, as the cloud requires.
				RequireReadOnlyNativePostgresRole: true,
			})
			if err != nil {
				t.Fatalf("mount %s: %v", dataset, err)
			}
			t.Cleanup(func() { _ = db.Close() })
			baseURL, logs := pgITServeLogged(t, map[string]*core.Database{id: db})
			report := servertest.RequireEveryCollection(t, t.Context(), baseURL, id, servertest.Options{})
			if dataset == "chinook" {
				// The demo's catalogue keys: Track has three, and Album's page lists Track.
				track, _ := db.ResolveNativePostgresCollection("chinook", "Track")
				if keys, err := db.CollectionForeignKeys(t.Context(), track); err != nil || len(keys) != 3 {
					t.Errorf("Track foreign keys = %d, %v; want 3", len(keys), err)
				}
				album, _ := db.ResolveNativePostgresCollection("chinook", "Album")
				page := relHTTPDo(t, baseURL, http.MethodGet, "/ovdb/dbs/"+id+"/collections/"+url.PathEscape(album), "", "", nil)
				if page.status != http.StatusOK || !strings.Contains(page.raw, ">chinook.Track</a>.<code>AlbumId</code>") {
					t.Errorf("Album page does not list Track among the collections that reference it: %d", page.status)
				}
			}
			if len(report.Failures()) > 0 {
				t.Logf("server log:\n%s", logs)
			}
		})
	}
}
