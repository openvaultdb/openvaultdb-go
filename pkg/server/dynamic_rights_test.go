package server

import (
	"strings"
	"testing"

	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/license"
	"github.com/openvaultdb/openvaultdb-go/pkg/manifest"
	"github.com/openvaultdb/openvaultdb-go/pkg/providerreads"
	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
)

func dynamicRightsFixture(t *testing.T) (*core.Database, ProviderReadProfile) {
	t.Helper()
	// In-memory driver only; this helper cannot fetch a real HTTP resource.
	m := &manifest.Manifest{Database: manifest.Database{ID: "synthetic", SchemaMode: schema.ModeStrict, License: &license.Declaration{Name: "Synthetic terms", URL: "https://example.org/terms"}},
		Storage: manifest.Storage{Engine: "http", HTTP: &manifest.HTTPOptions{Profile: manifest.HTTPProfileECBDaily, Collection: "daily"}},
		Schemas: &schema.Schemas{Collections: map[string]schema.Collection{"daily": {Fields: map[string]schema.Field{"time": {Type: schema.TypeString}, "currency": {Type: schema.TypeString}, "rate": {Type: schema.TypeString}}}}}}
	db, err := core.Open(m, &retentionDriver{}, []schema.Mode{schema.ModeStrict}, "")
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
	digest, err := providerreads.RightsDigest(*right)
	if err != nil {
		t.Fatal(err)
	}
	return db, ProviderReadProfile{Collection: "daily", SourceRight: right, Binding: providerreads.Binding{ProviderSourceID: "provider:synthetic/FxReferenceQuote", RightsSourceID: right.SourceID, ResourceID: "daily", DefinitionDigest: strings.Repeat("a", 64), DecoderDigest: strings.Repeat("b", 64), RightsDigest: digest}}
}

func TestDynamicProviderRightsFrozenAcrossOptionsAndAliases(t *testing.T) {
	db, profile := dynamicRightsFixture(t)
	expected := cloneDynamicRight(*profile.SourceRight)
	profiles := map[string]ProviderReadProfile{"first": profile, "second": profile}
	option := WithProviderReadProfiles(profiles)
	profile.SourceRight.Attribution.Text = "mutated before New"
	profile.SourceRight.Pins[0].Path = "mutated"
	profile.SourceRight.Transformations[0] = "mutated"
	delete(profiles, "first")
	s, err := NewChecked("test", map[string]*core.Database{"first": db, "second": db}, WithSourceRights("synthetic-server", nil), option)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.CloseSnapshots)
	for _, alias := range []string{"first", "second"} {
		response := retentionRequest(s.Handler(), "GET", "/v1/databases/"+alias, "", nil)
		if response.Code != 200 || !strings.Contains(response.Body.String(), "Synthetic provider") || strings.Contains(response.Body.String(), "mutated") {
			t.Fatalf("detached notices unavailable for alias: %d %s", response.Code, response.Body)
		}
	}
	rights, err := s.databaseRights(db, "daily")
	if err != nil || !equalProviderRights(rights, []license.SourceRight{expected}) {
		t.Fatal("wrong detached inventory", rights, err)
	}
	rights[0].Attribution.Text = "mutated after capture"
	rights[0].Pins[0].Path = "mutated"
	rights[0].Transformations[0] = "mutated"
	got, err := s.databaseRights(db, "daily")
	if err != nil || !equalProviderRights(got, []license.SourceRight{expected}) {
		t.Fatal("runtime inventory mutated", got, err)
	}
}

func TestHumanPagesRenderAdmittedDynamicNoticesAndRetention(t *testing.T) {
	db, profile := dynamicRightsFixture(t)
	profile.SourceRight.Attribution.Text = "Synthetic provider <script>secret</script>"
	profile.SourceRight.Transformations = []string{"XML <row> to quote"}
	digest, err := providerreads.RightsDigest(*profile.SourceRight)
	if err != nil {
		t.Fatal(err)
	}
	profile.Binding.RightsDigest = digest
	s, err := NewChecked("test", map[string]*core.Database{"synthetic": db}, WithSourceRights("synthetic-server", nil), WithProviderReadProfiles(map[string]ProviderReadProfile{"synthetic": profile}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.CloseSnapshots)
	for _, path := range []string{"/ovdb/dbs/synthetic", "/ovdb/dbs/synthetic/collections/daily"} {
		page := humanRequest(s.Handler(), path)
		body := page.Body.String()
		for _, want := range []string{
			"Synthetic terms", "Source terms", "do not grant an output license", "Attribution:", "Synthetic provider &lt;script&gt;secret&lt;/script&gt;",
			"Original free source:", `href="` + manifest.ECBDailyURL + `"`, "Synthetic original is free",
			"Transformations", "XML &lt;row&gt; to quote", "Retention: none", "not retained by this server",
		} {
			if page.Code != 200 || !strings.Contains(body, want) {
				t.Fatalf("%s missing %q: %d %s", path, want, page.Code, body)
			}
		}
		if strings.Contains(body, "<script>secret</script>") || strings.Contains(body, "<row>") || strings.Contains(body, profile.SourceRight.Pins[0].SHA256) {
			t.Fatalf("%s rendered unsafe text or internal pin: %s", path, body)
		}
	}
	for _, path := range []string{"/ovdb/", "/ovdb/dbs/"} {
		body := humanRequest(s.Handler(), path).Body.String()
		if strings.Contains(body, "Synthetic provider") || strings.Contains(body, "Original free source:") {
			t.Fatalf("%s disclosed source notices outside source page: %s", path, body)
		}
	}
}

func TestHumanPagesOmitUnadmittedDynamicNotices(t *testing.T) {
	db, _ := dynamicRightsFixture(t)
	s, err := NewChecked("test", map[string]*core.Database{"synthetic": db}, WithSourceRights("synthetic-server", nil))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.CloseSnapshots)
	for _, path := range []string{"/ovdb/dbs/synthetic", "/ovdb/dbs/synthetic/collections/daily"} {
		page := humanRequest(s.Handler(), path)
		body := page.Body.String()
		if page.Code != 200 || !strings.Contains(body, "Synthetic terms") || !strings.Contains(body, "Retention: none") ||
			strings.Contains(body, "Attribution:") || strings.Contains(body, "Original free source:") || strings.Contains(body, "Transformations") {
			t.Fatalf("%s does not distinguish declared terms from admitted notices: %d %s", path, page.Code, body)
		}
	}
}

func TestDynamicProviderRightsRefuseContradictoryAdmission(t *testing.T) {
	cases := map[string]func(*ProviderReadProfile){
		"terms":                   func(p *ProviderReadProfile) { p.SourceRight.Declaration.Text = "replacement terms" },
		"source":                  func(p *ProviderReadProfile) { p.SourceRight.Source.ServerID = "other-server" },
		"source ID":               func(p *ProviderReadProfile) { p.SourceRight.SourceID = "other-source" },
		"scope":                   func(p *ProviderReadProfile) { p.SourceRight.DeclarationScope = license.RecordsetScope },
		"declared at":             func(p *ProviderReadProfile) { p.SourceRight.DeclaredAt.Recordset = "daily" },
		"immutable input claim":   func(p *ProviderReadProfile) { p.SourceRight.EvidenceOrigin = "publisher-verified" },
		"extra input pin":         func(p *ProviderReadProfile) { p.SourceRight.Pins = append(p.SourceRight.Pins, p.SourceRight.Pins[0]) },
		"input pin role":          func(p *ProviderReadProfile) { p.SourceRight.Pins[0].Role = "input" },
		"definition digest":       func(p *ProviderReadProfile) { p.SourceRight.Pins[0].SHA256 = strings.Repeat("d", 64) },
		"moving revision":         func(p *ProviderReadProfile) { p.SourceRight.Pins[0].Revision = "main" },
		"pin bytes":               func(p *ProviderReadProfile) { p.SourceRight.Pins[0].Bytes = 0 },
		"pin repository":          func(p *ProviderReadProfile) { p.SourceRight.Pins[0].Repository += "?token=synthetic" },
		"pin path":                func(p *ProviderReadProfile) { p.SourceRight.Pins[0].Path = "../ovdb.yaml" },
		"missing attribution":     func(p *ProviderReadProfile) { p.SourceRight.Attribution = nil },
		"blank attribution":       func(p *ProviderReadProfile) { p.SourceRight.Attribution.Text = " " },
		"missing free source":     func(p *ProviderReadProfile) { p.SourceRight.FreeSource = nil },
		"wrong original resource": func(p *ProviderReadProfile) { p.SourceRight.FreeSource.URL = "https://example.org/other.xml" },
		"missing transformations": func(p *ProviderReadProfile) { p.SourceRight.Transformations = nil },
		"rights digest":           func(p *ProviderReadProfile) { p.Binding.RightsDigest = strings.Repeat("d", 64) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			db, profile := dynamicRightsFixture(t)
			mutate(&profile)
			if _, err := NewChecked("test", map[string]*core.Database{"synthetic": db}, WithSourceRights("synthetic-server", nil), WithProviderReadProfiles(map[string]ProviderReadProfile{"synthetic": profile})); err == nil {
				t.Fatal("contradictory rights accepted")
			}
		})
	}
}
