package server

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dal-go/dalgo2http"
	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/license"
	"github.com/openvaultdb/openvaultdb-go/pkg/manifest"
	"github.com/openvaultdb/openvaultdb-go/pkg/providerreads"
	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
)

func TestProviderObservedHTTPResponses(t *testing.T) {
	// Fabricated future labels/code, no real source body or rows retained.
	body := `<g:Envelope xmlns:g="http://www.gesmes.org/xml/2002-08-01" xmlns="http://www.ecb.int/vocabulary/2002-08-01/eurofxref"><g:subject>Synthetic</g:subject><g:Sender><g:name>Test</g:name></g:Sender><Cube><Cube time="2037-02-03"><Cube currency="AAA" rate="001.23000"/></Cube></Cube></g:Envelope>`
	calls := 0
	driver, err := dalgo2http.NewDB(dalgo2http.Config{Mode: dalgo2http.ModeLive, Collections: []dalgo2http.Collection{{Name: "daily", URLTemplate: manifest.ECBDailyURL, Decoder: dalgo2http.DecoderECBEuroFXRef, KeyField: "currency", Timeout: 10 * time.Second, ClientSideFilter: true}}, Client: &http.Client{Transport: liveHTTPTestTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.String() != manifest.ECBDailyURL || r.Header.Get("Cache-Control") != "no-store, no-cache" {
			t.Fatal("unexpected request")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/xml"}, "Last-Modified": {"Tue, 03 Feb 2037 12:00:00 GMT"}, "Etag": {`"fabricated"`}}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}})
	if err != nil {
		t.Fatal(err)
	}
	m := &manifest.Manifest{Database: manifest.Database{ID: "ecb", SchemaMode: schema.ModeStrict, License: &license.Declaration{URL: "https://example.org/synthetic-terms#rights", Text: "Synthetic\nmultiline\tterms"}}, Storage: manifest.Storage{Engine: "http", HTTP: &manifest.HTTPOptions{Profile: manifest.HTTPProfileECBDaily, Collection: "daily"}}, Schemas: &schema.Schemas{Collections: map[string]schema.Collection{"daily": {Fields: map[string]schema.Field{"time": {Type: schema.TypeString}, "currency": {Type: schema.TypeString}, "rate": {Type: schema.TypeString}}}}}}
	db, err := core.Open(m, driver, []schema.Mode{schema.ModeStrict}, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	right, err := db.SourceRight("synthetic-server", nil, "daily")
	if err != nil || right == nil {
		t.Fatal(right, err)
	}
	right.EvidenceOrigin = "publisher-definition-verified"
	right.Pins = []license.Pin{{Role: "provider", Repository: "https://github.com/synthetic/provider", Revision: strings.Repeat("c", 40), Path: "ovdb.yaml", SHA256: strings.Repeat("a", 64), Bytes: 42}}
	right.Attribution = &license.Notice{Text: "Synthetic provider", URL: "https://example.org/"}
	right.FreeSource = &license.Notice{Text: "Synthetic original is free", URL: manifest.ECBDailyURL}
	right.Transformations = []string{"Synthetic XML restructured into rows"}
	rd, err := providerreads.RightsDigest(*right)
	if err != nil {
		t.Fatal(err)
	}
	profile := ProviderReadProfile{Collection: "daily", SourceRight: right, Binding: providerreads.Binding{ProviderSourceID: "provider:synthetic/FxReferenceQuote", RightsSourceID: right.SourceID, ResourceID: "ecb-daily", DefinitionDigest: strings.Repeat("a", 64), DecoderDigest: strings.Repeat("b", 64), RightsDigest: rd}}
	profiles := map[string]ProviderReadProfile{"ecb": profile}
	s, err := NewChecked("test", map[string]*core.Database{"ecb": db}, WithSourceRights("synthetic-server", nil), WithProviderReadProfiles(profiles))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.CloseSnapshots)
	profiles["ecb"] = ProviderReadProfile{} // caller cannot race a frozen profile.
	for _, route := range []struct {
		path, query string
		empty       bool
	}{
		{"/v1/databases/ecb/query", `{"collection":"daily"}`, false},
		{"/v1/databases/ecb/query", `{"collection":"daily","where":[{"field":"currency","op":"==","value":"ZZZ"}]}`, true},
		{"/v1/databases/ecb/dtql", "from: {name: daily}\n", false},
		{"/v1/databases/ecb/dtql", "from: {name: daily}\nwhere: {op: '==', left: {field: currency}, right: {value: ZZZ}}\n", true},
	} {
		before := calls
		w := retentionRequest(s.Handler(), "POST", route.path, route.query, nil)
		if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("response %d %s", w.Code, w.Body)
		}
		var result struct {
			SourceRights []license.SourceRight `json:"sourceRights"`
			Used         []string              `json:"usedSourceIds"`
			Evidence     json.RawMessage       `json:"providerReads"`
			Records      []any                 `json:"records"`
		}
		if err = json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		evidence, err := providerreads.Decode(result.Evidence)
		if err != nil {
			t.Fatal(err)
		}
		plan := s.providerPlan(profile, rightsCapture{rights: []license.SourceRight{*right}}, evidence.Execution.ID)
		if err = providerreads.Validate(evidence, plan, result.Used); err != nil {
			t.Fatal(err)
		}
		if !equalProviderRights(result.SourceRights, []license.SourceRight{*right}) || calls != before+1 || len(evidence.Reads) != 1 || len(evidence.Usage) != 1 {
			t.Fatal("read inventory mismatch")
		}
		read := evidence.Reads[0]
		expected := sha256.Sum256([]byte(body))
		if read.SHA256 != hexHash(expected) || read.Bytes != int64(len(body)) || read.ReferenceDate != "2037-02-03" || read.ETag != `"fabricated"` || read.LastModified != "Tue, 03 Feb 2037 12:00:00 GMT" {
			t.Fatal(read)
		}
		if route.empty && len(result.Records) != 0 {
			t.Fatal("expected zero-row result", result.Records)
		}
		if strings.Contains(string(result.Evidence), "001.23000") || strings.Contains(string(result.Evidence), "Envelope") {
			t.Fatal("evidence contains source bytes")
		}
	}
	// Missing observations refuse before output; unsupported joins do not read.
	before := calls
	w := retentionRequest(s.Handler(), "POST", "/v1/databases/ecb/dtql", "from: {name: daily, as: a}\njoins: [{from: {name: daily, as: b}, on: {left: a.currency, right: b.currency}}]\n", nil)
	if w.Code == 200 || calls != before {
		t.Fatalf("join read: %d %s", w.Code, w.Body)
	}
	broken := profile
	broken.Binding.RightsDigest = strings.Repeat("c", 64)
	bad, err := NewChecked("test", map[string]*core.Database{"ecb": db}, WithSourceRights("synthetic-server", nil), WithProviderReadProfiles(map[string]ProviderReadProfile{"ecb": broken}))
	if bad != nil {
		bad.CloseSnapshots()
	}
	if err == nil {
		t.Fatal("changed rights admitted")
	}
}
func equalProviderRights(a, b any) bool {
	x, e := providerreads.Canonical(a)
	y, f := providerreads.Canonical(b)
	return e == nil && f == nil && string(x) == string(y)
}
func hexHash(h [32]byte) string {
	const digits = "0123456789abcdef"
	b := make([]byte, 64)
	for i, v := range h {
		b[2*i] = digits[v>>4]
		b[2*i+1] = digits[v&15]
	}
	return string(b)
}

func TestProviderProfileSharedAliasLifetime(t *testing.T) {
	driver, err := dalgo2http.NewDB(dalgo2http.Config{Mode: dalgo2http.ModeLive, Collections: []dalgo2http.Collection{{Name: "daily", URLTemplate: manifest.ECBDailyURL, Decoder: dalgo2http.DecoderECBEuroFXRef, KeyField: "currency", Timeout: 10 * time.Second}}})
	if err != nil {
		t.Fatal(err)
	}
	m := &manifest.Manifest{Database: manifest.Database{ID: "ecb", SchemaMode: schema.ModeStrict, License: &license.Declaration{URL: "https://example.org/synthetic-terms"}}, Storage: manifest.Storage{Engine: "http", HTTP: &manifest.HTTPOptions{Profile: manifest.HTTPProfileECBDaily, Collection: "daily"}}, Schemas: &schema.Schemas{Collections: map[string]schema.Collection{"daily": {Fields: map[string]schema.Field{"time": {Type: schema.TypeString}, "currency": {Type: schema.TypeString}, "rate": {Type: schema.TypeString}}}}}}
	db, err := core.Open(m, driver, []schema.Mode{schema.ModeStrict}, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	right, err := db.SourceRight("synthetic-server", nil, "daily")
	if err != nil {
		t.Fatal(err)
	}
	digest, err := providerreads.RightsDigest(*right)
	if err != nil {
		t.Fatal(err)
	}
	profile := ProviderReadProfile{Collection: "daily", Binding: providerreads.Binding{ProviderSourceID: "provider:synthetic/FxReferenceQuote", RightsSourceID: right.SourceID, ResourceID: "ecb-daily", DefinitionDigest: strings.Repeat("a", 64), DecoderDigest: strings.Repeat("b", 64), RightsDigest: digest}}
	s, err := NewChecked("test", map[string]*core.Database{"alias": db, "second": db}, WithSourceRights("synthetic-server", nil), WithProviderReadProfiles(map[string]ProviderReadProfile{"alias": profile}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.CloseSnapshots)
	var closeCount atomic.Int32
	closed := make(chan struct{})
	db.OnClose(func() error {
		if closeCount.Add(1) == 1 {
			close(closed)
		}
		return nil
	})
	first, second := &leases{}, &leases{}
	for id, held := range map[string]*leases{"alias": first, "second": second} {
		r := httptest.NewRequest("GET", "/v1/databases/"+id, nil)
		r = r.WithContext(context.WithValue(r.Context(), leasesKey{}, held))
		if s.acquire(r, id) != db {
			t.Fatal("missing shared lease")
		}
		t.Cleanup(held.release)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err = s.UnmountContext(canceled, "alias"); err != nil {
		t.Fatal("nonfinal removal waited", err)
	}
	if closeCount.Load() != 0 || s.getDB("alias") != nil || s.getDB("second") != db {
		t.Fatal("nonfinal removal closed or unrouted remaining alias")
	}
	// New leases through the remaining alias must share the retained waitgroup.
	third := &leases{}
	r := httptest.NewRequest("GET", "/v1/databases/second", nil)
	r = r.WithContext(context.WithValue(r.Context(), leasesKey{}, third))
	if s.acquire(r, "second") != db {
		t.Fatal("remaining alias cannot lease")
	}
	t.Cleanup(third.release)
	meta := retentionRequest(s.Handler(), "GET", "/v1/databases/second", "", nil)
	if meta.Code != 200 || !strings.Contains(meta.Body.String(), providerreads.Format) {
		t.Fatalf("remaining admitted metadata: %d %s", meta.Code, meta.Body)
	}
	if err = s.UnmountContext(canceled, "second"); !errors.Is(err, context.Canceled) {
		t.Fatal("final removal did not respect held leases", err)
	}
	if closeCount.Load() != 0 || s.getDB("second") != nil {
		t.Fatal("final removal closed before leases drained")
	}
	if err = s.Mount(db); err == nil {
		t.Fatal("retiring provider pointer remounted under native ID")
	}
	first.release()
	second.release()
	if closeCount.Load() != 0 {
		t.Fatal("new remaining-alias lease lost its shared waitgroup")
	}
	third.release()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("final lease release did not close")
	}
	if closeCount.Load() != 1 {
		t.Fatal("close not exactly once")
	}
	if err = s.Mount(db); err == nil {
		t.Fatal("retired provider pointer remounted")
	}
}
