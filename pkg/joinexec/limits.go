package joinexec

import "time"

// Limits bounds one request. A zero or negative field takes its default from
// DefaultLimits, so an unconfigured server is bounded rather than unbounded.
type Limits struct {
	// MaxSourceRows caps the rows read from all sources together.
	MaxSourceRows int
	// MaxSourceBytes caps the JSON-encoded bytes read from all sources together.
	MaxSourceBytes int64
	// Timeout bounds the whole request; see Guard.Context.
	Timeout time.Duration
}

// DefaultLimits are the request bounds for a 512 MiB instance.
func DefaultLimits() Limits {
	return Limits{
		MaxSourceRows:  100_000,
		MaxSourceBytes: 64 << 20,
		Timeout:        10 * time.Second,
	}
}

func (l Limits) withDefaults() Limits {
	d := DefaultLimits()
	if l.MaxSourceRows <= 0 {
		l.MaxSourceRows = d.MaxSourceRows
	}
	if l.MaxSourceBytes <= 0 {
		l.MaxSourceBytes = d.MaxSourceBytes
	}
	if l.Timeout <= 0 {
		l.Timeout = d.Timeout
	}
	return l
}
