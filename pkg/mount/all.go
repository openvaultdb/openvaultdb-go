package mount

import (
	"fmt"
	"sync"
	"time"

	"github.com/openvaultdb/openvaultdb-go/pkg/core"
)

// Spec is one manifest All mounts.
type Spec struct {
	// Path is the manifest file.
	Path string
	// Options are the mount options of this manifest.
	Options Options
	// Optional lets the other mounts go on when this one fails, for example a
	// PostgreSQL database whose catalogue read fails or is slow while the SQLite
	// databases beside it can be served: the failure is reported in its Health and
	// All does not fail because of it. A required mount (the zero value) that
	// fails fails All.
	Optional bool
}

// Health is the outcome of mounting one Spec.
type Health struct {
	Path     string
	ID       string // the database ID, when mounted
	Mounted  bool
	Optional bool
	Err      error // why the mount failed; nil when mounted
	Elapsed  time.Duration
}

// All mounts every spec, concurrently, each with FileWithOptions, and reports the
// health of each, in the order of specs. A database ID mounted twice fails the
// later spec. When a required spec fails, every database mounted is closed and
// the error names the first such spec; the health of every spec is still
// returned. Otherwise the error is nil and the map holds the databases mounted,
// keyed by ID.
func All(specs []Spec) (map[string]*core.Database, []Health, error) {
	return all(specs, FileWithOptions)
}

func all(specs []Spec, mountFile func(string, Options) (*core.Database, error)) (map[string]*core.Database, []Health, error) {
	opened := make([]*core.Database, len(specs))
	health := make([]Health, len(specs))
	var wg sync.WaitGroup
	for i, spec := range specs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			started := time.Now()
			db, err := mountFile(spec.Path, spec.Options)
			opened[i] = db
			health[i] = Health{Path: spec.Path, Optional: spec.Optional, Err: err, Elapsed: time.Since(started)}
		}()
	}
	wg.Wait()
	dbs := map[string]*core.Database{}
	var failure error
	for i, db := range opened {
		if db != nil {
			if _, duplicate := dbs[db.ID()]; duplicate {
				_ = db.Close()
				health[i].Err = fmt.Errorf("%s: duplicate database id %q", specs[i].Path, db.ID())
			} else {
				dbs[db.ID()] = db
				health[i].ID, health[i].Mounted = db.ID(), true
			}
		}
		if health[i].Err != nil && !specs[i].Optional && failure == nil {
			failure = health[i].Err
		}
	}
	if failure != nil {
		closeAll(dbs)
		for i := range health {
			health[i].Mounted = false
		}
		return nil, health, failure
	}
	return dbs, health, nil
}
