package manifest_test

import (
	"fmt"
	"github.com/openvaultdb/openvaultdb-go/pkg/manifest"
	"testing"
)

func TestSQLiteBusyTimeoutPresenceAndBounds(t *testing.T) {
	base := "database: {id: w1, schema_mode: strict}\nstorage: {engine: sqlite, path: data.sqlite%s}\nschemas: {collections: {things: {fields: {key: {type: string}}}}}\n"
	for _, tc := range []struct {
		option string
		valid  bool
	}{
		{"", true}, {", sqlite: {}", true}, {", sqlite: {busy_timeout: 0s}", true}, {", sqlite: {busy_timeout: 5s}", true}, {", sqlite: {busy_timeout: 1ms}", true},
		{", sqlite: {busy_timeout: -1ms}", false}, {", sqlite: {busy_timeout: 5001ms}", false}, {", sqlite: {busy_timeout: 1us}", false}, {", sqlite: {busy_timeout: invalid}", false}, {", sqlite: {record_keys: {}}", false}, {", sqlite: {record_keys: null}", false}, {", sqlite: {busy_timeout: null}", false}, {", sqlite: {busy_timeout: 0}", false}, {", sqlite: null", false},
	} {
		t.Run(tc.option, func(t *testing.T) {
			_, err := manifest.Parse([]byte(fmt.Sprintf(base, tc.option)))
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v err=%v", tc.valid, err)
			}
		})
	}
	_, err := manifest.Parse([]byte("database: {id: w1, schema_mode: strict}\nstorage: {engine: postgres, sqlite: {busy_timeout: 0s}}\nschemas: {collections: {things: {fields: {key: {type: string}}}}}\n"))
	if err == nil {
		t.Fatal("non-SQLite options accepted")
	}
}
