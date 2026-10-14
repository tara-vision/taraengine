package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	goruntime "runtime"

	"github.com/spf13/cobra"

	"github.com/tara-vision/taraengine/internal/pull"
	"github.com/tara-vision/taraengine/internal/runtime"
)

// pinnedLock is the lock this taraengine ships. It is a variable only so that tests can break it.
var pinnedLock = runtime.LoadLock

// hostPlatform is the build that suits this machine. It is a variable only so that tests can describe another one.
var hostPlatform = func(ctx context.Context) (runtime.Platform, error) {
	return runtime.Select(goruntime.GOOS, goruntime.GOARCH, runtime.DriverVersion(ctx, runtime.ExecRunner))
}

type runtimeFlags struct {
	dataDir     string
	releaseBase string
	lockPath    string
	cudart      bool
	noCudart    bool
	dirOnly     bool
}

func newRuntimeCommand(flags *globalFlags) *cobra.Command {
	rf := &runtimeFlags{}
	cmd := &cobra.Command{Use: "runtime", Short: "Install, update or inspect the pinned llama.cpp build"}
	cmd.PersistentFlags().StringVar(&rf.dataDir, "data-dir", "", "data directory (default: the config's)")
	cmd.PersistentFlags().StringVar(&rf.releaseBase, "release-base", "", "mirror of the upstream release assets")
	cmd.PersistentFlags().StringVar(&rf.lockPath, "lock", "", "a lock file other than the embedded one")
	_ = cmd.PersistentFlags().MarkHidden("release-base")
	_ = cmd.PersistentFlags().MarkHidden("lock")
	install := &cobra.Command{Use: "install", Short: "Download, verify and unpack the pinned build", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return rf.install(cmd, flags, false) }}
	update := &cobra.Command{Use: "update", Short: "Move to the build pinned by this taraengine", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return rf.install(cmd, flags, true) }}
	for _, c := range []*cobra.Command{install, update} {
		c.Flags().BoolVar(&rf.cudart, "cudart", false, "also install the CUDA runtime bundle")
		c.Flags().BoolVar(&rf.noCudart, "no-cudart", false, "never install the CUDA runtime bundle")
		c.MarkFlagsMutuallyExclusive("cudart", "no-cudart")
	}
	status := &cobra.Command{Use: "status", Short: "Show the installed build", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return rf.status(cmd, flags) }}
	status.Flags().BoolVar(&rf.dirOnly, "dir-only", false, "print only the runtime directory")
	cmd.AddCommand(install, update, status)
	return cmd
}

// runtimeSetup is what every runtime command needs: the pinned lock, an installer for the data directory, and the
// runtime_dir of the configuration, which replaces the installer's directory for a daemon that has one.
type runtimeSetup struct {
	lock       runtime.Lock
	inst       *runtime.Installer
	runtimeDir string
}

func (rf *runtimeFlags) setup(flags *globalFlags) (runtimeSetup, error) {
	cfg, err := loadConfig(flags)
	if err != nil {
		return runtimeSetup{}, err
	}
	lock, err := loadLock(rf.lockPath)
	if err != nil {
		return runtimeSetup{}, err
	}
	dataDir := cfg.DataDir
	if rf.dataDir != "" {
		dataDir = rf.dataDir
	}
	inst := &runtime.Installer{Dir: runtimeRoot(dataDir), BaseURL: rf.releaseBase}
	return runtimeSetup{lock: lock, inst: inst, runtimeDir: cfg.RuntimeDir}, nil
}

// loadLock is the lock file at path, or the embedded lock when path is empty.
func loadLock(path string) (runtime.Lock, error) {
	if path == "" {
		return pinnedLock()
	}
	data, err := os.ReadFile(path) //nolint:gosec // G304: the lock file is the operator's own --lock flag
	if err != nil {
		return runtime.Lock{}, fmt.Errorf("lock file: %w", err)
	}
	return runtime.ParseLock(data)
}

func (rf *runtimeFlags) install(cmd *cobra.Command, flags *globalFlags, onlyIfNeeded bool) error {
	setup, err := rf.setup(flags)
	if err != nil {
		return err
	}
	lock, inst := setup.lock, setup.inst
	p := &printer{w: cmd.OutOrStdout()}
	if onlyIfNeeded {
		if st := inst.Status(lock); st.Verified {
			p.printf("already at %s (%s)\n", lock.Build, st.Dir)
			return p.err
		}
	}
	ctx := cmd.Context()
	platform, err := hostPlatform(ctx)
	if err != nil {
		return err
	}
	withCudart := rf.wantsCudart(platform.CUDAMajor(), func(major int) bool {
		return runtime.HasCUDARuntime(ctx, runtime.ExecRunner, major)
	})
	inst.OnProgress = downloadLines(p)
	dir, err := inst.Install(ctx, lock, platform, withCudart)
	if err != nil {
		return err
	}
	p.printf("installed %s for %s in %s\n", lock.Build, platform.Key(), dir)
	return p.err
}

// wantsCudart decides whether the CUDA runtime bundle goes in with the server. The flags decide first, and without
// one cudartWanted does.
func (rf *runtimeFlags) wantsCudart(major int, installed func(major int) bool) bool {
	if rf.cudart {
		return true
	}
	return !rf.noCudart && cudartWanted(major, installed)
}

// cudartWanted is the rule without flags: a CUDA build gets the bundle when the machine has no CUDA runtime of the
// major version the build links against, and a build without CUDA never needs it.
func cudartWanted(major int, installed func(major int) bool) bool {
	return major > 0 && !installed(major)
}

// downloadLines prints one line for every tenth of a download. A download of unknown length shows none.
func downloadLines(p *printer) func(pull.Progress) {
	lastStep := int64(-1)
	return func(pr pull.Progress) {
		if pr.Total <= 0 {
			return
		}
		if step := pr.Bytes * 10 / pr.Total; step != lastStep {
			lastStep = step
			p.printf("downloading %s / %s\n", size(pr.Bytes), size(pr.Total))
		}
	}
}

// status reports the runtime the daemon would run on: the runtime_dir of the configuration when there is one, else
// the pinned build under the data directory, which is what the daemon and doctor look at too.
func (rf *runtimeFlags) status(cmd *cobra.Command, flags *globalFlags) error {
	setup, err := rf.setup(flags)
	if err != nil {
		return err
	}
	st := runtimeStatus(setup.runtimeDir, setup.inst, setup.lock)
	p := &printer{w: cmd.OutOrStdout()}
	switch {
	case rf.dirOnly:
		p.printf("%s\n", st.Dir)
	case st.Verified:
		p.printf("runtime %s verified in %s\n", st.Build, st.Dir)
	case st.Installed:
		p.printf("runtime %s installed in %s but NOT verified: %s\n", st.Build, st.Dir, st.Err)
	case st.Build == "":
		p.printf("%s\n", st.Err) // a configured directory with no build to name: the problem is the whole line
	default:
		p.printf("runtime %s %s\n", st.Build, st.Err)
	}
	return p.err
}

// runtimeRoot is where the builds live inside a data directory, one directory per build.
func runtimeRoot(dataDir string) string { return filepath.Join(dataDir, "runtime") }
