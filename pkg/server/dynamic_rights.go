package server

import (
	"fmt"
	"maps"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/license"
	"github.com/openvaultdb/openvaultdb-go/pkg/manifest"
)

var dynamicRepository = regexp.MustCompile(`^https://github\.com/[A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z0-9][A-Za-z0-9._-]*$`)
var dynamicRevision = regexp.MustCompile(`^[a-f0-9]{40}$`)

func cloneDynamicRight(right license.SourceRight) license.SourceRight {
	right.Pins = slices.Clone(right.Pins)
	right.Transformations = slices.Clone(right.Transformations)
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
		right.EvidenceOrigin != "publisher-definition-verified" || len(right.Pins) != 1 ||
		right.Attribution == nil || right.FreeSource == nil || right.FreeSource.URL != freeSourceURL ||
		len(right.Transformations) == 0 {
		return fmt.Errorf("dynamic provider rights must preserve mounted identity/terms and complete definition notices")
	}
	pin := right.Pins[0]
	if pin.Role != "provider" || !dynamicRepository.MatchString(pin.Repository) ||
		!dynamicRevision.MatchString(pin.Revision) || pin.SHA256 != profile.Binding.DefinitionDigest || pin.Bytes <= 0 ||
		pin.Path == "" || pin.Path == "." || path.Clean(pin.Path) != pin.Path || strings.HasPrefix(pin.Path, "/") ||
		strings.HasPrefix(pin.Path, "../") || strings.Contains(pin.Path, "\\") {
		return fmt.Errorf("dynamic provider rights require the exact immutable publisher definition pin")
	}
	return nil // ProviderReadPlan validation additionally checks text/digest/budget bounds.
}
