package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/license"
)

// WithSourceRights supplies the operator's stable server identity and optional
// authored server license. The identity is never inferred from a request Host.
// A nil declaration permits database/recordset declarations without server terms.
// Legacy SPDX parsing uses the Directory-shaped runtime syntax policy; this
// option does not certify Publisher/Directory admission or legal applicability.
func WithSourceRights(serverID string, declaration *license.Declaration) Option {
	return func(s *Server) {
		if strings.TrimSpace(serverID) == "" || len(serverID) > 256 || !utf8.ValidString(serverID) {
			panic("invalid source-rights server identity")
		}
		for _, ch := range serverID {
			if unicode.IsControl(ch) {
				panic("invalid source-rights server identity")
			}
		}
		s.rightsServerID = serverID
		s.serverLicense = nil
		if declaration != nil {
			if err := declaration.Validate(license.Directory); err != nil {
				panic(err)
			}
			copy := *declaration
			s.serverLicense = &copy
		}
	}
}
func (s *Server) validateSourceRights() error {
	if _, _, err := license.Inventory(s.serverRights()); err != nil {
		return err
	}
	for _, db := range s.dbs {
		if db != nil && db.HasLicenseDeclarations() && s.rightsServerID == "" {
			return fmt.Errorf("database terms require WithSourceRights and a stable server identity")
		}
	}
	return nil
}
func (s *Server) serverRights() []license.SourceRight {
	if s.serverLicense == nil {
		return nil
	}
	right, _ := license.Resolve(license.Identity{ServerID: s.rightsServerID}, s.serverLicense, nil, nil)
	if right == nil {
		return nil
	}
	return []license.SourceRight{*right}
}
func (s *Server) databaseRights(db *core.Database, collections ...string) ([]license.SourceRight, error) {
	if s.rightsServerID == "" {
		return nil, nil
	}
	out := make([]license.SourceRight, 0, len(collections))
	for _, collection := range collections {
		if canonical, declared := db.CanonicalCollection(collection); declared {
			collection = canonical
		}
		if profile, ok := s.providerProfilesByDB[db]; ok && profile.SourceRight != nil && profile.Collection == collection {
			out = append(out, cloneDynamicRight(*profile.SourceRight))
			continue
		}
		right, err := db.SourceRight(s.rightsServerID, s.serverLicense, collection)
		if err != nil {
			return nil, err
		}
		if right != nil {
			out = append(out, *right)
		}
	}
	inventory, _, err := license.Inventory(out)
	return inventory, err
}
func attachRights(response map[string]any, rights []license.SourceRight, used []string) {
	if len(rights) == 0 {
		return
	}
	response["sourceRights"] = rights
	if used != nil {
		response["usedSourceIds"] = used
	}
}

// rightsCapture is reserved before execution. It contains the entire classified
// source inventory; Used sources are supplied from the executor after reading.
type rightsCapture struct {
	rights        []license.SourceRight
	planned       map[string]bool
	reservedBytes int
}

func (s *Server) captureRights(databases map[string]*core.Database, targets []relationalTarget) (rightsCapture, error) {
	capture := rightsCapture{planned: map[string]bool{}}
	if s.rightsServerID == "" {
		return capture, nil
	}
	var all []license.SourceRight
	for _, target := range targets {
		db := databases[target.database]
		if db == nil {
			return capture, fmt.Errorf("source rights target is unavailable")
		}
		identity := license.Identity{ServerID: s.rightsServerID, DatabaseID: db.ID(), Recordset: target.collection}
		if canonical, declared := db.CanonicalCollection(target.collection); declared {
			identity.Recordset = canonical
		}
		capture.planned[identity.SourceID()] = true
		rights, err := s.databaseRights(db, target.collection)
		if err != nil {
			return capture, err
		}
		all = append(all, rights...)
	}
	var err error
	capture.rights, _, err = license.Inventory(all)
	if err != nil {
		return capture, err
	}
	if len(capture.rights) > 0 {
		used := make([]string, 0, len(capture.planned))
		for id := range capture.planned {
			used = append(used, id)
		}
		sort.Strings(used)
		// Reserve keys, inventory and the worst-case complete used-source list.
		metadata := map[string]any{}
		attachRights(metadata, capture.rights, used)
		encoded, err := json.Marshal(metadata)
		if err != nil {
			return capture, err
		}
		capture.reservedBytes = len(encoded)
		if capture.reservedBytes > license.MaxEvidenceBytes {
			return capture, fmt.Errorf("source rights exceed 256 KiB evidence limit")
		}
	}
	return capture, nil
}
func (s *Server) singleRights(db *core.Database, collection string) (rightsCapture, error) {
	return s.captureRights(map[string]*core.Database{db.ID(): db}, []relationalTarget{{database: db.ID(), collection: collection}})
}
func (c rightsCapture) used(ids []string) ([]string, error) {
	if len(c.rights) == 0 {
		return nil, nil
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if !c.planned[id] {
			return nil, fmt.Errorf("execution read an unexpected source")
		}
		seen[id] = true
	}
	used := make([]string, 0, len(seen))
	for id := range seen {
		used = append(used, id)
	}
	sort.Strings(used)
	return used, nil
}
func (c rightsCapture) allUsed() []string {
	if len(c.rights) == 0 {
		return nil
	}
	ids := make([]string, 0, len(c.planned))
	for id := range c.planned {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
func (s *Server) rightsError(w http.ResponseWriter, err error) {
	// Configuration/query evidence failures never disclose protected identities.
	writeError(w, http.StatusUnprocessableEntity, "source_rights_invalid", "source data terms could not be captured within the evidence budget")
}
func (s *Server) writeRightsResult(w http.ResponseWriter, r *http.Request, response map[string]any, capture rightsCapture, used []string, maxBytes int) {
	attachRights(response, capture.rights, used)
	if len(capture.rights) > 0 {
		encoded, err := json.Marshal(response)
		if err != nil {
			if r != nil {
				s.writeMappedError(w, r, err)
			} else {
				writeMappedError(w, err)
			}
			return
		}
		if len(encoded)+1 > maxBytes {
			s.writeMappedError(w, r, core.ErrResultTooLarge)
			return
		}
	}
	writeJSON(w, http.StatusOK, response)
}
