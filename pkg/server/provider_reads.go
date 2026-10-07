package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/dal-go/dalgo2http"
	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/license"
	"github.com/openvaultdb/openvaultdb-go/pkg/manifest"
	"github.com/openvaultdb/openvaultdb-go/pkg/providerreads"
)

// ProviderExecutionIDHeader correlates a query with its independently admitted
// execution. It grants no source, resource, rights or executor authority.
const ProviderExecutionIDHeader = "OVDB-Execution-ID"

var errProviderExecutionID = errors.New("OVDB-Execution-ID must have exactly one 32-character lowercase hexadecimal value")

// ProviderReadProfile is trusted, externally verified admission configuration.
// It does not verify artifact existence or authorize public activation. Definition
// digest must bind the exact decoder, model/meaning and local recordset mapping.
// Consumers must advertise Format before this opt-in producer is configured.
type ProviderReadProfile struct {
	// RequestProfile opts this admitted instance into a fixed request contract.
	RequestProfile string `json:"requestProfile,omitempty"`
	Collection     string
	Binding        providerreads.Binding
	// SourceRight optionally supplies reviewed dynamic-definition notices and
	// a metadata-only publisher pin. It cannot replace the mounted declaration,
	// change access/retention, or certify immutable upstream response bytes.
	SourceRight *license.SourceRight `json:"sourceRight,omitempty"`
}

// WithProviderReadProfiles opts selected HTTP instances into providerReads v1.
// Neither query input nor request Host can choose a binding or upstream URL.
func WithProviderReadProfiles(profiles map[string]ProviderReadProfile) Option {
	frozen := cloneProviderProfiles(profiles)
	return func(s *Server) { s.providerProfiles = cloneProviderProfiles(frozen) }
}
func (s *Server) validateProviderProfiles() error {
	s.providerProfilesByDB = map[*core.Database]ProviderReadProfile{}
	for id, p := range s.providerProfiles {
		db := s.dbs[id]
		if db == nil || !db.ReadOnlyHTTP() || !db.NoRetention() || s.rightsServerID == "" {
			return fmt.Errorf("provider profile requires a no-retention HTTP instance and stable rights server")
		}
		if err := validateProviderRequestProfile(db, p); err != nil {
			return err
		}
		canonical, ok := db.CanonicalCollection(p.Collection)
		if !ok || canonical != p.Collection {
			return fmt.Errorf("provider profile collection is undeclared")
		}
		if prior, ok := s.providerProfilesByDB[db]; ok && !reflect.DeepEqual(prior, p) {
			return fmt.Errorf("conflicting provider profiles across mount aliases")
		}
		if err := s.validateDynamicProviderRight(db, p); err != nil {
			return err
		}
		// The trusted detached notices participate in the same startup rights
		// capture and digest validation as legacy declarations.
		s.providerProfilesByDB[db] = p
		capture, err := s.singleRights(db, p.Collection)
		if err != nil {
			return err
		}
		if len(capture.rights) != 1 || capture.rights[0].SourceID != p.Binding.RightsSourceID {
			return fmt.Errorf("provider profile rights mismatch")
		}
		if _, err = providerreads.NewCollector(s.providerPlan(p, capture, "startup")); err != nil {
			return err
		}
	}
	// A library mount alone is transport capability, not an admitted server
	// execution path. Every IANA server alias must resolve to the closed native
	// operator profile and its checked rights/binding before any route starts.
	for _, db := range s.dbs {
		if db.Manifest.Storage.HTTP != nil && db.Manifest.Storage.HTTP.Profile == manifest.HTTPProfileIANAHTTPStatus &&
			s.providerProfilesByDB[db].RequestProfile != IANANativeOperatorRequestProfile {
			return fmt.Errorf("IANA HTTP mount requires admitted native operator profile")
		}
	}
	if len(s.providerProfilesByDB) > 0 && s.corsCfg != nil {
		cfg, err := s.corsCfg.WithHeaders([]string{ProviderExecutionIDHeader}, nil)
		if err != nil {
			return err
		}
		s.corsCfg = cfg
	}
	return nil
}
func (s *Server) providerPlan(p ProviderReadProfile, capture rightsCapture, id string) providerreads.Plan {
	upstreamURL := manifest.ECBDailyURL
	if p.RequestProfile == IANANativeOperatorRequestProfile {
		upstreamURL = manifest.IANAHTTPStatusURL
	}
	return providerreads.Plan{Execution: providerreads.Execution{ID: id, Mode: "proxy", ExecutorID: s.rightsServerID}, Bindings: []providerreads.Binding{p.Binding}, Requests: []providerreads.Request{{ResourceID: p.Binding.ResourceID, Method: "GET", UpstreamURL: upstreamURL, Params: map[string]any{}}}, SourceRights: capture.rights, MaxReads: new(1), MaxMetadataBytes: providerreads.MaxMetadataBytes}
}

type providerCapture struct {
	collector *providerreads.Collector
	mu        sync.Mutex
	err       error
}

func (s *Server) beginProviderRead(r *http.Request, db *core.Database, collection string, capture rightsCapture) (context.Context, *providerCapture, error) {
	ctx := r.Context()
	p, configured := s.providerProfilesByDB[db]
	if !configured {
		return ctx, nil, nil
	}
	if p.Collection != collection {
		return ctx, nil, fmt.Errorf("unplanned provider recordset")
	}
	id, err := providerExecutionID(r.Header)
	if err != nil {
		return ctx, nil, err
	}
	plan := s.providerPlan(p, capture, id)
	collector, err := providerreads.NewCollector(plan)
	if err != nil {
		return ctx, nil, err
	}
	pc := &providerCapture{collector: collector}
	// ECB can produce one read; reserve bounded header + envelope space before
	// fetching, within the smaller combined evidence and whole-response budgets.
	if capture.reservedBytes+16*1024 > providerreads.MaxMetadataBytes {
		return ctx, nil, fmt.Errorf("provider metadata reservation exceeded")
	}
	observed := dalgo2http.ContextWithProvenanceObserver(ctx, func(o dalgo2http.Provenance) {
		pc.mu.Lock()
		defer pc.mu.Unlock()
		if pc.err != nil {
			return
		}
		valid := o.Source == dalgo2http.SourceLive && o.Collection == collection && !o.FetchedAt.IsZero()
		if p.RequestProfile == IANANativeOperatorRequestProfile {
			valid = valid && o.Decoder == dalgo2http.DecoderStrictCSV3 && o.BaseCurrency == "" && o.ReferenceDate == "" && o.Bytes <= 64<<10
		} else {
			valid = valid && o.Decoder == dalgo2http.DecoderECBEuroFXRef && o.BaseCurrency == "EUR" && o.ReferenceDate != "" && o.Bytes <= 2<<20
		}
		if !valid {
			pc.err = fmt.Errorf("provider transport observation mismatch")
			return
		}
		pc.err = collector.Add(plan.Requests[0], providerreads.Observation{ResourceID: p.Binding.ResourceID, FetchedAt: o.FetchedAt.UTC().Truncate(time.Millisecond).Format("2006-01-02T15:04:05.000Z"), UpstreamURL: o.UpstreamURL, Status: o.StatusCode, ContentType: o.ContentType, SHA256: o.SHA256, Bytes: int64(o.Bytes), ReferenceDate: o.ReferenceDate, LastModified: o.LastModified, ETag: o.ETag})
	})
	return observed, pc, nil
}

func providerExecutionID(headers http.Header) (string, error) {
	// Inspect every case variant, rather than Header.Get's first value. HTTP
	// parsers may retain duplicates separately or coalesce them with commas.
	count, id, present := 0, "", false
	for name, values := range headers {
		if strings.EqualFold(name, ProviderExecutionIDHeader) {
			if len(values) != 1 {
				return "", errProviderExecutionID
			}
			present = true
			count++
			id = values[0]
		}
	}
	if present {
		if count != 1 || len(id) != 32 {
			return "", errProviderExecutionID
		}
		for _, c := range id {
			if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
				return "", errProviderExecutionID
			}
		}
		return id, nil
	}
	// Legacy callers cannot independently predict this nonce. Strict consumers
	// supply their fresh client ID instead; no retained replay ledger is needed.
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(nonce[:]), nil
}

func (s *Server) providerReadError(w http.ResponseWriter, err error) {
	if errors.Is(err, errProviderExecutionID) {
		writeError(w, http.StatusBadRequest, "invalid_execution_id", errProviderExecutionID.Error())
		return
	}
	s.rightsError(w, err)
}

func (pc *providerCapture) finish(used []string) (*providerreads.Envelope, error) {
	if pc == nil {
		return nil, nil
	}
	pc.mu.Lock()
	defer pc.mu.Unlock()
	if pc.err != nil {
		return nil, pc.err
	}
	e, err := pc.collector.Finish(used)
	return &e, err
}
