package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/dal-go/dalgo2http"
	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/license"
	"github.com/openvaultdb/openvaultdb-go/pkg/manifest"
	"github.com/openvaultdb/openvaultdb-go/pkg/providerreads"
	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
)

type providerRequestBody struct {
	io.Reader
	closed *int
}

func (b providerRequestBody) Close() error { *b.closed++; return nil }

func providerRequestFixture(t *testing.T, restricted bool) (*Server, ProviderReadProfile, *int, *int) {
	t.Helper()
	calls, closed := new(int), new(int)
	var rows strings.Builder
	for i := 0; i < 60; i++ {
		fmt.Fprintf(&rows, `<Cube currency="A%c%c" rate="001.23000"/>`, 'A'+i/26, 'A'+i%26)
	}
	body := `<g:Envelope xmlns:g="http://www.gesmes.org/xml/2002-08-01" xmlns="http://www.ecb.int/vocabulary/2002-08-01/eurofxref"><g:subject>Synthetic</g:subject><g:Sender><g:name>Test</g:name></g:Sender><Cube><Cube time="2037-02-03">` + rows.String() + `</Cube></Cube></g:Envelope>`
	driver, err := dalgo2http.NewDB(dalgo2http.Config{Mode: dalgo2http.ModeLive, Collections: []dalgo2http.Collection{{Name: "daily", URLTemplate: manifest.ECBDailyURL, Decoder: dalgo2http.DecoderECBEuroFXRef, KeyField: "currency", Timeout: time.Second, ClientSideFilter: true}}, Client: &http.Client{Transport: liveHTTPTestTransport(func(r *http.Request) (*http.Response, error) {
		*calls++ // positive tests deliberately exercise this synthetic trap
		if r.Method != "GET" || r.URL.String() != manifest.ECBDailyURL || r.Header.Get("Cache-Control") != "no-store, no-cache" {
			t.Fatal("unexpected synthetic upstream request")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/xml"}}, Body: providerRequestBody{Reader: strings.NewReader(body), closed: closed}}, nil
	})}})
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
	profile := ProviderReadProfile{Collection: "daily", Binding: providerreads.Binding{ProviderSourceID: "provider:ecb/FxReferenceQuote", RightsSourceID: right.SourceID, ResourceID: "ecb-daily", DefinitionDigest: strings.Repeat("a", 64), DecoderDigest: strings.Repeat("b", 64), RightsDigest: digest}}
	if restricted {
		profile.RequestProfile = ECBPublicFreeRequestProfile
	}
	profiles := map[string]ProviderReadProfile{"alias": profile}
	s, err := NewChecked("test", map[string]*core.Database{"ecb": db, "alias": db}, WithSourceRights("synthetic-server", nil), WithProviderReadProfiles(profiles))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.CloseSnapshots)
	profiles["alias"] = ProviderReadProfile{} // marker is detached and binds both mount IDs
	return s, profile, calls, closed
}

func TestProviderRequestPositiveControls(t *testing.T) {
	s, profile, calls, closed := providerRequestFixture(t, true)
	for _, tc := range []struct {
		name, route, body string
		rows              int
		projected         bool
	}{
		{"json-limit-one", "/query", `{"collection":"daily","limit":1}`, 1, false},
		{"json-limit-fifty", "/query", `{"collection":"daily","limit":50}`, 50, false},
		{"json-native-in", "/query", `{"collection":"daily","limit":50,"where":[{"field":"currency","op":"in","value":["AAA","AAB"]}]}`, 2, false},
		{"dtql-native", "/dtql", "from: {name: daily}\nlimit: 50\n", 50, false},
		{"projection", "/dtql", "from: {name: daily}\ncolumns: [{field: rate}]\nlimit: 1\n", 1, true},
		{"filter-group", "/dtql", "from: {name: daily}\nwhere: {or: [{op: '==', left: {field: currency}, right: {value: AAA}}, {op: '==', left: {field: currency}, right: {value: AAB}}]}\nlimit: 50\n", 2, false},
		{"empty", "/dtql", "from: {name: daily}\nwhere: {op: '==', left: {field: currency}, right: {value: ZZZ}}\nlimit: 50\n", 0, false},
	} {
		for _, mount := range []string{"ecb", "alias"} {
			for _, account := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/account=%v", tc.name, mount, account), func(t *testing.T) {
					before := *calls
					id := strings.Repeat("d", 32)
					headers := http.Header{ProviderExecutionIDHeader: {id}}
					if account {
						headers.Set("Authorization", "irrelevant-synthetic-account")
						headers.Set("Cookie", "plan=irrelevant")
					}
					w := retentionRequest(s.Handler(), "POST", "/v1/databases/"+mount+tc.route, tc.body, headers)
					if w.Code != 200 || *calls != before+1 || *closed != *calls || w.Header().Get("Cache-Control") != "no-store" {
						t.Fatalf("positive trap: %d %s calls=%d closed=%d", w.Code, w.Body, *calls, *closed)
					}
					var response struct {
						Records  []struct{ Data map[string]any } `json:"records"`
						Rights   []license.SourceRight           `json:"sourceRights"`
						Used     []string                        `json:"usedSourceIds"`
						Evidence json.RawMessage                 `json:"providerReads"`
					}
					if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
						t.Fatal(err)
					}
					if len(response.Records) != tc.rows {
						t.Fatalf("rows %d want %d", len(response.Records), tc.rows)
					}
					for _, row := range response.Records {
						if row.Data["rate"] != "001.23000" || (tc.projected && len(row.Data) != 2) || (!tc.projected && len(row.Data) != 3) {
							t.Fatal("native projection changed", row.Data)
						}
					}
					capture, err := s.singleRights(s.getDB(mount), "daily")
					if err != nil || !equalProviderRights(response.Rights, capture.rights) {
						t.Fatal("rights changed", err)
					}
					proof, err := providerreads.Decode(response.Evidence)
					if err != nil {
						t.Fatal(err)
					}
					if err = providerreads.Validate(proof, s.providerPlan(profile, capture, id), response.Used); err != nil || len(proof.Reads) != 1 || len(proof.Usage) != 1 {
						t.Fatal("evidence changed", err)
					}
					if strings.Contains(string(response.Evidence), "001.23000") {
						t.Fatal("evidence retained row")
					}
				})
			}
		}
	}
	// Existing GET and JSON-parameter DTQL envelopes share the same route guard.
	for _, tc := range []struct {
		method, route, body string
		headers             http.Header
	}{
		{"GET", "/query?q=" + url.QueryEscape(`{"collection":"daily","limit":1}`), "", nil},
		{"GET", "/dtql?q=" + url.QueryEscape("from: {name: daily}\nlimit: 1\n"), "", nil},
		{"POST", "/dtql", `{"query":"from: {name: daily}\nwhere: {op: '==', left: {field: currency}, right: {param: code}}\nlimit: 1\n","parameters":{"code":"AAA"}}`, http.Header{"Content-Type": {"application/json"}}},
	} {
		before := *calls
		w := retentionRequest(s.Handler(), tc.method, "/v1/databases/ecb"+tc.route, tc.body, tc.headers)
		if w.Code != 200 || *calls != before+1 || *closed != *calls {
			t.Fatalf("legacy/envelope: %d %s", w.Code, w.Body)
		}
	}
}

func TestProviderRequestRefusesBeforeUpstream(t *testing.T) {
	s, _, calls, _ := providerRequestFixture(t, true)
	jsonBase := `{"collection":"daily","limit":1}`
	dtqlBase := "from: {name: daily}\nlimit: 1\n"
	for _, tc := range []struct {
		name, method, route, body string
		headers                   http.Header
	}{
		{"missing-limit", "POST", "/query", `{"collection":"daily"}`, nil},
		{"zero-limit", "POST", "/query", `{"collection":"daily","limit":0}`, nil},
		{"negative-limit", "POST", "/query", `{"collection":"daily","limit":-1}`, nil},
		{"excess-limit", "POST", "/query", `{"collection":"daily","limit":51}`, nil},
		{"wrong-collection", "POST", "/query", `{"collection":"history","limit":1}`, nil},
		{"undeclared-spelling", "POST", "/query", `{"collection":"DAILY","limit":1}`, nil},
		{"json-order-empty", "POST", "/query", `{"collection":"daily","limit":1,"orderBy":[]}`, nil},
		{"json-offset-zero", "POST", "/query", `{"collection":"daily","limit":1,"offset":0}`, nil},
		{"keys-only", "POST", "/query", `{"collection":"daily","limit":1,"keysOnly":true}`, nil},
		{"subcollection", "POST", "/query", `{"collection":"daily","limit":1,"parent":"daily/AAA"}`, nil},
		{"duplicate-limit", "POST", "/query", `{"collection":"daily","limit":51,"limit":1}`, nil},
		{"case-option", "POST", "/query", `{"collection":"daily","Limit":1}`, nil},
		{"unknown-option", "POST", "/query", `{"collection":"daily","limit":1,"export":true}`, nil},
		{"json-projection", "POST", "/query", `{"collection":"daily","limit":1,"columns":["rate"]}`, nil},
		{"filter-option", "POST", "/query", `{"collection":"daily","limit":1,"where":[{"field":"rate","op":"==","value":"001.23000","conversion":"USD"}]}`, nil},
		{"filter-field", "POST", "/query", `{"collection":"daily","limit":1,"where":[{"field":"convertedRate","op":"==","value":"1"}]}`, nil},
		{"filter-number", "POST", "/query", `{"collection":"daily","limit":1,"where":[{"field":"rate","op":">","value":1}]}`, nil},
		{"array-operation", "POST", "/query", `{"collection":"daily","limit":1,"where":[{"field":"currency","op":"array-contains","value":"AAA"}]}`, nil},
		{"unknown-url", "GET", "/query?q=" + url.QueryEscape(jsonBase) + "&history=false", "", nil},
		{"duplicate-url", "GET", "/query?q=" + url.QueryEscape(jsonBase) + "&q=" + url.QueryEscape(jsonBase), "", nil},
		{"post-options", "POST", "/query?cache=false", jsonBase, nil},
		{"paging-case", "POST", "/query", jsonBase, http.Header{"ovdb-PAGE-size": {"1"}}},
		{"paging-duplicate", "POST", "/query", jsonBase, http.Header{"OVDB-Page-Size": {"1", "1"}}},
		{"coalesced-nonce", "POST", "/query", jsonBase, http.Header{ProviderExecutionIDHeader: {strings.Repeat("a", 32) + "," + strings.Repeat("b", 32)}}},
		{"invalid-nonce", "POST", "/query", jsonBase, http.Header{ProviderExecutionIDHeader: {strings.Repeat("A", 32)}}},
		{"duplicate-nonce", "POST", "/dtql", dtqlBase, http.Header{ProviderExecutionIDHeader: {strings.Repeat("a", 32), strings.Repeat("b", 32)}}},
		{"case-duplicate-nonce", "POST", "/query", jsonBase, http.Header{ProviderExecutionIDHeader: {strings.Repeat("a", 32)}, "ovdb-execution-id": {strings.Repeat("b", 32)}}},
		{"point-read", "GET", "/read?key=daily/AAA", "", nil},
		{"record-read", "GET", "/records/daily/AAA", "", nil},
		{"record-write", "PUT", "/records/daily/AAA", `{}`, nil},
		{"batch", "POST", "/batch", `{"ops":[]}`, nil},
		{"dtql-missing-limit", "POST", "/dtql", "from: {name: daily}\n", nil},
		{"dtql-negative-limit", "POST", "/dtql", "from: {name: daily}\nlimit: -1\n", nil},
		{"dtql-zero-limit", "POST", "/dtql", "from: {name: daily}\nlimit: 0\n", nil},
		{"dtql-excess-limit", "POST", "/dtql", "from: {name: daily}\nlimit: 51\n", nil},
		{"dtql-order-empty", "POST", "/dtql", dtqlBase + "orderBy: []\n", nil},
		{"dtql-offset-zero", "POST", "/dtql", dtqlBase + "offset: 0\n", nil},
		{"dtql-alias", "POST", "/dtql", "from: {name: daily, alias: x}\nlimit: 1\n", nil},
		{"dtql-own-database", "POST", "/dtql", "from: {name: daily, database: ecb}\nlimit: 1\n", nil},
		{"dtql-scan", "POST", "/dtql", "from: {name: daily, scanLimit: 1}\nlimit: 1\n", nil},
		{"dtql-projection-alias", "POST", "/dtql", dtqlBase + "columns: [{field: rate, as: ''}]\n", nil},
		{"dtql-computed", "POST", "/dtql", dtqlBase + "columns: [{value: '1'}]\n", nil},
		{"dtql-qualified-field", "POST", "/dtql", dtqlBase + "columns: [{field: rate, source: ''}]\n", nil},
		{"dtql-join", "POST", "/dtql", dtqlBase + "joins: [{from: {name: daily, alias: x}, on: {op: '==', left: {field: currency}, right: {field: currency, source: x}}}]\n", nil},
		{"dtql-subquery", "POST", "/dtql", dtqlBase + "where: {exists: {query: {from: {name: daily}}}}\n", nil},
		{"dtql-duplicate", "POST", "/dtql", dtqlBase + "limit: 50\n", nil},
		{"dtql-yaml-alias", "POST", "/dtql", "from: &f {name: daily}\nlimit: 1\n", nil},
		{"dtql-envelope-duplicate", "POST", "/dtql", `{"query":"from: {name: history}\nlimit: 1\n","query":"from: {name: daily}\nlimit: 1\n"}`, http.Header{"Content-Type": {"application/json"}}},
		{"dtql-parameter-duplicate", "POST", "/dtql", `{"query":"from: {name: daily}\nwhere: {op: '==', left: {field: currency}, right: {param: code}}\nlimit: 1\n","parameters":{"code":"ZZZ","code":"AAA"}}`, http.Header{"Content-Type": {"application/json"}}},
	} {
		for _, mount := range []string{"ecb", "alias"} {
			t.Run(tc.name+"/"+mount, func(t *testing.T) {
				before := *calls
				w := retentionRequest(s.Handler(), tc.method, "/v1/databases/"+mount+tc.route, tc.body, tc.headers)
				expectedStatus, expectedCode := 422, "provider_request_unsupported"
				if strings.Contains(tc.name, "nonce") {
					expectedStatus, expectedCode = 400, "invalid_execution_id"
				}
				var refusal struct{ Error struct{ Code string } }
				if err := json.Unmarshal(w.Body.Bytes(), &refusal); err != nil {
					t.Fatal(err)
				}
				if w.Code != expectedStatus || refusal.Error.Code != expectedCode || *calls != before || w.Header().Get("Cache-Control") != "no-store" {
					t.Fatalf("refusal: %d %s calls=%d before=%d", w.Code, w.Body, *calls, before)
				}
			})
		}
	}
	for _, intent := range []string{"snapshot", "cache", "history", "export", "ai"} {
		before := *calls
		body := fmt.Sprintf(`{"collection":"daily","limit":1,"%s":false}`, intent)
		w := retentionRequest(s.Handler(), "POST", "/v1/databases/ecb/query", body, nil)
		if w.Code != 422 || *calls != before {
			t.Fatalf("unsupported intent %s: %d %s", intent, w.Code, w.Body)
		}
	}
	for _, mount := range []string{"ecb", "alias"} {
		before := *calls
		w := retentionRequest(s.Handler(), "POST", "/v1/dtql", "from: {database: "+mount+", name: daily}\nlimit: 1\n", nil)
		if w.Code != 422 || *calls != before {
			t.Fatalf("global: %d %s", w.Code, w.Body)
		}
	}
}

func TestProviderRequestUnmarkedCompatibilityAndStartup(t *testing.T) {
	s, p, calls, _ := providerRequestFixture(t, false)
	w := retentionRequest(s.Handler(), "POST", "/v1/databases/ecb/query", `{"collection":"daily","ignoredLegacyOption":true}`, nil)
	if w.Code != 200 || *calls != 1 {
		t.Fatalf("unmarked compatibility: %d %s", w.Code, w.Body)
	}
	for _, mutate := range []func(*ProviderReadProfile){
		func(p *ProviderReadProfile) { p.RequestProfile = "unknown" },
		func(p *ProviderReadProfile) { p.Binding.ResourceID = "history" },
		func(p *ProviderReadProfile) { p.Binding.ProviderSourceID = "provider:other/FxReferenceQuote" },
		func(p *ProviderReadProfile) { p.Collection = "other" },
	} {
		bad := p
		bad.RequestProfile = ECBPublicFreeRequestProfile
		mutate(&bad)
		candidate, err := NewChecked("test", map[string]*core.Database{"ecb": s.getDB("ecb")}, WithSourceRights("synthetic-server", nil), WithProviderReadProfiles(map[string]ProviderReadProfile{"ecb": bad}))
		if candidate != nil {
			candidate.CloseSnapshots()
		}
		if err == nil {
			t.Fatal("incompatible profile admitted")
		}
	}
}
