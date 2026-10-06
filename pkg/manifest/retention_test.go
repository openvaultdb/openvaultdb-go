package manifest

import "testing"

func TestRetentionValidationDefaultsAndCache(t *testing.T) {
	for _, engine := range []string{"sqlite", "ingitdb", "firestore", "postgres", "mysql", "http", "future"} {
		for _, retention := range []string{"", "none", "authorized", "unknown"} {
			for _, cache := range []string{"", "0s", "0h", "1h", "bad"} {
				m := &Manifest{Database: Database{Retention: retention, CacheTTL: cache}, Storage: Storage{Engine: engine}}
				noRetention := retention == "none" || engine == "http" || engine == "future"
				valid := (retention == "" || retention == "none") && (!noRetention || cache == "" || cache == "0s" || cache == "0h")
				if err := m.ValidateRetention(); (err == nil) != valid {
					t.Fatalf("engine=%s retention=%s cache=%s: %v", engine, retention, cache, err)
				}
			}
		}
	}
}
