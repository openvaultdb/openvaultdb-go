package core

import (
	"fmt"
	"github.com/openvaultdb/openvaultdb-go/pkg/manifest"
	"time"
)

// Retention is fixed when Open accepts the source; callers cannot weaken it by
// changing the exposed manifest. Instances not opened by Open fail closed.
func (d *Database) Retention() string {
	if d.retention == manifest.RetentionNone || d.db == nil {
		return manifest.RetentionNone
	}
	return ""
}

func (d *Database) NoRetention() bool { return d.Retention() == manifest.RetentionNone }

// ValidateRetentionCache rejects changed cache configuration at server admission.
// Enforcement still uses the immutable capability if the manifest later changes.
func (d *Database) ValidateRetentionCache() error {
	if d.NoRetention() && d.Manifest != nil && d.Manifest.Database.CacheTTL != "" {
		ttl, err := time.ParseDuration(d.Manifest.Database.CacheTTL)
		if err != nil || ttl != 0 {
			return fmt.Errorf("database.retention none requires absent or zero database.cache_ttl")
		}
	}
	return nil
}
