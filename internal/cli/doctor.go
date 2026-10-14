package cli

import (
	"context"
	"os"
	goruntime "runtime"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/tara-vision/taraengine/internal/backend"
	"github.com/tara-vision/taraengine/internal/machine"
	"github.com/tara-vision/taraengine/internal/registry"
	"github.com/tara-vision/taraengine/internal/runtime"
	apitypes "github.com/tara-vision/taraengine/pkg/api"
	"github.com/tara-vision/taraengine/pkg/client"
)

// detectMachine reads the host this process runs on. It is a variable only so that tests can describe another host.
var detectMachine = func(ctx context.Context) machine.Info {
	return machine.Detect(ctx, goruntime.GOOS, machine.ExecRunner, os.ReadFile)
}

func newDoctorCommand(flags *globalFlags) *cobra.Command {
	var dataDir string
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Show the machine, the runtime and the daemon",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			p := &printer{w: cmd.OutOrStdout()}
			info := detectMachine(ctx)
			reportMachine(p, info)
			cfg, err := loadConfig(flags)
			if err != nil {
				return err
			}
			dir := cfg.DataDir
			if dataDir != "" {
				dir = dataDir
			}
			lock, err := pinnedLock()
			if err != nil {
				return err
			}
			daemon := flags.withEnvironment().withConfig(cfg)
			if !reportDaemonRuntime(ctx, p, daemon) {
				reportRuntime(p, lock, cfg.RuntimeDir, dir)
			}
			reg := reportRegistry(p, lock, dir)
			reportDaemon(ctx, p, daemon, info.Tier, reg)
			if err := ctx.Err(); err != nil {
				return err // interrupted: what the calls that were cut off reported means nothing
			}
			return p.err
		},
	}
	cmd.Flags().StringVar(&dataDir, "data-dir", "", "data directory (default: the config's)")
	return cmd
}

func reportMachine(p *printer, info machine.Info) {
	gpu := info.GPU
	if gpu == "" {
		gpu = "no NVIDIA GPU found"
	}
	p.printf("Machine: %s/%s, %s", info.OS, info.Arch, gpu)
	if info.GPUMemoryBytes > 0 && !info.UnifiedMemory {
		p.printf(" (%s)", gib(info.GPUMemoryBytes))
	}
	p.printf(", %s RAM, tier %s\n", gib(info.RAMBytes), info.Tier)
}

// reportDaemonRuntime prints the runtime as the daemon at the host of f sees it, and reports whether a daemon told:
// the runtime a daemon runs is the one that counts, and under a system service it is not in this user's data
// directory. A daemon that is starting says what it is doing with its runtime.
func reportDaemonRuntime(ctx context.Context, p *printer, f *globalFlags) bool {
	c := f.client()
	if doc, err := c.Doctor(ctx); err == nil {
		rt := doc.Runtime
		if rt.Verified {
			p.printf("Runtime: %s verified in %s (as the engine at %s sees it)\n", rt.Build, rt.Dir, f.host)
		} else {
			p.printf("Runtime: %s (as the engine at %s sees it)\n", strings.TrimSpace(rt.Build+" "+rt.Error), f.host)
		}
		return true
	}
	if h, err := c.Health(ctx); err == nil && h.Status == apitypes.HealthStarting {
		p.printf("Runtime: %s (the engine at %s is starting)\n", h.Detail, f.host)
		return true
	}
	return false
}

// reportRuntime describes the runtime the daemon would run on: the configured runtime_dir, or the pinned build under
// the data directory. A configured directory that does not verify may have no build to name.
func reportRuntime(p *printer, lock runtime.Lock, runtimeDir, dataDir string) {
	st := runtimeStatus(runtimeDir, &runtime.Installer{Dir: runtimeRoot(dataDir)}, lock)
	if st.Verified {
		p.printf("Runtime: %s verified in %s\n", st.Build, st.Dir)
		return
	}
	p.printf("Runtime: %s\n", strings.TrimSpace(st.Build+" "+st.Err))
}

// reportRegistry names the models that need a newer runtime than the one this taraengine pins. It returns the
// catalog, or nil when the catalog cannot be read.
func reportRegistry(p *printer, lock runtime.Lock, dataDir string) *registry.Registry {
	reg, err := loadRegistry(dataDir)
	if err != nil {
		p.printf("Registry: %v\n", err)
		return nil
	}
	for _, e := range reg.Entries {
		if buildNumber(e.MinRuntime) > buildNumber(lock.Build) {
			p.printf("Model %s needs runtime %s or newer; this taraengine pins %s\n", e.Name, e.MinRuntime, lock.Build)
		}
	}
	return reg
}

// reportDaemon asks the daemon at the host of the flags about itself. Only a daemon that cannot be reached is "not
// running": one that answers with an error, such as a refused key, is running. The model it recommends is the one
// for the machine that the daemon reports, which is not this one when the host is another machine. Without a daemon
// the local catalog recommends for the tier of this machine, tier.
func reportDaemon(ctx context.Context, p *printer, flags *globalFlags, tier string, reg *registry.Registry) {
	c := flags.client()
	v, err := c.Version(ctx)
	switch {
	case ctx.Err() != nil:
		return
	case client.IsConnectionError(err):
		p.printf("Daemon: not running at %s (start it with `taraengine serve`)\n", flags.host)
		if e, ok := recommendedFor(reg, tier); ok {
			p.printf("Recommended for this tier: %s\n", e.Name)
		}
		return
	case err != nil:
		p.printf("Daemon: at %s, but it did not answer as expected: %v\n", flags.host, flags.explain(err))
		return
	}
	p.printf("Daemon: taraengine %s on %s, runtime %s, at %s", v.Version, v.Platform, v.Runtime.Build, flags.host)
	if h, err := c.Health(ctx); err == nil && h.Status == apitypes.HealthStarting {
		p.printf(": starting: %s\n", h.Detail)
		if e, ok := recommendedFor(reg, tier); ok {
			p.printf("Recommended for this tier: %s\n", e.Name)
		}
		return
	}
	p.printf("\n")
	if loaded, err := c.Loaded(ctx); err == nil {
		for _, m := range loaded {
			p.printf("Loaded: %s at %d tokens%s\n", m.Name, m.Context, sleepingNote(m.Status))
		}
	}
	reportFailures(ctx, p, c)
	if doc, err := c.Doctor(ctx); err == nil {
		if rec, ok := doc.Recommendation[doc.Machine.Tier]; ok {
			p.printf("Recommended for this tier: %s\n", rec)
		}
	}
}

// recommendedFor is the catalog's default model for a tier, when the catalog could be read and has one.
func recommendedFor(reg *registry.Registry, tier string) (registry.Entry, bool) {
	if reg == nil {
		return registry.Entry{}, false
	}
	return reg.Default(tier)
}

// reportFailures prints every model whose last load failed or that is marked faulty, with the end of the server's
// log, which the daemon keeps on the model: the place to look after a refused load.
func reportFailures(ctx context.Context, p *printer, c *client.Client) {
	models, err := c.Models(ctx)
	if err != nil {
		return
	}
	for _, m := range models {
		switch backend.Status(m.Status) {
		case backend.StatusFailed:
			p.printf("Model %s: failed%s\n", m.Name, exitCodeNote(m.ExitCode))
		case backend.StatusFaulty:
			p.printf("Model %s: faulty after two failed loads%s, retry with `taraengine load %s --force`\n",
				m.Name, exitCodeNote(m.ExitCode), m.Name)
		default:
			continue
		}
		if detail, err := c.Model(ctx, m.Name); err == nil {
			printLogTail(p, detail.LogTail)
		}
	}
}

// printLogTail prints the end of a log two spaces in. A blank line stays empty and no line ends in whitespace, and
// the blank lines at the end are left out.
func printLogTail(p *printer, tail string) {
	tail = strings.TrimRight(tail, " \t\r\n")
	if tail == "" {
		return
	}
	for _, line := range strings.Split(tail, "\n") {
		if line = strings.TrimRight(line, " \t\r"); line == "" {
			p.printf("\n")
			continue
		}
		p.printf("  %s\n", line)
	}
}

// exitCodeNote says how a child ended, when it did not end well.
func exitCodeNote(code int) string {
	if code == 0 {
		return ""
	}
	return ", exit code " + strconv.Itoa(code)
}

// sleepingNote marks a resident model that sleeps until its next request.
func sleepingNote(status string) string {
	if status == string(backend.StatusSleeping) {
		return ", sleeping"
	}
	return ""
}
