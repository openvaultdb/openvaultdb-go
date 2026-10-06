package providerreads

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/openvaultdb/openvaultdb-go/pkg/license"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"
)

var digestShape = regexp.MustCompile(`^[a-f0-9]{64}$`)
var instantShape = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d{3})?Z$`)

func equal(a, b any) bool {
	x, e := Canonical(a)
	y, f := Canonical(b)
	return e == nil && f == nil && bytes.Equal(x, y)
}
func text(s string, empty, multiline bool) error {
	if !utf8.ValidString(s) || len(utf16.Encode([]rune(s))) > 4096 || (!empty && s == "") {
		return fmt.Errorf("invalid or overbound text")
	}
	for _, r := range s {
		if unicode.IsControl(r) && (!multiline || (r != '\n' && r != '\r' && r != '\t')) {
			return fmt.Errorf("invalid text control")
		}
	}
	return nil
}
func texts(ss ...string) error {
	for _, s := range ss {
		if err := text(s, false, false); err != nil {
			return err
		}
	}
	return nil
}
func digests(ss ...string) error {
	for _, s := range ss {
		if !digestShape.MatchString(s) {
			return fmt.Errorf("noncanonical digest")
		}
	}
	return nil
}

// safeURL admits a deliberately constrained canonical HTTPS subset that the
// JS WHATWG gate accepts unchanged. It never normalizes a bound locator.
func safeURL(s string) error {
	if err := text(s, false, false); err != nil {
		return err
	}
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Opaque != "" || u.Fragment != "" || u.String() != s || u.Path == "" || strings.Contains(s, "#") {
		return fmt.Errorf("unsafe or noncanonical upstream URL")
	}
	// DNS names only: no IP/WHATWG numeric host forms, IDNA, single-label names,
	// trailing dots, uppercase or bracketed authorities in this v1 Go profile.
	if !canonicalDNS.MatchString(u.Hostname()) {
		return fmt.Errorf("unsupported upstream host grammar")
	}
	authority := u.Hostname()
	if port := u.Port(); port != "" {
		n, e := strconv.Atoi(port)
		if e != nil || n < 1 || n > 65535 || n == 443 || strconv.Itoa(n) != port {
			return fmt.Errorf("noncanonical upstream port")
		}
		authority += ":" + port
	}
	if u.Host != authority {
		return fmt.Errorf("noncanonical upstream authority")
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c > 127 || !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || strings.ContainsRune("-._~!$&()*+,;=:/?@%", rune(c))) {
			return fmt.Errorf("unsupported upstream URL character")
		}
		if c == '%' {
			if i+2 >= len(s) {
				return fmt.Errorf("invalid URL escape")
			}
			if _, e := strconv.ParseUint(s[i+1:i+3], 16, 8); e != nil {
				return fmt.Errorf("invalid URL escape")
			}
			i += 2
		}
	}
	for _, segment := range strings.Split(u.EscapedPath(), "/") {
		decoded, e := url.PathUnescape(segment)
		if e != nil || decoded == "." || decoded == ".." {
			return fmt.Errorf("noncanonical upstream path")
		}
	}
	return nil
}

var canonicalDNS = regexp.MustCompile(`^(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$`)

func requestValue(v any) error {
	switch x := v.(type) {
	case nil, bool:
		return nil
	case string:
		return text(x, true, false)
	case float64:
		if x >= -9007199254740991 && x <= 9007199254740991 && x == float64(int64(x)) {
			return nil
		}
	case int:
		if int64(x) >= -9007199254740991 && int64(x) <= 9007199254740991 {
			return nil
		}
	case int64:
		if x >= -9007199254740991 && x <= 9007199254740991 {
			return nil
		}
	case json.Number:
		f, e := x.Float64()
		if e == nil {
			return requestValue(f)
		}
	}
	return fmt.Errorf("invalid request parameter")
}
func validateRequest(r Request) error {
	if err := texts(r.ResourceID); err != nil {
		return err
	}
	if r.Method != "GET" || r.Params == nil || len(r.Params) > MaxItems {
		return fmt.Errorf("invalid resource request")
	}
	if err := safeURL(r.UpstreamURL); err != nil {
		return err
	}
	for key, v := range r.Params {
		if err := texts(key); err != nil {
			return err
		}
		if array, ok := v.([]any); ok {
			if len(array) > MaxItems {
				return fmt.Errorf("overbound parameters")
			}
			for _, item := range array {
				if err := requestValue(item); err != nil {
					return err
				}
			}
		} else {
			if err := requestValue(v); err != nil {
				return err
			}
		}
	}
	return nil
}
func validateExecution(e Execution) error {
	if err := texts(e.ID, e.ExecutorID); err != nil {
		return err
	}
	if e.Mode != "direct" && e.Mode != "proxy" {
		return fmt.Errorf("invalid execution authority")
	}
	return nil
}
func validateBinding(b Binding) error {
	if err := texts(b.ProviderSourceID, b.RightsSourceID, b.ResourceID); err != nil {
		return err
	}
	if b.ProviderSourceID == b.RightsSourceID {
		return fmt.Errorf("conflated provider identity")
	}
	return digests(b.DefinitionDigest, b.DecoderDigest, b.RightsDigest)
}
func validateObservation(o Observation, e Execution) error {
	if err := digests(o.ObservationID, o.RequestDigest, o.SHA256); err != nil {
		return err
	}
	if err := texts(o.ResourceID, o.FetchedAt, o.ContentType, o.Attestation); err != nil {
		return err
	}
	if err := safeURL(o.UpstreamURL); err != nil {
		return err
	}
	if o.Status < 200 || o.Status > 299 || o.Bytes < 0 || o.Bytes > 9007199254740991 || o.Attestation != e.Mode+"-executor-observed" {
		return fmt.Errorf("invalid observation")
	}
	if !instantShape.MatchString(o.FetchedAt) {
		return fmt.Errorf("invalid fetchedAt")
	}
	if _, err := time.Parse(time.RFC3339Nano, o.FetchedAt); err != nil {
		return fmt.Errorf("invalid fetchedAt")
	}
	if o.ReferenceDate != "" {
		t, err := time.Parse("2006-01-02", o.ReferenceDate)
		if err != nil || t.Format("2006-01-02") != o.ReferenceDate {
			return fmt.Errorf("invalid referenceDate")
		}
	}
	for _, s := range []string{o.LastModified, o.ETag} {
		if s != "" {
			if err := texts(s); err != nil {
				return err
			}
		}
	}
	return nil
}
func validateRights(p Plan) error {
	if p.SourceRights == nil || len(p.SourceRights) > MaxItems {
		return fmt.Errorf("invalid source rights")
	}
	seen := map[string]bool{}
	for _, r := range p.SourceRights {
		if err := texts(r.SourceID, r.Source.ServerID, string(r.DeclarationScope), r.DeclaredAt.ServerID, r.EvidenceOrigin); err != nil {
			return err
		}
		if seen[r.SourceID] || r.Pins == nil || r.Transformations == nil || len(r.Pins) > MaxItems || len(r.Transformations) > MaxItems || r.Declaration.Legacy() {
			return fmt.Errorf("invalid normalized source right")
		}
		seen[r.SourceID] = true
		if err := r.Declaration.Validate(0); err != nil {
			return err
		}
		for _, notice := range []*license.Notice{r.Attribution, r.FreeSource} {
			if notice == nil {
				continue
			}
			if strings.TrimSpace(notice.Text) == "" {
				return fmt.Errorf("empty rights notice")
			}
			if notice.URL != "" {
				if err := license.ValidateURL(notice.URL); err != nil {
					return err
				}
			}
		}
		if r.FreeSource != nil && r.FreeSource.URL == "" {
			return fmt.Errorf("free-source URL missing")
		}
		for _, pin := range r.Pins {
			if err := texts(pin.Role, pin.Repository, pin.Revision, pin.Path); err != nil {
				return err
			}
			if err := digests(pin.SHA256); err != nil {
				return err
			}
			if pin.Bytes < 0 || pin.Bytes > 9007199254740991 {
				return fmt.Errorf("unsafe pin bytes")
			}
		}
		for _, transform := range r.Transformations {
			if err := texts(transform); err != nil {
				return err
			}
		}
		// Every string follows v1 bounds, with multiline rights text preserved.
		raw, err := Canonical(r)
		if err != nil {
			return err
		}
		var obj any
		if err = json.Unmarshal(raw, &obj); err != nil {
			return err
		}
		if err = boundedJSON(obj, ""); err != nil {
			return err
		}
	}
	return nil
}
func boundedJSON(v any, key string) error {
	switch x := v.(type) {
	case string:
		return text(x, true, key == "text")
	case []any:
		if len(x) > MaxItems {
			return fmt.Errorf("overbound array")
		}
		for _, item := range x {
			if err := boundedJSON(item, key); err != nil {
				return err
			}
		}
	case map[string]any:
		for k, item := range x {
			if err := boundedJSON(item, k); err != nil {
				return err
			}
		}
	}
	return nil
}
func validatePlan(p Plan) error {
	if err := validateExecution(p.Execution); err != nil {
		return err
	}
	if p.Bindings == nil || p.Requests == nil || len(p.Bindings) > MaxItems || len(p.Requests) > MaxItems {
		return fmt.Errorf("invalid planned inventory")
	}
	if (p.MaxReads != nil && (*p.MaxReads < 0 || *p.MaxReads > MaxItems)) || p.MaxMetadataBytes < 0 || p.MaxMetadataBytes > MaxMetadataBytes {
		return fmt.Errorf("invalid planned budgets")
	}
	if err := validateRights(p); err != nil {
		return err
	}
	resources := map[string]bool{}
	for _, b := range p.Bindings {
		if err := validateBinding(b); err != nil {
			return err
		}
		if resources[b.ResourceID] {
			return fmt.Errorf("duplicate resource binding")
		}
		resources[b.ResourceID] = true
		found := false
		for _, r := range p.SourceRights {
			if r.SourceID == b.RightsSourceID {
				h, e := RightsDigest(r)
				if e != nil || h != b.RightsDigest {
					return fmt.Errorf("rights digest mismatch")
				}
				found = true
			}
		}
		if !found {
			return fmt.Errorf("binding source right missing")
		}
	}
	requests := map[string]bool{}
	for _, r := range p.Requests {
		if err := validateRequest(r); err != nil {
			return err
		}
		if !resources[r.ResourceID] {
			return fmt.Errorf("unplanned request resource")
		}
		h, e := RequestDigest(r)
		if e != nil {
			return e
		}
		if requests[h] {
			return fmt.Errorf("duplicate planned request")
		}
		requests[h] = true
	}
	return nil
}

// Validate verifies detached producer/consumer facts against trusted preflight,
// including actual legacy usage supplied by the execution, before output.
func Validate(e Envelope, p Plan, used []string) error {
	if err := validatePlan(p); err != nil {
		return err
	}
	if e.Format != Format || !equal(e.Execution, p.Execution) || !equal(e.Bindings, p.Bindings) {
		return fmt.Errorf("provider preflight mismatch")
	}
	maxReads, maxBytes := MaxItems, p.MaxMetadataBytes
	if p.MaxReads != nil {
		maxReads = *p.MaxReads
	}
	if maxBytes == 0 {
		maxBytes = MaxMetadataBytes
	}
	if e.Reads == nil || e.Usage == nil || len(e.Reads) > maxReads || len(e.Usage) > MaxItems || used == nil || len(used) > MaxItems {
		return fmt.Errorf("invalid evidence inventory")
	}
	usedSet := map[string]bool{}
	for _, id := range used {
		if usedSet[id] {
			return fmt.Errorf("duplicate used source")
		}
		known := false
		for _, r := range p.SourceRights {
			if r.SourceID == id {
				known = true
			}
		}
		if !known {
			return fmt.Errorf("unknown used source")
		}
		usedSet[id] = true
	}
	observations := map[string]Observation{}
	for _, o := range e.Reads {
		if err := validateObservation(o, e.Execution); err != nil {
			return err
		}
		if prior, ok := observations[o.ObservationID]; ok && !equal(prior, o) {
			return fmt.Errorf("conflicting observation ID")
		}
		observations[o.ObservationID] = o
		found := false
		for _, r := range p.Requests {
			h, _ := RequestDigest(r)
			if h == o.RequestDigest && r.ResourceID == o.ResourceID && r.UpstreamURL == o.UpstreamURL {
				found = true
			}
		}
		if !found {
			return fmt.Errorf("unplanned resource request")
		}
		found = false
		for _, b := range e.Bindings {
			if b.ResourceID == o.ResourceID {
				h, err := ObservationDigest(e.Execution, b, o)
				if err != nil || h != o.ObservationID {
					return fmt.Errorf("observation digest mismatch")
				}
				found = true
			}
		}
		if !found {
			return fmt.Errorf("unplanned observation binding")
		}
	}
	claimed := map[string]bool{}
	usages := map[string]Usage{}
	live := map[string]bool{}
	for _, u := range e.Usage {
		if err := texts(u.ProviderSourceID, u.RightsSourceID); err != nil {
			return err
		}
		if len(u.ObservationIDs) == 0 || len(u.ObservationIDs) > MaxItems || !usedSet[u.RightsSourceID] {
			return fmt.Errorf("invalid live usage")
		}
		key, _ := Digest([]string{u.ProviderSourceID, u.RightsSourceID})
		if prev, ok := usages[key]; ok && !equal(prev, u) {
			return fmt.Errorf("conflicting source usage")
		}
		usages[key] = u
		live[u.RightsSourceID] = true
		for _, id := range u.ObservationIDs {
			o, ok := observations[id]
			if !ok {
				return fmt.Errorf("missing usage observation")
			}
			found := false
			for _, b := range e.Bindings {
				if b.ResourceID == o.ResourceID && b.ProviderSourceID == u.ProviderSourceID && b.RightsSourceID == u.RightsSourceID {
					found = true
				}
			}
			if !found {
				return fmt.Errorf("source observation binding mismatch")
			}
			claimed[id] = true
		}
	}
	if len(claimed) != len(observations) {
		return fmt.Errorf("unclaimed observation")
	}
	for _, b := range e.Bindings {
		if usedSet[b.RightsSourceID] && !live[b.RightsSourceID] {
			return fmt.Errorf("missing live usage")
		}
	}
	raw, err := Canonical(map[string]any{"sourceRights": p.SourceRights, "usedSourceIds": used, "providerReads": e})
	if err != nil {
		return err
	}
	if len(raw) > maxBytes {
		return fmt.Errorf("metadata budget exceeded")
	}
	return nil
}
