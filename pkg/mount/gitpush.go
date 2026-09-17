package mount

import (
	"context"
	"fmt"
	"log"
	"os/exec"
	"sync"
	"time"

	"github.com/openvaultdb/openvaultdb-go/pkg/manifest"
)

// newGitPushHook builds the after-write hook implementing the manifest's
// storage.ingitdb.push policy, plus a stop func that releases what the hook
// holds (the async pusher goroutine); stop is nil when hook is nil:
//
//	none  → nil hook (commit locally only);
//	sync  → push before the write request is acknowledged; a failed push
//	        fails the request (the batch is still committed locally);
//	async → signal a coalescing background pusher and return immediately;
//	        push failures are logged, not surfaced to the writer.
func newGitPushHook(dir string, o *manifest.InGitDBOptions) (hook func(ctx context.Context) error, stop func() error) {
	switch o.PushMode() {
	case "sync":
		return func(ctx context.Context) error {
			return gitPush(ctx, dir, o.PushRemote(), o.PushBranch())
		}, func() error { return nil }
	case "async":
		signal := make(chan struct{}, 1)
		var mu sync.Mutex
		stopped := false
		go func() {
			for range signal {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
				if err := gitPush(ctx, dir, o.PushRemote(), o.PushBranch()); err != nil {
					log.Printf("ovdb: async git push failed for %s: %v", dir, err)
				}
				cancel()
			}
		}()
		return func(context.Context) error {
				mu.Lock()
				defer mu.Unlock()
				if stopped {
					return nil
				}
				select {
				case signal <- struct{}{}: // pusher will run; bursts coalesce
				default: // a push is already pending — it will cover this write
				}
				return nil
			}, func() error {
				mu.Lock()
				defer mu.Unlock()
				if !stopped {
					stopped = true
					close(signal) // a pending push still runs, then the pusher exits
				}
				return nil
			}
	default:
		return nil, nil
	}
}

func gitPush(ctx context.Context, dir, remote, branch string) error {
	out, err := exec.CommandContext(ctx, "git", "-C", dir, "push", remote, branch).CombinedOutput()
	if err != nil {
		return fmt.Errorf("git push %s %s: %w: %s", remote, branch, err, out)
	}
	return nil
}
