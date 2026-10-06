package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"maps"
	"sync"
	"time"

	"github.com/dal-go/dalgo2http"
	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/manifest"
	"github.com/openvaultdb/openvaultdb-go/pkg/providerreads"
)

// ProviderReadProfile is trusted, externally verified admission configuration.
// It does not verify artifact existence or authorize public activation. Definition
// digest must bind the exact decoder, model/meaning and local recordset mapping.
// Consumers must advertise Format before this opt-in producer is configured.
type ProviderReadProfile struct {
	Collection string
	Binding    providerreads.Binding
}

// WithProviderReadProfiles opts selected HTTP instances into providerReads v1.
// Neither query input nor request Host can choose a binding or upstream URL.
func WithProviderReadProfiles(profiles map[string]ProviderReadProfile) Option {
	frozen := maps.Clone(profiles)
	return func(s *Server) { s.providerProfiles = maps.Clone(frozen) }
}
func (s *Server) validateProviderProfiles() error {
	s.providerProfilesByDB = map[*core.Database]ProviderReadProfile{}
	for id, p := range s.providerProfiles {
		db := s.dbs[id]
		if db == nil || !db.ReadOnlyHTTP() || !db.NoRetention() || s.rightsServerID == "" {
			return fmt.Errorf("provider profile requires a no-retention HTTP instance and stable rights server")
		}
		canonical, ok := db.CanonicalCollection(p.Collection)
		if !ok || canonical != p.Collection {
			return fmt.Errorf("provider profile collection is undeclared")
		}
		if prior, ok := s.providerProfilesByDB[db]; ok && prior != p {
			return fmt.Errorf("conflicting provider profiles across mount aliases")
		}
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
		s.providerProfilesByDB[db] = p
	}
	return nil
}
func (s *Server) providerPlan(p ProviderReadProfile, capture rightsCapture, id string) providerreads.Plan {
	return providerreads.Plan{Execution: providerreads.Execution{ID: id, Mode: "proxy", ExecutorID: s.rightsServerID}, Bindings: []providerreads.Binding{p.Binding}, Requests: []providerreads.Request{{ResourceID: p.Binding.ResourceID, Method: "GET", UpstreamURL: manifest.ECBDailyURL, Params: map[string]any{}}}, SourceRights: capture.rights, MaxReads: new(1), MaxMetadataBytes: providerreads.MaxMetadataBytes}
}

type providerCapture struct {
	collector *providerreads.Collector
	mu        sync.Mutex
	err       error
}

func (s *Server) beginProviderRead(ctx context.Context, db *core.Database, collection string, capture rightsCapture) (context.Context, *providerCapture, error) {
	p, configured := s.providerProfilesByDB[db]
	if !configured {
		return ctx, nil, nil
	}
	if p.Collection != collection {
		return ctx, nil, fmt.Errorf("unplanned provider recordset")
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return ctx, nil, err
	}
	plan := s.providerPlan(p, capture, hex.EncodeToString(nonce[:]))
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
		if o.Source != dalgo2http.SourceLive || o.Collection != collection || o.Decoder != dalgo2http.DecoderECBEuroFXRef || o.BaseCurrency != "EUR" || o.ReferenceDate == "" || o.FetchedAt.IsZero() || o.Bytes > 2<<20 {
			pc.err = fmt.Errorf("provider transport observation mismatch")
			return
		}
		pc.err = collector.Add(plan.Requests[0], providerreads.Observation{ResourceID: p.Binding.ResourceID, FetchedAt: o.FetchedAt.UTC().Truncate(time.Millisecond).Format("2006-01-02T15:04:05.000Z"), UpstreamURL: o.UpstreamURL, Status: o.StatusCode, ContentType: o.ContentType, SHA256: o.SHA256, Bytes: int64(o.Bytes), ReferenceDate: o.ReferenceDate, LastModified: o.LastModified, ETag: o.ETag})
	})
	return observed, pc, nil
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
