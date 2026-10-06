package manifest

import (
	"fmt"
	"github.com/openvaultdb/openvaultdb-go/pkg/license"
	"gopkg.in/yaml.v3"
	"strings"
)

// ValidateLicenses also covers manifests constructed by embedding applications.
func (m *Manifest) ValidateLicenses() error {
	if m.Database.License != nil {
		if err := m.Database.License.Validate(license.Directory); err != nil {
			return fmt.Errorf("database.license: %w", err)
		}
	}
	for name, decl := range m.RecordsetLicenses {
		if strings.TrimSpace(name) == "" || len(name) > 256 {
			return fmt.Errorf("recordset license name is invalid")
		}
		if err := decl.Validate(license.Directory); err != nil {
			return fmt.Errorf("recordset license: %w", err)
		}
	}
	return nil
}

// yaml.v3 skips UnmarshalYAML for null pointer fields. Inspect authorship,
// including aliases/merge defaults, so an invalid declaration cannot become
// omission and accidentally inherit broader terms.
func validateDatabaseLicenseNode(node *yaml.Node, depth int) error {
	if depth > 32 || node == nil {
		return fmt.Errorf("database license nesting is invalid")
	}
	if node.Kind == yaml.AliasNode {
		return validateDatabaseLicenseNode(node.Alias, depth+1)
	}
	if node.Kind == yaml.SequenceNode {
		for _, item := range node.Content {
			if err := validateDatabaseLicenseNode(item, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	if node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		switch node.Content[i].Value {
		case "license":
			if _, err := license.ParseYAML(node.Content[i+1], license.Directory); err != nil {
				return fmt.Errorf("database.license: %w", err)
			}
		case "<<":
			if err := validateDatabaseLicenseNode(node.Content[i+1], depth+1); err != nil {
				return err
			}
		}
	}
	return nil
}
