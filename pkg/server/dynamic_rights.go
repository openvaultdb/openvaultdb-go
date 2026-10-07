package server

import (
	"fmt"
	"maps"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/license"
	"github.com/openvaultdb/openvaultdb-go/pkg/manifest"
	"github.com/openvaultdb/openvaultdb-go/pkg/providerreads"
)

var dynamicRepository = regexp.MustCompile(`^https://github\.com/[A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z0-9][A-Za-z0-9._-]*$`)
var dynamicRevision = regexp.MustCompile(`^[a-f0-9]{40}$`)

func cloneDynamicRight(right license.SourceRight) license.SourceRight {
	right.Pins = slices.Clone(right.Pins)
	right.Transformations = slices.Clone(right.Transformations)
	if right.PublisherHTMLDefinition != nil {
		copy := *right.PublisherHTMLDefinition
		copy.NativeFields = slices.Clone(copy.NativeFields)
		right.PublisherHTMLDefinition = &copy
	}
	if right.Attribution != nil {
		copy := *right.Attribution
		right.Attribution = &copy
	}
	if right.FreeSource != nil {
		copy := *right.FreeSource
		right.FreeSource = &copy
	}
	return right
}

func cloneProviderProfiles(profiles map[string]ProviderReadProfile) map[string]ProviderReadProfile {
	copy := maps.Clone(profiles)
	for id, profile := range copy {
		if profile.SourceRight != nil {
			right := cloneDynamicRight(*profile.SourceRight)
			profile.SourceRight = &right
			copy[id] = profile
		}
	}
	return copy
}

// Trusted startup configuration asserts repository verification; the server
// validates exact binding, not Git authenticity, semantics or legal clearance.
func (s *Server) validateDynamicProviderRight(db *core.Database, profile ProviderReadProfile) error {
	right := profile.SourceRight
	if profile.RequestProfile == IANANativeOperatorRequestProfile &&
		(right == nil || right.Declaration.URL != manifest.IANALicensingTermsURL) {
		return fmt.Errorf("IANA operator profile requires pinned publisher definition and exact registry terms")
	}
	if right == nil {
		return nil
	}
	base, err := db.SourceRight(s.rightsServerID, s.serverLicense, profile.Collection)
	freeSourceURL := manifest.ECBDailyURL
	if db.HTTPProfile() == manifest.HTTPProfileIANAHTTPStatus {
		freeSourceURL = manifest.IANAHTTPStatusURL
	}
	if err != nil || base == nil || right.SourceID != base.SourceID || right.Source != base.Source ||
		right.DeclaredAt != base.DeclaredAt || right.DeclarationScope != base.DeclarationScope ||
		right.Declaration.Legacy() || right.Declaration != base.Declaration ||
		len(right.Pins) != 1 ||
		right.Attribution == nil || right.FreeSource == nil || right.FreeSource.URL != freeSourceURL ||
		len(right.Transformations) == 0 {
		return fmt.Errorf("dynamic provider rights must preserve mounted identity/terms and complete definition notices")
	}
	pin := right.Pins[0]
	if right.EvidenceOrigin == "publisher-html-metadata-verified" {
		return validateIANAPublisherHTML(profile, pin)
	}
	if right.EvidenceOrigin != "publisher-definition-verified" || right.PublisherHTMLDefinition != nil {
		return fmt.Errorf("unsupported dynamic publisher evidence origin")
	}
	if pin.Role != "provider" || !dynamicRepository.MatchString(pin.Repository) ||
		!dynamicRevision.MatchString(pin.Revision) || pin.SHA256 != profile.Binding.DefinitionDigest || pin.Bytes <= 0 ||
		pin.Path == "" || pin.Path == "." || path.Clean(pin.Path) != pin.Path || strings.HasPrefix(pin.Path, "/") ||
		strings.HasPrefix(pin.Path, "../") || strings.Contains(pin.Path, "\\") {
		return fmt.Errorf("dynamic provider rights require the exact immutable publisher definition pin")
	}
	return nil // ProviderReadPlan validation additionally checks text/digest/budget bounds.
}

func validateIANAPublisherHTML(profile ProviderReadProfile, pin license.Pin) error {
	d := profile.SourceRight.PublisherHTMLDefinition
	if profile.RequestProfile != IANANativeOperatorRequestProfile || d == nil {
		return fmt.Errorf("publisher HTML metadata requires the IANA native operator profile")
	}
	observed, err := time.Parse(time.RFC3339, d.ObservedAt)
	if err != nil || observed.IsZero() ||
		d.Format != "ovdb-iana-publisher-html-definition/1" ||
		d.RegistryURL != "https://www.iana.org/assignments/http-status-codes" ||
		d.TermsURL != manifest.IANALicensingTermsURL || d.ResourceURL != manifest.IANAHTTPStatusURL ||
		d.RegistryBytes <= 0 || d.RegistryBytes > 2<<20 || d.TermsBytes <= 0 || d.TermsBytes > 64<<10 ||
		!slices.Equal(d.NativeFields, []string{"Value", "Description", "Reference"}) ||
		d.RightsScope != "iana-ietf-held-protocol-registry-rights-cc0-excluding-linked-material" {
		return fmt.Errorf("invalid IANA publisher HTML metadata")
	}
	for _, hash := range []string{d.RegistrySHA256, d.TermsSHA256, pin.SHA256} {
		if len(hash) != 64 || strings.Trim(hash, "0123456789abcdef") != "" {
			return fmt.Errorf("invalid publisher metadata digest")
		}
	}
	definitionDigest, err := providerreads.Digest(d)
	if err != nil || definitionDigest != profile.Binding.DefinitionDigest ||
		pin.Role != "discovery" || pin.Repository != "https://github.com/openvaultdb/directory" ||
		!dynamicRevision.MatchString(pin.Revision) || pin.Path != "sources/$records/iana-http-status-codes.yaml" ||
		pin.Bytes <= 0 || pin.Bytes > 32<<10 {
		return fmt.Errorf("publisher HTML definition and Directory discovery pins must remain distinct")
	}
	return nil
}
