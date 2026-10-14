package cli

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/tara-vision/taraengine/internal/config"
	"github.com/tara-vision/taraengine/internal/pull"
	"github.com/tara-vision/taraengine/internal/runtime"
)

// retrySchedule is how long serve --service waits after each failed runtime install: 10, 30 and 60 seconds, then
// five minutes after every further failure.
var retrySchedule = []time.Duration{10 * time.Second, 30 * time.Second, 60 * time.Second, 5 * time.Minute}

// serviceReleaseBase is where serve --service fetches the pinned build: the upstream releases when empty. It is a
// variable only so that a test can serve the build itself.
var serviceReleaseBase = ""

// sleepFor waits for d or until ctx ends, and returns ctx's error in that case. It is a variable only so that a test
// can run the retry schedule without waiting it out.
var sleepFor = func(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ensureRuntime installs the build this taraengine pins when it is missing or does not verify, and then removes
// every other build from the data directory. A failure does not end it: it says in the health detail why and when it
// tries again, waits on the retry schedule, and tries until it succeeds or ctx ends. The download is checked against
// the lock embedded in this binary before anything is unpacked. A configured runtime_dir is the operator's: nothing
// is fetched or removed for it. The caller holds the data directory's lock.
func ensureRuntime(ctx context.Context, cfg config.Config, steps *progress, logger *slog.Logger) error {
	if cfg.RuntimeDir != "" {
		return nil
	}
	lock, err := pinnedLock()
	if err != nil {
		return err
	}
	inst := &runtime.Installer{Dir: runtimeRoot(cfg.DataDir), BaseURL: serviceReleaseBase}
	for attempt := 0; !inst.Status(lock).Verified; attempt++ {
		err := installPinned(ctx, inst, lock, steps)
		if err == nil {
			break
		}
		wait := retrySchedule[min(attempt, len(retrySchedule)-1)]
		steps.set(fmt.Sprintf("runtime download failed: %s; retrying in %d s", oneLine(err.Error()), int(wait.Seconds())))
		logger.Warn("the runtime install failed; trying again", "build", lock.Build, "error", err, "retry_in", wait.String())
		if err := sleepFor(ctx, wait); err != nil {
			return err
		}
	}
	removed, err := runtime.RemoveOtherBuilds(inst.Dir, lock.Build)
	if err != nil {
		logger.Warn("could not remove the runtime builds this taraengine no longer pins", "error", err)
	}
	for _, name := range removed {
		logger.Info("removed a runtime build this taraengine no longer pins", "build", name)
	}
	return nil
}

// installPinned installs the pinned build for this machine and reports how far the download is in the health
// detail. The CUDA runtime bundle comes along when the machine lacks the CUDA runtime the build links against.
func installPinned(ctx context.Context, inst *runtime.Installer, lock runtime.Lock, steps *progress) error {
	platform, err := hostPlatform(ctx)
	if err != nil {
		return err
	}
	withCudart := cudartWanted(platform.CUDAMajor(), func(major int) bool {
		return runtime.HasCUDARuntime(ctx, runtime.ExecRunner, major)
	})
	steps.set(fmt.Sprintf("installing runtime %s (0%%)", lock.Build))
	inst.OnProgress = func(p pull.Progress) {
		if p.Total > 0 {
			steps.set(fmt.Sprintf("installing runtime %s (%d%%)", lock.Build, p.Bytes*100/p.Total))
		}
	}
	_, err = inst.Install(ctx, lock, platform, withCudart)
	return err
}

// oneLine is s with its line breaks turned into spaces, for a detail that a terminal shows on one line.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
