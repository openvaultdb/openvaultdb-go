package manifest_test

import (
	"strings"
	"testing"

	"github.com/openvaultdb/openvaultdb-go/pkg/manifest"
)

// TestParseErrorOfAValueOfTheWrongTypeDoesNotRepeatTheManifest: a manifest that
// the YAML decoder cannot decode (a value of the wrong type, a field the manifest
// does not have, a key written twice) is refused with an error that says which
// line and what kind of mistake, and nothing of what the line holds: a connection
// string written one level up, as the value of storage.postgres or storage.mysql,
// or as a field name, is not repeated in whole or in part.
func TestParseErrorOfAValueOfTheWrongTypeDoesNotRepeatTheManifest(t *testing.T) {
	const head = "database: {id: sqlmount, schema_mode: strict}\nstorage:\n  engine: postgres\n"
	for _, c := range []struct {
		name, doc, want string
	}{
		{"a connection string as the options of postgres", head + "  postgres: 'postgres://app:pw-MARKER-31c8@db.example.test/orders'\n", "line 4: a value of the wrong type"},
		{"a go-sql-driver string as the options of mysql", head + "  mysql: 'app:pw-MARKER-31c8@tcp(db.example.test:3306)/orders'\n", "line 4: a value of the wrong type"},
		{"a value short enough to be quoted whole", head + "  postgres: pw-MARKER\n", "line 4: a value of the wrong type"},
		{"a connection string as a field name", head + "  postgres: {postgres://app:pw-MARKER-31c8@db.example.test/orders}\n", "line 4: a field the manifest does not have"},
		{"a field name that is not a secret", head + "  bogus: 1\n", "line 4: a field the manifest does not have"},
		{"a key written twice", "database: {id: sqlmount, schema_mode: strict}\nstorage:\n  engine: sqlite\n  path: x\n  path: y\n", "line 5: a key written twice"},
		{"a list where a string goes", "database: {id: sqlmount, schema_mode: strict}\nstorage:\n  engine: sqlite\n  path: [a]\n", "line 4: a value of the wrong type"},
		{"a value of the wrong type in the schemas", head + "schemas: postgres://app:pw-MARKER-31c8@db.example.test/orders\n", "line 4: a value of the wrong type"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := manifest.Parse([]byte(c.doc))
			if err == nil {
				t.Fatal("a manifest the decoder cannot decode is accepted")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("the message does not say %q: %v", c.want, err)
			}
			for _, repeated := range []string{"MARKER", "app:", "postgre", "bogus", "path", "tcp", "example", "cannot unmarshal", "manifest."} {
				if strings.Contains(err.Error(), repeated) {
					t.Errorf("the message repeats %q: %v", repeated, err)
				}
			}
		})
	}
	t.Run("several mistakes", func(t *testing.T) {
		_, err := manifest.Parse([]byte(head + "  postgres: pw-MARKER\n  bogus: 1\n"))
		if err == nil || !strings.Contains(err.Error(), "line 4: a value of the wrong type") || !strings.Contains(err.Error(), "line 5: a field the manifest does not have") {
			t.Errorf("got %v", err)
		}
	})
	t.Run("a syntax error is still reported", func(t *testing.T) {
		_, err := manifest.Parse([]byte("database: {id: sqlmount\n"))
		if err == nil || !strings.Contains(err.Error(), "failed to parse manifest YAML") {
			t.Errorf("got %v", err)
		}
	})
}
