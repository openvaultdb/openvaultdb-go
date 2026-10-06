package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
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

const syntheticProviderXML = `<g:Envelope xmlns:g="http://www.gesmes.org/xml/2002-08-01" xmlns="http://www.ecb.int/vocabulary/2002-08-01/eurofxref"><g:subject>Synthetic</g:subject><g:Sender><g:name>Test</g:name></g:Sender><Cube><Cube time="2037-02-03"><Cube currency="AAA" rate="001.23000"/></Cube></Cube></g:Envelope>`

func newExecutionTestServer(t *testing.T, transport http.RoundTripper, enabled bool, options ...Option) (*Server, providerreads.Plan) {
	t.Helper()
	driver, err := dalgo2http.NewDB(dalgo2http.Config{Mode: dalgo2http.ModeLive, Collections: []dalgo2http.Collection{{Name: "daily", URLTemplate: manifest.ECBDailyURL, Decoder: dalgo2http.DecoderECBEuroFXRef, KeyField: "currency", Timeout: time.Second, ClientSideFilter: true}}, Client: &http.Client{Transport: transport}})
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
	if err != nil || right == nil {
		t.Fatal(right, err)
	}
	digest, err := providerreads.RightsDigest(*right)
	if err != nil {
		t.Fatal(err)
	}
	profile := ProviderReadProfile{Collection: "daily", Binding: providerreads.Binding{ProviderSourceID: "provider:synthetic/FxReferenceQuote", RightsSourceID: right.SourceID, ResourceID: "ecb-daily", DefinitionDigest: strings.Repeat("a", 64), DecoderDigest: strings.Repeat("b", 64), RightsDigest: digest}}
	options = append(options, WithSourceRights("synthetic-server", nil))
	if enabled {
		options = append(options, WithProviderReadProfiles(map[string]ProviderReadProfile{"ecb": profile}))
	}
	s, err := NewChecked("test", map[string]*core.Database{"ecb": db}, options...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.CloseSnapshots)
	// This plan is frozen independently of response evidence.
	return s, s.providerPlan(profile, rightsCapture{rights: []license.SourceRight{*right}}, "0123456789abcdef0123456789abcdef")
}

func syntheticExecutionTransport(calls *atomic.Int32) http.RoundTripper {
	return liveHTTPTestTransport(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		if r.Header.Get(ProviderExecutionIDHeader) != "" {
			return nil, fmt.Errorf("client correlation forwarded upstream")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/xml"}}, Body: io.NopCloser(strings.NewReader(syntheticProviderXML))}, nil
	})
}

func decodeExecutionEvidence(t *testing.T, body []byte) (providerreads.Envelope, []license.SourceRight, []string) {
	t.Helper()
	var out struct {
		Evidence json.RawMessage       `json:"providerReads"`
		Rights   []license.SourceRight `json:"sourceRights"`
		Used     []string              `json:"usedSourceIds"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	evidence, err := providerreads.Decode(out.Evidence)
	if err != nil {
		t.Fatal(err)
	}
	return evidence, out.Rights, out.Used
}

func TestProviderExecutionIndependentAdmission(t *testing.T) {
	var calls atomic.Int32
	s, admitted := newExecutionTestServer(t, syntheticExecutionTransport(&calls), true)
	handler := s.Handler()
	for _, route := range []struct{ path, query string }{
		{"/v1/databases/ecb/query", `{"collection":"daily"}`},
		{"/v1/databases/ecb/query", `{"collection":"daily","where":[{"field":"currency","op":"==","value":"ZZZ"}]}`},
		{"/v1/databases/ecb/dtql", "from: {name: daily}\n"},
		{"/v1/databases/ecb/dtql", "from: {name: daily}\nwhere: {op: '==', left: {field: currency}, right: {value: ZZZ}}\n"},
	} {
		for _, method := range []string{"GET", "POST"} {
			t.Run(method+route.path+route.query, func(t *testing.T) {
				path, body := route.path, route.query
				if method == "GET" {
					path += "?q=" + url.QueryEscape(body)
					body = ""
				}
				w := retentionRequest(handler, method, path, body, http.Header{ProviderExecutionIDHeader: {admitted.Execution.ID}})
				if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
					t.Fatalf("response %d %s", w.Code, w.Body)
				}
				evidence, rights, used := decodeExecutionEvidence(t, w.Body.Bytes())
				if err := providerreads.ValidateMetadata(providerreads.Metadata{ProviderReads: &evidence, SourceRights: rights, UsedSourceIDs: used}, admitted, used); err != nil {
					t.Fatal(err)
				}
				wrong := admitted
				wrong.Execution.ID = strings.Repeat("f", 32)
				if providerreads.ValidateMetadata(providerreads.Metadata{ProviderReads: &evidence, SourceRights: rights, UsedSourceIDs: used}, wrong, used) == nil {
					t.Fatal("wrong execution accepted")
				}
				if evidence.Execution != admitted.Execution {
					t.Fatal("client nonce changed executor authority")
				}
			})
		}
	}
	// A legacy caller still gets unpredictable per-call nonces; a strict client
	// cannot validate this response against its independently frozen ID.
	ids := map[string]bool{}
	for range 2 {
		w := retentionRequest(handler, "POST", "/v1/databases/ecb/query", `{"collection":"daily"}`, nil)
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body)
		}
		evidence, rights, used := decodeExecutionEvidence(t, w.Body.Bytes())
		id, err := providerExecutionID(http.Header{ProviderExecutionIDHeader: {evidence.Execution.ID}})
		if err != nil || ids[id] || id == admitted.Execution.ID {
			t.Fatal("invalid or reused legacy nonce", id, err)
		}
		ids[id] = true
		if providerreads.ValidateMetadata(providerreads.Metadata{ProviderReads: &evidence, SourceRights: rights, UsedSourceIDs: used}, admitted, used) == nil {
			t.Fatal("legacy execution accepted by unrelated strict plan")
		}
	}
	if calls.Load() != 10 {
		t.Fatal("unexpected upstream reads", calls.Load())
	}
}

func TestProviderExecutionInvalidHeadersNeverRead(t *testing.T) {
	var calls atomic.Int32
	s, admitted := newExecutionTestServer(t, syntheticExecutionTransport(&calls), true)
	for name, headers := range map[string]http.Header{
		"empty":                {ProviderExecutionIDHeader: {""}},
		"empty-slice":          {ProviderExecutionIDHeader: {}},
		"short":                {ProviderExecutionIDHeader: {strings.Repeat("a", 31)}},
		"long":                 {ProviderExecutionIDHeader: {strings.Repeat("a", 33)}},
		"uppercase":            {ProviderExecutionIDHeader: {strings.ToUpper(admitted.Execution.ID)}},
		"nonhex":               {ProviderExecutionIDHeader: {strings.Repeat("g", 32)}},
		"space":                {ProviderExecutionIDHeader: {" " + admitted.Execution.ID}},
		"coalesced":            {ProviderExecutionIDHeader: {admitted.Execution.ID + "," + admitted.Execution.ID}},
		"duplicate":            {ProviderExecutionIDHeader: {admitted.Execution.ID, admitted.Execution.ID}},
		"mixed-case-duplicate": {ProviderExecutionIDHeader: {admitted.Execution.ID}, "ovdb-execution-id": {admitted.Execution.ID}},
	} {
		t.Run(name, func(t *testing.T) {
			for _, route := range []struct{ path, query string }{{"/v1/databases/ecb/query", `{"collection":"daily"}`}, {"/v1/databases/ecb/dtql", "from: {name: daily}\n"}} {
				w := retentionRequest(s.Handler(), "POST", route.path, route.query, headers)
				if w.Code != 400 || w.Header().Get("Cache-Control") != "no-store" || !strings.Contains(w.Body.String(), `"invalid_execution_id"`) {
					t.Fatalf("response %d %s", w.Code, w.Body)
				}
				if strings.Contains(w.Body.String(), admitted.Execution.ID) {
					t.Fatal("error reflected nonce")
				}
			}
		})
	}
	if calls.Load() != 0 {
		t.Fatal("invalid header reached upstream", calls.Load())
	}
	// This real HTTP round trip proves duplicate field lines are rejected too.
	endpoint := httptest.NewServer(s.Handler())
	t.Cleanup(endpoint.Close)
	req, err := http.NewRequest("POST", endpoint.URL+"/v1/databases/ecb/query", strings.NewReader(`{"collection":"daily"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Add(ProviderExecutionIDHeader, admitted.Execution.ID)
	req.Header.Add(ProviderExecutionIDHeader, admitted.Execution.ID)
	response, err := endpoint.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != 400 || calls.Load() != 0 {
		t.Fatal("wire duplicates reached upstream")
	}
}

func TestProviderExecutionConcurrentIsolation(t *testing.T) {
	var calls atomic.Int32
	s, admitted := newExecutionTestServer(t, syntheticExecutionTransport(&calls), true)
	handler := s.Handler()
	const count = 12
	responses := make([]*httptest.ResponseRecorder, count)
	var wg sync.WaitGroup
	for i := range count {
		wg.Go(func() {
			id := fmt.Sprintf("%032x", i+1)
			responses[i] = retentionRequest(handler, "POST", "/v1/databases/ecb/query", `{"collection":"daily"}`, http.Header{ProviderExecutionIDHeader: {id}})
		})
	}
	wg.Wait()
	observations := map[string]bool{}
	for i, response := range responses {
		if response.Code != 200 {
			t.Fatal(response.Code, response.Body)
		}
		evidence, rights, used := decodeExecutionEvidence(t, response.Body.Bytes())
		plan := admitted
		plan.Execution.ID = fmt.Sprintf("%032x", i+1)
		if err := providerreads.ValidateMetadata(providerreads.Metadata{ProviderReads: &evidence, SourceRights: rights, UsedSourceIDs: used}, plan, used); err != nil {
			t.Fatal(err)
		}
		observationID := evidence.Reads[0].ObservationID
		if observations[observationID] {
			t.Fatal("concurrent observation not bound to execution")
		}
		observations[observationID] = true
	}
	if calls.Load() != count {
		t.Fatal("unexpected concurrent fetch count", calls.Load())
	}
}

func TestProviderExecutionErrorAndLegacyIsolation(t *testing.T) {
	var calls atomic.Int32
	failed := liveHTTPTestTransport(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, fmt.Errorf("synthetic provider failure")
	})
	s, admitted := newExecutionTestServer(t, failed, true)
	response := retentionRequest(s.Handler(), "POST", "/v1/databases/ecb/query", `{"collection":"daily"}`, http.Header{ProviderExecutionIDHeader: {admitted.Execution.ID}})
	if response.Code == 200 || response.Header().Get("Cache-Control") != "no-store" || strings.Contains(response.Body.String(), `"providerReads"`) || strings.Contains(response.Body.String(), admitted.Execution.ID) {
		t.Fatal(response.Code, response.Body)
	}
	if calls.Load() != 1 {
		t.Fatal(calls.Load())
	}
	legacy, _ := newExecutionTestServer(t, syntheticExecutionTransport(&calls), false)
	response = retentionRequest(legacy.Handler(), "POST", "/v1/databases/ecb/query", `{"collection":"daily"}`, http.Header{ProviderExecutionIDHeader: {"malformed"}})
	if response.Code != 200 || strings.Contains(response.Body.String(), `"providerReads"`) {
		t.Fatal("non-profile behavior changed", response.Code, response.Body)
	}
}

func TestProviderExecutionCORSOptIn(t *testing.T) {
	var calls atomic.Int32
	original := ParseCORSOrigins([]string{"https://datatug.app"})
	admitted, _ := newExecutionTestServer(t, syntheticExecutionTransport(&calls), true, WithCORS(original))
	legacy, _ := newExecutionTestServer(t, syntheticExecutionTransport(&calls), false, WithCORS(original))
	for _, tc := range []struct {
		server *Server
		origin string
		allows bool
	}{{admitted, "https://datatug.app", true}, {admitted, "https://evil.example", false}, {legacy, "https://datatug.app", false}} {
		w := retentionRequest(tc.server.Handler(), "OPTIONS", "/v1/databases/ecb/query", "", http.Header{"Origin": {tc.origin}, "Access-Control-Request-Headers": {ProviderExecutionIDHeader}})
		if strings.Contains(w.Header().Get("Access-Control-Allow-Headers"), ProviderExecutionIDHeader) != tc.allows {
			t.Fatal(w.Header())
		}
		if tc.origin != "https://datatug.app" && w.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Fatal("CORS origin expanded")
		}
	}
	if len(original.allowHeaders) != 0 {
		t.Fatal("shared CORS configuration mutated")
	}
	if calls.Load() != 0 {
		t.Fatal("preflight fetched upstream")
	}
}
