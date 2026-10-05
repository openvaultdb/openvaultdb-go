package manifest

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestParseErrorDescribesEachMistakeByItsLineAndKind: the errors of the YAML
// decoder are reported by line and kind, whatever text they quote, and any other
// error is wrapped as it is.
func TestParseErrorDescribesEachMistakeByItsLineAndKind(t *testing.T) {
	typed := func(texts ...string) error { return &yaml.TypeError{Errors: texts} }
	long := make([]string, 0, maxParseProblems+3)
	for i := 1; i <= maxParseProblems+3; i++ {
		long = append(long, fmt.Sprintf("line %d: cannot unmarshal !!str `x` into string", i))
	}
	for _, c := range []struct {
		name string
		err  error
		want string
	}{
		{"a value of the wrong type", typed("line 4: cannot unmarshal !!str `secret` into manifest.PostgresOptions"), "failed to parse manifest YAML: line 4: a value of the wrong type"},
		{"a field the manifest does not have", typed("line 7: field secret not found in type manifest.Storage"), "failed to parse manifest YAML: line 7: a field the manifest does not have"},
		{"a key written twice", typed(`line 9: mapping key "secret" already defined at line 8`), "failed to parse manifest YAML: line 9: a key written twice"},
		{"a mistake with no line", typed("cannot unmarshal !!str `secret` into string"), "failed to parse manifest YAML: a value of the wrong type"},
		{"several mistakes", typed("line 2: field a not found in type T", "line 3: cannot unmarshal !!seq into string"), "failed to parse manifest YAML: line 2: a field the manifest does not have; line 3: a value of the wrong type"},
		{"more mistakes than are listed", typed(long...), "failed to parse manifest YAML: " + "line 1: a value of the wrong type; line 2: a value of the wrong type; line 3: a value of the wrong type; line 4: a value of the wrong type; line 5: a value of the wrong type; line 6: a value of the wrong type; line 7: a value of the wrong type; line 8: a value of the wrong type; line 9: a value of the wrong type; line 10: a value of the wrong type; and more"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := parseError(c.err)
			if got.Error() != c.want {
				t.Errorf("got  %q\nwant %q", got.Error(), c.want)
			}
			if strings.Contains(got.Error(), "secret") {
				t.Errorf("the message repeats the text the decoder quoted: %v", got)
			}
		})
	}
	other := errors.New("yaml: line 3: did not find expected key")
	if got := parseError(other); !errors.Is(got, other) || got.Error() != "failed to parse manifest YAML: "+other.Error() {
		t.Errorf("an error that is not a type error: %v", got)
	}
}
