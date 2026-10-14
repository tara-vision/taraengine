package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	goruntime "runtime"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/tara-vision/taraengine/internal/selfupdate"
	"github.com/tara-vision/taraengine/internal/service"
	"github.com/tara-vision/taraengine/internal/version"
)

// caskName is the cask Homebrew installs taraengine from.
const caskName = "tara-vision/taracode/taraengine"

// Seams of the update command: the tests point them at a release of their own and at fakes.
var (
	hostArch       = goruntime.GOARCH
	newFinder      = selfupdate.DefaultFinder
	newDownloader  = selfupdate.DefaultDownloader
	updateRunner   = selfupdate.Runner(service.ExecRunner)
	lookupUserID   = user.LookupId
	runInteractive = func(ctx context.Context, name string, args ...string) error {
		cmd := exec.CommandContext(ctx, name, args...)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("update: %s: %w", name, err)
		}
		return nil
	}
)

func newUpdateCommand() *cobra.Command {
	var check bool
	cmd := &cobra.Command{
		Use:   "update",
		Short: "Update taraengine to the latest release, the way it was installed",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return update(cmd, check) },
	}
	cmd.Flags().BoolVar(&check, "check", false, "only say whether a newer release is out")
	return cmd
}

// update brings this taraengine to the latest stable release through the channel it came from. It looks the release
// up only when someone runs it: the engine checks for nothing in the background.
func update(cmd *cobra.Command, check bool) error {
	ctx := cmd.Context()
	p := &printer{w: cmd.OutOrStdout()}
	current, err := selfupdate.ParseVersion(version.Version)
	if err != nil {
		return errors.New("this taraengine is a development build; install a release with install.sh " +
			"(https://engine.tara.vision)")
	}
	latest, err := newFinder().Latest(ctx)
	if err != nil {
		return err
	}
	switch c := latest.Compare(current); {
	case c < 0:
		p.printf("taraengine %s is newer than the latest release, %s\n", current, latest)
		return p.err
	case c == 0:
		p.printf("taraengine %s is the latest release\n", current)
		return p.err
	case check:
		p.printf("taraengine %s is out (this is %s); `taraengine update` installs it\n", latest, current)
		return p.err
	}
	exe, err := executable()
	if err != nil {
		return err
	}
	inst := selfupdate.DetectInstall(ctx, updateRunner, hostOS, exe)
	switch inst.Channel {
	case selfupdate.ChannelHomebrew:
		return updateHomebrew(ctx, p, inst)
	case selfupdate.ChannelDeb, selfupdate.ChannelRPM:
		return updatePackage(ctx, p, inst, latest)
	}
	return updateFile(ctx, p, inst, current, latest)
}

// updateHomebrew hands the update to Homebrew, whose cask restarts the agent on the new binary.
func updateHomebrew(ctx context.Context, p *printer, inst selfupdate.Install) error {
	if processEUID() == 0 {
		return errors.New("a cask is updated as your user, as Homebrew asks: run `taraengine update` without sudo")
	}
	brew := filepath.Join(inst.Prefix, "bin", "brew")
	p.printf("running %s upgrade --cask %s\n", brew, caskName)
	if p.err != nil {
		return p.err
	}
	return runInteractive(ctx, brew, "upgrade", "--cask", caskName)
}

// updatePackage downloads the release's deb or rpm, checks it, and installs it with dpkg or rpm, whose script
// restarts the service.
func updatePackage(ctx context.Context, p *printer, inst selfupdate.Install, latest selfupdate.Version) error {
	if processEUID() != 0 {
		return errors.New("a package is updated as root: run `sudo taraengine update`")
	}
	ext, tool, flag := "deb", "dpkg", "-i"
	if inst.Channel == selfupdate.ChannelRPM {
		ext, tool, flag = "rpm", "rpm", "-U"
	}
	dir, err := os.MkdirTemp("", "taraengine-update-")
	if err != nil {
		return fmt.Errorf("update: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	name := selfupdate.PackageName(latest, hostArch, ext)
	pkg := filepath.Join(dir, name)
	p.printf("downloading %s\n", name)
	if err := newDownloader().Fetch(ctx, latest, name, pkg); err != nil {
		return err
	}
	p.printf("installing it with %s; the package restarts the service\n", tool)
	if p.err != nil {
		return p.err
	}
	return runInteractive(ctx, tool, flag, pkg)
}

// updateFile replaces the binary in place and restarts the service on it.
func updateFile(
	ctx context.Context, p *printer, inst selfupdate.Install, current, latest selfupdate.Version,
) error {
	asset := selfupdate.AssetName(hostOS, hostArch)
	p.printf("downloading %s %s\n", asset, latest)
	err := selfupdate.Replace(inst.Path, func(dest string) error {
		return newDownloader().Fetch(ctx, latest, asset, dest)
	})
	if errors.Is(err, os.ErrPermission) {
		return fmt.Errorf("%s is not writable by this user: run `sudo taraengine update`", filepath.Dir(inst.Path))
	}
	if err != nil {
		return err
	}
	p.printf("taraengine %s -> %s in %s\n", current, latest, inst.Path)
	return restartAfterUpdate(ctx, p)
}

// restartAfterUpdate restarts the service on the new binary when one is installed and was not stopped on purpose; the
// restarted service fetches the runtime the new binary pins. A system without a service has nothing to restart.
func restartAfterUpdate(ctx context.Context, p *printer) error {
	m, err := updateManager()
	if err != nil {
		return p.err
	}
	st, err := m.State(ctx)
	if err != nil || !st.Installed || !st.Enabled {
		return p.err
	}
	p.printf("restarting the taraengine service on the new binary; a generation in progress is cut off\n")
	err = m.Restart(ctx)
	switch {
	case errors.Is(err, service.ErrNeedsRoot):
		return errors.New("the binary is updated; restarting the system service takes root: " +
			"run `sudo taraengine service restart`")
	case err != nil:
		return err
	}
	p.printf("restarted the taraengine service\n")
	return p.err
}

// updateManager is the manager of the service an update restarts. Under sudo on macOS it is the agent of the user
// who ran sudo, since the agent belongs to that user and not to root.
func updateManager() (service.Manager, error) {
	uid := os.Getenv("SUDO_UID")
	if hostOS != "darwin" || processEUID() != 0 || uid == "" {
		return newServiceManager()
	}
	u, err := lookupUserID(uid)
	if err != nil {
		return nil, fmt.Errorf("update: %w", err)
	}
	n, err := strconv.Atoi(u.Uid)
	if err != nil {
		return nil, fmt.Errorf("update: user id %q: %w", u.Uid, err)
	}
	return &service.Launchd{Run: service.ExecRunner, UID: n, Home: u.HomeDir}, nil
}
