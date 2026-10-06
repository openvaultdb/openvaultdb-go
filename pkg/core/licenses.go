package core

import (
	"fmt"

	"github.com/openvaultdb/openvaultdb-go/pkg/license"
	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
)

type licenseSnapshot struct {
	databaseID string
	database   *license.Declaration
	recordsets map[string]license.Declaration
}

func (d *Database) snapshotLicenses() error {
	d.rights.databaseID = d.ID()
	if declared := d.Manifest.Database.License; declared != nil {
		copy := *declared
		d.rights.database = &copy
	}
	d.rights.recordsets = map[string]license.Declaration{}
	for name, decl := range d.Manifest.RecordsetLicenses {
		if err := ValidateCollectionName(name); err != nil {
			return fmt.Errorf("recordset license: %w", err)
		}
		if canonical, declared := d.CanonicalCollection(name); declared && canonical != name {
			return fmt.Errorf("recordset licenses require canonical collection names")
		}
		if !d.isDocumentEngine() || d.Manifest.Database.SchemaMode == schema.ModeStrict {
			if _, declared := d.CanonicalCollection(name); !declared {
				return fmt.Errorf("recordset license references an undeclared collection")
			}
		}
		d.rights.recordsets[name] = decl
	}
	return nil
}

// HasLicenseDeclarations reports the frozen authored declarations, never ACLs.
func (d *Database) HasLicenseDeclarations() bool {
	return d.rights.database != nil || len(d.rights.recordsets) > 0
}

// SourceRight resolves terms for an already authorized source. Callers must
// enforce access before disclosing the result. Core does not infer permission.
func (d *Database) SourceRight(serverID string, server *license.Declaration, collection string) (*license.SourceRight, error) {
	if canonical, declared := d.CanonicalCollection(collection); declared {
		collection = canonical
	}
	var override *license.Declaration
	if declared, ok := d.rights.recordsets[collection]; ok {
		copy := declared
		override = &copy
	}
	return license.Resolve(license.Identity{ServerID: serverID, DatabaseID: d.rights.databaseID, Recordset: collection}, server, d.rights.database, override)
}
