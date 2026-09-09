package core

import (
	"context"

	"github.com/dal-go/dalgo/access"
	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
)

// PolicyLayer is a trusted in-process snapshot, never an HTTP DTO. Every
// enabled participant remains represented even if another participant denies.
// Hosts must separately authorize disclosure before exposing any of its facts.
type PolicyLayer struct {
	Kind     string
	Enabled  bool
	Policies []access.Policy
	Err      error
}

func (d *Database) HasAccessPolicies() bool { return d.ownerPolicies != nil || d.lowerPolicies != nil }

func (d *Database) Coordinator() *access.EnforcementCoordinator { return d.coordinator }

type fixedOwnerLease struct{ policies []access.Policy }

func (l fixedOwnerLease) Policies() []access.Policy {
	return append([]access.Policy(nil), l.policies...)
}
func (fixedOwnerLease) Revision() string { return "fixed" }
func (fixedOwnerLease) Release()         {}
func (d *Database) fixedPolicyLease(ctx context.Context) (access.PolicyLease, error) {
	policies, err := d.ownerPolicies(ctx)
	if err != nil {
		return nil, err
	}
	return fixedOwnerLease{policies}, nil
}

func (d *Database) PolicyLayers(ctx context.Context) []PolicyLayer {
	upper := PolicyLayer{Kind: "openvaultdb", Enabled: d.ownerPolicies != nil}
	if upper.Enabled {
		upper.Policies, upper.Err = d.ownerPolicies(ctx)
	}
	layers := []PolicyLayer{upper}
	if d.Manifest.Storage.Engine == "ingitdb" || d.lowerPolicies != nil {
		lower := PolicyLayer{Kind: "ingitdb", Enabled: d.lowerPolicies != nil}
		if lower.Enabled {
			lower.Policies, lower.Err = d.lowerPolicies(ctx)
		}
		layers = append(layers, lower)
	}
	return layers
}

func (d *Database) validateProtectedCandidate(ctx context.Context, op access.ProtectedOperation, candidate map[string]any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return schema.ValidateRecord(d.Manifest.Database.SchemaMode, op.Key().Collection(), d.Manifest.Schemas.Collection(op.Key().Collection()), candidate)
}
