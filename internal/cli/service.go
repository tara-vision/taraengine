package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"

	"github.com/spf13/cobra"

	"github.com/tara-vision/taraengine/internal/service"
	"github.com/tara-vision/taraengine/pkg/api"
	"github.com/tara-vision/taraengine/pkg/client"
)

// statusNotRunning is the exit status of `service status` for a service that does not run, as the LSB asks.
const statusNotRunning = 3

// Seams of the service command: the tests set the system, the user and the manager.
var (
	hostOS            = goruntime.GOOS
	processEUID       = os.Geteuid
	executable        = os.Executable
	lookPath          = exec.LookPath
	newServiceManager = hostServiceManager
)

// hostServiceManager is the service manager of the system this binary runs on.
func hostServiceManager() (service.Manager, error) { return systemManager(hostOS) }

// systemManager is the service manager of a system: the user's LaunchAgent on macOS, a system service on Linux.
func systemManager(goos string) (service.Manager, error) {
	switch goos {
	case "darwin":
		return service.NewLaunchd(service.ExecRunner)
	case "linux":
		return service.NewSystemd(service.ExecRunner), nil
	}
	return nil, fmt.Errorf("service: taraengine has no service manager for %s", goos)
}

func newServiceCommand(flags *globalFlags) *cobra.Command {
	cmd := &cobra.Command{Use: "service", Short: "Run taraengine as a service: at login on macOS, at boot on Linux"}
	var binary, dataDir string
	install := &cobra.Command{Use: "install", Short: "Install the service for this binary and start it",
		Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
			path, err := installedBinary(binary)
			if err != nil {
				return err
			}
			return withService(cmd, true, func(m service.Manager) (string, error) { return m.Install(cmd.Context(), path) })
		}}
	install.Flags().StringVar(&binary, "binary", "", "the binary the service runs (default: this one, as found on PATH)")
	uninstall := &cobra.Command{Use: "uninstall", Short: "Stop the service and remove it; models and config stay",
		Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
			return withService(cmd, true, func(m service.Manager) (string, error) { return m.Uninstall(cmd.Context()) })
		}}
	status := &cobra.Command{Use: "status", Short: "Show the service and the engine it runs", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return serviceStatus(cmd, flags, dataDir) }}
	status.Flags().StringVar(&dataDir, "data-dir", "", "data directory (default: the config's)")
	cmd.AddCommand(install, uninstall, status,
		simpleServiceCommand("start", "Enable the service and start it", "started the taraengine service",
			service.Manager.Start),
		simpleServiceCommand("stop", "Stop the service and keep it stopped until a start",
			"stopped the taraengine service; it stays stopped until `taraengine service start`", service.Manager.Stop),
		simpleServiceCommand("restart", "Restart the service, or start it", "restarted the taraengine service",
			service.Manager.Restart))
	return cmd
}

// simpleServiceCommand is a service command that runs one manager method and says done when it succeeds.
func simpleServiceCommand(
	use, short, done string, op func(service.Manager, context.Context) error,
) *cobra.Command {
	return &cobra.Command{Use: use, Short: short, Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		return withService(cmd, true, func(m service.Manager) (string, error) { return done, op(m, cmd.Context()) })
	}}
}

// withService builds the manager, refuses root on macOS for a command that changes the service, runs op and prints
// what it returned.
func withService(cmd *cobra.Command, changes bool, op func(service.Manager) (string, error)) error {
	if changes && hostOS == "darwin" && processEUID() == 0 {
		return errors.New("the taraengine service belongs to your user on macOS: run the command without sudo")
	}
	m, err := newServiceManager()
	if err != nil {
		return err
	}
	note, err := op(m)
	if err != nil {
		return err
	}
	p := &printer{w: cmd.OutOrStdout()}
	p.printf("%s\n", note)
	return p.err
}

// installedBinary is the path the service definition names: --binary when given, else the first taraengine on PATH
// when it is this executable, else this executable. A path on PATH survives an upgrade that replaces what it points
// at; the Caskroom path a symlink resolves to would not.
func installedBinary(flag string) (string, error) {
	if flag != "" {
		return filepath.Abs(flag)
	}
	exe, err := executable()
	if err != nil {
		return "", fmt.Errorf("service: %w", err)
	}
	if onPath, err := lookPath("taraengine"); err == nil && sameFile(onPath, exe) {
		return filepath.Abs(onPath)
	}
	return exe, nil
}

func sameFile(a, b string) bool {
	ia, errA := os.Stat(a)
	ib, errB := os.Stat(b)
	return errA == nil && errB == nil && os.SameFile(ia, ib)
}

// serviceStatus prints the service as its manager sees it, the engine as its API answers, and where the logs are.
// It ends with status 3 when the service does not run or the engine does not answer.
func serviceStatus(cmd *cobra.Command, flags *globalFlags, dataDir string) error {
	m, err := newServiceManager()
	if err != nil {
		return err
	}
	st, err := m.State(cmd.Context())
	if err != nil {
		return err
	}
	cfg, err := loadConfig(flags)
	if err != nil {
		return err
	}
	if dataDir == "" {
		dataDir = cfg.DataDir
	}
	f := flags.withEnvironment().withConfig(cfg)
	p := &printer{w: cmd.OutOrStdout()}
	p.printf("Service: %s\n", serviceLine(st))
	answers := engineLine(cmd.Context(), p, f)
	logs := st.Logs
	if hostOS == "darwin" {
		logs = append(logs, filepath.Join(dataDir, "logs", "taraengine.log"))
	}
	if len(logs) > 0 {
		p.printf("Logs: %s\n", strings.Join(logs, ", "))
	}
	if p.err == nil && (!st.Running || !answers) {
		return &ExitError{Code: statusNotRunning}
	}
	return p.err
}

// serviceLine is what the manager says about the service, in one line.
func serviceLine(st service.State) string {
	if !st.Installed {
		return "not installed; `taraengine service install` installs it"
	}
	parts := []string{"installed (" + st.Definition + ")"}
	if st.Enabled {
		parts = append(parts, "enabled")
	} else {
		parts = append(parts, "stopped")
	}
	switch {
	case st.Running:
		parts = append(parts, fmt.Sprintf("running (pid %d)", st.PID))
	case st.LastExit != "":
		parts = append(parts, "not running (last exit "+st.LastExit+")")
	default:
		parts = append(parts, "not running")
	}
	return strings.Join(parts, ", ")
}

// engineLine prints what the engine at the host of f answers, and reports whether it answered.
func engineLine(ctx context.Context, p *printer, f *globalFlags) bool {
	c := f.client()
	h, err := c.Health(ctx)
	if err != nil {
		if client.IsConnectionError(err) {
			p.printf("Engine: not answering at %s\n", f.host)
		} else {
			p.printf("Engine: at %s, but it did not answer as expected: %v\n", f.host, f.explain(err))
		}
		return false
	}
	if h.Status == api.HealthStarting {
		p.printf("Engine: starting: %s, at %s\n", h.Detail, f.host)
		return true
	}
	line := h.Status
	if h.Detail != "" {
		line += " (" + h.Detail + ")"
	}
	if v, err := c.Version(ctx); err == nil {
		line += fmt.Sprintf(", taraengine %s on %s, runtime %s", v.Version, v.Platform, v.Runtime.Build)
	}
	p.printf("Engine: %s, at %s\n", line, f.host)
	return true
}
