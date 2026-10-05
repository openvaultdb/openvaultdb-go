package core

import (
	"context"
	"errors"
	"strconv"

	"github.com/dal-go/dalgo2postgres"
)

// ErrDatabaseUnreachable identifies a failure to reach the database server of a
// mount: the adapter reported a connection that cannot be made or that failed
// while a call was running (a refused or reset connection, a timeout, a failed TLS
// handshake, a server that rejects the connection). The HTTP server answers it
// `503 database_unavailable` on every route that reads or writes the mount, with
// one fixed message, and logs it with the ID of the mount. It is not a mistake of
// the caller and not a fault in the server's own code, and it is always a
// *UnreachableError.
var ErrDatabaseUnreachable = errors.New("the database of the mount cannot be reached")

// UnreachableError is the built error of a mount whose database server cannot be
// reached. It holds the ID of the mount and the adapter's fixed sentence for the
// failure (the adapter's own text of a *dalgo2postgres.ConnectionError, without the
// host, the port and the database name it may add). Nothing of the connection
// string and nothing of the driver's or the server's own text is in it, and it wraps
// no cause, so a log line or an answer that prints it cannot repeat one.
type UnreachableError struct {
	// Database is the ID of the mount.
	Database string
	// Reason is the adapter's fixed sentence, with the SQLSTATE code of a server
	// that answered.
	Reason string
}

func (e *UnreachableError) Error() string {
	return "database " + strconv.Quote(e.Database) + " cannot be reached: " + e.Reason
}

// Is makes errors.Is(err, ErrDatabaseUnreachable) true.
func (e *UnreachableError) Is(target error) bool { return target == ErrDatabaseUnreachable }

// unreachableOf returns the *UnreachableError for err when the adapter reported a
// failed connection anywhere in it, and nil otherwise. The adapter's error names the
// host, the port and the database name when it had checked them; they are dropped
// here, so the sentence is the adapter's fixed one and holds nothing of the
// connection string.
func (d *Database) unreachableOf(err error) *UnreachableError {
	var connection *dalgo2postgres.ConnectionError
	if !errors.As(err, &connection) {
		return nil
	}
	fixed := *connection
	fixed.Host, fixed.Port, fixed.Database = "", "", ""
	return &UnreachableError{Database: d.ID(), Reason: fixed.Error()}
}

// reached returns err, or, when the adapter reported a failed connection in it and
// the request's own context has not ended, the *UnreachableError that says so. A
// call that ended because the request was canceled or ran past its deadline keeps
// the error it had: the context is the reason, not the database.
func (d *Database) reached(ctx context.Context, err error) error {
	if err == nil || ctx.Err() != nil {
		return err
	}
	if unreachable := d.unreachableOf(err); unreachable != nil {
		return unreachable
	}
	return err
}
