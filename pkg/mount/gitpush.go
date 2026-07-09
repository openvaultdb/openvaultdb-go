package mount

import (
	"context"
	"fmt"
	"log"
	"os/exec"
	"time"

	"github.com/openvaultdb/openvaultdb-go/pkg/manifest"
)

// newGitPushHook builds the after-write hook implementing the manifest's
// storage.ingitdb.push policy:
//
//	none  → nil hook (commit locally only);
//	sync  → push before the write request is acknowledged; a failed push
//	        fails the request (the batch is still committed locally);
//	async → signal a coalescing background pusher and return immediately;
//	        push failures are logged, not surfaced to the writer.
func newGitPushHook(dir string, o *manifest.InGitDBOptions) func(ctx context.Context) error {
	switch o.PushMode() {
	case "sync":
		return func(ctx context.Context) error {
			return gitPush(ctx, dir, o.PushRemote(), o.PushBranch())
		}
	case "async":
		signal := make(chan struct{}, 1)
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
			select {
			case signal <- struct{}{}: // pusher will run; bursts coalesce
			default: // a push is already pending — it will cover this write
			}
			return nil
		}
	default:
		return nil
	}
}

func gitPush(ctx context.Context, dir, remote, branch string) error {
	out, err := exec.CommandContext(ctx, "git", "-C", dir, "push", remote, branch).CombinedOutput()
	if err != nil {
		return fmt.Errorf("git push %s %s: %w: %s", remote, branch, err, out)
	}
	return nil
}
