package manifest

import (
	"fmt"

	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
)

const (
	HTTPProfileECBDaily = "ecb-daily/1"
	ECBDailyURL         = "https://www.ecb.europa.eu/stats/eurofxref/eurofxref-daily.xml"
)

// HTTPOptions selects an immutable public resource profile, never an arbitrary
// proxy URL, cache, snapshot store or executable decoder. Collection is its
// local recordset name; native ECB fields remain time, currency and rate.
type HTTPOptions struct {
	Profile    string `yaml:"profile" json:"profile"`
	Collection string `yaml:"collection" json:"collection"`
}

// ValidateHTTP is also called by core.Open for programmatically built mounts.
func (m *Manifest) ValidateHTTP() error {
	if m.Storage.Engine != "http" {
		if m.Storage.HTTP != nil {
			return fmt.Errorf("storage.http requires the http engine")
		}
		return nil
	}
	o := m.Storage.HTTP
	if o == nil || o.Profile != HTTPProfileECBDaily || o.Collection == "" {
		return fmt.Errorf("storage.http requires profile ecb-daily/1 and a collection name")
	}
	if m.Storage.Path != "" || m.Storage.InGitDB != nil || m.Storage.SQLite != nil || m.Storage.Firestore != nil || m.Storage.Postgres != nil || m.Storage.MySQL != nil {
		return fmt.Errorf("http sources cannot configure local storage or other engines")
	}
	if m.Database.SchemaMode != schema.ModeStrict || m.Schemas == nil || len(m.Schemas.Collections) != 1 {
		return fmt.Errorf("http sources require strict mode and exactly their declared collection")
	}
	c, ok := m.Schemas.Collections[o.Collection]
	if !ok || len(c.Fields) != 3 || len(c.References) != 0 {
		return fmt.Errorf("ECB collection must declare only native time, currency and rate fields")
	}
	for _, name := range []string{"time", "currency", "rate"} {
		f, ok := c.Fields[name]
		if !ok || f.Type != schema.TypeString || f.Decimal != nil {
			return fmt.Errorf("ECB native field %s must have string type", name)
		}
	}
	return m.ValidateRetention()
}
