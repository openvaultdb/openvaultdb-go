package providerreads

import (
	"encoding/json"
	"math"
	"os"
	"strings"
	"testing"

	"github.com/openvaultdb/openvaultdb-go/pkg/license"
)

func TestProviderJCSJavaScriptVector(t *testing.T) {
	// Exact independently calculated vector in dalgo-js PR15 provider-reads.test.ts.
	value := map[string]any{"z": math.Copysign(0, -1), "a": []any{1e30, 4.50, 2e-3, "€\n\"\\"}, "😀": true, "�": nil}
	expected := `{"a":[1e+30,4.5,0.002,"€\n\"\\"],"z":0,"😀":true,"�":null}`
	b, e := Canonical(value)
	if e != nil || string(b) != expected {
		t.Fatalf("canonical %s %v", b, e)
	}
	h, e := Digest(value)
	if e != nil || h != "6e27eb4b8e4332bf78cc20af8e637ae42bf981d23e15ca2a2fde38c09b737ab2" {
		t.Fatalf("digest %s %v", h, e)
	}
	for _, tc := range []struct {
		number float64
		want   string
	}{{1e-6, "0.000001"}, {1e-7, "1e-7"}, {1e20, "100000000000000000000"}, {1e21, "1e+21"}} {
		b, e = Canonical(tc.number)
		if e != nil || string(b) != tc.want {
			t.Fatal(tc, b, e)
		}
	}
	b, e = Canonical("\u2028\u2029<>&")
	if e != nil || string(b) != "\"\u2028\u2029<>&\"" {
		t.Fatal(string(b), e)
	}
	b, e = Canonical(`\u2028`)
	if e != nil || string(b) != `"\\u2028"` {
		t.Fatal(string(b), e)
	}
	if _, e = Canonical(string([]byte{0xff})); e == nil {
		t.Fatal("invalid Unicode accepted")
	}
}
func syntheticPlan(t *testing.T, mode string) Plan {
	t.Helper()
	hash := strings.Repeat("a", 64)
	right := license.SourceRight{SourceID: "ovdb:gateway/db/daily", Source: license.Identity{ServerID: "gateway", DatabaseID: "db", Recordset: "daily"}, Declaration: license.Declaration{Name: "Synthetic terms", URL: "https://example.com/terms"}, DeclaredAt: license.Identity{ServerID: "gateway", DatabaseID: "db"}, DeclarationScope: license.DatabaseScope, EvidenceOrigin: "publisher-definition-verified", Pins: []license.Pin{}, Transformations: []string{"Synthetic XML to rows"}, Attribution: &license.Notice{Text: "Synthetic provider"}, FreeSource: &license.Notice{Text: "Free original", URL: "https://example.com/original.xml"}}
	executor := "gateway"
	if mode == "direct" {
		right.SourceID = "direct:synthetic/daily"
		executor = "admitted-browser"
	}
	rightsDigest, e := RightsDigest(right)
	if e != nil {
		t.Fatal(e)
	}
	return Plan{Execution: Execution{ID: "synthetic-execution", Mode: mode, ExecutorID: executor}, Bindings: []Binding{{ProviderSourceID: "provider:synthetic/FxReferenceQuote", RightsSourceID: right.SourceID, ResourceID: "synthetic-daily", DefinitionDigest: hash, DecoderDigest: hash, RightsDigest: rightsDigest}}, Requests: []Request{{ResourceID: "synthetic-daily", Method: "GET", UpstreamURL: "https://example.com/original.xml", Params: map[string]any{}}}, SourceRights: []license.SourceRight{right}}
}
func syntheticObservation() Observation {
	return Observation{ResourceID: "synthetic-daily", FetchedAt: "2026-10-06T09:00:00Z", UpstreamURL: "https://example.com/original.xml", Status: 200, ContentType: "text/xml", SHA256: strings.Repeat("a", 64), Bytes: 42, ReferenceDate: "2026-10-05", ETag: `"synthetic"`, LastModified: "Mon, 05 Oct 2026 13:00:00 GMT"}
}
func fixture(t *testing.T, p Plan) Envelope {
	t.Helper()
	c, e := NewCollector(p)
	if e != nil {
		t.Fatal(e)
	}
	if e = c.Add(p.Requests[0], syntheticObservation()); e != nil {
		t.Fatal(e)
	}
	out, e := c.Finish([]string{p.Bindings[0].RightsSourceID})
	if e != nil {
		t.Fatal(e)
	}
	return out
}
func TestProviderCollectorAndConsumer(t *testing.T) {
	p := syntheticPlan(t, "proxy")
	e := fixture(t, p)
	// These payloads match the exact JS synthetic fixture; cross-runtime script
	// separately checks all three hash contracts against the built JS consumer.
	direct := fixture(t, syntheticPlan(t, "direct"))
	if direct.Reads[0].ObservationID == e.Reads[0].ObservationID || direct.Reads[0].SHA256 != e.Reads[0].SHA256 {
		t.Fatal("execution authority parity failed")
	}
	c, err := NewCollector(p)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err = c.Add(p.Requests[0], syntheticObservation()); err != nil {
			t.Fatal(err)
		}
	}
	out, err := c.Finish([]string{p.Bindings[0].RightsSourceID})
	if err != nil || len(out.Reads) != 1 || len(out.Usage) != 1 {
		t.Fatal(out, err)
	}
	p.SourceRights[0].Declaration.Name = "changed"
	if err = Validate(e, p, []string{p.Bindings[0].RightsSourceID}); err == nil {
		t.Fatal("changed rights accepted")
	}
	p = syntheticPlan(t, "proxy")
	e.Reads[0].Bytes++
	if err = Validate(e, p, []string{p.Bindings[0].RightsSourceID}); err == nil {
		t.Fatal("forged body evidence accepted")
	}
	e = fixture(t, p)
	if err = Validate(e, p, []string{}); err == nil {
		t.Fatal("missing legacy usage accepted")
	}
	e.Usage = nil
	if err = Validate(e, p, []string{p.Bindings[0].RightsSourceID}); err == nil {
		t.Fatal("missing observations accepted")
	}
	c, err = NewCollector(p)
	if err != nil {
		t.Fatal(err)
	}
	r := p.Requests[0]
	r.UpstreamURL = "https://example.com/unplanned"
	if err = c.Add(r, syntheticObservation()); err == nil {
		t.Fatal("late resource accepted")
	}
	if _, err = c.Finish([]string{}); err == nil {
		t.Fatal("failed collector not poisoned")
	}
	p = syntheticPlan(t, "proxy")
	p.MaxReads = new(1)
	c, err = NewCollector(p)
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Add(p.Requests[0], syntheticObservation()); err != nil {
		t.Fatal(err)
	}
	o := syntheticObservation()
	o.FetchedAt = "2026-10-06T09:00:01Z"
	if err = c.Add(p.Requests[0], o); err == nil {
		t.Fatal("read budget exceeded")
	}
}
func TestProviderClosedDecoder(t *testing.T) {
	p := syntheticPlan(t, "proxy")
	e := fixture(t, p)
	raw, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := Decode(raw)
	if err != nil || !equal(decoded, e) {
		t.Fatal(decoded, err)
	}
	for _, bad := range []string{strings.Replace(string(raw), `"bytes":42,`, "", 1), strings.Replace(string(raw), `"bytes":42`, `"bytes":42,"bytes":42`, 1), strings.Replace(string(raw), `"status":200`, `"status":200,"body":"secret"`, 1), strings.Replace(string(raw), `"reads":[`, `"reads":null,"discard":[`, 1), strings.Replace(string(raw), `synthetic-execution`, `\ud800`, 1), strings.Replace(string(raw), `synthetic-execution`, `\udc00`, 1)} {
		if _, err = Decode([]byte(bad)); err == nil {
			t.Fatalf("bad wire accepted %s", bad)
		}
	}
}

func TestProviderSharedJavaScriptCorpus(t *testing.T) {
	raw, err := os.ReadFile("testdata/parity-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var corpus struct {
		Cases []struct {
			Mode     string
			Metadata struct {
				SourceRights  []license.SourceRight `json:"sourceRights"`
				Used          []string              `json:"usedSourceIds"`
				ProviderReads json.RawMessage       `json:"providerReads"`
			}
			Plan           Plan
			Canonical      string
			MetadataDigest string
		}
	}
	if err = json.Unmarshal(raw, &corpus); err != nil {
		t.Fatal(err)
	}
	for _, tc := range corpus.Cases {
		t.Run(tc.Mode, func(t *testing.T) {
			evidence, err := Decode(tc.Metadata.ProviderReads)
			if err != nil {
				t.Fatal(err)
			}
			if err = Validate(evidence, tc.Plan, tc.Metadata.Used); err != nil {
				t.Fatal(err)
			}
			produced := fixture(t, syntheticPlan(t, tc.Mode))
			if !equal(produced, evidence) {
				t.Fatal("Go/JS observation payload differs")
			}
			metadata := map[string]any{"sourceRights": tc.Metadata.SourceRights, "usedSourceIds": tc.Metadata.Used, "providerReads": produced}
			canonical, err := Canonical(metadata)
			if err != nil || string(canonical) != tc.Canonical {
				t.Fatal("Go/JS canonical mismatch", err)
			}
			digest, err := Digest(metadata)
			if err != nil || digest != tc.MetadataDigest {
				t.Fatal("Go/JS digest mismatch", digest, err)
			}
		})
	}
}
func TestProviderMissingReadAndZeroBudget(t *testing.T) {
	p := syntheticPlan(t, "proxy")
	c, err := NewCollector(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.Finish([]string{p.Bindings[0].RightsSourceID}); err == nil {
		t.Fatal("used source without observation accepted")
	}
	p.MaxReads = new(0)
	c, err = NewCollector(p)
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Add(p.Requests[0], syntheticObservation()); err == nil {
		t.Fatal("zero read budget accepted a read")
	}
}
