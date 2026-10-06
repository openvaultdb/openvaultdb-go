package providerreads

import (
	"encoding/json"
	"fmt"
	"sync"
)

// Collector owns only observation metadata for one execution. It cannot fetch,
// retain source bytes or deduplicate network reads. Callers must reuse the actual
// transient response for any admitted self-join; joins remain unsupported here.
type Collector struct {
	mu       sync.Mutex
	plan     Plan
	envelope Envelope
	err      error
}

func NewCollector(plan Plan) (*Collector, error) {
	// Detach mutable admission maps/slices before validation or any read.
	raw, err := json.Marshal(plan)
	if err != nil {
		return nil, err
	}
	var frozen Plan
	if err = json.Unmarshal(raw, &frozen); err != nil {
		return nil, err
	}
	if err = validStringsValue(plan); err != nil {
		return nil, err
	}
	if err = validatePlan(frozen); err != nil {
		return nil, err
	}
	c := &Collector{plan: frozen, envelope: Envelope{Format: Format, Execution: frozen.Execution, Bindings: frozen.Bindings, Reads: []Observation{}, Usage: []Usage{}}}
	// Reserve fixed preflight metadata before reads; actual combined budget is
	// checked for every observation and again at finalization before output.
	used := []string{}
	raw, err = Canonical(map[string]any{"sourceRights": frozen.SourceRights, "usedSourceIds": used, "providerReads": c.envelope})
	if err != nil {
		return nil, err
	}
	bound := frozen.MaxMetadataBytes
	if bound == 0 {
		bound = MaxMetadataBytes
	}
	if len(raw) > bound {
		return nil, fmt.Errorf("preflight metadata budget exceeded")
	}
	return c, nil
}
func validStringsValue(v any) error { _, err := Canonical(v); return err }

// Add consumes an adapter observation, never a response body. Request must be
// independently admitted; the caller cannot introduce a late arbitrary URL.
func (c *Collector) Add(request Request, read Observation) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return c.err
	}
	err := c.add(request, read)
	if err != nil {
		c.err = err
	}
	return err
}
func (c *Collector) add(request Request, read Observation) error {
	h, err := RequestDigest(request)
	if err != nil {
		return err
	}
	planned := false
	for _, r := range c.plan.Requests {
		if equal(r, request) {
			planned = true
		}
	}
	if !planned {
		return fmt.Errorf("unplanned request")
	}
	if read.ResourceID != request.ResourceID || read.UpstreamURL != request.UpstreamURL {
		return fmt.Errorf("observed resource mismatch")
	}
	read.RequestDigest = h
	read.Attestation = c.plan.Execution.Mode + "-executor-observed"
	var binding Binding
	found := false
	for _, b := range c.plan.Bindings {
		if b.ResourceID == read.ResourceID {
			binding = b
			found = true
		}
	}
	if !found {
		return fmt.Errorf("missing resource binding")
	}
	read.ObservationID, err = ObservationDigest(c.plan.Execution, binding, read)
	if err != nil {
		return err
	}
	if err = validateObservation(read, c.plan.Execution); err != nil {
		return err
	}
	for _, prior := range c.envelope.Reads {
		if prior.ObservationID == read.ObservationID {
			if !equal(prior, read) {
				return fmt.Errorf("conflicting observation")
			}
			return nil
		}
	}
	c.envelope.Reads = append(c.envelope.Reads, read)
	index := -1
	for i, u := range c.envelope.Usage {
		if u.ProviderSourceID == binding.ProviderSourceID && u.RightsSourceID == binding.RightsSourceID {
			index = i
		}
	}
	if index < 0 {
		c.envelope.Usage = append(c.envelope.Usage, Usage{ProviderSourceID: binding.ProviderSourceID, RightsSourceID: binding.RightsSourceID, ObservationIDs: []string{read.ObservationID}})
	} else {
		c.envelope.Usage[index].ObservationIDs = append(c.envelope.Usage[index].ObservationIDs, read.ObservationID)
	}
	used := []string{}
	for _, u := range c.envelope.Usage {
		seen := false
		for _, id := range used {
			if id == u.RightsSourceID {
				seen = true
			}
		}
		if !seen {
			used = append(used, u.RightsSourceID)
		}
	}
	return Validate(c.envelope, c.plan, used)
}
func (c *Collector) Finish(used []string) (Envelope, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return Envelope{}, c.err
	}
	if err := Validate(c.envelope, c.plan, used); err != nil {
		return Envelope{}, err
	}
	raw, err := json.Marshal(c.envelope)
	if err != nil {
		return Envelope{}, err
	}
	var out Envelope
	err = json.Unmarshal(raw, &out)
	return out, err
}
