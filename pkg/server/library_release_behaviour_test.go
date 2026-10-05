package server_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// TestAScanOrderOverAHiddenFieldIsRefusedOnALayeredACLMount: a field list of a
// policy holds the fields of a scan order as it holds those of a join condition
// (dalgo v0.89.6). On this server a scan bound never reaches the library: the
// relational profile refuses it on every mount, with the 400 invalid_dtql a
// mount without policies gives, so a caller learns nothing from the policy. The
// hidden field of a join condition is held by the shapes of
// TestRelationalShapesOverALayeredACLMountAreRefusedWithOneAnswer (the join ON
// names country, which the policy hides): each is the one answer of a relational
// document on a mount with policies.
func TestAScanOrderOverAHiddenFieldIsRefusedOnALayeredACLMount(t *testing.T) {
	base := relIntLayeredServer(t)
	for _, protected := range []string{"sqlite", "ingitdb"} {
		for _, shape := range []struct{ name, doc string }{
			{"hidden field, ascending", "from: {name: customers, scan: {limit: 5, orderBy: [{field: secret}]}}\ncolumns: [{field: name}]\n"},
			{"hidden field, descending", "from: {name: customers, scan: {limit: 5, orderBy: [{field: secret, desc: true}]}}\ncolumns: [{field: name}]\n"},
			{"visible field", "from: {name: customers, scan: {limit: 5, orderBy: [{field: name}]}}\ncolumns: [{field: name}]\n"},
		} {
			for _, caller := range relIntCallers {
				t.Run(fmt.Sprintf("%s, %s, %s", protected, shape.name, caller.name), func(t *testing.T) {
					resp := relHTTPPost(t, base, "/v1/databases/"+protected+"/dtql", caller.token, shape.doc)
					if resp.status != http.StatusBadRequest || resp.errorField("code") != "invalid_dtql" || resp.body["records"] != nil {
						t.Fatalf("status %d, want a 400 invalid_dtql with no rows: %s", resp.status, resp.raw)
					}
					for _, leak := range []string{"secret", "hidden", "Customer 0"} {
						if strings.Contains(resp.raw, leak) {
							t.Fatalf("the answer repeats %q: %s", leak, resp.raw)
						}
					}
				})
			}
		}
	}
}
