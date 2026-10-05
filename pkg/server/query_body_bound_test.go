package server_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/server"
)

// The body of POST /v1/databases/{id}/query is bounded like the bodies of the DTQL
// endpoints: at 1 MiB. A body of exactly that size is read, and one byte more is the
// answer the DTQL endpoints give for a body over their cap: 400 bad_request, with the
// same sentence.

const queryBodyBound = 1 << 20

// queryBodyOfSize is a valid wire query padded with white space to n bytes.
func queryBodyOfSize(n int) string {
	const query = `{"collection":"Customer","limit":1}`
	return query + strings.Repeat(" ", n-len(query))
}

func queryBodyHost(t *testing.T) *httptest.Server {
	t.Helper()
	service := server.New("test", map[string]*core.Database{"chinook": relHTTPChinook(t, "")})
	t.Cleanup(service.CloseSnapshots)
	host := httptest.NewServer(service.Handler())
	t.Cleanup(host.Close)
	return host
}

func TestThePostBodyOfAQueryIsBoundedAtOneMiB(t *testing.T) {
	host := queryBodyHost(t)
	at := relHTTPDo(t, host.URL, http.MethodPost, "/v1/databases/chinook/query", "", queryBodyOfSize(queryBodyBound), nil)
	if at.status != http.StatusOK || !strings.Contains(at.raw, `"records"`) {
		t.Fatalf("a body of exactly 1 MiB: status %d: %.200s", at.status, at.raw)
	}
	over := relHTTPDo(t, host.URL, http.MethodPost, "/v1/databases/chinook/query", "", queryBodyOfSize(queryBodyBound+1), nil)
	if over.status != http.StatusBadRequest || over.errorField("code") != "bad_request" {
		t.Fatalf("a body one byte over 1 MiB: status %d: %.200s", over.status, over.raw)
	}
	if message := over.errorField("message"); !strings.HasPrefix(message, "failed to read body: ") {
		t.Errorf("message = %q, want the sentence the DTQL endpoints give", message)
	}
	// The bound is the one of the DTQL endpoints: the same status, the same code and the
	// same message for a body one byte over.
	dtql := relHTTPDo(t, host.URL, http.MethodPost, "/v1/databases/chinook/dtql", "", strings.Repeat("x", queryBodyBound+1), nil)
	if dtql.status != over.status || dtql.errorField("code") != over.errorField("code") || dtql.errorField("message") != over.errorField("message") {
		t.Errorf("DTQL answers %d %s %q for a body over its cap, /query answers %d %s %q",
			dtql.status, dtql.errorField("code"), dtql.errorField("message"), over.status, over.errorField("code"), over.errorField("message"))
	}
	// A body far over the bound is refused the same way, and a small body is read as before.
	if far := relHTTPDo(t, host.URL, http.MethodPost, "/v1/databases/chinook/query", "", queryBodyOfSize(8<<20), nil); far.status != http.StatusBadRequest {
		t.Errorf("a body of 8 MiB: status %d", far.status)
	}
	small := relHTTPDo(t, host.URL, http.MethodPost, "/v1/databases/chinook/query", "", `{"collection":"Customer"}`, nil)
	if small.status != http.StatusOK {
		t.Errorf("a small body: status %d: %s", small.status, small.raw)
	}
}

// A body that is not a JSON query is read whole and refused as it was, whatever its size.
func TestThePostBodyOfAQueryThatIsNotJSONIsStillA400(t *testing.T) {
	host := queryBodyHost(t)
	for name, body := range map[string]string{"truncated": `{"collection":`, "empty": "", "not an object": `[1]`} {
		resp := relHTTPDo(t, host.URL, http.MethodPost, "/v1/databases/chinook/query", "", body, nil)
		if resp.status != http.StatusBadRequest || resp.errorField("code") != "bad_request" || !strings.HasPrefix(resp.errorField("message"), "invalid JSON body: ") {
			t.Errorf("%s: status %d: %s", name, resp.status, resp.raw)
		}
	}
}

// The bound counts the bytes read, whatever the framing: a body sent chunked, with no length
// announced, is read up to 1 MiB and refused one byte over, with the same answer.
func TestThePostBodyOfAQuerySentChunkedIsBoundedAtOneMiB(t *testing.T) {
	service := server.New("test", map[string]*core.Database{"chinook": relHTTPChinook(t, "")})
	t.Cleanup(service.CloseSnapshots)
	var lengths []int64
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lengths = append(lengths, r.ContentLength)
		service.Handler().ServeHTTP(w, r)
	}))
	t.Cleanup(host.Close)
	post := func(size int) (int, string) {
		t.Helper()
		// The reader hides its length, so the client sends the body chunked.
		req, err := http.NewRequest(http.MethodPost, host.URL+"/v1/databases/chinook/query", struct{ io.Reader }{strings.NewReader(queryBodyOfSize(size))})
		if err != nil {
			t.Fatal(err)
		}
		req.ContentLength = -1
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, string(body)
	}
	if status, body := post(queryBodyBound); status != http.StatusOK {
		t.Fatalf("a chunked body of exactly 1 MiB: status %d: %.200s", status, body)
	}
	status, body := post(queryBodyBound + 1)
	if status != http.StatusBadRequest || !strings.Contains(body, `"bad_request"`) || !strings.Contains(body, "failed to read body: ") {
		t.Fatalf("a chunked body one byte over 1 MiB: status %d: %.200s", status, body)
	}
	for i, length := range lengths {
		if length != -1 {
			t.Errorf("request %d announced a length of %d: the body was not chunked", i, length)
		}
	}
	if len(lengths) != 2 {
		t.Errorf("the server saw %d requests, want 2", len(lengths))
	}
}
