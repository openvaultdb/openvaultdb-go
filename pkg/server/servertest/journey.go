// Package servertest drives a running OpenVaultDB HTTP API the way a client
// does, so that a server, or a service built on the library such as the cloud,
// can prove in its own tests that every collection of a mount answers.
//
// EveryCollection reads the metadata of one database and runs a fixed battery
// of relational DTQL reads on each collection it lists: a bounded read, an
// unbounded streamed read, a deterministic order, an order by each field, an
// equality filter on each scalar field of a sampled row, an offset page, and the
// refusal of values an integer column cannot hold. It returns a Report; a test
// asserts on Report.Failures, or calls RequireEveryCollection.
package servertest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"testing"
)

// Check kinds, as Check.Kind and the keys of Report.Matrix spell them.
const (
	KindPlain        = "plain"        // {from, limit: 5} answers 200
	KindNoLimit      = "nolimit"      // {from} streamed answers complete or query_budget_exceeded
	KindOrder        = "order"        // ordered by the primary key, else by every field
	KindOrderEach    = "orderEach"    // ordered by each field, descending
	KindFilter       = "filter"       // == on each scalar field of a sampled row finds a row
	KindOffset       = "offset"       // ordered page with an offset
	KindBadInteger   = "badInteger"   // a value an integer column cannot hold is a 400
	KindExactInteger = "exactInteger" // an exact integer above 2^53 is accepted on a bigint column
)

// Check is the outcome of one request of the battery.
type Check struct {
	Database   string
	Collection string // the collection ID the metadata lists
	Schema     string // the source schema, for a native PostgreSQL collection
	Name       string // the source relation name, else the collection ID
	Kind       string
	Detail     string // the field or value the request is about, if any
	Status     int    // the HTTP status, 0 when no response was read
	Passed     bool
	Message    string // why a check failed
}

func (c Check) String() string {
	label := c.Name
	if c.Schema != "" {
		label = c.Schema + "." + c.Name
	}
	text := fmt.Sprintf("%s:%s %s", c.Database, label, c.Kind)
	if c.Detail != "" {
		text += " " + c.Detail
	}
	return fmt.Sprintf("%s -> %d %s", text, c.Status, c.Message)
}

// Report is the outcome of the battery on one database.
type Report struct {
	Database    string
	Collections int
	Checks      []Check
}

// Failures returns the checks that did not pass, in the order they ran.
func (r Report) Failures() []Check {
	var failed []Check
	for _, check := range r.Checks {
		if !check.Passed {
			failed = append(failed, check)
		}
	}
	return failed
}

// Matrix counts the checks by kind and outcome, as "<kind>:pass" and "<kind>:fail".
func (r Report) Matrix() map[string]int {
	counts := map[string]int{}
	for _, check := range r.Checks {
		outcome := ":pass"
		if !check.Passed {
			outcome = ":fail"
		}
		counts[check.Kind+outcome]++
	}
	return counts
}

// MatrixString is Matrix as one sorted line.
func (r Report) MatrixString() string {
	matrix := r.Matrix()
	keys := make([]string, 0, len(matrix))
	for key := range matrix {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, key := range keys {
		parts[i] = fmt.Sprintf("%s=%d", key, matrix[key])
	}
	return fmt.Sprintf("%s collections=%d %s", r.Database, r.Collections, strings.Join(parts, " "))
}

// Options tunes EveryCollection. The zero value uses http.DefaultClient.
type Options struct {
	// Client sends the requests; nil is http.DefaultClient.
	Client *http.Client
	// Header is added to every request, for example an Authorization header.
	Header http.Header
	// Expected reports a failed check as one the caller documents and expects; it
	// is then counted as passed, with its message kept. Nil expects none.
	Expected func(Check) bool
	// Collections, when not empty, limits the battery to these collection IDs.
	Collections []string
}

// collectionMetadata is the part of the database metadata the battery reads.
type collectionMetadata struct {
	Source *struct {
		Schema string `json:"schema"`
		Name   string `json:"name"`
	} `json:"source"`
	Fields map[string]struct {
		NativeType string `json:"nativeType"`
		PrimaryKey bool   `json:"primaryKey"`
	} `json:"fields"`
}

type databaseMetadata struct {
	Collections []string `json:"collections"`
	Schemas     struct {
		Collections map[string]collectionMetadata `json:"collections"`
	} `json:"schemas"`
}

// EveryCollection runs the battery on every collection databaseID lists at
// baseURL (the origin of the API, without /v1). The error is the failure to read
// the metadata or a cancelled context; a failed request is a failed Check.
func EveryCollection(ctx context.Context, baseURL, databaseID string, opts Options) (Report, error) {
	j := journey{ctx: ctx, base: strings.TrimRight(baseURL, "/"), database: databaseID, opts: opts}
	if j.opts.Client == nil {
		j.opts.Client = http.DefaultClient
	}
	report := Report{Database: databaseID}
	status, body, err := j.do(http.MethodGet, "/v1/databases/"+url.PathEscape(databaseID), nil, false)
	if err != nil {
		return report, err
	}
	if status != http.StatusOK {
		return report, fmt.Errorf("database metadata of %q: HTTP %d: %s", databaseID, status, clip(body))
	}
	var metadata databaseMetadata
	if err := json.Unmarshal(body, &metadata); err != nil {
		return report, fmt.Errorf("database metadata of %q: %w", databaseID, err)
	}
	ids := metadata.Collections
	if len(opts.Collections) > 0 {
		ids = opts.Collections
	}
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		report.Collections++
		report.Checks = append(report.Checks, j.collection(id, metadata.Schemas.Collections[id])...)
	}
	return report, nil
}

// RequireEveryCollection runs EveryCollection, logs the matrix and fails t on
// an error or on any failed check, listing each.
func RequireEveryCollection(t testing.TB, ctx context.Context, baseURL, databaseID string, opts Options) Report {
	t.Helper()
	report, err := EveryCollection(ctx, baseURL, databaseID, opts)
	if err != nil {
		t.Fatalf("every collection of %q: %v", databaseID, err)
	}
	t.Log(report.MatrixString())
	for _, failure := range report.Failures() {
		t.Errorf("%s", failure)
	}
	return report
}

type journey struct {
	ctx      context.Context
	base     string
	database string
	opts     Options
}

func (j journey) do(method, path string, body []byte, stream bool) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(j.ctx, method, j.base+path, reader)
	if err != nil {
		return 0, nil, err
	}
	for name, values := range j.opts.Header {
		for _, value := range values {
			request.Header.Add(name, value)
		}
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if stream {
		request.Header.Set("Accept", "application/vnd.openvaultdb.query-stream+json")
	}
	response, err := j.opts.Client.Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = response.Body.Close() }()
	raw, err := io.ReadAll(response.Body)
	return response.StatusCode, raw, err
}

// answer is a decoded DTQL response.
type answer struct {
	Records []struct {
		Data map[string]json.RawMessage `json:"data"`
	} `json:"records"`
	Complete bool `json:"complete"`
	Error    *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

type target struct {
	id, schema, name string
	from             map[string]string
	fields           []string
	nativeTypes      map[string]string
	primaryKey       []string
}

func (j journey) collection(id string, meta collectionMetadata) []Check {
	c := target{id: id, name: id, from: map[string]string{"name": id}, nativeTypes: map[string]string{}}
	if meta.Source != nil {
		c.schema, c.name = meta.Source.Schema, meta.Source.Name
		c.from = map[string]string{"schema": c.schema, "name": c.name}
	}
	for field, described := range meta.Fields {
		c.fields = append(c.fields, field)
		c.nativeTypes[field] = strings.ToLower(described.NativeType)
		if described.PrimaryKey {
			c.primaryKey = append(c.primaryKey, field)
		}
	}
	sort.Strings(c.fields)
	sort.Strings(c.primaryKey)
	var checks []Check
	record := func(kind, detail string, status int, passed bool, message string) {
		check := Check{Database: j.database, Collection: id, Schema: c.schema, Name: c.name, Kind: kind, Detail: detail, Status: status, Passed: passed, Message: message}
		if !check.Passed && j.opts.Expected != nil && j.opts.Expected(check) {
			check.Passed = true
		}
		checks = append(checks, check)
	}

	status, plain, message := j.query(map[string]any{"from": c.from, "limit": 5}, false)
	record(KindPlain, "", status, status == http.StatusOK && message == "", message)

	status, unbounded, message := j.query(map[string]any{"from": c.from}, true)
	complete := status == http.StatusOK && message == "" && unbounded.Complete
	budget := unbounded.Error != nil && unbounded.Error.Code == "query_budget_exceeded"
	if complete || budget {
		message = ""
	} else if message == "" {
		message = "the streamed answer did not complete"
	}
	record(KindNoLimit, "", status, complete || budget, message)

	order := c.primaryKey
	if len(order) == 0 {
		order = c.fields
	}
	orderBy := make([]map[string]any, len(order))
	for i, field := range order {
		orderBy[i] = map[string]any{"field": field}
	}
	status, _, message = j.query(map[string]any{"from": c.from, "orderBy": orderBy, "limit": 5}, false)
	record(KindOrder, "", status, status == http.StatusOK && message == "", message)

	for _, field := range c.fields {
		status, _, message = j.query(map[string]any{"from": c.from, "orderBy": []map[string]any{{"field": field, "desc": true}}, "limit": 2}, false)
		record(KindOrderEach, field+" ("+c.nativeTypes[field]+")", status, status == http.StatusOK && message == "", message)
	}

	if len(plain.Records) > 0 {
		sample := plain.Records[0].Data
		for _, field := range c.fields {
			raw, present := sample[field]
			if !present || !scalarJSON(raw) {
				continue
			}
			detail := fmt.Sprintf("%s==%s (%s)", field, clip(raw), c.nativeTypes[field])
			status, found, message := j.query(map[string]any{"from": c.from, "where": equals(field, raw), "limit": 3}, false)
			if c.nativeTypes[field] == "bytea" {
				// A bytea column is returned as base64 text, and a filter cannot compare
				// it with a value: a 400 (docs/native-postgres-read.md).
				if status == http.StatusBadRequest {
					message = ""
				} else if message == "" {
					message = "a filter on a bytea column was not refused"
				}
				record(KindFilter, detail, status, status == http.StatusBadRequest, message)
				continue
			}
			if status == http.StatusOK && message == "" && len(found.Records) == 0 {
				message = "no row matched the value of the sampled row"
			}
			record(KindFilter, detail, status, status == http.StatusOK && message == "", message)
		}
	}

	status, _, message = j.query(map[string]any{"from": c.from, "orderBy": orderBy, "limit": 3, "offset": 2}, false)
	record(KindOffset, "", status, status == http.StatusOK && message == "", message)

	for _, field := range c.fields {
		if !integerType(c.nativeTypes[field]) {
			continue
		}
		for _, bad := range []json.RawMessage{json.RawMessage(`"abc"`), json.RawMessage(`1.5`), json.RawMessage(`1180591620717411303424`), json.RawMessage(`true`), json.RawMessage(`"1e3"`)} {
			status, _, message := j.query(map[string]any{"from": c.from, "where": equals(field, bad), "limit": 1}, false)
			if status == http.StatusBadRequest {
				message = ""
			} else if message == "" {
				message = "a value the integer column cannot hold was not refused"
			}
			record(KindBadInteger, fmt.Sprintf("%s==%s", field, bad), status, status == http.StatusBadRequest, message)
		}
		if c.nativeTypes[field] == "bigint" {
			status, _, message := j.query(map[string]any{"from": c.from, "where": equals(field, json.RawMessage(`9007199254740993`)), "limit": 1}, false)
			record(KindExactInteger, field+"==9007199254740993", status, status == http.StatusOK && message == "", message)
		}
		break // one integer column per collection is enough to prove the rule
	}
	return checks
}

// query posts a DTQL document, as the JSON body {"query": "<document>"}, to the
// per-database endpoint. message is empty when the answer is a 200 without an
// error; otherwise it says what came back.
func (j journey) query(document map[string]any, stream bool) (int, answer, string) {
	var decoded answer
	text, err := json.Marshal(document)
	if err != nil {
		return 0, decoded, err.Error()
	}
	body, err := json.Marshal(map[string]string{"query": string(text)})
	if err != nil {
		return 0, decoded, err.Error()
	}
	status, raw, err := j.do(http.MethodPost, "/v1/databases/"+url.PathEscape(j.database)+"/dtql", body, stream)
	if err != nil {
		return status, decoded, err.Error()
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return status, decoded, "the answer is not JSON: " + clip(raw)
	}
	if decoded.Error != nil {
		return status, decoded, decoded.Error.Code + ": " + decoded.Error.Message
	}
	if status != http.StatusOK {
		return status, decoded, clip(raw)
	}
	return status, decoded, ""
}

func equals(field string, value json.RawMessage) map[string]any {
	return map[string]any{"op": "==", "left": map[string]any{"field": field}, "right": map[string]any{"value": value}}
}

// scalarJSON reports whether raw is a JSON string, number or boolean.
func scalarJSON(raw json.RawMessage) bool {
	text := bytes.TrimSpace(raw)
	if len(text) == 0 {
		return false
	}
	switch text[0] {
	case '{', '[', 'n':
		return false
	}
	return true
}

func integerType(nativeType string) bool {
	switch nativeType {
	case "smallint", "integer", "bigint", "int2", "int4", "int8":
		return true
	}
	return false
}

func clip(raw []byte) string {
	const most = 300
	text := string(raw)
	if len(text) > most {
		return text[:most] + "…"
	}
	return text
}
