package server

import (
	"context"
	"fmt"

	"github.com/dal-go/dalgo/access"
	"github.com/openvaultdb/openvaultdb-go/pkg/auth"
)

// Membership is authoritative current state, loaded anew for each request.
// External OAuth/OIDC bindings remain in the deployment's identity directory.
type Membership struct {
	Roles    []string
	Groups   []string
	Revision string
}
type MembershipResolver func(context.Context, access.PrincipalRef) (Membership, error)

// GrantIdentityConfig maps verified bearer grants to typed identities.
// Bootstrap must come from persistent host configuration. It is never derived
// from a token, generated on startup, or inferred from a provider subject.
type GrantIdentityConfig struct {
	Bootstrap access.PrincipalRef
	Resolve   MembershipResolver
}

// WithGrantIdentity reuses token authentication and capability checks. It adds
// typed subject/actor propagation and current membership resolution; it does
// not validate arbitrary external bearer tokens or create an identity store.
func WithGrantIdentity(config GrantIdentityConfig) Option {
	return WithPrincipalResolver(func(ctx context.Context, authenticated *auth.Principal) (access.Principal, error) {
		if err := config.Bootstrap.Validate(); err != nil {
			return access.Principal{}, err
		}
		if config.Resolve == nil || authenticated == nil {
			return access.Principal{}, fmt.Errorf("trusted identity resolver is required")
		}
		subject, actor := config.Bootstrap, config.Bootstrap
		if !authenticated.Owner {
			grant := authenticated.Grant
			if err := grant.ValidateIdentity(); err != nil {
				return access.Principal{}, err
			}
			if grant.Subject != nil {
				subject, actor = *grant.Subject, *grant.Actor
			} else {
				if grant.PrincipalID == "" {
					return access.Principal{}, fmt.Errorf("legacy grant lacks registered client identity")
				}
				actor = access.PrincipalRef{Realm: config.Bootstrap.Realm, Kind: access.PrincipalKindApplication, ID: grant.PrincipalID}
				subject = actor
			}
		}
		if err := subject.Validate(); err != nil {
			return access.Principal{}, err
		}
		if err := actor.Validate(); err != nil {
			return access.Principal{}, err
		}
		membership, err := config.Resolve(ctx, subject)
		if err != nil {
			return access.Principal{}, err
		}
		return access.Principal{Subject: &subject, Actor: &actor, Roles: membership.Roles, Groups: membership.Groups, MembershipRevision: membership.Revision}, nil
	})
}
