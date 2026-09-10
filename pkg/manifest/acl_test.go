package manifest

import "testing"

func TestACLModeMustBeExplicit(t *testing.T) {
	base := "database: {id: crm, schema_mode: schemaless}\nstorage: {engine: ingitdb, path: data}\n"
	for _, acl := range []string{"acl: {}\n", "acl: null\n", "acl: {policies: [p.yaml]}\n", "acl: {enabled: null}\n", "---\nacl: {enabled: false}\n"} {
		if _, err := Parse([]byte(base + acl)); err == nil {
			t.Errorf("accepted incomplete ACL: %s", acl)
		}
	}
	for _, acl := range []string{"", "acl: {enabled: false}\n", "acl: {enabled: true, policies: [p.yaml]}\n"} {
		if _, err := Parse([]byte(base + acl)); err != nil {
			t.Errorf("valid ACL mode: %v", err)
		}
	}
}
