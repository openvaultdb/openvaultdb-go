package core

import (
	"fmt"
	"strings"
	"testing"
)

// RelationalBounds states the numbers the classifier enforces: a document at each
// bound is classified and one just over it is refused.
func TestRelationalBoundsAreTheOnesTheClassifierEnforces(t *testing.T) {
	bounds := RelationalBounds()
	if bounds.MaxSources < 2 || bounds.MaxSubqueryDepth < 1 || bounds.MaxLimit < 1 || bounds.MaxOffset < 1 {
		t.Fatalf("bounds = %+v", bounds)
	}
	sources := func(n int) string {
		var b strings.Builder
		b.WriteString("from:\n  name: t0\n  alias: a0\n  joins:\n")
		for i := 1; i < n; i++ {
			fmt.Fprintf(&b, "    - from: {name: t%d, alias: a%d}\n      on: [{left: {field: id, source: a0}, op: '==', right: {field: id, source: a%d}}]\n", i, i, i)
		}
		return b.String()
	}
	depth := func(levels int) string {
		doc := "from: {name: leaf, alias: l}\n"
		for i := 0; i < levels; i++ {
			doc = fmt.Sprintf("from:\n  query:\n    as: q%d\n%s", i, indent(doc, "    "))
		}
		return doc
	}
	for _, tc := range []struct{ name, atBound, over string }{
		{"sources", sources(bounds.MaxSources), sources(bounds.MaxSources + 1)},
		{"subquery depth", depth(bounds.MaxSubqueryDepth), depth(bounds.MaxSubqueryDepth + 1)},
		{"limit", fmt.Sprintf("from: {name: a}\nlimit: %d\n", bounds.MaxLimit), fmt.Sprintf("from: {name: a}\nlimit: %d\n", bounds.MaxLimit+1)},
		{"offset", fmt.Sprintf("from: {name: a}\noffset: %d\n", bounds.MaxOffset), fmt.Sprintf("from: {name: a}\noffset: %d\n", bounds.MaxOffset+1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ClassifyDTQL(mustDeserialize(t, tc.atBound)); err != nil {
				t.Errorf("at the bound: %v", err)
			}
			if _, err := ClassifyDTQL(mustDeserialize(t, tc.over)); err == nil {
				t.Error("just over the bound: classified")
			}
		})
	}
}
