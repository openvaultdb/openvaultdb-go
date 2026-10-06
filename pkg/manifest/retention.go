package manifest

import (
	"fmt"
	"time"
)

// RetentionNone permits a bounded transient response, but no reusable copy,
// result snapshot, continuation spool or response cache.
const RetentionNone = "none"

// EffectiveRetention preserves established native mounts. New live/unknown
// engines fail closed; no declaration can grant them copy authorization.
func (m *Manifest) EffectiveRetention() string {
	if m.Database.Retention == RetentionNone {
		return RetentionNone
	}
	switch m.Storage.Engine {
	case "sqlite", "ingitdb", "firestore", "postgres", "mysql":
		return ""
	default:
		return RetentionNone
	}
}

// ValidateRetention also runs in core.Open, before any driver provisioning.
func (m *Manifest) ValidateRetention() error {
	if m.Database.Retention != "" && m.Database.Retention != RetentionNone {
		return fmt.Errorf("database.retention must be none when supplied")
	}
	if m.EffectiveRetention() == RetentionNone && m.Database.CacheTTL != "" {
		ttl, err := time.ParseDuration(m.Database.CacheTTL)
		if err != nil || ttl != 0 {
			return fmt.Errorf("database.retention none requires absent or zero database.cache_ttl")
		}
	}
	return nil
}
