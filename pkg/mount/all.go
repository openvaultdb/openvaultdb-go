package mount

import (
	"errors"
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
	Err      error // why the mount failed or was not attempted; nil when mounted
	Elapsed  time.Duration
}

// DefaultMountConcurrency is how many manifests All mounts at once.
const DefaultMountConcurrency = 4

// AllOptions tunes AllWithOptions. The zero value is the behaviour of All.
type AllOptions struct {
	// Concurrency is how many manifests are mounted at once; zero or less is
	// DefaultMountConcurrency.
	Concurrency int
}

// ErrMountSkipped is the Health error of a spec that was not attempted because a
// required spec had already failed.
var ErrMountSkipped = errors.New("not mounted: a required mount failed first")

// All mounts every spec with FileWithOptions, DefaultMountConcurrency at a time,
// and reports the health of each, in the order of specs. A database ID mounted
// twice fails the later spec, and a mount that panics fails its spec. When a
// required spec fails, the specs not yet started are skipped (ErrMountSkipped),
// every database mounted is closed, and the error names the first required
// failure; the health of every spec is still returned. Otherwise the error is
// nil and the map holds the databases mounted, keyed by ID.
func All(specs []Spec) (map[string]*core.Database, []Health, error) {
	return AllWithOptions(specs, AllOptions{})
}

// AllWithOptions is All with options.
func AllWithOptions(specs []Spec, opts AllOptions) (map[string]*core.Database, []Health, error) {
	return all(specs, opts, FileWithOptions)
}

func all(specs []Spec, opts AllOptions, mountFile func(string, Options) (*core.Database, error)) (map[string]*core.Database, []Health, error) {
	concurrency := opts.Concurrency
	if concurrency <= 0 {
		concurrency = DefaultMountConcurrency
	}
	opened := make([]*core.Database, len(specs))
	health := make([]Health, len(specs))
	jobs := make(chan int, len(specs))
	for i := range specs {
		jobs <- i
	}
	close(jobs)
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		required bool // a required spec has failed
	)
	for range min(concurrency, len(specs)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				spec := specs[i]
				health[i] = Health{Path: spec.Path, Optional: spec.Optional}
				mu.Lock()
				skip := required
				mu.Unlock()
				if skip {
					health[i].Err = ErrMountSkipped
					continue
				}
				started := time.Now()
				db, err := mountOne(mountFile, spec)
				opened[i], health[i].Err, health[i].Elapsed = db, err, time.Since(started)
				if err != nil && !spec.Optional {
					mu.Lock()
					required = true
					mu.Unlock()
				}
			}
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
		if health[i].Err != nil && !errors.Is(health[i].Err, ErrMountSkipped) && !specs[i].Optional && failure == nil {
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

// mountOne mounts one spec, and turns a panic of the mount into its error.
func mountOne(mountFile func(string, Options) (*core.Database, error), spec Spec) (db *core.Database, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			db, err = nil, fmt.Errorf("%s: mounting panicked: %v", spec.Path, recovered)
		}
	}()
	return mountFile(spec.Path, spec.Options)
}
