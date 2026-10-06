package mount

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo2http"
	"github.com/dal-go/record"
	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/manifest"
)

const httpManifest = `database: {id: ecb, schema_mode: strict, retention: none}
storage:
  engine: http
  http: {profile: ecb-daily/1, collection: daily}
schemas:
  collections:
    daily:
      fields:
        time: {type: string}
        currency: {type: string}
        rate: {type: string}
`

// All rates/codes/dates here are fabricated; these are not recorded ECB bytes.
const httpSyntheticXML = `<g:Envelope xmlns:g="http://www.gesmes.org/xml/2002-08-01" xmlns="http://www.ecb.int/vocabulary/2002-08-01/eurofxref"><g:subject>Synthetic</g:subject><g:Sender><g:name>Test</g:name></g:Sender><Cube><Cube time="2037-02-03"><Cube currency="AAA" rate="001.23000"/><Cube currency="ZZZ" rate="0.00001"/></Cube></Cube></g:Envelope>`

type httpTestTransport func(*http.Request) (*http.Response, error)

func (f httpTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func httpTestMount(t *testing.T, transport httpTestTransport) *core.Database {
	t.Helper()
	m, err := manifest.Parse([]byte(httpManifest))
	if err != nil {
		t.Fatal(err)
	}
	driver, modes, err := openHTTP(m, &http.Client{Transport: transport})
	if err != nil {
		t.Fatal(err)
	}
	db, err := core.Open(m, driver, modes, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestHTTPMountNativeLiveQuery(t *testing.T) {
	calls := 0
	db := httpTestMount(t, func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Method != "GET" || r.URL.String() != manifest.ECBDailyURL || r.Header.Get("Cache-Control") != "no-store, no-cache" {
			t.Fatalf("request escaped the fixed no-store profile: %v", r)
		}
		if _, ok := r.Context().Deadline(); !ok {
			t.Fatal("unbounded upstream request")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/xml"}, "Last-Modified": {"synthetic update"}}, Body: io.NopCloser(strings.NewReader(httpSyntheticXML))}, nil
	})
	if !db.NoRetention() || !db.ReadOnlyHTTP() || !db.CanQuery() || db.InferredSnapshot() != nil {
		t.Fatal("incorrect mount capabilities")
	}
	collections, err := db.Collections(context.Background())
	if err != nil || !reflect.DeepEqual(collections, []string{"daily"}) {
		t.Fatalf("collections: %v, %v", collections, err)
	}
	var observations []dalgo2http.Provenance
	ctx := dalgo2http.ContextWithProvenanceObserver(context.Background(), func(p dalgo2http.Provenance) { observations = append(observations, p) })
	rows, err := db.Execute(ctx, core.Query{Collection: "daily", Where: []core.Filter{{Field: "currency", Op: "==", Value: "AAA"}}})
	if err != nil || len(rows) != 1 || rows[0].Data["rate"] != "001.23000" || rows[0].Data["time"] != "2037-02-03" || len(rows[0].Data) != 3 {
		t.Fatalf("lexical native query: %v, %v", rows, err)
	}
	if len(observations) != 1 || observations[0].SHA256 != fmt.Sprintf("%x", sha256.Sum256([]byte(httpSyntheticXML))) || observations[0].Bytes != len(httpSyntheticXML) || observations[0].UpstreamURL != manifest.ECBDailyURL || observations[0].ReferenceDate != "2037-02-03" || observations[0].BaseCurrency != "EUR" || observations[0].Source != dalgo2http.SourceLive {
		t.Fatalf("observation: %+v", observations)
	}
	query, _, err := core.ParseDTQL([]byte("from: {name: daily}\ncolumns: [{field: rate}]\n"))
	if err != nil {
		t.Fatal(err)
	}
	projected, err := db.ExecuteDTQLQuery(ctx, query)
	if err != nil || len(projected) != 2 || projected[0].Data["time"] != nil || projected[0].Data["rate"] != "001.23000" {
		t.Fatalf("projection: %v, %v", projected, err)
	}
	_, err = db.Execute(ctx, core.Query{Collection: "daily", Where: []core.Filter{{Field: "currency", Op: "==", Value: "QQQ"}}})
	if err != nil || len(observations) != 3 || calls != 3 {
		t.Fatalf("empty-result read lost evidence or cached rows: %d, %d, %v", calls, len(observations), err)
	}
}

func TestHTTPMountRefusesUnsupportedBeforeFetch(t *testing.T) {
	db := httpTestMount(t, func(*http.Request) (*http.Response, error) {
		t.Fatal("unsupported request fetched upstream")
		return nil, nil
	})
	ctx := context.Background()
	if _, err := db.Get(ctx, record.NewKeyWithID("daily", "AAA")); !errors.Is(err, dal.ErrNotSupported) {
		t.Fatal(err)
	}
	if _, err := db.Exists(ctx, record.NewKeyWithID("daily", "AAA")); !errors.Is(err, dal.ErrNotSupported) {
		t.Fatal(err)
	}
	if _, err := db.Apply(ctx, []core.Op{{Op: "set", Key: record.NewKeyWithID("daily", "AAA"), Data: map[string]any{"rate": "1"}}}, ""); !errors.Is(err, dal.ErrNotSupported) {
		t.Fatal(err)
	}
	if _, err := db.Execute(ctx, core.Query{Collection: "daily", OrderBy: []core.OrderBy{{Field: "currency"}}}); !errors.Is(err, dal.ErrNotSupported) {
		t.Fatal(err)
	}
	if _, err := db.Execute(ctx, core.Query{Collection: "unknown"}); !errors.Is(err, core.ErrNotFound) {
		t.Fatal(err)
	}
	for _, suffix := range []string{"offset: 1\n", "order_by: [{field: currency}]\n"} {
		q, _, err := core.ParseDTQL([]byte("from: {name: daily}\n" + suffix))
		if err != nil {
			continue
		} // an unsupported syntax is also a pre-read refusal
		if _, err = db.ExecuteDTQLQuery(ctx, q); err == nil {
			t.Fatal("unsupported DTQL accepted")
		}
	}
	fields, err := db.Executor().(dal.JoinFieldsProvider).JoinFields(ctx, dal.NewRootCollectionRef("daily", ""))
	if err != nil || !reflect.DeepEqual(fields, []string{"currency", "rate", "time"}) {
		t.Fatalf("phantom key column: %v, %v", fields, err)
	}
	// Exposed manifest mutation cannot grant writes, point reads or retention.
	db.Manifest.Storage.Engine = "sqlite"
	db.Manifest.Database.Retention = ""
	if !db.ReadOnlyHTTP() || !db.NoRetention() {
		t.Fatal("mutable capability")
	}
}

func TestHTTPMountNoFilesAndFailureDoesNotExposeSourceValues(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ecb.yaml")
	if err := os.WriteFile(path, []byte(httpManifest), 0600); err != nil {
		t.Fatal(err)
	}
	db, err := FileWithOptions(path, Options{CatalogueDir: filepath.Join(dir, "catalogue")})
	if err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	files, err := os.ReadDir(dir)
	if err != nil || len(files) != 1 {
		t.Fatalf("mount retained files: %v, %v", files, err)
	}
	db = httpTestMount(t, func(*http.Request) (*http.Response, error) { return nil, errors.New("synthetic-private-value") })
	if _, err = db.Execute(context.Background(), core.Query{Collection: "daily"}); !errors.Is(err, core.ErrDatabaseUnreachable) || strings.Contains(err.Error(), "synthetic-private-value") {
		t.Fatalf("unsanitized failure: %v", err)
	}
}

func TestHTTPManifestClosedProfile(t *testing.T) {
	for name, bad := range map[string]string{
		"url":     strings.Replace(httpManifest, "profile: ecb-daily/1", "url: https://example.org", 1),
		"profile": strings.Replace(httpManifest, "ecb-daily/1", "arbitrary", 1),
		"cache":   strings.Replace(httpManifest, "retention: none", "cache_ttl: 1h", 1),
		"path":    strings.Replace(httpManifest, "engine: http", "engine: http\n  path: data.xml", 1),
		"mode":    strings.Replace(httpManifest, "strict", "schemaless", 1),
		"field":   strings.Replace(httpManifest, "rate: {type: string}", "rate: {type: number}", 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := manifest.Parse([]byte(bad)); err == nil {
				t.Fatal("invalid profile accepted")
			}
		})
	}
}
