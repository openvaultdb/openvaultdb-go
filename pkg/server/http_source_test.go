package server

import (
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/dal-go/dalgo2http"
	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/license"
	"github.com/openvaultdb/openvaultdb-go/pkg/manifest"
	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
)

type liveHTTPTestTransport func(*http.Request) (*http.Response, error)

func (f liveHTTPTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestLiveHTTPSourceEndpoints(t *testing.T) {
	calls := 0
	// Fabricated document: no real source bytes are stored by this test.
	body := `<g:Envelope xmlns:g="http://www.gesmes.org/xml/2002-08-01" xmlns="http://www.ecb.int/vocabulary/2002-08-01/eurofxref"><g:subject>Synthetic</g:subject><g:Sender><g:name>Test</g:name></g:Sender><Cube><Cube time="2037-02-03"><Cube currency="AAA" rate="001.23000"/></Cube></Cube></g:Envelope>`
	driver, err := dalgo2http.NewDB(dalgo2http.Config{Mode: dalgo2http.ModeLive,
		Collections: []dalgo2http.Collection{{Name: "daily", URLTemplate: manifest.ECBDailyURL, Decoder: dalgo2http.DecoderECBEuroFXRef, KeyField: "currency", Timeout: 10 * time.Second, ClientSideFilter: true}},
		Client: &http.Client{Transport: liveHTTPTestTransport(func(r *http.Request) (*http.Response, error) {
			calls++
			if r.URL.String() != manifest.ECBDailyURL {
				t.Fatal("unexpected upstream")
			}
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/xml"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
		})},
	})
	if err != nil {
		t.Fatal(err)
	}
	m := &manifest.Manifest{Database: manifest.Database{ID: "ecb", SchemaMode: schema.ModeStrict, License: &license.Declaration{URL: "https://example.org/synthetic-terms"}}, Storage: manifest.Storage{Engine: "http", HTTP: &manifest.HTTPOptions{Profile: manifest.HTTPProfileECBDaily, Collection: "daily"}}, Schemas: &schema.Schemas{Collections: map[string]schema.Collection{"daily": {Fields: map[string]schema.Field{"time": {Type: schema.TypeString}, "currency": {Type: schema.TypeString}, "rate": {Type: schema.TypeString}}}}}}
	db, err := core.Open(m, driver, []schema.Mode{schema.ModeStrict}, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s := New("test", map[string]*core.Database{"ecb": db}, WithSourceRights("synthetic-server", nil))
	t.Cleanup(s.CloseSnapshots)
	handler := s.Handler()
	query := "from: {name: daily}\n"
	for _, route := range []struct{ method, path, body string }{
		{"POST", "/v1/databases/ecb/query", `{"collection":"daily"}`},
		{"GET", "/v1/databases/ecb/query?q=" + url.QueryEscape(`{"collection":"daily"}`), ""},
		{"POST", "/v1/databases/ecb/dtql", query},
		{"GET", "/v1/databases/ecb/dtql?q=" + url.QueryEscape(query), ""},
	} {
		w := retentionRequest(handler, route.method, route.path, route.body, nil)
		if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" || !strings.Contains(w.Body.String(), "001.23000") || !strings.Contains(w.Body.String(), "sourceRights") || !strings.Contains(w.Body.String(), "usedSourceIds") {
			t.Fatalf("%s: %d %v %s", route.path, w.Code, w.Header(), w.Body)
		}
	}
	if calls != 4 {
		t.Fatalf("read cache or duplicated fetch: %d", calls)
	}
	for _, path := range []string{"/v1/databases/ecb", "/.well-known/openvaultdb"} {
		w := retentionRequest(handler, "GET", path, "", nil)
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"read":false`) || !strings.Contains(w.Body.String(), `"write":false`) || !strings.Contains(w.Body.String(), `"query":true`) || !strings.Contains(w.Body.String(), `"joins":false`) {
			t.Fatalf("metadata: %d %s", w.Code, w.Body)
		}
	}
	for _, route := range []struct{ method, path, body string }{
		{"GET", "/v1/databases/ecb/records/daily/AAA", ""},
		{"HEAD", "/v1/databases/ecb/records/daily/AAA", ""},
		{"GET", "/v1/databases/ecb/read?key=daily/AAA", ""},
		{"POST", "/v1/databases/ecb/query", `{"collection":"daily","orderBy":[{"field":"rate"}]}`},
		{"PUT", "/v1/databases/ecb/records/daily/AAA", `{"data":{"rate":"2"}}`},
	} {
		w := retentionRequest(handler, route.method, route.path, route.body, nil)
		if w.Code != 501 || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("unsupported %s %s: %d %v %s", route.method, route.path, w.Code, w.Header(), w.Body)
		}
	}
	t.Run("read-only refusals", func(t *testing.T) {
		readOnly := New("test", map[string]*core.Database{"ecb": db}, WithReadOnly(true), WithSourceRights("synthetic-server", nil))
		t.Cleanup(readOnly.CloseSnapshots)
		before := calls
		for _, route := range []struct{ method, path, body string }{
			{"PUT", "/v1/databases/ecb/records/daily/AAA", `{"data":{"rate":"2"}}`},
			{"PATCH", "/v1/databases/ecb/records/daily/AAA", `{"updates":[]}`},
			{"DELETE", "/v1/databases/ecb/records/daily/AAA", ""},
			{"POST", "/v1/databases/ecb/records/daily/AAA", `{"data":{"rate":"2"}}`},
			{"POST", "/v1/databases/ecb/batch", `{"ops":[]}`},
		} {
			w := retentionRequest(readOnly.Handler(), route.method, route.path, route.body, nil)
			if w.Code != 403 || w.Header().Get("Cache-Control") != "no-store" || !strings.Contains(w.Body.String(), `"code":"read_only"`) {
				t.Fatalf("%s %s: %d %v %s", route.method, route.path, w.Code, w.Header(), w.Body)
			}
		}
		files, err := os.ReadDir(readOnly.snapshotDir)
		if err != nil || calls != before || readOnly.snapshotSlots != 0 || len(readOnly.snapshots) != 0 || len(files) != 0 {
			t.Fatalf("read-only refusal read or retained source: calls=%d slots=%d files=%v err=%v", calls-before, readOnly.snapshotSlots, files, err)
		}
	})
	for _, header := range []http.Header{{"OVDB-Page-Size": {"1"}}, {"OVDB-Page-Token": {""}}, {"OVDB-Page-Close": {"true", "true"}}} {
		assertRetentionRefusal(t, retentionRequest(handler, "POST", "/v1/databases/ecb/dtql", query, header))
		assertRetentionRefusal(t, retentionRequest(handler, "POST", "/v1/dtql", "from: {database: ecb, name: daily}\n", header))
	}
	body = "<malformed synthetic-private-value>"
	w := retentionRequest(handler, "POST", "/v1/databases/ecb/query", `{"collection":"daily"}`, nil)
	if w.Code != 503 || w.Header().Get("Cache-Control") != "no-store" || strings.Contains(w.Body.String(), "synthetic-private-value") {
		t.Fatalf("malformed source error: %d %v %s", w.Code, w.Header(), w.Body)
	}
	files, err := os.ReadDir(s.snapshotDir)
	if err != nil || calls != 5 || s.snapshotSlots != 0 || len(s.snapshots) != 0 || len(files) != 0 {
		t.Fatalf("refusal retained/read source: calls=%d slots=%d files=%v err=%v", calls, s.snapshotSlots, files, err)
	}
}
