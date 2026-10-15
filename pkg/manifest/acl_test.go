package manifest

import "testing"

func TestACLModeMustBeExplicit(t *testing.T) {
	t.Parallel()
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
	if _, err := Parse([]byte(base + "acl: {enabled: false}\nacl_store: {path: store}\n")); err == nil {
		t.Error("accepted acl_store with disabled acl")
	}
	if _, err := Parse([]byte(base + "acl: {enabled: true}\nacl_store: {path: store}\n")); err != nil {
		t.Errorf("valid acl_store rejected: %v", err)
	}
}
