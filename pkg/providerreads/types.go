package providerreads

import (
	"fmt"
	"github.com/openvaultdb/openvaultdb-go/pkg/license"
)

const Format = "ovdb-provider-read/1"
const MaxItems = 64
const MaxMetadataBytes = 256 * 1024

type Execution struct {
	ID         string `json:"id"`
	Mode       string `json:"mode"`
	ExecutorID string `json:"executorId"`
}
type Binding struct {
	ProviderSourceID string `json:"providerSourceId"`
	RightsSourceID   string `json:"rightsSourceId"`
	ResourceID       string `json:"resourceId"`
	DefinitionDigest string `json:"definitionDigest"`
	DecoderDigest    string `json:"decoderDigest"`
	RightsDigest     string `json:"rightsDigest"`
}
type Observation struct {
	ObservationID string `json:"observationId"`
	ResourceID    string `json:"resourceId"`
	RequestDigest string `json:"requestDigest"`
	FetchedAt     string `json:"fetchedAt"`
	UpstreamURL   string `json:"upstreamUrl"`
	Status        int    `json:"status"`
	ContentType   string `json:"contentType"`
	SHA256        string `json:"sha256"`
	Bytes         int64  `json:"bytes"`
	ReferenceDate string `json:"referenceDate,omitempty"`
	LastModified  string `json:"lastModified,omitempty"`
	ETag          string `json:"etag,omitempty"`
	Attestation   string `json:"attestation"`
}
type Usage struct {
	ProviderSourceID string   `json:"providerSourceId"`
	RightsSourceID   string   `json:"rightsSourceId"`
	ObservationIDs   []string `json:"observationIds"`
}
type Envelope struct {
	Format    string        `json:"format"`
	Execution Execution     `json:"execution"`
	Bindings  []Binding     `json:"bindings"`
	Reads     []Observation `json:"reads"`
	Usage     []Usage       `json:"usage"`
}
type Request struct {
	ResourceID  string         `json:"resourceId"`
	Method      string         `json:"method"`
	UpstreamURL string         `json:"upstreamUrl"`
	Params      map[string]any `json:"params"`
}

// Plan is trusted admission input, detached before reads. DefinitionDigest must
// resolve a verified immutable definition that binds exact model/meaning artifacts.
type Plan struct {
	Execution        Execution
	Bindings         []Binding
	Requests         []Request
	SourceRights     []license.SourceRight
	MaxReads         *int
	MaxMetadataBytes int
}

func RightsDigest(right license.SourceRight) (string, error) {
	return Digest(map[string]any{"format": "ovdb-rights-binding/1", "right": right})
}
func RequestDigest(r Request) (string, error) {
	return Digest(map[string]any{"format": "ovdb-resource-request/1", "resourceId": r.ResourceID, "method": r.Method, "upstreamUrl": r.UpstreamURL, "params": r.Params})
}
func ObservationDigest(ex Execution, b Binding, o Observation) (string, error) {
	// Omit exactly the top-level observationId; nested keys are preserved.
	raw := map[string]any{"resourceId": o.ResourceID, "requestDigest": o.RequestDigest, "fetchedAt": o.FetchedAt, "upstreamUrl": o.UpstreamURL, "status": o.Status, "contentType": o.ContentType, "sha256": o.SHA256, "bytes": o.Bytes, "attestation": o.Attestation}
	if o.ReferenceDate != "" {
		raw["referenceDate"] = o.ReferenceDate
	}
	if o.LastModified != "" {
		raw["lastModified"] = o.LastModified
	}
	if o.ETag != "" {
		raw["etag"] = o.ETag
	}
	return Digest(map[string]any{"format": "ovdb-read-observation-id/1", "execution": ex, "binding": b, "read": raw})
}

// Metadata captures only response evidence. Rows and other parent response
// fields must not be accessed or cloned by the evidence gate.
type Metadata struct {
	SourceRights  []license.SourceRight `json:"sourceRights"`
	UsedSourceIDs []string              `json:"usedSourceIds"`
	ProviderReads *Envelope             `json:"providerReads"`
}

// ValidateMetadata requires full response-rights and legacy-usage equality with
// independent admission facts before checking the envelope/digest bindings.
func ValidateMetadata(metadata Metadata, plan Plan, admittedUsed []string) error {
	if metadata.ProviderReads == nil {
		return fmt.Errorf("required provider evidence missing")
	}
	if !equal(metadata.SourceRights, plan.SourceRights) || !equal(metadata.UsedSourceIDs, admittedUsed) {
		return fmt.Errorf("response rights or usage preflight mismatch")
	}
	return Validate(*metadata.ProviderReads, plan, admittedUsed)
}
