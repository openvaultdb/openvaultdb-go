package providerreads

import (
	"encoding/json"
	"errors"
	"math"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/openvaultdb/openvaultdb-go/pkg/license"
)

func TestProviderJCSJavaScriptVector(t *testing.T) {
	t.Parallel()
	// Exact independently calculated vector in dalgo-js PR15 provider-reads.test.ts.
	value := map[string]any{"z": math.Copysign(0, -1), "a": []any{1e30, 4.50, 2e-3, "€\n\"\\"}, "😀": true, "\uFFFD": nil}
	expected := `{"a":[1e+30,4.5,0.002,"€\n\"\\"],"z":0,"😀":true,"` + "\uFFFD" + `":null}`
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

func TestProviderMetadataFullPreflight(t *testing.T) {
	t.Parallel()
	p := syntheticPlan(t, "proxy")
	e := fixture(t, p)
	used := []string{p.Bindings[0].RightsSourceID}
	actual := Metadata{SourceRights: p.SourceRights, UsedSourceIDs: used, ProviderReads: &e}
	if err := ValidateMetadata(actual, p, used); err != nil {
		t.Fatal(err)
	}
	actual.SourceRights = append([]license.SourceRight{}, p.SourceRights...)
	actual.SourceRights[0].Declaration.Name = "changed response"
	if err := ValidateMetadata(actual, p, used); err == nil {
		t.Fatal("changed response rights accepted")
	}
	actual.SourceRights = p.SourceRights
	actual.UsedSourceIDs = []string{}
	if err := ValidateMetadata(actual, p, used); err == nil {
		t.Fatal("changed response usage accepted")
	}
	actual.UsedSourceIDs = used
	actual.ProviderReads = nil
	if err := ValidateMetadata(actual, p, used); err == nil {
		t.Fatal("missing required metadata accepted")
	}
}

func TestProviderConstrainedURLGrammar(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("testdata/url-grammar-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		URL        string
		GoAccepted bool
		JSAccepted bool
	}
	if err = json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		t.Run(tc.URL, func(t *testing.T) {
			p := syntheticPlan(t, "proxy")
			p.Requests[0].UpstreamURL = tc.URL
			_, err := NewCollector(p)
			if (err == nil) != tc.GoAccepted {
				t.Fatalf("Go admission differs: %v", err)
			}
			if tc.GoAccepted && !tc.JSAccepted {
				t.Fatal("Go profile may not admit a JS-incompatible URL")
			}
		})
	}
}

func TestProviderPunycodeLabelsFailClosed(t *testing.T) {
	t.Parallel()
	// Includes the independent r2 mismatches plus a valid A-label deliberately
	// refused by the narrower Go profile; no locator is normalized silently.
	for _, host := range []string{"xn--0.com", "xn--a-ecp.com", "xn--bcher-kva.example", "sub.xn--0.com", "xn--0.sub.example.com"} {
		for _, path := range []string{"/file", "/a%2Fb?x=one%20two"} {
			plan := syntheticPlan(t, "proxy")
			plan.Requests[0].UpstreamURL = "https://" + host + path
			if _, err := NewCollector(plan); err == nil {
				t.Fatalf("punycode label admitted: %s", plan.Requests[0].UpstreamURL)
			}
		}
	}
}

func TestProviderCanonicalDecodeAndValidateEdgeCases(t *testing.T) {
	t.Parallel()
	badUTF8 := string([]byte{0xff})

	// Canonical / Digest / validStrings / canonical
	if b, err := Canonical(nil); err != nil || string(b) != "null" {
		t.Fatalf("Canonical(nil) = %s, %v", b, err)
	}
	if _, err := Canonical(make(chan int)); err == nil {
		t.Fatal("expected error for unmarshalable channel")
	}
	if _, err := Digest(badUTF8); err == nil {
		t.Fatal("expected error for invalid UTF-8 in Digest")
	}
	for _, v := range []any{
		map[string]string{badUTF8: "ok"},
		map[string]string{"ok": badUTF8},
		[]string{badUTF8},
		struct{ S string }{S: badUTF8},
	} {
		if _, err := Canonical(v); err == nil {
			t.Fatalf("expected validStrings error for %#v", v)
		}
	}
	if b, err := Canonical("\b\f\r\t\x01"); err != nil || string(b) != `"\b\f\r\t\u0001"` {
		t.Fatalf("control escapes = %s, %v", b, err)
	}
	for _, v := range []any{
		json.Number("NaN"),
		[]any{struct{}{}},
		map[string]any{"k": struct{}{}},
		struct{}{},
	} {
		if _, err := canonical(v); err == nil {
			t.Fatalf("expected canonical error for %#v", v)
		}
	}

	// Decode / readJSON / closedShape / scalarEscapes
	if _, err := Decode([]byte(badUTF8)); err == nil {
		t.Fatal("expected Decode error for invalid UTF-8")
	}
	for _, bad := range []string{
		`{} {}`,
		``,
		`{"a"`,
		`}`,
		`123`,
		`{"format":"x","execution":{"id":"i","mode":"direct","executorId":"e"},"bindings":[],"reads":"not-array","usage":[]}`,
		`{"format":"","execution":{"id":"i","mode":"direct","executorId":"e"},"bindings":[],"reads":[],"usage":[]}`,
		`{"format":"ovdb-provider-read/1","execution":{"id":"i","mode":"direct","executorId":"e"},"bindings":[],"reads":[{"observationId":"o","resourceId":"r","requestDigest":"d","fetchedAt":"f","upstreamUrl":"u","status":"not-num","contentType":"c","sha256":"s","bytes":1,"attestation":"a"}],"usage":[]}`,
		`"abc\`,
		`"\u12"`,
		`"\uGGGG"`,
		`"\ud800\u0041"`,
		`"\ud800\ud800"`,
		`"\ud83d\ude00"`, // valid surrogate pair in non-object JSON fails closedShape
	} {
		if _, err := Decode([]byte(bad)); err == nil {
			t.Fatalf("expected Decode error for %q", bad)
		}
	}
	// Fake token stream for non-string key in readJSON
	if err := closedShape(map[string]any{
		"observationId": "o", "resourceId": "r", "requestDigest": "d", "fetchedAt": "f",
		"upstreamUrl": "u", "status": json.Number("200"), "contentType": "c",
		"sha256": "s", "bytes": json.Number("1"), "attestation": "a",
	}, reflect.TypeFor[Observation]()); err != nil {
		t.Fatalf("closedShape with omitted optional fields failed: %v", err)
	}

	// validate.go helpers: text, texts, digests, safeURL, requestValue, validateRequest
	if err := text(badUTF8, true, true); err == nil {
		t.Fatal("expected text error for bad UTF-8")
	}
	if err := text("a\x01b", true, true); err == nil {
		t.Fatal("expected text control error")
	}
	if err := texts(""); err == nil {
		t.Fatal("expected texts error for empty string")
	}
	if err := digests("short"); err == nil {
		t.Fatal("expected digests error")
	}
	if err := safeURL(""); err == nil {
		t.Fatal("expected safeURL error for empty")
	}
	if err := safeURL("https://example.com/%2"); err == nil {
		t.Fatal("expected safeURL error for short escape")
	}
	if err := safeURL("https://example.com/%GG"); err == nil {
		t.Fatal("expected safeURL error for bad hex escape")
	}
	for _, validVal := range []any{nil, true, "str", float64(42), int(42), int64(42), json.Number("42")} {
		if err := requestValue(validVal); err != nil {
			t.Fatalf("unexpected requestValue error for %#v: %v", validVal, err)
		}
	}
	for _, badVal := range []any{float64(1.5), int(9007199254740992), int64(9007199254740992), json.Number("bad"), struct{}{}} {
		if err := requestValue(badVal); err == nil {
			t.Fatalf("expected requestValue error for %#v", badVal)
		}
	}
	if err := validateRequest(Request{ResourceID: "", Method: "GET", UpstreamURL: "https://example.com/a", Params: map[string]any{}}); err == nil {
		t.Fatal("expected validateRequest error for empty ResourceID")
	}
	if err := validateRequest(Request{ResourceID: "r", Method: "POST", UpstreamURL: "https://example.com/a", Params: map[string]any{}}); err == nil {
		t.Fatal("expected validateRequest error for POST")
	}
	if err := validateRequest(Request{ResourceID: "r", Method: "GET", UpstreamURL: "https://example.com/a", Params: map[string]any{"": "v"}}); err == nil {
		t.Fatal("expected validateRequest error for empty param key")
	}
	manyItems := make([]any, MaxItems+1)
	if err := validateRequest(Request{ResourceID: "r", Method: "GET", UpstreamURL: "https://example.com/a", Params: map[string]any{"k": manyItems}}); err == nil {
		t.Fatal("expected validateRequest error for overbound array param")
	}
	if err := validateRequest(Request{ResourceID: "r", Method: "GET", UpstreamURL: "https://example.com/a", Params: map[string]any{"k": []any{"ok", struct{}{}}}}); err == nil {
		t.Fatal("expected validateRequest error for bad array item")
	}
	if err := validateRequest(Request{ResourceID: "r", Method: "GET", UpstreamURL: "https://example.com/a", Params: map[string]any{"k": []any{"ok"}}}); err != nil {
		t.Fatalf("unexpected validateRequest error for valid array param: %v", err)
	}
	if err := validateRequest(Request{ResourceID: "r", Method: "GET", UpstreamURL: "https://example.com/a", Params: map[string]any{"k": struct{}{}}}); err == nil {
		t.Fatal("expected validateRequest error for bad scalar param")
	}

	// validateExecution & validateBinding
	if err := validateExecution(Execution{ID: "", Mode: "direct", ExecutorID: "e"}); err == nil {
		t.Fatal("expected validateExecution error for empty ID")
	}
	if err := validateExecution(Execution{ID: "i", Mode: "invalid", ExecutorID: "e"}); err == nil {
		t.Fatal("expected validateExecution error for invalid mode")
	}
	if err := validateBinding(Binding{ProviderSourceID: "", RightsSourceID: "r", ResourceID: "res"}); err == nil {
		t.Fatal("expected validateBinding error for empty ProviderSourceID")
	}
	if err := validateBinding(Binding{ProviderSourceID: "same", RightsSourceID: "same", ResourceID: "res"}); err == nil {
		t.Fatal("expected validateBinding error for conflated identity")
	}

	// validateObservation
	exec := Execution{ID: "i", Mode: "proxy", ExecutorID: "e"}
	obs := syntheticObservation()
	obs.ObservationID = "bad"
	if err := validateObservation(obs, exec); err == nil {
		t.Fatal("expected validateObservation error for bad digest")
	}
	hash := strings.Repeat("a", 64)
	obs.ObservationID = hash
	obs.RequestDigest = hash
	obs.Attestation = "proxy-executor-observed"
	obs.ContentType = ""
	if err := validateObservation(obs, exec); err == nil {
		t.Fatal("expected validateObservation error for empty ContentType")
	}
	obs.ContentType = "text/xml"
	obs.UpstreamURL = "http://example.com/a"
	if err := validateObservation(obs, exec); err == nil {
		t.Fatal("expected validateObservation error for unsafe URL")
	}
	obs.UpstreamURL = "https://example.com/a"
	obs.Status = 500
	if err := validateObservation(obs, exec); err == nil {
		t.Fatal("expected validateObservation error for status 500")
	}
	obs.Status = 200
	obs.FetchedAt = "not-a-date"
	if err := validateObservation(obs, exec); err == nil {
		t.Fatal("expected validateObservation error for bad FetchedAt shape")
	}
	obs.FetchedAt = "2026-13-40T99:99:99Z"
	if err := validateObservation(obs, exec); err == nil {
		t.Fatal("expected validateObservation error for unparseable FetchedAt")
	}
	obs.FetchedAt = "2026-10-06T09:00:00Z"
	obs.ReferenceDate = "2026-02-30"
	if err := validateObservation(obs, exec); err == nil {
		t.Fatal("expected validateObservation error for invalid ReferenceDate")
	}
	obs.ReferenceDate = "2026-10-05"
	obs.ETag = "bad\x01etag"
	if err := validateObservation(obs, exec); err == nil {
		t.Fatal("expected validateObservation error for control char in ETag")
	}

	// boundedJSON
	if err := boundedJSON(make([]any, MaxItems+1), ""); err == nil {
		t.Fatal("expected boundedJSON error for overbound array")
	}
	if err := boundedJSON([]any{"bad\x01"}, ""); err == nil {
		t.Fatal("expected boundedJSON error for bad item in array")
	}
	if err := boundedJSON(map[string]any{"k": "bad\x01"}, ""); err == nil {
		t.Fatal("expected boundedJSON error for bad item in map")
	}

	// validateRights & validatePlan
	basePlan := syntheticPlan(t, "proxy")
	p := basePlan
	p.SourceRights = nil
	if err := validateRights(p); err == nil {
		t.Fatal("expected validateRights error for nil SourceRights")
	}
	p = syntheticPlan(t, "proxy")
	p.SourceRights[0].SourceID = ""
	if err := validateRights(p); err == nil {
		t.Fatal("expected validateRights error for empty SourceID")
	}
	p = syntheticPlan(t, "proxy")
	p.SourceRights[0].Pins = nil
	if err := validateRights(p); err == nil {
		t.Fatal("expected validateRights error for nil Pins")
	}
	p = syntheticPlan(t, "proxy")
	p.SourceRights[0].Declaration = license.Declaration{}
	if err := validateRights(p); err == nil {
		t.Fatal("expected validateRights error for invalid Declaration")
	}
	p = syntheticPlan(t, "proxy")
	p.SourceRights[0].Attribution = nil
	p.SourceRights[0].FreeSource = &license.Notice{Text: "   ", URL: "https://example.com/a"}
	if err := validateRights(p); err == nil {
		t.Fatal("expected validateRights error for empty notice text")
	}
	p = syntheticPlan(t, "proxy")
	p.SourceRights[0].FreeSource = &license.Notice{Text: "ok", URL: "javascript:alert(1)"}
	if err := validateRights(p); err == nil {
		t.Fatal("expected validateRights error for bad notice URL")
	}
	p = syntheticPlan(t, "proxy")
	p.SourceRights[0].FreeSource = &license.Notice{Text: "ok", URL: ""}
	if err := validateRights(p); err == nil {
		t.Fatal("expected validateRights error for missing FreeSource URL")
	}
	p = syntheticPlan(t, "proxy")
	p.SourceRights[0].Pins = []license.Pin{{Role: "", Repository: "r", Revision: "v", Path: "p", SHA256: hash, Bytes: 1}}
	if err := validateRights(p); err == nil {
		t.Fatal("expected validateRights error for empty pin Role")
	}
	p.SourceRights[0].Pins = []license.Pin{{Role: "r", Repository: "r", Revision: "v", Path: "p", SHA256: "bad", Bytes: 1}}
	if err := validateRights(p); err == nil {
		t.Fatal("expected validateRights error for bad pin SHA256")
	}
	p.SourceRights[0].Pins = []license.Pin{{Role: "r", Repository: "r", Revision: "v", Path: "p", SHA256: hash, Bytes: -1}}
	if err := validateRights(p); err == nil {
		t.Fatal("expected validateRights error for negative pin Bytes")
	}
	p.SourceRights[0].Pins = []license.Pin{{Role: "r", Repository: "r", Revision: "v", Path: "p", SHA256: hash, Bytes: 1}}
	p.SourceRights[0].Transformations = []string{""}
	if err := validateRights(p); err == nil {
		t.Fatal("expected validateRights error for empty transformation")
	}
	p = syntheticPlan(t, "proxy")
	p.SourceRights[0].Declaration.Name = "bad\x01name"
	if err := validateRights(p); err == nil {
		t.Fatal("expected validateRights error for control char in Declaration.Name via boundedJSON")
	}

	// validatePlan edge cases
	p = syntheticPlan(t, "proxy")
	p.Execution.Mode = "bad"
	if err := validatePlan(p); err == nil {
		t.Fatal("expected validatePlan error for bad execution")
	}
	p = syntheticPlan(t, "proxy")
	p.Bindings = nil
	if err := validatePlan(p); err == nil {
		t.Fatal("expected validatePlan error for nil Bindings")
	}
	p = syntheticPlan(t, "proxy")
	p.MaxMetadataBytes = -1
	if err := validatePlan(p); err == nil {
		t.Fatal("expected validatePlan error for negative MaxMetadataBytes")
	}
	p = syntheticPlan(t, "proxy")
	p.Bindings[0].ProviderSourceID = ""
	if err := validatePlan(p); err == nil {
		t.Fatal("expected validatePlan error for bad binding")
	}
	p = syntheticPlan(t, "proxy")
	p.Bindings = append(p.Bindings, p.Bindings[0])
	if err := validatePlan(p); err == nil {
		t.Fatal("expected validatePlan error for duplicate resource binding")
	}
	p = syntheticPlan(t, "proxy")
	p.Bindings[0].RightsSourceID = "missing"
	if err := validatePlan(p); err == nil {
		t.Fatal("expected validatePlan error for missing binding source right")
	}
	p = syntheticPlan(t, "proxy")
	p.Requests[0].ResourceID = "unplanned"
	if err := validatePlan(p); err == nil {
		t.Fatal("expected validatePlan error for unplanned request resource")
	}
	p = syntheticPlan(t, "proxy")
	p.Requests = append(p.Requests, p.Requests[0])
	if err := validatePlan(p); err == nil {
		t.Fatal("expected validatePlan error for duplicate planned request")
	}

	// Validate & Collector edge cases
	p = syntheticPlan(t, "proxy")
	e := fixture(t, p)
	used := []string{p.Bindings[0].RightsSourceID}
	badEnv := e
	badEnv.Format = "bad"
	if err := Validate(badEnv, p, used); err == nil {
		t.Fatal("expected Validate error for bad format")
	}
	if err := Validate(e, p, []string{used[0], used[0]}); err == nil {
		t.Fatal("expected Validate error for duplicate used source")
	}
	if err := Validate(e, p, []string{"unknown"}); err == nil {
		t.Fatal("expected Validate error for unknown used source")
	}
	badEnv = fixture(t, p)
	badEnv.Reads[0].Status = 500
	if err := Validate(badEnv, p, used); err == nil {
		t.Fatal("expected Validate error for invalid observation in Reads")
	}
	badEnv = fixture(t, p)
	obs2 := badEnv.Reads[0]
	obs2.Bytes = 99
	badEnv.Reads = append(badEnv.Reads, obs2)
	if err := Validate(badEnv, p, used); err == nil {
		t.Fatal("expected Validate error for conflicting observation ID")
	}
	badEnv = fixture(t, p)
	badEnv.Reads[0].RequestDigest = strings.Repeat("b", 64)
	if err := Validate(badEnv, p, used); err == nil {
		t.Fatal("expected Validate error for unplanned resource request")
	}
	badEnv = fixture(t, p)
	badEnv.Usage[0].ProviderSourceID = ""
	if err := Validate(badEnv, p, used); err == nil {
		t.Fatal("expected Validate error for empty ProviderSourceID in Usage")
	}
	badEnv = fixture(t, p)
	badEnv.Usage[0].ObservationIDs = nil
	if err := Validate(badEnv, p, used); err == nil {
		t.Fatal("expected Validate error for empty ObservationIDs in Usage")
	}
	badEnv = fixture(t, p)
	u2 := badEnv.Usage[0]
	u2.ObservationIDs = []string{u2.ObservationIDs[0], u2.ObservationIDs[0]}
	badEnv.Usage = append(badEnv.Usage, u2)
	if err := Validate(badEnv, p, used); err == nil {
		t.Fatal("expected Validate error for conflicting source usage")
	}
	badEnv = fixture(t, p)
	badEnv.Usage[0].ObservationIDs = []string{"missing"}
	if err := Validate(badEnv, p, used); err == nil {
		t.Fatal("expected Validate error for missing usage observation")
	}
	badEnv = fixture(t, p)
	badEnv.Usage[0].ProviderSourceID = "wrong-provider"
	if err := Validate(badEnv, p, used); err == nil {
		t.Fatal("expected Validate error for source observation binding mismatch")
	}
	smallPlan := syntheticPlan(t, "proxy")
	smallPlan.MaxMetadataBytes = 10
	if _, err := NewCollector(smallPlan); err == nil {
		t.Fatal("expected preflight metadata budget exceeded")
	}
	if err := Validate(e, smallPlan, used); err == nil {
		t.Fatal("expected metadata budget exceeded in Validate")
	}

	// Collector.Add error sticky and branches
	c, err := NewCollector(p)
	if err != nil {
		t.Fatal(err)
	}
	mismatchObs := syntheticObservation()
	mismatchObs.ResourceID = "other"
	if err := c.Add(p.Requests[0], mismatchObs); err == nil {
		t.Fatal("expected observed resource mismatch")
	}
	if err := c.Add(p.Requests[0], syntheticObservation()); err == nil {
		t.Fatal("expected sticky error on subsequent Add")
	}
	c, err = NewCollector(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Add(Request{ResourceID: badUTF8}, syntheticObservation()); err == nil {
		t.Fatal("expected RequestDigest error in Add")
	}
	c, err = NewCollector(p)
	if err != nil {
		t.Fatal(err)
	}
	badObs := syntheticObservation()
	badObs.Status = 500
	if err := c.Add(p.Requests[0], badObs); err == nil {
		t.Fatal("expected validateObservation error in Add")
	}
	// Multiple usages with same RightsSourceID in Collector.add
	pMulti := syntheticPlan(t, "proxy")
	b2 := pMulti.Bindings[0]
	b2.ResourceID = "synthetic-daily-2"
	b2.ProviderSourceID = "provider:synthetic/OtherQuote"
	pMulti.Bindings = append(pMulti.Bindings, b2)
	r2 := pMulti.Requests[0]
	r2.ResourceID = "synthetic-daily-2"
	pMulti.Requests = append(pMulti.Requests, r2)
	cMulti, err := NewCollector(pMulti)
	if err != nil {
		t.Fatal(err)
	}
	if err := cMulti.Add(pMulti.Requests[0], syntheticObservation()); err != nil {
		t.Fatal(err)
	}
	obsMulti2 := syntheticObservation()
	obsMulti2.ResourceID = "synthetic-daily-2"
	if err := cMulti.Add(r2, obsMulti2); err != nil {
		t.Fatal(err)
	}
}

func TestProviderReadsCoverageURLAndDecoders(t *testing.T) {
	t.Parallel()
	if err := safeURL("https://example.com/path?q=%"); err == nil || !strings.Contains(err.Error(), "invalid URL escape") {
		t.Fatalf("expected invalid URL escape, got: %v", err)
	}
	if err := safeURL("https://example.com/path?q=%ZZ"); err == nil || !strings.Contains(err.Error(), "invalid URL escape") {
		t.Fatalf("expected invalid URL escape, got: %v", err)
	}
	if _, err := readJSON(json.NewDecoder(strings.NewReader("{\"a\": 1,"))); err == nil {
		t.Fatal("expected error from incomplete JSON")
	}
}

func TestProviderReadsValidationAndPlanCoverage(t *testing.T) {
	t.Parallel()
	badUTF8 := string([]byte{0xff})

	pBadRights := syntheticPlan(t, "proxy")
	pBadRights.SourceRights[0].Declaration.Name = badUTF8
	if err := validateRights(pBadRights); err == nil {
		t.Fatal("expected validateRights error for bad UTF-8 in declaration name")
	}
	if err := validatePlan(pBadRights); err == nil {
		t.Fatal("expected validatePlan error for bad rights")
	}

	pBadCanonicalRight := syntheticPlan(t, "proxy")
	pBadCanonicalRight.SourceRights[0].Source.DatabaseID = badUTF8
	if err := validateRights(pBadCanonicalRight); err == nil {
		t.Fatal("expected validateRights error for bad UTF-8 in source database ID")
	}

	pBadNotice := syntheticPlan(t, "proxy")
	pBadNotice.SourceRights[0].Attribution = &license.Notice{Text: "bad\x01notice"}
	if err := validateRights(pBadNotice); err == nil {
		t.Fatal("expected validateRights error for control char in notice")
	}

	pBadTransform := syntheticPlan(t, "proxy")
	pBadTransform.SourceRights[0].Transformations = []string{""}
	if err := validateRights(pBadTransform); err == nil {
		t.Fatal("expected validateRights error for empty transform")
	}

	pBadReq := syntheticPlan(t, "proxy")
	pBadReq.Requests[0].Params = map[string]any{"bad": badUTF8}
	if err := validatePlan(pBadReq); err == nil {
		t.Fatal("expected validatePlan error for bad params")
	}

	pBadPlan := syntheticPlan(t, "proxy")
	pBadPlan.Execution.Mode = badUTF8
	if _, err := NewCollector(pBadPlan); err == nil {
		t.Fatal("expected NewCollector error for bad UTF-8 in execution mode")
	}

	p := syntheticPlan(t, "proxy")
	cUnbound := &Collector{plan: Plan{Requests: p.Requests, Bindings: []Binding{}}}
	if err := cUnbound.Add(p.Requests[0], syntheticObservation()); err == nil || !strings.Contains(err.Error(), "missing resource binding") {
		t.Fatalf("expected missing resource binding, got %v", err)
	}

	cBadObs := &Collector{plan: p}
	badObs := syntheticObservation()
	badObs.ContentType = badUTF8
	if err := cBadObs.Add(p.Requests[0], badObs); err == nil {
		t.Fatal("expected Add error for bad UTF-8 observation")
	}

	cConflict, err := NewCollector(p)
	if err != nil {
		t.Fatal(err)
	}
	obs := syntheticObservation()
	if err := cConflict.Add(p.Requests[0], obs); err != nil {
		t.Fatal(err)
	}
	cConflict.envelope.Reads[0].Bytes = 999999
	if err := cConflict.Add(p.Requests[0], obs); err == nil || !strings.Contains(err.Error(), "conflicting observation") {
		t.Fatalf("expected conflicting observation, got %v", err)
	}

	pMulti := syntheticPlan(t, "proxy")
	b2 := pMulti.Bindings[0]
	b2.ResourceID = "synthetic-daily-2"
	b2.ProviderSourceID = "provider:synthetic/OtherQuote"
	pMulti.Bindings = append(pMulti.Bindings, b2)
	r2 := pMulti.Requests[0]
	r2.ResourceID = "synthetic-daily-2"
	pMulti.Requests = append(pMulti.Requests, r2)

	cMulti, err := NewCollector(pMulti)
	if err != nil {
		t.Fatal(err)
	}
	if err := cMulti.Add(pMulti.Requests[0], syntheticObservation()); err != nil {
		t.Fatal(err)
	}
	obs2 := syntheticObservation()
	obs2.ResourceID = "synthetic-daily-2"
	if err := cMulti.Add(r2, obs2); err != nil {
		t.Fatal(err)
	}
	envUnclaimed := cMulti.envelope
	envUnclaimed.Usage = envUnclaimed.Usage[:1]
	if err := Validate(envUnclaimed, pMulti, []string{pMulti.SourceRights[0].SourceID}); err == nil || !strings.Contains(err.Error(), "unclaimed observation") {
		t.Fatalf("expected unclaimed observation error, got: %v", err)
	}
}

// Not parallel: modifies package-level seam variables
func TestProviderReadsPackageSeams(t *testing.T) {
	origMarshal := jsonMarshal
	origUnmarshal := jsonUnmarshal
	defer func() {
		jsonMarshal = origMarshal
		jsonUnmarshal = origUnmarshal
	}()

	p := syntheticPlan(t, "proxy")

	// Canonical decode error
	jsonMarshal = func(v any) ([]byte, error) {
		return []byte("{invalid json"), nil
	}
	if _, err := Canonical("test"); err == nil {
		t.Fatal("expected error from malformed JSON in Canonical")
	}

	// NewCollector jsonMarshal error
	jsonMarshal = func(v any) ([]byte, error) {
		return nil, errors.New("forced marshal error")
	}
	if _, err := NewCollector(p); err == nil {
		t.Fatal("expected NewCollector marshal error")
	}

	// NewCollector jsonUnmarshal error
	jsonMarshal = origMarshal
	jsonUnmarshal = func(_ []byte, _ any) error {
		return errors.New("forced unmarshal error")
	}
	if _, err := NewCollector(p); err == nil {
		t.Fatal("expected NewCollector unmarshal error")
	}

	// NewCollector Canonical error
	jsonMarshal = func(v any) ([]byte, error) {
		if m, ok := v.(map[string]any); ok && m["sourceRights"] != nil && m["usedSourceIds"] != nil {
			return nil, errors.New("forced canonical marshal error")
		}
		return origMarshal(v)
	}
	jsonUnmarshal = origUnmarshal
	if _, err := NewCollector(p); err == nil {
		t.Fatal("expected NewCollector Canonical error")
	}

	// validatePlan RequestDigest error
	jsonMarshal = func(v any) ([]byte, error) {
		if m, ok := v.(map[string]any); ok && m["format"] == "ovdb-resource-request/1" {
			return nil, errors.New("forced request digest error")
		}
		return origMarshal(v)
	}
	if err := validatePlan(p); err == nil {
		t.Fatal("expected validatePlan RequestDigest error")
	}

	// validateRights jsonUnmarshal error
	jsonMarshal = origMarshal
	jsonUnmarshal = func(_ []byte, _ any) error {
		return errors.New("forced unmarshal error in rights")
	}
	if err := validateRights(p); err == nil {
		t.Fatal("expected validateRights unmarshal error")
	}

	// Collector.Finish jsonMarshal error
	jsonMarshal = origMarshal
	jsonUnmarshal = origUnmarshal
	c, err := NewCollector(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Add(p.Requests[0], syntheticObservation()); err != nil {
		t.Fatal(err)
	}
	jsonMarshal = func(v any) ([]byte, error) {
		if _, ok := v.(Envelope); ok {
			return nil, errors.New("finish marshal error")
		}
		return origMarshal(v)
	}
	if _, err := c.Finish([]string{p.SourceRights[0].SourceID}); err == nil {
		t.Fatal("expected Finish marshal error")
	}

	// Validate metadata Canonical error at line 470
	jsonMarshal = func(v any) ([]byte, error) {
		if m, ok := v.(map[string]any); ok && m["providerReads"] != nil {
			return nil, errors.New("validate metadata marshal error")
		}
		return origMarshal(v)
	}
	if err := Validate(c.envelope, p, []string{p.SourceRights[0].SourceID}); err == nil {
		t.Fatal("expected Validate Canonical error")
	}
}
