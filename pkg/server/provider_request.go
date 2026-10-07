package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strings"

	"github.com/dal-go/dalgo/dal"
	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/manifest"
	"gopkg.in/yaml.v3"
)

// ECBPublicFreeRequestProfile is an opt-in contract for an independently
// admitted ECB daily instance. It grants no admission, activation or entitlement.
const ECBPublicFreeRequestProfile = "ecb-public-free/1"
const IANANativeOperatorRequestProfile = "iana-native-operator/1"

func validateProviderRequestProfile(db *core.Database, p ProviderReadProfile) error {
	if db.Manifest.Storage.HTTP == nil {
		return fmt.Errorf("incompatible provider request profile")
	}
	if p.RequestProfile == "" && db.Manifest.Storage.HTTP.Profile != manifest.HTTPProfileIANAHTTPStatus {
		return nil
	}
	if db.Manifest.Storage.HTTP.Collection != p.Collection {
		return fmt.Errorf("incompatible provider request profile")
	}
	switch p.RequestProfile {
	case ECBPublicFreeRequestProfile:
		if db.ID() != "ecb" || p.Collection != "daily" || p.Binding.ResourceID != "ecb-daily" ||
			p.Binding.ProviderSourceID != "provider:ecb/FxReferenceQuote" || db.Manifest.Storage.HTTP.Profile != manifest.HTTPProfileECBDaily {
			return fmt.Errorf("incompatible provider request profile")
		}
	case IANANativeOperatorRequestProfile:
		if db.ID() != "iana-http-status" || p.Collection != "rows" || p.Binding.ResourceID != "iana-http-status-codes" ||
			p.Binding.ProviderSourceID != "provider:iana/HttpStatusRegistryRow" || db.Manifest.Storage.HTTP.Profile != manifest.HTTPProfileIANAHTTPStatus {
			return fmt.Errorf("incompatible provider request profile")
		}
	default:
		return fmt.Errorf("incompatible provider request profile")
	}
	return db.Manifest.ValidateHTTP()
}

func (s *Server) providerRequestRestricted(db *core.Database) bool {
	return s.providerProfilesByDB[db].RequestProfile != ""
}

func refuseProviderRequest(w http.ResponseWriter, requestProfile string) {
	w.Header().Set("Cache-Control", "no-store")
	message := "ECB public queries require native daily fields and an explicit limit of 1..50"
	if requestProfile == IANANativeOperatorRequestProfile {
		message = "IANA operator queries require native registry fields and an explicit limit of 1..50"
	}
	writeError(w, http.StatusUnprocessableEntity, "provider_request_unsupported", message)
}

// Check original wire intent before decoders can erase duplicates, aliases or
// unsupported no-op options. Restore the bounded body for the existing handlers.
func (s *Server) guardProviderRequest(w http.ResponseWriter, r *http.Request, db *core.Database) bool {
	if !s.providerRequestRestricted(db) {
		return true
	}
	requestProfile := s.providerProfilesByDB[db].RequestProfile
	path := r.URL.Path
	query, dtql := strings.HasSuffix(path, "/query"), strings.HasSuffix(path, "/dtql")
	if strings.HasSuffix(path, "/read") || strings.Contains(path, "/records/") || strings.HasSuffix(path, "/batch") {
		refuseProviderRequest(w, requestProfile)
		return false
	}
	if !query && !dtql {
		return true // other routes retain the existing HTTP mount capability checks
	}
	values, err := url.ParseQuery(r.URL.RawQuery)
	valid := err == nil && !hasPagingHeaders(r)
	for name, entries := range values {
		valid = valid && r.Method == http.MethodGet && len(entries) == 1 &&
			(name == "q" || (dtql && name == "parameters"))
	}
	var body []byte
	switch r.Method {
	case http.MethodPost:
		body, err = io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBodyBytes))
		r.Body = io.NopCloser(bytes.NewReader(body))
		valid = valid && err == nil
	case http.MethodGet:
		valid = valid && len(values["q"]) == 1 && len(values["q"][0]) <= maxQueryRequestBytes
		if valid {
			body = []byte(values["q"][0])
		}
	default:
		valid = false
	}
	if query {
		valid = valid && validProviderJSONQuery(body)
	} else if r.Method == http.MethodPost && strings.EqualFold(strings.TrimSpace(strings.SplitN(r.Header.Get("Content-Type"), ";", 2)[0]), "application/json") {
		node := providerWireNode(body)
		_, validEnvelope := providerMapping(node, "query", "parameters")
		valid = valid && json.Valid(body) && validEnvelope
	} else if raw, ok := values["parameters"]; ok {
		valid = valid && json.Valid([]byte(raw[0])) && providerWireNode([]byte(raw[0])) != nil
	}
	if !valid {
		refuseProviderRequest(w, requestProfile)
	}
	return valid
}

// YAML nodes retain duplicate keys even in JSON. Reject aliases/merge keys and
// duplicates recursively before either JSON or DTQL binding consumes them.
func providerWireNode(data []byte) *yaml.Node {
	d := yaml.NewDecoder(bytes.NewReader(data))
	var node yaml.Node
	if d.Decode(&node) != nil || node.Kind != yaml.DocumentNode || len(node.Content) != 1 || d.Decode(new(yaml.Node)) != io.EOF {
		return nil
	}
	var unique func(*yaml.Node) bool
	unique = func(n *yaml.Node) bool {
		if n.Kind == yaml.AliasNode || n.Anchor != "" {
			return false
		}
		if n.Kind == yaml.MappingNode {
			seen := map[string]bool{}
			for i := 0; i < len(n.Content); i += 2 {
				key := n.Content[i]
				if key.Kind != yaml.ScalarNode || key.Tag != "!!str" || key.Value == "<<" || seen[key.Value] {
					return false
				}
				seen[key.Value] = true
			}
		}
		for _, child := range n.Content {
			if !unique(child) {
				return false
			}
		}
		return true
	}
	if !unique(node.Content[0]) {
		return nil
	}
	return node.Content[0]
}

func providerMapping(n *yaml.Node, allowed ...string) (map[string]*yaml.Node, bool) {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil, false
	}
	fields := map[string]*yaml.Node{}
	for i := 0; i < len(n.Content); i += 2 {
		name := n.Content[i].Value
		if !slices.Contains(allowed, name) {
			return nil, false
		}
		fields[name] = n.Content[i+1]
	}
	return fields, true
}

func validProviderJSONQuery(body []byte) bool {
	fields, ok := providerMapping(providerWireNode(body), "collection", "where", "limit")
	if !ok || !json.Valid(body) {
		return false
	}
	if where := fields["where"]; where != nil {
		if where.Kind != yaml.SequenceNode {
			return false
		}
		for _, filter := range where.Content {
			if _, ok := providerMapping(filter, "field", "op", "value"); !ok {
				return false
			}
		}
	}
	return true
}

func validProviderDTQLWire(doc []byte) bool {
	fields, ok := providerMapping(providerWireNode(doc), "from", "columns", "where", "limit")
	if !ok {
		return false
	}
	if _, ok = providerMapping(fields["from"], "name"); !ok {
		return false
	}
	if columns := fields["columns"]; columns != nil {
		if columns.Kind != yaml.SequenceNode {
			return false
		}
		for _, column := range columns.Content {
			if _, ok := providerMapping(column, "field"); !ok {
				return false
			}
		}
	}
	if where := fields["where"]; where != nil && !validProviderConditionWire(where) {
		return false
	}
	limit := fields["limit"]
	var bound int
	return limit != nil && limit.Kind == yaml.ScalarNode && limit.Tag == "!!int" && limit.Decode(&bound) == nil && bound >= 1 && bound <= 50
}

func validProviderConditionWire(node *yaml.Node) bool {
	fields, ok := providerMapping(node, "op", "left", "right", "and", "or")
	if !ok {
		return false
	}
	for _, name := range []string{"and", "or"} {
		if group := fields[name]; group != nil {
			if group.Kind != yaml.SequenceNode {
				return false
			}
			for _, child := range group.Content {
				if !validProviderConditionWire(child) {
					return false
				}
			}
		}
	}
	if left := fields["left"]; left != nil {
		if _, ok := providerMapping(left, "field"); !ok {
			return false
		}
	}
	if right := fields["right"]; right != nil {
		if _, ok := providerMapping(right, "value", "values"); !ok {
			return false
		}
	}
	return true
}

func providerNativeField(requestProfile, field string) bool {
	if requestProfile == IANANativeOperatorRequestProfile {
		return field == "Value" || field == "Description" || field == "Reference"
	}
	return field == "time" || field == "currency" || field == "rate"
}

func providerCollection(requestProfile string) string {
	if requestProfile == IANANativeOperatorRequestProfile {
		return "rows"
	}
	return "daily"
}

func providerNativeValue(op string, value any) bool {
	if op == "in" {
		v := reflect.ValueOf(value)
		if !v.IsValid() || v.Kind() != reflect.Slice || v.Len() == 0 || v.Len() > 1000 {
			return false
		}
		for i := 0; i < v.Len(); i++ {
			if _, ok := v.Index(i).Interface().(string); !ok {
				return false
			}
		}
		return true
	}
	_, stringValue := value.(string)
	return stringValue && slices.Contains([]string{"==", "<", "<=", ">", ">="}, op)
}

func validProviderQuery(db *core.Database, q core.Query, requestProfile string) bool {
	canonical, declared := db.CanonicalCollection(q.Collection)
	if !declared || canonical != providerCollection(requestProfile) || q.Parent != "" || q.KeysOnly || len(q.OrderBy) != 0 || q.Limit < 1 || q.Limit > 50 {
		return false
	}
	for _, f := range q.Where {
		if !providerNativeField(requestProfile, f.Field) || !providerNativeValue(f.Op, f.Value) {
			return false
		}
	}
	return true
}

func validProviderDTQL(db *core.Database, q dal.StructuredQuery, profile core.Profile, requestProfile string) bool {
	if profile.Kind == core.ProfileRelational || q.Limit() < 1 || q.Limit() > 50 || q.Offset() != 0 || len(q.OrderBy()) != 0 {
		return false
	}
	source, ok := q.From().Base().(dal.CollectionRef)
	if !ok {
		return false
	}
	canonical, declared := db.CanonicalCollection(source.Name())
	if !declared || canonical != providerCollection(requestProfile) || source.Parent() != nil || source.Alias() != "" || source.Database() != "" || source.Schema() != "" || source.ScanLimit() != 0 || len(source.ScanOrders()) != 0 {
		return false
	}
	for _, col := range q.Columns() {
		field, ok := col.Expression.(dal.FieldRef)
		if !ok || col.Alias != "" || field.Source() != "" || !providerNativeField(requestProfile, field.Name()) {
			return false
		}
	}
	return validProviderCondition(requestProfile, q.Where())
}

func validProviderCondition(requestProfile string, condition dal.Condition) bool {
	switch c := condition.(type) {
	case nil:
		return true
	case dal.GroupCondition:
		if c.Operator() != dal.And && c.Operator() != dal.Or {
			return false
		}
		for _, child := range c.Conditions() {
			if !validProviderCondition(requestProfile, child) {
				return false
			}
		}
		return len(c.Conditions()) != 0
	case dal.Comparison:
		field, ok := c.Left.(dal.FieldRef)
		if !ok || field.Source() != "" || !providerNativeField(requestProfile, field.Name()) {
			return false
		}
		switch value := c.Right.(type) {
		case dal.Constant:
			return providerNativeValue(string(c.Operator), value.Value)
		case dal.Array:
			return c.Operator == dal.In && providerNativeValue("in", value.Value)
		}
	}
	return false
}
