package core

import (
	"context"
	"errors"
	"fmt"

	"github.com/dal-go/dalgo/dal"
)

// serverEngines lists the engines whose adapter reaches a database server through
// a connection string. The error of a collection that one of them cannot
// provision is built here (see provisionError) and never wraps the adapter's.
var serverEngines = map[string]bool{"postgres": true, "mysql": true}

// provisionError is the error of a collection that a server database could not
// provision: a fixed sentence and the collection name. It wraps nothing, and no
// text of the adapter's error is used. The sentence is chosen from the type of
// that error, never from its text: one that is a cancellation or a deadline, and
// one that is the adapter's refusal of an operation it does not support
// (dal.ErrNotSupported, as *dbschema.NotSupportedError is); any other error
// gets the general sentence.
func provisionError(collection string, err error) error {
	reason := "the table could not be created"
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		reason = "the request timed out or was canceled"
	case errors.Is(err, dal.ErrNotSupported):
		reason = "the adapter does not support it"
	}
	return fmt.Errorf("failed to ensure collection %q: %s", clipName(collection), reason)
}
