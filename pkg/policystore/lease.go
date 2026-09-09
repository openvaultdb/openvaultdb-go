package policystore

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/dal-go/dalgo/access"
)

// AcquirePolicyLease is called only after the adapter storage admission lock.
// Publication waits for this lease through the actual commit/abort boundary.
func (c *Controller) AcquirePolicyLease(ctx context.Context) (access.PolicyLease, error) {
	for !c.mu.TryRLock() {
		timer := time.NewTimer(5 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	if err := ctx.Err(); err != nil {
		c.mu.RUnlock()
		return nil, err
	}
	if c.active == nil {
		c.mu.RUnlock()
		return nil, fmt.Errorf("policy generation unavailable")
	}
	return &controllerLease{snapshot: copySnapshot(*c.active), release: c.mu.RUnlock}, nil
}

type controllerLease struct {
	snapshot Snapshot
	once     sync.Once
	release  func()
}

func (l *controllerLease) Policies() []access.Policy { return l.snapshot.Policies() }
func (l *controllerLease) Revision() string          { return l.snapshot.Revision }
func (l *controllerLease) Release()                  { l.once.Do(l.release) }
