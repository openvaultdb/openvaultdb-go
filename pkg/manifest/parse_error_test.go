package manifest_test

import (
	"errors"
	"fmt"
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
		if err == nil || !strings.Contains(err.Error(), "failed to parse manifest YAML: yaml: line ") {
			t.Errorf("got %v", err)
		}
	})
}

// TestParseErrorOfADocumentTheDecoderRefusesForAnotherReasonDoesNotRepeatIt: the
// decoder quotes the document in some errors that are not about the type of a
// value (a scalar written with an explicit tag, an anchor that is not defined or
// that holds itself, a key that cannot be hashed). The error that Parse gives for
// each holds none of the document: not in its message, not in its detail, and not
// in an error that it wraps.
func TestParseErrorOfADocumentTheDecoderRefusesForAnotherReasonDoesNotRepeatIt(t *testing.T) {
	const head = "database: {id: sqlmount, schema_mode: strict}\nstorage:\n  engine: postgres\n"
	for _, c := range []struct{ name, doc string }{
		{"a connection string with an explicit tag", head + "  postgres:\n    dsn_env: !!int postgres://app:pw-MARKER-31c8@db/x\n"},
		{"a token variable with an explicit tag", head + "  ingitdb:\n    github:\n      token_env: !!int ghp_MARKER\n"},
		{"an anchor that is not defined", head + "  postgres:\n    dsn_env: *MARKER\n"},
		{"a token variable that is an undefined anchor", "database: {id: sqlmount, schema_mode: strict}\nstorage:\n  engine: ingitdb\n  ingitdb:\n    github:\n      token_env: *MARKER\n"},
		{"an anchor that holds itself", head + "  postgres: &MARKER\n    dsn_env: [*MARKER]\n"},
		{"a key that cannot be hashed", head + "  postgres:\n    ? [pw-MARKER]\n    : x\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := manifest.Parse([]byte(c.doc))
			if err == nil {
				t.Fatal("a manifest the decoder cannot decode is accepted")
			}
			for _, shown := range []string{err.Error(), fmt.Sprintf("%+v", err), fmt.Sprintf("%#v", err)} {
				if strings.Contains(shown, "MARKER") || strings.Contains(shown, "app:") {
					t.Errorf("the error repeats the document: %s", shown)
				}
			}
			for wrapped := errors.Unwrap(err); wrapped != nil; wrapped = errors.Unwrap(wrapped) {
				if strings.Contains(wrapped.Error(), "MARKER") {
					t.Errorf("the error wraps one that repeats the document: %v", wrapped)
				}
			}
			if !strings.HasPrefix(err.Error(), "failed to parse manifest YAML") {
				t.Errorf("got %v", err)
			}
		})
	}
}
