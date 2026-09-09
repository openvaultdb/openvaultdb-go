package core

import (
	"context"
	"fmt"
	"github.com/dal-go/dalgo/access"
	"github.com/dal-go/dalgo/dal"
)

// requesterPolicy adds the authenticated requester's restrictions to the target
// subject's existing secured query. Both predicates reach storage before paging.
type requesterPolicy struct {
	access.Policy
	principal access.Principal
}

func (p requesterPolicy) Decide(ctx context.Context, r access.Request) access.Decision {
	return p.Policy.Decide(access.WithPrincipal(ctx, p.principal), r)
}
func (p requesterPolicy) Authorize(ctx context.Context, r access.Request) error {
	return p.Policy.Authorize(access.WithPrincipal(ctx, p.principal), r)
}

type sampleQuery struct {
	dal.StructuredQuery
	count int
	order []dal.OrderExpression
}

func (q sampleQuery) Limit() int  { return q.count }
func (q sampleQuery) Offset() int { return 0 }
func (q sampleQuery) OrderBy() []dal.OrderExpression {
	return append([]dal.OrderExpression(nil), q.order...)
}
func (q sampleQuery) String() string { return dal.QueryString(q) }

// SelectAccessSample selects only the readable intersection. Its adapters use
// different key expressions; neither stored document id fields nor post-page
// sorting substitute for canonical record identity.
func (d *Database) SelectAccessSample(ctx context.Context, query dal.StructuredQuery, n int, requester access.Principal) ([]Record, []dal.OrderExpression, error) {
	collection, err := validateDTQL(query)
	if err != nil {
		return nil, nil, err
	}
	if n < 1 || n > 100 || query.Offset() != 0 {
		return nil, nil, fmt.Errorf("unsupported sample bounds")
	}
	key := "id"
	switch d.Manifest.Storage.Engine {
	case "sqlite":
	case "ingitdb":
		key = "$id"
	default:
		return nil, nil, fmt.Errorf("sample ordering unsupported")
	}
	order := append([]dal.OrderExpression(nil), query.OrderBy()...)
	hasKey := false
	for i, item := range order {
		field, ok := item.Expression().(dal.FieldRef)
		if !ok || field.Source() != "" {
			return nil, nil, fmt.Errorf("sample requires field ordering")
		}
		if field.Name() == key {
			if i != len(order)-1 {
				return nil, nil, fmt.Errorf("canonical key must be last ordering field")
			}
			hasKey = true
		}
	}
	if !hasKey {
		order = append(order, dal.AscendingField(key))
	}
	if len(order) > 32 {
		return nil, nil, fmt.Errorf("sample order too large")
	}
	readDB := d.db
	if d.HasAccessPolicies() {
		readDB, err = access.SecureDB(readDB, access.WithDatabasePolicyProvider(func(ctx context.Context) ([]access.Policy, error) {
			var policies []access.Policy
			for _, owner := range d.PolicyLayers(ctx) {
				if !owner.Enabled {
					continue
				}
				if owner.Err != nil {
					return nil, owner.Err
				}
				if len(owner.Policies) == 0 {
					return nil, fmt.Errorf("mandatory policy source unavailable")
				}
				for _, policy := range owner.Policies {
					policies = append(policies, requesterPolicy{Policy: policy, principal: requester})
				}
			}
			return policies, nil
		}))
		if err != nil {
			return nil, order, err
		}
	}
	records, err := d.executeDalQueryOn(ctx, readDB, boundedDTQL{sampleQuery{StructuredQuery: query, count: n, order: order}}, collection, false)
	return records, order, err
}
