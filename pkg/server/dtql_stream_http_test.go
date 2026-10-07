package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/record"
	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/joinexec"
	"github.com/openvaultdb/openvaultdb-go/pkg/license"
	"github.com/openvaultdb/openvaultdb-go/pkg/manifest"
	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
)

type internalPartialResponse struct {
	status  int
	header  http.Header
	raw     string
	body    map[string]any
	readErr error
}

func internalHTTPPostPartial(t *testing.T, host, path, body, accept string) internalPartialResponse {
	t.Helper()
	return internalHTTPDoPartial(t, http.MethodPost, host+path, body, accept)
}

func internalHTTPDoPartial(t *testing.T, method, url, body, accept string) internalPartialResponse {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, readErr := io.ReadAll(resp.Body)
	out := internalPartialResponse{status: resp.StatusCode, header: resp.Header, raw: string(raw), readErr: readErr}
	_ = json.Unmarshal(raw, &out.body)
	return out
}

func TestRelationalHTTPStreamsRowsBeforeExecutionReturns(t *testing.T) {
	service, host := relFakeServer(t, &relFakeExecutor{}, relFakeDefaultMounts())
	firstRow := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan struct{})
	service.joinStreamExecute = func(_ context.Context, _ dal.StructuredQuery, _ joinexec.Profile, _ string, _ joinexec.Registry, _ joinexec.Authorize, _ joinexec.Limits, emit func(record.Record) error, _ ...joinexec.Option) (joinexec.StreamResult, error) {
		row := record.NewRecordWithData(record.NewKeyWithID("answer", "one"), map[string]any{"id": "one"})
		if err := emit(row); err != nil {
			return joinexec.StreamResult{}, err
		}
		close(firstRow)
		<-release // stands in for a reader that has not reached EOF yet
		close(finished)
		return joinexec.StreamResult{
			Columns:      []string{"id"},
			RowsReturned: 1,
			Execution:    joinexec.Execution{Route: joinexec.RouteDatabase, RowsReturned: 1, Sources: []joinexec.ExecutionSource{{Database: "alpha", Collection: "orders"}}},
		}, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, host.URL+"/v1/dtql", strings.NewReader(relFakeAcross))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/yaml")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	if got := resp.Header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("default stream content type = %q", got)
	}
	select {
	case <-firstRow:
	case <-ctx.Done():
		t.Fatal("handler did not emit its first row")
	}
	prefix := `{"records":[`
	firstRecord := `{"data":{"id":"one"}}`
	chunk := make([]byte, len(prefix)+len(firstRecord))
	if _, err := io.ReadFull(resp.Body, chunk); err != nil {
		t.Fatalf("first row did not flush: %v", err)
	}
	if string(chunk) != prefix+firstRecord {
		t.Fatalf("first flushed bytes = %q", chunk)
	}
	select {
	case <-finished:
		t.Fatal("execution returned before the reader reached EOF")
	default:
	}
	close(release)
	remaining, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	body := append(append([]byte(nil), chunk...), remaining...)
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("stream is not complete JSON: %q: %v", body, err)
	}
	if decoded["complete"] != true {
		t.Fatalf("completion footer = %v", decoded["complete"])
	}
	<-finished
}

func TestRelationalHTTPWriteDeadlineReleasesSlowClientStream(t *testing.T) {
	service, host := relFakeServer(t, &relFakeExecutor{}, relFakeDefaultMounts(), WithQueryLimits(QueryLimits{Timeout: 100 * time.Millisecond}))
	writeFailed := make(chan error, 1)
	service.joinStreamExecute = func(_ context.Context, _ dal.StructuredQuery, _ joinexec.Profile, _ string, _ joinexec.Registry, _ joinexec.Authorize, _ joinexec.Limits, emit func(record.Record) error, _ ...joinexec.Option) (joinexec.StreamResult, error) {
		for i := 0; i < 20; i++ {
			row := record.NewRecordWithData(record.NewKeyWithID("answer", fmt.Sprint(i)), map[string]any{"payload": strings.Repeat("x", 4<<20)})
			if err := emit(row); err != nil {
				writeFailed <- err
				return joinexec.StreamResult{}, err
			}
		}
		writeFailed <- nil
		return joinexec.StreamResult{}, nil
	}
	req, err := http.NewRequest(http.MethodPost, host.URL+"/v1/dtql", strings.NewReader(relFakeAcross))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	select {
	case err := <-writeFailed:
		if err == nil {
			t.Fatal("server streamed all rows to a client that was not reading")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("write deadline did not release the blocked response stream")
	}
}

func TestRelationalHTTPAbortsWhenEvidenceFailsAfterFirstRow(t *testing.T) {
	mounts := []*core.Database{
		relFakeOpen(&manifest.Manifest{Database: manifest.Database{ID: "alpha", SchemaMode: schema.ModeStrict, License: &license.Declaration{Text: "alpha terms"}}, Storage: manifest.Storage{Engine: "sqlite"}}),
		relFakeOpen(&manifest.Manifest{Database: manifest.Database{ID: "beta", SchemaMode: schema.ModeStrict, License: &license.Declaration{Text: "beta terms"}}, Storage: manifest.Storage{Engine: "sqlite"}}),
	}
	service, host := relFakeServer(t, &relFakeExecutor{}, mounts, WithSourceRights("stream-server", nil))
	service.joinStreamExecute = func(_ context.Context, _ dal.StructuredQuery, _ joinexec.Profile, _ string, _ joinexec.Registry, _ joinexec.Authorize, _ joinexec.Limits, emit func(record.Record) error, _ ...joinexec.Option) (joinexec.StreamResult, error) {
		if err := emit(record.NewRecordWithData(record.NewKeyWithID("answer", "one"), map[string]any{"id": "one"})); err != nil {
			return joinexec.StreamResult{}, err
		}
		return joinexec.StreamResult{
			Columns: []string{"id"},
			Execution: joinexec.Execution{Route: joinexec.RouteInMemory, RowsReturned: 1,
				Sources: []joinexec.ExecutionSource{{Database: "unexpected", Collection: "orders"}}},
		}, nil
	}
	resp := internalHTTPPostPartial(t, host.URL, "/v1/dtql", relFakeAcross, queryErrorStreamMediaType)
	if got := resp.header.Get("Content-Type"); got != queryErrorStreamMediaType {
		t.Fatalf("error stream content type = %q", got)
	}
	if resp.status != http.StatusOK || resp.readErr != nil || resp.body["complete"] != false {
		t.Fatalf("late evidence failure must finish an error stream: status %d, read error %v, body %s", resp.status, resp.readErr, resp.raw)
	}
	if _, ok := resp.body["columns"]; ok {
		t.Fatalf("failed execution must omit success metadata: %s", resp.raw)
	}
	errorDetail, _ := resp.body["error"].(map[string]any)
	if errorDetail["code"] != "source_rights_invalid" || strings.Contains(resp.raw, "unexpected source") {
		t.Fatalf("late evidence error was not sanitized: %s", resp.raw)
	}
}

func TestRelationalHTTPAbortsWhenLaterRowCannotEncode(t *testing.T) {
	service, host := relFakeServer(t, &relFakeExecutor{}, relFakeDefaultMounts())
	service.joinStreamExecute = func(_ context.Context, _ dal.StructuredQuery, _ joinexec.Profile, _ string, _ joinexec.Registry, _ joinexec.Authorize, _ joinexec.Limits, emit func(record.Record) error, _ ...joinexec.Option) (joinexec.StreamResult, error) {
		if err := emit(record.NewRecordWithData(record.NewKeyWithID("answer", "one"), map[string]any{"id": "one"})); err != nil {
			return joinexec.StreamResult{}, err
		}
		if err := emit(record.NewRecordWithData(record.NewKeyWithID("answer", "two"), map[string]any{"bad": make(chan int)})); err != nil {
			return joinexec.StreamResult{}, err
		}
		return joinexec.StreamResult{}, nil
	}
	resp := internalHTTPPostPartial(t, host.URL, "/v1/dtql", relFakeAcross, "application/json")
	if resp.status != http.StatusOK || resp.readErr == nil || resp.body["complete"] == true {
		t.Fatalf("late encoding failure must abort the response: status %d, read error %v, body %s", resp.status, resp.readErr, resp.raw)
	}
}

func TestRelationalHTTPStreamLateExecutionFailureUsesSanitizedErrorFooter(t *testing.T) {
	service, host := relFakeServer(t, &relFakeExecutor{}, relFakeDefaultMounts())
	service.joinStreamExecute = func(_ context.Context, _ dal.StructuredQuery, _ joinexec.Profile, _ string, _ joinexec.Registry, _ joinexec.Authorize, _ joinexec.Limits, emit func(record.Record) error, _ ...joinexec.Option) (joinexec.StreamResult, error) {
		if err := emit(record.NewRecordWithData(record.NewKeyWithID("answer", "one"), map[string]any{"id": "one"})); err != nil {
			return joinexec.StreamResult{}, err
		}
		return joinexec.StreamResult{}, errors.New("driver secret: password=do-not-disclose")
	}
	resp := internalHTTPPostPartial(t, host.URL, "/v1/dtql", relFakeAcross, queryErrorStreamMediaType)
	if resp.status != http.StatusOK || resp.readErr != nil || resp.body["complete"] != false {
		t.Fatalf("late execution error must finish an error stream: status %d, read error %v, body %s", resp.status, resp.readErr, resp.raw)
	}
	detail, _ := resp.body["error"].(map[string]any)
	if detail["code"] != "internal" || detail["message"] != "internal server error" || strings.Contains(resp.raw, "password=") {
		t.Fatalf("late execution error was not mapped safely: %s", resp.raw)
	}
	if resp.body["columns"] != nil || resp.body["execution"] != nil {
		t.Fatalf("late error footer includes success metadata: %s", resp.raw)
	}
}

func TestRelationalHTTPOptInGetErrorStreamIsNotCached(t *testing.T) {
	mounts := []*core.Database{
		relFakeOpen(&manifest.Manifest{Database: manifest.Database{ID: "alpha", SchemaMode: schema.ModeStrict, CacheTTL: "60s"}, Storage: manifest.Storage{Engine: "sqlite"}}),
		relFakeOpen(&manifest.Manifest{Database: manifest.Database{ID: "beta", SchemaMode: schema.ModeStrict, CacheTTL: "60s"}, Storage: manifest.Storage{Engine: "sqlite"}}),
	}
	service, host := relFakeServer(t, &relFakeExecutor{}, mounts, WithReadOnly(true))
	service.joinStreamExecute = func(_ context.Context, _ dal.StructuredQuery, _ joinexec.Profile, _ string, _ joinexec.Registry, _ joinexec.Authorize, _ joinexec.Limits, emit func(record.Record) error, _ ...joinexec.Option) (joinexec.StreamResult, error) {
		if err := emit(record.NewRecordWithData(record.NewKeyWithID("answer", "one"), map[string]any{"id": "one"})); err != nil {
			return joinexec.StreamResult{}, err
		}
		return joinexec.StreamResult{}, errors.New("query failed after first row")
	}
	reqURL := host.URL + "/v1/dtql?q=" + url.QueryEscape(relFakeAcross)
	resp := internalHTTPDoPartial(t, http.MethodGet, reqURL, "", queryErrorStreamMediaType)
	if resp.status != http.StatusOK || resp.readErr != nil || resp.body["complete"] != false {
		t.Fatalf("late GET error should have a valid error terminal: status %d, read error %v, body %s", resp.status, resp.readErr, resp.raw)
	}
	if got := resp.header.Get("Cache-Control"); got != "no-store" {
		t.Fatalf("error stream Cache-Control = %q", got)
	}
	if !strings.Contains(resp.header.Get("Vary"), "Accept") {
		t.Fatalf("error stream Vary = %q", resp.header.Get("Vary"))
	}
}

func TestRelationalHTTPOptInGetSuccessStreamIsNotCached(t *testing.T) {
	mounts := []*core.Database{
		relFakeOpen(&manifest.Manifest{Database: manifest.Database{ID: "alpha", SchemaMode: schema.ModeStrict, CacheTTL: "60s"}, Storage: manifest.Storage{Engine: "sqlite"}}),
		relFakeOpen(&manifest.Manifest{Database: manifest.Database{ID: "beta", SchemaMode: schema.ModeStrict, CacheTTL: "60s"}, Storage: manifest.Storage{Engine: "sqlite"}}),
	}
	service, host := relFakeServer(t, &relFakeExecutor{}, mounts, WithReadOnly(true))
	service.joinStreamExecute = func(_ context.Context, _ dal.StructuredQuery, _ joinexec.Profile, _ string, _ joinexec.Registry, _ joinexec.Authorize, _ joinexec.Limits, emit func(record.Record) error, _ ...joinexec.Option) (joinexec.StreamResult, error) {
		if err := emit(record.NewRecordWithData(record.NewKeyWithID("answer", "one"), map[string]any{"id": "one"})); err != nil {
			return joinexec.StreamResult{}, err
		}
		return joinexec.StreamResult{Columns: []string{"id"}, Execution: joinexec.Execution{Route: joinexec.RouteDatabase, RowsReturned: 1}}, nil
	}
	reqURL := host.URL + "/v1/dtql?q=" + url.QueryEscape(relFakeAcross)
	resp := internalHTTPDoPartial(t, http.MethodGet, reqURL, "", queryErrorStreamMediaType)
	if resp.status != http.StatusOK || resp.readErr != nil || resp.body["complete"] != true {
		t.Fatalf("successful GET should finish the opted-in stream: status %d, read error %v, body %s", resp.status, resp.readErr, resp.raw)
	}
	if got := resp.header.Get("Cache-Control"); got != "no-store" {
		t.Fatalf("opt-in stream Cache-Control = %q", got)
	}
}

func TestRelationalHTTPErrorStreamOptInKeepsEarlyErrorsAsHTTPJSON(t *testing.T) {
	_, host := relFakeServer(t, &relFakeExecutor{}, relFakeDefaultMounts())
	resp := internalHTTPPostPartial(t, host.URL, "/v1/dtql", "not: valid query shape\n", queryErrorStreamMediaType)
	if resp.status != http.StatusBadRequest || resp.readErr != nil || resp.header.Get("Content-Type") != "application/json" {
		t.Fatalf("early refusal must keep ordinary JSON status: status %d, read error %v, content type %q, body %s", resp.status, resp.readErr, resp.header.Get("Content-Type"), resp.raw)
	}
}
