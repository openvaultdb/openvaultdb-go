package mount

import (
	"net/http"
	"time"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo2http"
	"github.com/openvaultdb/openvaultdb-go/pkg/manifest"
	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
)

// openHTTP compiles a fixed live-only resource. The nil production client uses
// dalgo2http's guarded HTTPS transport, DNS/address checks and redirect refusal.
// A client parameter is private to this package for synthetic transport tests.
func openHTTP(m *manifest.Manifest, client *http.Client) (dal.DB, []schema.Mode, error) {
	if err := m.ValidateHTTP(); err != nil {
		return nil, nil, err
	}
	collection := dalgo2http.Collection{Name: m.Storage.HTTP.Collection, Timeout: 10 * time.Second, ClientSideFilter: true}
	switch m.Storage.HTTP.Profile {
	case manifest.HTTPProfileECBDaily:
		collection.URLTemplate = manifest.ECBDailyURL
		collection.Decoder = dalgo2http.DecoderECBEuroFXRef
		collection.KeyField = "currency"
	case manifest.HTTPProfileIANAHTTPStatus:
		collection.URLTemplate = manifest.IANAHTTPStatusURL
		collection.Method = dalgo2http.MethodGET
		collection.Decoder = dalgo2http.DecoderStrictCSV3
		collection.KeyField = "Value" // Query transport only; no point-read or scalar-code promise.
	}
	db, err := dalgo2http.NewDB(dalgo2http.Config{
		Mode: dalgo2http.ModeLive, Client: client,
		Collections: []dalgo2http.Collection{collection},
	})
	return db, []schema.Mode{schema.ModeStrict}, err
}
