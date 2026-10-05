package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo/dbschema"
	"github.com/dal-go/dalgo/ddl"

	"github.com/openvaultdb/openvaultdb-go/pkg/manifest"
	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
)

const (
	provisionMarkerUser = "svc-MARKER-user-4c1d"
	provisionMarkerHost = "db-MARKER-host-9e2b.example.test"
)

// provisionDB is a dal.DB whose only behaviour is a CreateCollection that fails
// with err; any other method panics (nil embedded values).
type provisionDB struct {
	dal.DB
	ddl.SchemaModifier
	err error
}

func (f provisionDB) CreateCollection(context.Context, dbschema.CollectionDef, ...ddl.Option) error {
	return f.err
}

// driverConnectError is a driver error that holds a user name and a host and
// wraps a cause that callers can match.
type driverConnectError struct{ cause error }

func (e *driverConnectError) Error() string {
	return "failed to connect to `user=" + provisionMarkerUser + " database=orders host=" + provisionMarkerHost + "`: " + e.cause.Error()
}

func (e *driverConnectError) Unwrap() error { return e.cause }

var errProvisionCause = errors.New("server closed the connection")

// provisionChain is every error reachable from err through errors.Unwrap and
// the Unwrap() []error form, err itself first.
func provisionChain(err error) []error {
	var out []error
	var walk func(error)
	walk = func(e error) {
		if e == nil {
			return
		}
		out = append(out, e)
		switch u := e.(type) {
		case interface{ Unwrap() error }:
			walk(u.Unwrap())
		case interface{ Unwrap() []error }:
			for _, inner := range u.Unwrap() {
				walk(inner)
			}
		}
	}
	walk(err)
	return out
}

func openProvisioning(engine string, create error) error {
	m := &manifest.Manifest{
		Database: manifest.Database{ID: "provision", SchemaMode: schema.ModeStrict},
		Storage:  manifest.Storage{Engine: engine},
		Schemas: &schema.Schemas{Collections: map[string]schema.Collection{
			"orders":    {Fields: map[string]schema.Field{"total": {Type: schema.TypeString}}},
			"customers": {Fields: map[string]schema.Field{"name": {Type: schema.TypeString}}},
		}},
	}
	_, err := Open(m, provisionDB{err: create}, []schema.Mode{schema.ModeStrict}, "")
	return err
}

// TestServerMountProvisioningErrorIsBuilt: a postgres or mysql mount that opens
// and then fails while it provisions a declared collection returns an error built
// here, a fixed sentence and the collection name. It wraps nothing, and nothing of
// the adapter's error is returned, printed in any form or reachable through
// errors.Unwrap, errors.Is or errors.As. The sentence is chosen from the type of
// the adapter's error, never from its text.
func TestServerMountProvisioningErrorIsBuilt(t *testing.T) {
	type typed struct{ error }
	for _, engine := range []string{"postgres", "mysql"} {
		for _, c := range []struct {
			name   string
			create error
			want   string
		}{
			{"an error that names the user and the host",
				&driverConnectError{errProvisionCause}, `failed to ensure collection "customers": the table could not be created`},
			{"an error that wraps another one",
				fmt.Errorf("dalgo2sql: CreateCollection exec: %w", &driverConnectError{errProvisionCause}), `failed to ensure collection "customers": the table could not be created`},
			{"an error that is a deadline",
				&driverConnectError{context.DeadlineExceeded}, `failed to ensure collection "customers": the request timed out or was canceled`},
			{"an error that is a cancellation",
				&driverConnectError{context.Canceled}, `failed to ensure collection "customers": the request timed out or was canceled`},
			{"an operation the adapter does not support",
				&dbschema.NotSupportedError{Op: "CreateCollection", Backend: provisionMarkerHost, Reason: provisionMarkerUser}, `failed to ensure collection "customers": the adapter does not support it`},
			{"an error whose text is not an error of the driver",
				typed{errors.New(provisionMarkerUser + "@" + provisionMarkerHost)}, `failed to ensure collection "customers": the table could not be created`},
		} {
			t.Run(engine+", "+c.name, func(t *testing.T) {
				err := openProvisioning(engine, c.create)
				if err == nil {
					t.Fatal("want an error")
				}
				if err.Error() != c.want {
					t.Errorf("got %q, want %q", err.Error(), c.want)
				}
				if errors.Unwrap(err) != nil || len(provisionChain(err)) != 1 {
					t.Errorf("the error reaches %d errors through Unwrap", len(provisionChain(err))-1)
				}
				if errors.Is(err, errProvisionCause) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) || errors.Is(err, dal.ErrNotSupported) || errors.Is(err, c.create) {
					t.Errorf("errors.Is reaches the adapter's error: %v", err)
				}
				var connect *driverConnectError
				var unsupported *dbschema.NotSupportedError
				var wrapper typed
				if errors.As(err, &connect) || errors.As(err, &unsupported) || errors.As(err, &wrapper) {
					t.Errorf("errors.As reaches the adapter's error: %v", err)
				}
				for _, form := range []string{err.Error(), fmt.Sprintf("%v", err), fmt.Sprintf("%+v", err), fmt.Sprintf("%#v", err), fmt.Sprintf("%q", err)} {
					for _, marker := range []string{provisionMarkerUser, provisionMarkerHost, "svc-", "MARKER", "server closed"} {
						if strings.Contains(form, marker) {
							t.Errorf("%q holds %q", form, marker)
						}
					}
				}
			})
		}
	}
}

// TestOtherEnginesProvisioningErrorWrapsTheAdaptersError: the provisioning error
// of the engines that keep their data in a file or a directory wraps the adapter's
// error.
func TestOtherEnginesProvisioningErrorWrapsTheAdaptersError(t *testing.T) {
	for _, engine := range []string{"sqlite", "ingitdb"} {
		t.Run(engine, func(t *testing.T) {
			err := openProvisioning(engine, errProvisionCause)
			if !errors.Is(err, errProvisionCause) || !strings.Contains(err.Error(), `failed to ensure collection "customers"`) {
				t.Errorf("got %v", err)
			}
		})
	}
}
