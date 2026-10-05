package server_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/dal-go/dalgo/dal"

	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/manifest"
	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
	"github.com/openvaultdb/openvaultdb-go/pkg/server"
)

// The examples of docs/api.md are run. An example is a comment line
//
//	<!-- doc-example method=POST path=/v1/dtql status=200 -->
//
// followed by a fenced block with the request body (a POST only) and a fenced json
// block with the response. The test posts the request to a server with real SQLite
// files and compares the answer with the block, ignoring the elapsed times. The
// helpers of this file all start with docExample so they cannot clash with the
// helpers of the other test files of the package.

const docExampleOrigin = "http://localhost:8080"

// docExampleCount is the number of runnable examples in docs/api.md.
const docExampleCount = 13

// docExampleSection is the heading of the section whose fenced blocks are examples.
const docExampleSection = "### Query profile and relational documents"

// docExampleEveryBlockHasAMarker fails for a yaml or json fenced block of the
// section that is neither the body of a marked example nor the response of one: a
// block opens directly after a marker, or directly after the request block that
// follows one.
func docExampleEveryBlockHasAMarker(t *testing.T, text string) {
	t.Helper()
	lines := strings.Split(text, "\n")
	start := -1
	for i, line := range lines {
		if line == docExampleSection {
			start = i
			break
		}
	}
	if start < 0 {
		t.Fatalf("docs/api.md has no %q section", docExampleSection)
	}
	// covered is the line of a fence the examples account for: the first fence after
	// a marker and, for a POST, the second.
	covered := map[int]bool{}
	fenceAfter := func(from int) int {
		for ; from < len(lines); from++ {
			if strings.HasPrefix(lines[from], "```") {
				return from
			}
		}
		return len(lines)
	}
	closeOf := func(open int) int {
		return fenceAfter(open + 1)
	}
	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		if strings.HasPrefix(lines[i], "## ") {
			end = i
			break
		}
	}
	for i := start; i < end; i++ {
		match := docExampleMarker.FindStringSubmatch(lines[i])
		if match == nil {
			continue
		}
		open := fenceAfter(i + 1)
		covered[open] = true
		if match[1] == http.MethodPost {
			covered[fenceAfter(closeOf(open)+1)] = true
		}
	}
	for i := start; i < end; i++ {
		if !strings.HasPrefix(lines[i], "```") || strings.TrimPrefix(lines[i], "```") == "" {
			continue
		}
		if !covered[i] {
			t.Errorf("docs/api.md line %d: a %s block of the section has no doc-example marker", i+1, strings.TrimPrefix(lines[i], "```"))
		}
	}
}

var docExampleMarker = regexp.MustCompile(`^<!-- doc-example method=(GET|POST) path=(\S+) status=(\d+)(?: headers=(\S+))? -->$`)

type docExample struct {
	line     int
	method   string
	path     string
	status   int
	headers  map[string]string
	request  string
	response string
}

// docExamplesOf reads the examples out of the markdown text.
func docExamplesOf(t *testing.T, text string) []docExample {
	t.Helper()
	lines := strings.Split(text, "\n")
	fenced := func(from int) (body string, next int) {
		t.Helper()
		for from < len(lines) && !strings.HasPrefix(lines[from], "```") {
			from++
		}
		if from >= len(lines) {
			t.Fatal("a doc-example has no fenced block after it")
		}
		var block []string
		for from++; from < len(lines) && !strings.HasPrefix(lines[from], "```"); from++ {
			block = append(block, lines[from])
		}
		return strings.Join(block, "\n") + "\n", from + 1
	}
	var examples []docExample
	for i := 0; i < len(lines); i++ {
		if !strings.Contains(lines[i], "doc-example") {
			continue
		}
		match := docExampleMarker.FindStringSubmatch(lines[i])
		if match == nil {
			t.Fatalf("line %d: a doc-example marker that does not parse: %s", i+1, lines[i])
		}
		example := docExample{line: i + 1, method: match[1], path: match[2]}
		example.status, _ = strconv.Atoi(match[3])
		if match[4] != "" {
			example.headers = map[string]string{}
			for _, pair := range strings.Split(match[4], ",") {
				name, value, _ := strings.Cut(pair, "=")
				example.headers[name] = value
			}
		}
		next := i + 1
		if example.method == http.MethodPost {
			example.request, next = fenced(next)
		}
		example.response, next = fenced(next)
		examples = append(examples, example)
		i = next - 1
	}
	return examples
}

// docExampleWithoutTimes removes every elapsed time, which differs from run to run.
func docExampleWithoutTimes(value any) any {
	switch v := value.(type) {
	case map[string]any:
		for key, inner := range v {
			if key == "elapsedMs" {
				delete(v, key)
				continue
			}
			v[key] = docExampleWithoutTimes(inner)
		}
	case []any:
		for i, inner := range v {
			v[i] = docExampleWithoutTimes(inner)
		}
	}
	return value
}

// docExampleEventsMount is a database on an engine the operator's list leaves out of
// joins (the default list is sqlite and ingitdb). No example reads it.
func docExampleEventsMount(t *testing.T) *core.Database {
	t.Helper()
	db, err := core.Open(&manifest.Manifest{
		Database: manifest.Database{ID: "events", SchemaMode: schema.ModeStrict},
		Storage:  manifest.Storage{Engine: "firestore"},
		Schemas:  &schema.Schemas{Collections: map[string]schema.Collection{"Event": {}}},
	}, struct{ dal.DB }{}, []schema.Mode{schema.ModeStrict}, "")
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func TestDocumentedExamplesReturnWhatTheDocumentShows(t *testing.T) {
	text, err := os.ReadFile("../../docs/api.md")
	if err != nil {
		t.Fatal(err)
	}
	examples := docExamplesOf(t, string(text))
	// Every example block of the section carries a marker, and the count is pinned,
	// so a deleted marker or an example added without one fails here. Change the
	// count with the document.
	if len(examples) != docExampleCount {
		t.Fatalf("docs/api.md holds %d runnable examples, want %d", len(examples), docExampleCount)
	}
	docExampleEveryBlockHasAMarker(t, string(text))
	service := server.New("0.1.0", map[string]*core.Database{
		"chinook":   relHTTPChinook(t, ""),
		"countries": relHTTPCountries(t, ""),
		"crm":       relHTTPProtected(t),
		"events":    docExampleEventsMount(t),
	})
	defer service.CloseSnapshots()
	host := httptest.NewServer(service.Handler())
	defer host.Close()

	for _, example := range examples {
		t.Run(example.method+" "+example.path+" (line "+strconv.Itoa(example.line)+")", func(t *testing.T) {
			resp := relHTTPDo(t, host.URL, example.method, example.path, "", example.request, example.headers)
			if resp.status != example.status {
				t.Fatalf("status %d, the document says %d: %s", resp.status, example.status, resp.raw)
			}
			var want, got any
			expected := strings.ReplaceAll(example.response, docExampleOrigin, host.URL)
			if err := json.Unmarshal([]byte(expected), &want); err != nil {
				t.Fatalf("the documented response is not JSON: %v", err)
			}
			if err := json.Unmarshal([]byte(resp.raw), &got); err != nil {
				t.Fatalf("the response is not JSON: %v: %s", err, resp.raw)
			}
			want, got = docExampleWithoutTimes(want), docExampleWithoutTimes(got)
			if !reflect.DeepEqual(want, got) {
				actual, _ := json.MarshalIndent(got, "", "  ")
				t.Fatalf("the response differs from the document\nactual (origin %s):\n%s", host.URL, actual)
			}
		})
	}
}

// Every aggregate the profile advertises is answered by real SQLite files, as a
// document over one database and as a join, so a client is never told an aggregate
// is supported and then refused.
func TestEveryAdvertisedAggregateIsAnsweredOnSQLite(t *testing.T) {
	service := server.New("0.1.0", map[string]*core.Database{"chinook": relHTTPChinook(t, ""), "countries": relHTTPCountries(t, "")})
	defer service.CloseSnapshots()
	host := httptest.NewServer(service.Handler())
	defer host.Close()

	var discovery struct {
		Query struct {
			Features struct {
				Aggregates []string `json:"aggregates"`
			} `json:"features"`
		} `json:"query"`
	}
	resp := relHTTPDo(t, host.URL, http.MethodGet, "/.well-known/openvaultdb", "", "", nil)
	if err := json.Unmarshal([]byte(resp.raw), &discovery); err != nil || len(discovery.Query.Features.Aggregates) == 0 {
		t.Fatalf("no advertised aggregates: %v: %s", err, resp.raw)
	}
	for _, function := range discovery.Query.Features.Aggregates {
		aggregate := "{aggregate: {function: " + function + ", args: [{field: total, source: i}]}, as: x}"
		join := func(database, other string) string {
			return "from:\n  " + database + "name: Invoice\n  alias: i\n  joins:\n    - type: left\n      from: {" + other + "name: Customer, alias: c}\n      on:\n        - {left: {field: customer_id, source: i}, op: '==', right: {field: id, source: c}}\ncolumns: [" + aggregate + "]\n"
		}
		documents := map[string]struct{ path, body string }{
			"one source":       {"/v1/databases/chinook/dtql", "from: {name: Invoice, alias: i}\ncolumns: [" + aggregate + "]\n"},
			"a join":           {"/v1/databases/chinook/dtql", join("", "")},
			"a join over HTTP": {"/v1/dtql", join("database: chinook\n  ", "database: chinook, ")},
			"in memory":        {"/v1/dtql", "from:\n  database: chinook\n  name: Invoice\n  alias: i\n  joins:\n    - type: left\n      from: {database: countries, name: Country, alias: k}\n      on:\n        - {left: {field: customer_id, source: i}, op: '==', right: {field: code, source: k}}\ncolumns: [" + aggregate + "]\n"},
		}
		for name, document := range documents {
			t.Run(function+" "+name, func(t *testing.T) {
				resp := relHTTPDo(t, host.URL, http.MethodPost, document.path, "", document.body, nil)
				if resp.status != http.StatusOK {
					t.Fatalf("status %d: %s", resp.status, resp.raw)
				}
			})
		}
	}
}
