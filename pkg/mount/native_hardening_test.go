package mount

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/openvaultdb/openvaultdb-go/pkg/manifest"
	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
)

// testRootCert writes a self-signed CA certificate and returns its path, for an
// sslrootcert parameter.
func testRootCert(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "ovdb test CA"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "root.crt")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestNativePostgresRequiresAVerifiedTLSConnection(t *testing.T) {
	t.Setenv("PGSSLMODE", "")
	t.Setenv("PGSSLROOTCERT", "")
	root := testRootCert(t)
	secure := nativeSettingsOf(Options{})
	for _, tc := range []struct {
		dsn string
		ok  bool
	}{
		{"postgres://reader@db.example.test/samples?sslmode=verify-full", true},
		{"postgres://reader@db.example.test/samples?sslmode=verify-ca", true},
		{"postgres://reader@db.example.test/samples?sslmode=require&sslrootcert=" + root, true},
		{"postgres://reader@db.example.test/samples?sslrootcert=system", true},
		{"host=db.example.test user=reader dbname=samples sslmode=verify-full", true},
		{"postgres://reader@db.example.test/samples?sslmode=require", false},
		{"postgres://reader@db.example.test/samples?sslmode=prefer", false},
		{"postgres://reader@db.example.test/samples", false},
		{"postgres://reader@db.example.test/samples?sslmode=allow", false},
		{"postgres://reader@db.example.test/samples?sslmode=disable", false},
		{"host=/tmp user=reader dbname=samples", false},
	} {
		got, err := postgresReadOnlyDSN(tc.dsn, secure)
		if tc.ok != (err == nil) {
			t.Errorf("%s: %q, %v; want accepted=%v", tc.dsn, got, err, tc.ok)
			continue
		}
		if err != nil && (!strings.Contains(err.Error(), "verify-full") || strings.Contains(err.Error(), "reader")) {
			t.Errorf("%s: refusal %q does not name the requirement or repeats the DSN", tc.dsn, err)
		}
	}
	// A verified first attempt with a plaintext fallback is refused too.
	t.Setenv("PGSSLMODE", "")
	if _, err := postgresReadOnlyDSN("postgres://reader@a.example.test,b.example.test/samples?sslmode=verify-full", secure); err != nil {
		t.Fatalf("two verified hosts: %v", err)
	}
	if _, err := postgresReadOnlyDSN("postgres://reader@db.example.test/samples?sslmode=disable", insecureNativeSettings); err != nil {
		t.Fatalf("the insecure option allows a plain connection: %v", err)
	}
}

func TestNativeSettingsDefaultsAndStatementTimeout(t *testing.T) {
	defaults := nativeSettingsOf(Options{})
	if defaults.statementTimeout != DefaultNativePostgresStatementTimeout || defaults.discoveryTimeout != DefaultNativePostgresDiscoveryTimeout || defaults.allowInsecure || defaults.requireReadOnlyRole {
		t.Fatalf("defaults = %+v", defaults)
	}
	set := nativeSettingsOf(Options{NativePostgresStatementTimeout: 1500 * time.Millisecond, NativePostgresDiscoveryTimeout: time.Second, RequireReadOnlyNativePostgresRole: true})
	if set.statementTimeout != 1500*time.Millisecond || set.discoveryTimeout != time.Second || !set.requireReadOnlyRole {
		t.Fatalf("settings = %+v", set)
	}
	set.allowInsecure = true
	got, err := postgresReadOnlyDSN("postgres://reader@db.example.test/samples?statement_timeout=0", set)
	if err != nil || !strings.Contains(got, "statement_timeout=1500") || strings.Contains(got, "statement_timeout=0") {
		t.Fatalf("dsn = %q, %v; want the mount's statement_timeout to win", got, err)
	}
}

type fakeAccess struct {
	reasons []string
	err     error
	got     [3][]string
}

func (f *fakeAccess) SelectableRelations(context.Context, string) (map[string]bool, error) {
	return nil, nil
}

func (f *fakeAccess) WriteCapabilities(_ context.Context, schemas, relationSchemas, relationNames []string) ([]string, error) {
	f.got = [3][]string{schemas, relationSchemas, relationNames}
	return f.reasons, f.err
}

func (f *fakeAccess) Close() error { return nil }

func TestRefuseWritableRole(t *testing.T) {
	exposed := &schema.Schemas{Collections: map[string]schema.Collection{}}
	for _, source := range []schema.NativeCollectionSource{{Schema: "s", Name: "b"}, {Schema: "s", Name: "a"}, {Schema: "t", Name: "c"}} {
		id, _ := schema.NativePostgresCollectionID(source.Schema, source.Name)
		exposed.Collections[id] = schema.Collection{Source: &source}
	}
	readOnly := &fakeAccess{}
	if err := refuseWritableRole(context.Background(), readOnly, exposed); err != nil {
		t.Fatal(err)
	}
	if strings.Join(readOnly.got[0], ",") != "s,t" || len(readOnly.got[1]) != 3 || len(readOnly.got[2]) != 3 {
		t.Fatalf("asked about %v", readOnly.got)
	}
	err := refuseWritableRole(context.Background(), &fakeAccess{reasons: []string{"z reason", "a reason"}}, exposed)
	if err == nil || !strings.Contains(err.Error(), "not read-only: a reason; z reason;") {
		t.Fatalf("error = %v", err)
	}
	boom := errors.New("boom")
	if err := refuseWritableRole(context.Background(), &fakeAccess{err: boom}, exposed); !errors.Is(err, boom) {
		t.Fatalf("error = %v", err)
	}
}

func TestOpenPostgresNativeRefusesBeforeConnectingWithoutTLS(t *testing.T) {
	t.Setenv("PGSSLMODE", "")
	t.Setenv("TEST_NATIVE_TLS_DSN", "postgres://reader@db.example.test/samples?sslmode=disable")
	m := &manifest.Manifest{
		Database: manifest.Database{ID: "samples"},
		Storage:  manifest.Storage{Engine: "postgres", Postgres: &manifest.PostgresOptions{DSNEnv: "TEST_NATIVE_TLS_DSN", ReadOnly: true}},
	}
	if _, _, err := openPostgresWithOptions(m, Options{}); err == nil || !strings.Contains(err.Error(), "verify-full") {
		t.Fatalf("error = %v, want the TLS requirement", err)
	}
}
