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
	db, err := dalgo2http.NewDB(dalgo2http.Config{
		Mode: dalgo2http.ModeLive, Client: client,
		Collections: []dalgo2http.Collection{{
			Name:        m.Storage.HTTP.Collection,
			URLTemplate: manifest.ECBDailyURL,
			Decoder:     dalgo2http.DecoderECBEuroFXRef,
			KeyField:    "currency", Timeout: 10 * time.Second,
			ClientSideFilter: true,
		}},
	})
	return db, []schema.Mode{schema.ModeStrict}, err
}
