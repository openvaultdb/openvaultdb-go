package license

import (
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
)

type Scope string

const (
	ServerScope    Scope = "server"
	DatabaseScope  Scope = "database"
	RecordsetScope Scope = "recordset"
)

type Identity struct {
	ServerID   string `json:"serverId"`
	DatabaseID string `json:"databaseId,omitempty"`
	Recordset  string `json:"recordset,omitempty"`
}

// SourceID encodes every identity component unambiguously, including punctuation.
func (i Identity) SourceID() string {
	return "ovdb:" + url.PathEscape(i.ServerID) + "/" + url.PathEscape(i.DatabaseID) + "/" + url.PathEscape(i.Recordset)
}

type Pin struct {
	Role       string `json:"role"`
	Repository string `json:"repository"`
	Revision   string `json:"revision"`
	Path       string `json:"path"`
	SHA256     string `json:"sha256"`
	Bytes      int64  `json:"bytes"`
}
type Notice struct {
	Text string `json:"text"`
	URL  string `json:"url,omitempty"`
}

// SourceRight is a source-data terms inventory entry, not an output license.
// This reader foundation emits only unpinned author declarations. Publisher
// evidence must be verified by publication/preparation before attaching pins.
type SourceRight struct {
	SourceID         string      `json:"sourceId"`
	Source           Identity    `json:"source"`
	Declaration      Declaration `json:"declaration"`
	DeclarationScope Scope       `json:"declarationScope"`
	DeclaredAt       Identity    `json:"declaredAt"`
	EvidenceOrigin   string      `json:"evidenceOrigin"`
	Pins             []Pin       `json:"pins"`
	Attribution      *Notice     `json:"attribution,omitempty"`
	FreeSource       *Notice     `json:"freeSource,omitempty"`
	Transformations  []string    `json:"transformations"`
}

// Resolve validates every supplied override. It chooses one whole declaration;
// fields of parent and child declarations are never combined.
func Resolve(source Identity, server, database, recordset *Declaration) (*SourceRight, error) {
	if source.ServerID == "" {
		return nil, fmt.Errorf("source rights require a configured server identity")
	}
	for _, d := range []*Declaration{server, database, recordset} {
		if d != nil {
			if err := d.Validate(Directory); err != nil {
				return nil, err
			}
		}
	}
	d, scope, at := server, ServerScope, Identity{ServerID: source.ServerID}
	if source.DatabaseID != "" && database != nil {
		d, scope, at = database, DatabaseScope, Identity{ServerID: source.ServerID, DatabaseID: source.DatabaseID}
	}
	if source.Recordset != "" && recordset != nil {
		d, scope, at = recordset, RecordsetScope, source
	}
	if d == nil {
		return nil, nil
	}
	origin := "server-declared"
	if d.Legacy() {
		origin = "legacy-metadata"
	}
	return &SourceRight{SourceID: source.SourceID(), Source: source, Declaration: d.Normalized(), DeclarationScope: scope, DeclaredAt: at, EvidenceOrigin: origin, Pins: []Pin{}, Transformations: []string{}}, nil
}

// Inventory returns deterministic distinct source identities and rejects stale
// conflicting evidence. Its encoded size is reserved before releasing data.
func Inventory(rights []SourceRight) ([]SourceRight, int, error) {
	byID := map[string]SourceRight{}
	encodedBytes := 2
	for _, right := range rights {
		if prior, ok := byID[right.SourceID]; ok {
			a, _ := json.Marshal(prior)
			b, _ := json.Marshal(right)
			if string(a) != string(b) {
				return nil, 0, fmt.Errorf("conflicting source rights")
			}
			continue
		}
		encoded, err := json.Marshal(right)
		if err != nil {
			return nil, 0, err
		}
		if len(byID) > 0 {
			encodedBytes++
		}
		encodedBytes += len(encoded)
		if encodedBytes > MaxEvidenceBytes {
			return nil, 0, fmt.Errorf("source rights exceed 256 KiB evidence limit")
		}
		byID[right.SourceID] = right
	}
	out := make([]SourceRight, 0, len(byID))
	for _, right := range byID {
		out = append(out, right)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SourceID < out[j].SourceID })
	encoded, err := json.Marshal(out)
	if err != nil {
		return nil, 0, err
	}
	if len(encoded) > MaxEvidenceBytes {
		return nil, 0, fmt.Errorf("source rights exceed 256 KiB evidence limit")
	}
	return out, len(encoded), nil
}
