package manifest

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
)

// maxPostgresNameBytes is how many bytes of a name PostgreSQL keeps. It holds a name
// that is longer by its first bytes, so two long names that differ later address one
// table.
const maxPostgresNameBytes = 63

// plainPostgresName is the shape of the names the PostgreSQL adapter writes into a
// statement that creates a table or a column: ASCII letters, digits and underscores,
// not starting with a digit. The adapter refuses any other name before it sends a
// statement. The rule is the adapter's; TestPostgresNameRuleIsTheAdapters (pkg/mount)
// holds this check equal to the adapter's verdict over a table of names.
var plainPostgresName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

const (
	postgresReasonShape = "a name holds only ASCII letters, digits and underscores and does not start with a digit"
	postgresRefusal     = "cannot be mounted on PostgreSQL"
)

// postgresNameProblem returns why PostgreSQL cannot keep name as it is given, or "".
// The adapter lower-cases a name before it writes it, so the length that counts is
// that of the lower-cased name.
func postgresNameProblem(name string) string {
	if n := len(strings.ToLower(name)); n > maxPostgresNameBytes {
		return fmt.Sprintf("its name is %d bytes and the most is %d", n, maxPostgresNameBytes)
	}
	if !plainPostgresName.MatchString(name) {
		return postgresReasonShape
	}
	return ""
}

// CheckPostgresNames refuses a manifest of a PostgreSQL mount that declares a
// collection or a field name the database cannot keep whole: a name that is not a
// plain identifier, or that is over 63 bytes. Such a name is refused here, before a
// connection is made, so that no table is created for the entries before it. The
// error is one sentence that names the entry and the rule; a name it repeats is
// bounded. The entries are looked at in name order, collections first and then the
// fields of each. A manifest of another engine, or with no schemas, is not refused.
//
// Validate calls it. A mount calls it again for a manifest that was not validated.
func (m *Manifest) CheckPostgresNames() error {
	if m.Storage.Engine != "postgres" || m.Schemas == nil || (m.Storage.Postgres != nil && m.Storage.Postgres.ReadOnly) {
		return nil
	}
	collections := slices.Sorted(maps.Keys(m.Schemas.Collections))
	for _, collection := range collections {
		if reason := postgresNameProblem(collection); reason != "" {
			return fmt.Errorf("schemas.collections: collection %q %s: %s", clipID(collection), postgresRefusal, reason)
		}
	}
	for _, collection := range collections {
		for _, field := range slices.Sorted(maps.Keys(m.Schemas.Collections[collection].Fields)) {
			if reason := postgresNameProblem(field); reason != "" {
				return fmt.Errorf("schemas.collections.%s.fields: field %q %s: %s", clipID(collection), clipID(field), postgresRefusal, reason)
			}
		}
	}
	return nil
}
