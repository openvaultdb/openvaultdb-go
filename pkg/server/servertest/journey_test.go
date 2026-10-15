package servertest

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const fakeMetadata = `{
  "collections": ["native", "plain", "broken"],
  "schemas": {"collections": {
    "native": {"source": {"schema": "s", "name": "Native Table"}, "fields": {
      "id": {"nativeType": "bigint", "primaryKey": true},
      "b": {"nativeType": "bytea"},
      "name": {"nativeType": "text"},
      "j": {"nativeType": "jsonb"},
      "missing": {"nativeType": "text"}
    }},
    "plain": {"fields": {"x": {"nativeType": "integer"}}},
    "broken": {"fields": {"y": {"nativeType": "text"}}}
  }}
}`

// fakeAPI answers the battery the way a correct server does for "native" and
// "plain", and wrongly for "broken".
func fakeAPI(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Test") != "yes" {
			http.Error(w, "missing header", http.StatusUnauthorized)
			return
		}
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, fakeMetadata)
			return
		}
		var body struct{ Query string }
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("request body: %v", err)
		}
		var doc struct {
			From  map[string]string
			Where *struct {
				Left  struct{ Field string }
				Right struct{ Value json.RawMessage }
			}
		}
		if err := json.Unmarshal([]byte(body.Query), &doc); err != nil {
			t.Errorf("query: %v", err)
		}
		stream := r.Header.Get("Accept") != ""
		name := doc.From["name"]
		switch {
		case name == "broken" && stream:
			_, _ = io.WriteString(w, `{"records":[],"complete":false}`)
		case name == "broken" && doc.Where != nil:
			_, _ = io.WriteString(w, `not json`)
		case name == "broken":
			w.WriteHeader(http.StatusTeapot)
			_, _ = io.WriteString(w, `{"records":[{"data":{"y":"v"}}]}`)
		case name == "plain" && stream:
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"records":[],"complete":false,"error":{"code":"query_budget_exceeded","message":"too many rows"}}`)
		case doc.Where != nil && doc.Where.Left.Field == "b":
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"code":"invalid_dtql","message":"bytea"}}`)
		case doc.Where != nil && doc.Where.Left.Field == "id" && string(doc.Where.Right.Value) != "1" && string(doc.Where.Right.Value) != "9007199254740993":
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"code":"invalid_dtql","message":"not an integer"}}`)
		case doc.Where != nil && doc.Where.Left.Field == "x":
			// plain: a bad integer is wrongly answered, and the sample matches nothing.
			_, _ = io.WriteString(w, `{"records":[],"complete":true}`)
		default:
			_, _ = io.WriteString(w, `{"records":[{"data":{"id":1,"b":"AA==","name":"n","j":{"k":1},"x":5,"missing":null}}],"complete":true}`)
		}
	}))
}

func TestEveryCollectionRunsTheBattery(t *testing.T) {
	api := fakeAPI(t)
	defer api.Close()
	opts := Options{Header: http.Header{"X-Test": {"yes"}}, Expected: func(c Check) bool { return c.Collection == "plain" && c.Kind == KindFilter }}
	report, err := EveryCollection(context.Background(), api.URL+"/", "db", opts)
	if err != nil {
		t.Fatal(err)
	}
	if report.Collections != 3 {
		t.Fatalf("collections = %d", report.Collections)
	}
	failures := map[string]bool{}
	for _, failure := range report.Failures() {
		failures[failure.Collection+" "+failure.Kind] = true
		if failure.String() == "" {
			t.Fatal("a failure has no text")
		}
	}
	for _, want := range []string{"plain badInteger", "broken plain", "broken nolimit", "broken order", "broken orderEach", "broken offset"} {
		if !failures[want] {
			t.Errorf("no failure %q in %v", want, failures)
		}
	}
	for failure := range failures {
		if strings.HasPrefix(failure, "native ") || failure == "plain filter" || failure == "plain nolimit" {
			t.Errorf("unexpected failure %q", failure)
		}
	}
	matrix := report.Matrix()
	if matrix["filter:pass"] == 0 || matrix["exactInteger:pass"] != 1 || matrix["badInteger:pass"] != 5 {
		t.Errorf("matrix = %v", matrix)
	}
	if line := report.MatrixString(); !strings.HasPrefix(line, "db collections=3 ") || !strings.Contains(line, "plain:pass=") {
		t.Errorf("matrix line = %q", line)
	}
	native := Check{Database: "db", Schema: "s", Name: "t", Kind: KindFilter, Detail: "id==1", Status: 200}
	if got := native.String(); got != "db:s.t filter id==1 -> 200 " {
		t.Errorf("String = %q", got)
	}

	only, err := EveryCollection(context.Background(), api.URL, "db", Options{Header: opts.Header, Collections: []string{"native"}})
	if err != nil || only.Collections != 1 || len(only.Failures()) != 0 {
		t.Fatalf("one collection: %v, %+v", err, only.Failures())
	}
	recorder := &recordingTB{TB: t}
	RequireEveryCollection(recorder, context.Background(), api.URL, "db", Options{Header: opts.Header, Collections: []string{"broken"}})
	if recorder.errors == 0 || recorder.logs == 0 {
		t.Fatalf("RequireEveryCollection reported %d errors and %d logs", recorder.errors, recorder.logs)
	}
}

func TestEveryCollectionMetadataFailures(t *testing.T) {
	api := fakeAPI(t)
	defer api.Close()
	if _, err := EveryCollection(context.Background(), api.URL, "db", Options{}); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("unauthorised metadata: %v", err)
	}
	garbled := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "{") }))
	defer garbled.Close()
	if _, err := EveryCollection(context.Background(), garbled.URL, "db", Options{}); err == nil {
		t.Fatal("garbled metadata was read")
	}
	if _, err := EveryCollection(context.Background(), "http://127.0.0.1:0", "db", Options{Client: &http.Client{}}); err == nil {
		t.Fatal("an unreachable server answered")
	}
	if _, err := EveryCollection(context.Background(), "http://bad host", "db", Options{}); err == nil {
		t.Fatal("a bad URL was requested")
	}
	ctx, cancel := context.WithCancel(context.Background())
	headerOnly := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		cancel()
		_, _ = io.WriteString(w, `{"collections":["a"],"schemas":{"collections":{}}}`)
	}))
	defer headerOnly.Close()
	if _, err := EveryCollection(ctx, headerOnly.URL, "db", Options{}); err == nil {
		t.Fatal("a cancelled battery went on")
	}
	recorder := &recordingTB{TB: t}
	func() {
		defer func() { _ = recover() }()
		RequireEveryCollection(recorder, context.Background(), garbled.URL, "db", Options{})
	}()
	if recorder.fatals != 1 {
		t.Fatalf("RequireEveryCollection did not stop on a metadata failure")
	}
}

func TestQueryReportsTransportFailures(t *testing.T) {
	j := journey{ctx: context.Background(), base: "http://127.0.0.1:0", database: "db", opts: Options{Client: http.DefaultClient}}
	if status, _, message := j.query(map[string]any{"from": "x"}, false); status != 0 || message == "" {
		t.Fatalf("unreachable: %d %q", status, message)
	}
	if _, _, message := j.query(map[string]any{"bad": make(chan int)}, false); message == "" {
		t.Fatal("an unencodable document was sent")
	}
	if scalarJSON(json.RawMessage(" ")) || !scalarJSON(json.RawMessage(`"x"`)) || scalarJSON(json.RawMessage(`null`)) {
		t.Fatal("scalarJSON")
	}
	if got := clip([]byte(strings.Repeat("x", 400))); len(got) > 310 {
		t.Fatalf("clip kept %d bytes", len(got))
	}
}

// recordingTB counts what RequireEveryCollection reports.
type recordingTB struct {
	testing.TB
	errors, logs, fatals int
}

func (r *recordingTB) Helper()               {}
func (r *recordingTB) Log(...any)            { r.logs++ }
func (r *recordingTB) Errorf(string, ...any) { r.errors++ }
func (r *recordingTB) Fatalf(string, ...any) {
	r.fatals++
	panic("fatal")
}

type cancelAfterGetRT struct {
	cancel context.CancelFunc
	rt     http.RoundTripper
}

func (c *cancelAfterGetRT) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := c.rt.RoundTrip(req)
	if req.Method == http.MethodGet {
		c.cancel()
	}
	return resp, err
}

func TestJourneyAdditionalCoverage(t *testing.T) {
	// 1. journey.go: lines 167-168: ctx.Err() != nil in EveryCollection
	api := fakeAPI(t)
	defer api.Close()
	ctxCancel, cancel := context.WithCancel(context.Background())
	defer cancel()
	opts := Options{
		Header: http.Header{"X-Test": {"yes"}},
		Client: &http.Client{Transport: &cancelAfterGetRT{cancel: cancel, rt: http.DefaultTransport}},
	}
	if _, err := EveryCollection(ctxCancel, api.URL+"/", "db", opts); err == nil {
		t.Fatal("expected error on cancelled context")
	}

	// 2. journey.go: lines 314-315: bytea filter not refused with message == ""
	srvBytea := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{
				"collections": ["tbl"],
				"schemas": {"collections": {"tbl": {"fields": {"b": {"nativeType": "bytea"}}}}}
			}`)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"records":[{"data":{"b":"AA=="}}],"complete":true}`)
	}))
	defer srvBytea.Close()
	repBytea, err := EveryCollection(context.Background(), srvBytea.URL+"/", "db", Options{})
	if err != nil {
		t.Fatal(err)
	}
	foundByteaMsg := false
	for _, ch := range repBytea.Checks {
		if ch.Kind == KindFilter && strings.Contains(ch.Message, "a filter on a bytea column was not refused") {
			foundByteaMsg = true
			break
		}
	}
	if !foundByteaMsg {
		t.Fatal("expected 'a filter on a bytea column was not refused' message")
	}

	// 3. journey.go: lines 362-363: jsonMarshalJourney error on query body
	oldMarshal := jsonMarshalJourney
	defer func() { jsonMarshalJourney = oldMarshal }()
	calls := 0
	jsonMarshalJourney = func(v any) ([]byte, error) {
		calls++
		if calls == 2 {
			return nil, errors.New("forced marshal body error")
		}
		return json.Marshal(v)
	}
	j := journey{ctx: context.Background(), base: "http://127.0.0.1:0", database: "db", opts: Options{Client: http.DefaultClient}}
	if _, _, msg := j.query(map[string]any{"from": "x"}, false); !strings.Contains(msg, "forced marshal body error") {
		t.Fatalf("expected forced marshal body error, got %q", msg)
	}
	jsonMarshalJourney = oldMarshal
}
