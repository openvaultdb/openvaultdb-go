package manifest

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestParseErrorDescribesEachMistakeByItsLineAndKind: the errors of the YAML
// decoder are reported by line and kind, whatever text they quote, and a scanner or
// parser error is wrapped as it is.
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
	scanner := errors.New("yaml: line 3: did not find expected key")
	if got := parseError(scanner); !errors.Is(got, scanner) || got.Error() != "failed to parse manifest YAML: "+scanner.Error() {
		t.Errorf("a scanner or parser error: %v", got)
	}
}

// TestParseErrorOfAnyOtherDecoderErrorIsOneFixedSentence: an error of the decoder
// that is neither a type error nor a scanner or parser error (the text of which
// is fixed and starts with the line) can hold the text of the document, so it is
// replaced by one sentence that wraps nothing.
func TestParseErrorOfAnyOtherDecoderErrorIsOneFixedSentence(t *testing.T) {
	const want = "failed to parse manifest YAML: the document could not be decoded"
	for _, c := range []struct{ name, text string }{
		{"a value that cannot be decoded as its tag", "yaml: cannot decode !!str `secret` as a !!int"},
		{"an unknown anchor", "yaml: unknown anchor 'secret' referenced"},
		{"an anchor that holds itself", "yaml: anchor 'secret' value contains itself"},
		{"a map key that cannot be hashed", "yaml: invalid map key: []interface {}{\"secret\"}"},
		{"a text that only looks like a scanner error", "line 3: secret"},
		{"a text that has the prefix of a scanner error without a line number", "yaml: line x: secret"},
		{"a text that has the prefix of a scanner error in the middle", "secret yaml: line 3: secret"},
		{"an empty text", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			cause := errors.New(c.text)
			got := parseError(cause)
			if got.Error() != want {
				t.Errorf("got  %q\nwant %q", got.Error(), want)
			}
			if errors.Is(got, cause) || errors.Unwrap(got) != nil {
				t.Errorf("the error wraps the decoder's: %v", errors.Unwrap(got))
			}
			if strings.Contains(fmt.Sprintf("%+v", got), "secret") {
				t.Errorf("the error repeats the text the decoder quoted: %+v", got)
			}
		})
	}
}
