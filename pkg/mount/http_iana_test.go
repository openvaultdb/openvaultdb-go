package mount

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/dal-go/dalgo2http"
	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/manifest"
	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
)

const ianaHTTPManifest = `database: {id: iana-http-status, schema_mode: strict, retention: none}
storage:
  engine: http
  http: {profile: iana-http-status/1, collection: rows}
schemas:
  collections:
    rows:
      fields:
        Value: {type: string}
        Description: {type: string}
        Reference: {type: string}
`

// Invented data exercises lexical ranges and quoted CSV without recording IANA rows.
const ianaSyntheticCSV = "Value,Description,Reference\n799,synthetic status,[Invented]\n800-899,\"synthetic, range\",[Invented]\n"

func TestIANAHTTPMountNativeQueryAndObservation(t *testing.T) {
	m, err := manifest.Parse([]byte(ianaHTTPManifest))
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	driver, modes, err := openHTTP(m, &http.Client{Transport: httpTestTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Method != http.MethodGet || r.URL.String() != manifest.IANAHTTPStatusURL ||
			r.Header.Get("Cache-Control") != "no-store, no-cache" || r.Header.Get("Pragma") != "no-cache" {
			t.Fatalf("request escaped fixed IANA profile: %s %s %v", r.Method, r.URL, r.Header)
		}
		if _, ok := r.Context().Deadline(); !ok {
			t.Fatal("unbounded IANA request")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/csv"}}, Body: io.NopCloser(strings.NewReader(ianaSyntheticCSV))}, nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	db, err := core.Open(m, driver, modes, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if !db.NoRetention() || !db.ReadOnlyHTTP() || db.InferredSnapshot() != nil {
		t.Fatal("IANA mount gained retention or mutation capability")
	}
	var observations []dalgo2http.Provenance
	ctx := dalgo2http.ContextWithProvenanceObserver(context.Background(), func(p dalgo2http.Provenance) { observations = append(observations, p) })
	rows, err := db.Execute(ctx, core.Query{Collection: "rows", Where: []core.Filter{{Field: "Value", Op: "==", Value: "800-899"}}})
	if err != nil || len(rows) != 1 || rows[0].Data["Description"] != "synthetic, range" || rows[0].Key.ID != "800-899" {
		t.Fatalf("native lexical query: %v %v", rows, err)
	}
	wantSHA := fmt.Sprintf("%x", sha256.Sum256([]byte(ianaSyntheticCSV)))
	if len(observations) != 1 || observations[0].Decoder != dalgo2http.DecoderStrictCSV3 ||
		observations[0].Source != dalgo2http.SourceLive || observations[0].UpstreamURL != manifest.IANAHTTPStatusURL ||
		observations[0].SHA256 != wantSHA || observations[0].Bytes != len(ianaSyntheticCSV) ||
		observations[0].BaseCurrency != "" || observations[0].ReferenceDate != "" {
		t.Fatalf("IANA observation: %+v", observations)
	}
	if calls != 1 {
		t.Fatalf("IANA query made %d reads", calls)
	}
}

func TestIANAHTTPManifestClosedNativeFields(t *testing.T) {
	for name, bad := range map[string]string{
		"profile":       strings.Replace(ianaHTTPManifest, "iana-http-status/1", "arbitrary", 1),
		"url":           strings.Replace(ianaHTTPManifest, "profile: iana-http-status/1", "url: https://example.org/other.csv", 1),
		"numeric value": strings.Replace(ianaHTTPManifest, "Value: {type: string}", "Value: {type: int}", 1),
		"semantic key":  strings.Replace(ianaHTTPManifest, "Value: {type: string}", "Value: {type: string, primary_key: true}", 1),
		"unknown field": strings.Replace(ianaHTTPManifest, "Reference: {type: string}", "Other: {type: string}", 1),
		"retention":     strings.Replace(ianaHTTPManifest, "retention: none", "retention: retained", 1),
		"cache":         strings.Replace(ianaHTTPManifest, "retention: none", "retention: none, cache_ttl: 1h", 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := manifest.Parse([]byte(bad)); err == nil {
				t.Fatal("invalid IANA profile accepted")
			}
		})
	}
	if m, err := manifest.Parse([]byte(ianaHTTPManifest)); err != nil || m.Database.SchemaMode != schema.ModeStrict {
		t.Fatalf("valid IANA manifest: %v", err)
	}
}
