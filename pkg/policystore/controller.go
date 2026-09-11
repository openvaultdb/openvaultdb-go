package policystore

import (
	"context"
	"fmt"
	"sync"

	"github.com/dal-go/dalgo/access"
)

// Controller owns one admitted, immutable in-process policy snapshot. Publish
// and reload are trusted owner operations, not data-policy administration grants.
type Controller struct {
	mu     sync.RWMutex
	store  *Store
	active *Snapshot
}

func NewController(ctx context.Context, store *Store) (*Controller, error) {
	if store == nil {
		return nil, fmt.Errorf("policy store required")
	}
	controller := &Controller{store: store}
	if _, err := controller.Reload(ctx); err != nil {
		return nil, err
	}
	return controller, nil
}
func copySnapshot(snapshot Snapshot) Snapshot {
	snapshot.Documents = append([]Document(nil), snapshot.Documents...)
	snapshot.policies = append([]access.Policy(nil), snapshot.policies...)
	return snapshot
}
func (c *Controller) Snapshot() (Snapshot, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.active == nil {
		return Snapshot{}, fmt.Errorf("policy generation unavailable")
	}
	return copySnapshot(*c.active), nil
}
func (c *Controller) Policies(ctx context.Context) ([]access.Policy, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	snapshot, err := c.Snapshot()
	if err != nil {
		return nil, err
	}
	return snapshot.Policies(), nil
}
func (c *Controller) Reload(ctx context.Context) (Snapshot, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	snapshot, err := c.store.Load(ctx)
	if err != nil {
		c.active = nil
		return Snapshot{}, err
	}
	c.active = &snapshot
	return copySnapshot(snapshot), nil
}
func (c *Controller) Activate(ctx context.Context, expected string, documents []access.DTQLDocument) (Snapshot, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, publishErr := c.store.Activate(ctx, expected, documents)
	// Always recover the authoritative pointer before reopening admissions,
	// including an error after the atomic publication point.
	snapshot, loadErr := c.store.Load(ctx)
	if loadErr != nil {
		c.active = nil
		if publishErr != nil {
			return Snapshot{}, publishErr
		}
		return Snapshot{}, loadErr
	}
	c.active = &snapshot
	if publishErr != nil {
		return Snapshot{}, publishErr
	}
	return copySnapshot(snapshot), nil
}
