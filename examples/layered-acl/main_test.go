package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dal-go/dalgo/access"
	"github.com/openvaultdb/openvaultdb-go/pkg/auth"
	"github.com/openvaultdb/openvaultdb-go/pkg/core"
)

func resetSeams(t *testing.T) {
	t.Helper()
	origLogFatalf := logFatalf
	origMkdirTemp := mkdirTemp
	origReadDir := readDir
	origWriteFile := writeFile
	origMountFile := mountFile
	origCoreApply := coreApply
	origStoreCreateGrant := storeCreateGrant
	origServeHTTP := serveHTTP
	t.Cleanup(func() {
		logFatalf = origLogFatalf
		mkdirTemp = origMkdirTemp
		readDir = origReadDir
		writeFile = origWriteFile
		mountFile = origMountFile
		coreApply = origCoreApply
		storeCreateGrant = origStoreCreateGrant
		serveHTTP = origServeHTTP
	})
}

func TestMainAndRunLifecycle(t *testing.T) {
	// Not parallel: mutates os.Args, flag.CommandLine, env vars, and package-level seams.
	resetSeams(t)

	// Cover default serveHTTP seam on an invalid address so it returns immediately.
	if err := serveHTTP(&http.Server{Addr: "127.0.0.1:-1"}); err == nil {
		t.Fatal("expected serveHTTP error on invalid address")
	}

	origArgs := os.Args
	origCommandLine := flag.CommandLine
	t.Cleanup(func() {
		os.Args = origArgs
		flag.CommandLine = origCommandLine
	})

	// 1. main() logs fatal when credentials are missing.
	t.Setenv("OVDB_OWNER_TOKEN", "")
	t.Setenv("OVDB_QUERY_TOKEN", "")
	flag.CommandLine = flag.NewFlagSet("layered-acl", flag.ContinueOnError)
	os.Args = []string{"layered-acl"}
	var fatalMsg string
	logFatalf = func(format string, v ...any) {
		fatalMsg = fmt.Sprintf(format, v...)
	}
	main()
	if !strings.Contains(fatalMsg, "OVDB_OWNER_TOKEN") {
		t.Fatalf("unexpected fatalMsg: %q", fatalMsg)
	}

	// 2. Full seed + serve + reuse flow using default temp dir creation.
	t.Setenv("OVDB_OWNER_TOKEN", "owner-secret-token")
	t.Setenv("OVDB_QUERY_TOKEN", "query-secret-token")
	tempRoot := t.TempDir()
	var createdDir string
	mkdirTemp = func(_ string, pattern string) (string, error) {
		d, err := os.MkdirTemp(tempRoot, pattern)
		createdDir = d
		return d, err
	}
	var capturedHandler http.Handler
	serveHTTP = func(srv *http.Server) error {
		capturedHandler = srv.Handler
		return nil
	}
	if err := run("", false, "127.0.0.1:0"); err != nil {
		t.Fatalf("run seed failed: %v", err)
	}
	if createdDir == "" || capturedHandler == nil {
		t.Fatalf("expected createdDir and capturedHandler")
	}

	// Exercise the Resolve callback for matching subject ("demo-user") via queryToken,
	// and for non-matching subject via a second grant.
	req := httptest.NewRequest(http.MethodPost, "/v1/databases/sqlite/dtql", bytes.NewBufferString(`{"query":"from: {name: customers}\n"}`))
	req.Header.Set("Authorization", "Bearer query-secret-token")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	capturedHandler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for queryToken, got %d: %s", rec.Code, rec.Body.String())
	}

	// Issue a second grant for a different subject to cover the non-matching branch in Resolve.
	store, err := auth.OpenStore(filepath.Join(createdDir, "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	otherSubj := access.PrincipalRef{Realm: "local-demo", Kind: access.PrincipalKindUser, ID: "other-user"}
	otherActor := access.PrincipalRef{Realm: "local-demo", Kind: access.PrincipalKindApplication, ID: "datatug-demo"}
	if err := store.CreateGrant(&auth.Grant{
		Subject:      &otherSubj,
		Actor:        &otherActor,
		Capabilities: []auth.Capability{{Action: auth.CapRecordsRead, Collection: "customers"}},
	}, "other-query-token"); err != nil {
		t.Fatal(err)
	}

	// Re-run with reuse=true and existing queryToken.
	if err := run(createdDir, true, "127.0.0.1:0"); err != nil {
		t.Fatalf("run reuse failed: %v", err)
	}
	reqOther := httptest.NewRequest(http.MethodPost, "/v1/databases/sqlite/dtql", bytes.NewBufferString(`{"query":"FROM customers SELECT id, name"}`))
	reqOther.Header.Set("Authorization", "Bearer other-query-token")
	reqOther.Header.Set("Content-Type", "application/json")
	recOther := httptest.NewRecorder()
	capturedHandler.ServeHTTP(recOther, reqOther)

	// Reuse with missing query credential must fail.
	t.Setenv("OVDB_QUERY_TOKEN", "unknown-query-token")
	if err := run(createdDir, true, "127.0.0.1:0"); err == nil || !strings.Contains(err.Error(), "reuse does not issue credentials") {
		t.Fatalf("expected reuse missing credential error, got: %v", err)
	}
}

func TestRunAndSeedErrorBranches(t *testing.T) {
	// Not parallel: mutates env vars and package-level seams.
	resetSeams(t)
	t.Setenv("OVDB_OWNER_TOKEN", "owner-token")
	t.Setenv("OVDB_QUERY_TOKEN", "query-token")
	serveHTTP = func(*http.Server) error { return nil }

	// 1. mkdirTemp error
	mkdirTemp = func(string, string) (string, error) {
		return "", errors.New("mkdirtemp boom")
	}
	if err := run("", false, "127.0.0.1:0"); err == nil || !strings.Contains(err.Error(), "mkdirtemp boom") {
		t.Fatalf("expected mkdirtemp error, got: %v", err)
	}

	// 2. os.MkdirAll(dir) error (dir is an existing file)
	fileAsDir := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(fileAsDir, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := run(fileAsDir, false, "127.0.0.1:0"); err == nil {
		t.Fatal("expected MkdirAll error when dir is a file")
	}

	// 3. readDir error
	origReadDir := readDir
	readDir = func(string) ([]os.DirEntry, error) {
		return nil, errors.New("readdir boom")
	}
	if err := run(t.TempDir(), false, "127.0.0.1:0"); err == nil || !strings.Contains(err.Error(), "readdir boom") {
		t.Fatalf("expected readdir error, got: %v", err)
	}
	readDir = origReadDir

	// 4. non-empty directory without reuse
	nonEmptyDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(nonEmptyDir, "extra.txt"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := run(nonEmptyDir, false, "127.0.0.1:0"); err == nil || !strings.Contains(err.Error(), "fixture directory must be empty") {
		t.Fatalf("expected non-empty dir error, got: %v", err)
	}

	// 5. write() MkdirAll failure (parent path is a file)
	if err := write(fileAsDir, "sub/file.yaml", "data"); err == nil {
		t.Fatal("expected write error when root is a file")
	}

	// 6. seed() failures at each write / mountFile / coreApply step
	origWriteFile := writeFile
	for failOnCall := 1; failOnCall <= 5; failOnCall++ {
		calls := 0
		writeFile = func(name string, data []byte, perm os.FileMode) error {
			calls++
			if calls == failOnCall {
				return fmt.Errorf("write fail on call %d", failOnCall)
			}
			return origWriteFile(name, data, perm)
		}
		if err := run(t.TempDir(), false, "127.0.0.1:0"); err == nil || !strings.Contains(err.Error(), fmt.Sprintf("write fail on call %d", failOnCall)) {
			t.Fatalf("expected write fail on call %d, got: %v", failOnCall, err)
		}
	}
	writeFile = origWriteFile

	// mountFile error in seed()
	origMountFile := mountFile
	mountFile = func(string) (*core.Database, error) {
		return nil, errors.New("mountFile boom")
	}
	if err := seed(t.TempDir(), "sqlite"); err == nil || !strings.Contains(err.Error(), "mountFile boom") {
		t.Fatalf("expected mountFile error, got: %v", err)
	}
	mountFile = origMountFile

	// coreApply error in seed()
	origCoreApply := coreApply
	coreApply = func(*core.Database, context.Context, []core.Op, string) error {
		return errors.New("coreApply boom")
	}
	if err := seed(t.TempDir(), "sqlite"); err == nil || !strings.Contains(err.Error(), "coreApply boom") {
		t.Fatalf("expected coreApply error, got: %v", err)
	}
	coreApply = origCoreApply

	// 7. mount.Dir error on reuse with invalid manifest
	badMountDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(badMountDir, "bad.yaml"), []byte(":::invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := run(badMountDir, true, "127.0.0.1:0"); err == nil {
		t.Fatal("expected mount.Dir error on invalid manifest")
	}

	// 8. Missing one of the two required manifests on reuse
	oneManifestDir := t.TempDir()
	if err := seed(oneManifestDir, "sqlite"); err != nil {
		t.Fatal(err)
	}
	if err := run(oneManifestDir, true, "127.0.0.1:0"); err == nil || !strings.Contains(err.Error(), "must contain the ingitdb and sqlite manifests") {
		t.Fatalf("expected missing manifest error, got: %v", err)
	}

	// 9. auth.OpenStore error (auth.json is a directory)
	badAuthDir := t.TempDir()
	if err := seed(badAuthDir, "ingitdb"); err != nil {
		t.Fatal(err)
	}
	if err := seed(badAuthDir, "sqlite"); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(badAuthDir, "auth.json"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := run(badAuthDir, true, "127.0.0.1:0"); err == nil {
		t.Fatal("expected auth.OpenStore error when auth.json is a directory")
	}

	// 10. storeCreateGrant error
	origStoreCreateGrant := storeCreateGrant
	storeCreateGrant = func(*auth.Store, *auth.Grant, string) error {
		return errors.New("createGrant boom")
	}
	if err := run(t.TempDir(), false, "127.0.0.1:0"); err == nil || !strings.Contains(err.Error(), "createGrant boom") {
		t.Fatalf("expected createGrant error, got: %v", err)
	}
	storeCreateGrant = origStoreCreateGrant
}
