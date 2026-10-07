package server

import (
	"encoding/json"
	"io"
	"net/http"
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

// This is invented CSV; it never requests or records IANA's resource.
const syntheticIANAProviderCSV = "Value,Description,Reference\n799,synthetic status,[Invented]\n800-899,synthetic range,[Invented]\n"

func ianaProviderFixture(t *testing.T, responseBody ...string) (*Server, ProviderReadProfile, *int) {
	t.Helper()
	body := syntheticIANAProviderCSV
	if len(responseBody) == 1 {
		body = responseBody[0]
	}
	calls := new(int)
	driver, err := dalgo2http.NewDB(dalgo2http.Config{Mode: dalgo2http.ModeLive, Collections: []dalgo2http.Collection{{
		Name: "rows", URLTemplate: manifest.IANAHTTPStatusURL, Method: dalgo2http.MethodGET,
		Decoder: dalgo2http.DecoderStrictCSV3, KeyField: "Value", ClientSideFilter: true, Timeout: 10 * time.Second,
	}}, Client: &http.Client{Transport: liveHTTPTestTransport(func(r *http.Request) (*http.Response, error) {
		*calls++
		if r.Method != http.MethodGet || r.URL.String() != manifest.IANAHTTPStatusURL ||
			r.Header.Get("Cache-Control") != "no-store, no-cache" {
			t.Fatal("IANA request escaped fixed no-store resource")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/csv"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}})
	if err != nil {
		t.Fatal(err)
	}
	m := &manifest.Manifest{Database: manifest.Database{ID: "iana-http-status", SchemaMode: schema.ModeStrict,
		License: &license.Declaration{Name: "Synthetic registry terms declaration", URL: manifest.IANALicensingTermsURL}},
		Storage: manifest.Storage{Engine: "http", HTTP: &manifest.HTTPOptions{Profile: manifest.HTTPProfileIANAHTTPStatus, Collection: "rows"}},
		Schemas: &schema.Schemas{Collections: map[string]schema.Collection{"rows": {Fields: map[string]schema.Field{
			"Value": {Type: schema.TypeString}, "Description": {Type: schema.TypeString}, "Reference": {Type: schema.TypeString},
		}}}}}
	db, err := core.Open(m, driver, []schema.Mode{schema.ModeStrict}, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	right, err := db.SourceRight("synthetic-operator", nil, "rows")
	if err != nil || right == nil {
		t.Fatal(err)
	}
	right.EvidenceOrigin = "publisher-definition-verified"
	right.Pins = []license.Pin{{Role: "provider", Repository: "https://github.com/synthetic/provider", Revision: strings.Repeat("c", 40), Path: "ovdb.yaml", SHA256: strings.Repeat("a", 64), Bytes: 42}}
	right.Attribution = &license.Notice{Text: "Synthetic registry attribution", URL: "https://example.org/"}
	right.FreeSource = &license.Notice{Text: "Synthetic original", URL: manifest.IANAHTTPStatusURL}
	right.Transformations = []string{"Synthetic CSV parsed into native strings"}
	rightsDigest, err := providerreads.RightsDigest(*right)
	if err != nil {
		t.Fatal(err)
	}
	profile := ProviderReadProfile{RequestProfile: IANANativeOperatorRequestProfile, Collection: "rows", SourceRight: right,
		Binding: providerreads.Binding{ProviderSourceID: "provider:iana/HttpStatusRegistryRow", RightsSourceID: right.SourceID,
			ResourceID: "iana-http-status-codes", DefinitionDigest: strings.Repeat("a", 64), DecoderDigest: strings.Repeat("b", 64), RightsDigest: rightsDigest}}
	s, err := NewChecked("synthetic", map[string]*core.Database{"iana-http-status": db},
		WithSourceRights("synthetic-operator", nil), WithProviderReadProfiles(map[string]ProviderReadProfile{"iana-http-status": profile}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.CloseSnapshots)
	return s, profile, calls
}

func TestIANANativeOperatorQueryAndEvidence(t *testing.T) {
	s, profile, calls := ianaProviderFixture(t)
	for _, tc := range []struct {
		path, body string
		count      int
	}{
		{"/query", `{"collection":"rows","limit":1,"where":[{"field":"Value","op":"==","value":"800-899"}]}`, 1},
		{"/dtql", "from: {name: rows}\ncolumns: [{field: Value}, {field: Reference}]\nlimit: 1\n", 1},
		{"/dtql", "from: {name: rows}\nwhere: {op: '==', left: {field: Value}, right: {value: never}}\nlimit: 1\n", 0},
	} {
		before := *calls
		w := retentionRequest(s.Handler(), http.MethodPost, "/v1/databases/iana-http-status"+tc.path, tc.body, nil)
		if w.Code != http.StatusOK || *calls != before+1 || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("IANA operator query: %d %s reads=%d", w.Code, w.Body, *calls)
		}
		var response struct {
			Records  []any                 `json:"records"`
			Evidence json.RawMessage       `json:"providerReads"`
			Rights   []license.SourceRight `json:"sourceRights"`
			Used     []string              `json:"usedSourceIds"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil || len(response.Records) != tc.count {
			t.Fatalf("IANA rows: %v %s", err, w.Body)
		}
		capture, err := s.singleRights(s.getDB("iana-http-status"), "rows")
		if err != nil || !equalProviderRights(response.Rights, capture.rights) {
			t.Fatal("IANA rights changed", err)
		}
		proof, err := providerreads.Decode(response.Evidence)
		if err != nil || providerreads.Validate(proof, s.providerPlan(profile, capture, proof.Execution.ID), response.Used) != nil || len(proof.Reads) != 1 {
			t.Fatal("IANA provider evidence invalid", err)
		}
		if proof.Reads[0].UpstreamURL != manifest.IANAHTTPStatusURL || proof.Reads[0].ReferenceDate != "" ||
			strings.Contains(string(response.Evidence), "synthetic range") {
			t.Fatal("IANA evidence carried wrong resource or row")
		}
	}
}

func TestIANANativeOperatorRefusesBeforeRead(t *testing.T) {
	s, profile, calls := ianaProviderFixture(t)
	for name, tc := range map[string]struct {
		method, path, body string
		headers            http.Header
	}{
		"missing limit":      {http.MethodPost, "/query", `{"collection":"rows"}`, nil},
		"unknown option":     {http.MethodPost, "/query", `{"collection":"rows","limit":1,"history":false}`, nil},
		"semantic field":     {http.MethodPost, "/query", `{"collection":"rows","limit":1,"where":[{"field":"statusCode","op":"==","value":"799"}]}`, nil},
		"numeric value":      {http.MethodPost, "/query", `{"collection":"rows","limit":1,"where":[{"field":"Value","op":"==","value":799}]}`, nil},
		"paging":             {http.MethodPost, "/query", `{"collection":"rows","limit":1}`, http.Header{"OVDB-Page-Size": {"1"}}},
		"point read":         {http.MethodGet, "/records/rows/799", "", nil},
		"write":              {http.MethodPut, "/records/rows/799", `{}`, nil},
		"join":               {http.MethodPost, "/dtql", "from: {name: rows}\njoins: [{from: {name: rows}}]\nlimit: 1\n", nil},
		"missing dtql limit": {http.MethodPost, "/dtql", "from: {name: rows}\n", nil},
	} {
		t.Run(name, func(t *testing.T) {
			before := *calls
			w := retentionRequest(s.Handler(), tc.method, "/v1/databases/iana-http-status"+tc.path, tc.body, tc.headers)
			if w.Code != http.StatusUnprocessableEntity || *calls != before || w.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("IANA refusal: %d %s reads=%d", w.Code, w.Body, *calls)
			}
		})
	}
	bad := profile
	bad.RequestProfile = ""
	if err := validateProviderRequestProfile(s.getDB("iana-http-status"), bad); err == nil {
		t.Fatal("IANA admission missing closed request profile")
	}
	withoutAdmission, err := NewChecked("synthetic", map[string]*core.Database{"iana-http-status": s.getDB("iana-http-status")},
		WithSourceRights("synthetic-operator", nil))
	if withoutAdmission != nil {
		withoutAdmission.CloseSnapshots()
	}
	if err == nil || *calls != 0 {
		t.Fatal("IANA server started without admission or read upstream", err, *calls)
	}
	bad.SourceRight = nil
	bad.RequestProfile = IANANativeOperatorRequestProfile
	withoutPin, err := NewChecked("synthetic", map[string]*core.Database{"iana-http-status": s.getDB("iana-http-status")},
		WithSourceRights("synthetic-operator", nil), WithProviderReadProfiles(map[string]ProviderReadProfile{"iana-http-status": bad}))
	if withoutPin != nil {
		withoutPin.CloseSnapshots()
	}
	if err == nil || *calls != 0 {
		t.Fatal("IANA server started without pinned publisher definition", err, *calls)
	}
}

func TestIANANativeOperatorMalformedResponseSuppressesRows(t *testing.T) {
	const marker = "private-synthetic-row-marker"
	s, _, calls := ianaProviderFixture(t, "Value,Description,Reference\n799,"+marker+",[Invented]\n799,duplicate,[Invented]\n")
	w := retentionRequest(s.Handler(), http.MethodPost, "/v1/databases/iana-http-status/dtql", "from: {name: rows}\nlimit: 1\n", nil)
	if w.Code == http.StatusOK || *calls != 1 || w.Header().Get("Cache-Control") != "no-store" ||
		strings.Contains(w.Body.String(), marker) || strings.Contains(w.Body.String(), "duplicate") {
		t.Fatalf("malformed IANA response leaked or succeeded: %d %s reads=%d", w.Code, w.Body, *calls)
	}
}

func TestIANANativeOperatorRequestProfileFrozenAfterManifestMutation(t *testing.T) {
	s, _, calls := ianaProviderFixture(t)
	db := s.getDB("iana-http-status")
	db.Manifest.Storage.HTTP.Profile = manifest.HTTPProfileECBDaily
	db.Manifest.Storage.HTTP.Collection = "daily"
	w := retentionRequest(s.Handler(), http.MethodPost, "/v1/databases/iana-http-status/query",
		`{"collection":"rows","limit":1,"where":[{"field":"Value","op":"==","value":"800-899"}]}`, nil)
	if w.Code != http.StatusOK || *calls != 1 {
		t.Fatalf("mutable manifest changed admitted IANA request contract: %d %s reads=%d", w.Code, w.Body, *calls)
	}
}
