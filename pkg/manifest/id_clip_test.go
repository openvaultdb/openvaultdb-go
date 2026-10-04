package manifest

import (
	"fmt"
	"strings"
	"testing"

	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
)

// TestDatabaseIDMessagesClipTheID: a database id longer than the bound is cut
// where an error message repeats it; a short one is shown whole.
func TestDatabaseIDMessagesClipTheID(t *testing.T) {
	long := strings.Repeat("a", 1<<16) + "/"
	err := ValidateID(long)
	if err == nil || len(err.Error()) > 1024 || strings.Contains(err.Error(), strings.Repeat("a", 1000)) {
		t.Fatalf("ValidateID: %d bytes of message: %.120v", len(fmt.Sprint(err)), err)
	}
	if err := ValidateID("bad id"); err == nil || !strings.Contains(err.Error(), `"bad id"`) {
		t.Fatalf("a short id is quoted whole: %v", err)
	}
	// An id at the bound is shown whole; one byte more is cut, with its length.
	atBound := strings.Repeat("a", maxEchoedIDLen-1) + "/"
	if err := ValidateID(atBound); err == nil || !strings.Contains(err.Error(), atBound) {
		t.Fatalf("an id at the bound: %v", err)
	}
	over := strings.Repeat("a", maxEchoedIDLen) + "/"
	if err := ValidateID(over); err == nil || strings.Contains(err.Error(), over) || !strings.Contains(err.Error(), fmt.Sprint(len(over))) {
		t.Fatalf("an id one byte over the bound: %v", err)
	}
	// A multi-byte character is never split.
	wide := "a" + strings.Repeat("\u00e9", maxEchoedIDLen)
	if got := clipID(wide); strings.ContainsRune(got, '\uFFFD') || !strings.HasPrefix(got, "a"+strings.Repeat("\u00e9", maxEchoedIDLen/2-1)) {
		t.Fatalf("clipped = %q", got)
	}
	// The same id in a manifest.
	m := &Manifest{Database: Database{ID: long, SchemaMode: schema.ModeSchemaless}, Storage: Storage{Engine: "ingitdb", Path: "./data"}}
	if err := m.Validate(); err == nil || len(err.Error()) > 1024 {
		t.Fatalf("Validate: %d bytes of message", len(fmt.Sprint(err)))
	}
}
