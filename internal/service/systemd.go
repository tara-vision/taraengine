package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// serviceUser is the system user and group the Linux service runs as.
const serviceUser = "taraengine"

// noteNoSystemd is what Install says on a system that systemd does not run, such as a container.
const noteNoSystemd = "this system does not run systemd; `taraengine serve` runs the engine under any supervisor"

// The refusals of the Linux service.
var (
	ErrNeedsRoot = errors.New("the taraengine system service is changed as root: run the command with sudo")
	ErrNoSystemd = errors.New("this system does not run systemd, so there is no taraengine service to manage")
)

// Systemd is the system service of a Linux machine. Root is the root of the file system, "/" for the machine; a
// test points it at a directory of its own. EUID is the effective user id: changing the service takes root, reading
// its state does not.
type Systemd struct {
	Run  Runner
	Root string
	EUID int
}

var _ Manager = (*Systemd)(nil)

// NewSystemd is the service of this machine.
func NewSystemd(run Runner) *Systemd { return &Systemd{Run: run, Root: "/", EUID: os.Geteuid()} }

func (s *Systemd) path(rel string) string { return filepath.Join(s.Root, rel) }

func (s *Systemd) unitPath() string { return s.path("etc/systemd/system/" + UnitName) }

// booted reports whether systemd runs the system, the way sd_booted tells.
func (s *Systemd) booted() bool { return exists(s.path("run/systemd/system")) }

// ready refuses a change without root, or on a system that systemd does not run.
func (s *Systemd) ready() error {
	if s.EUID != 0 {
		return ErrNeedsRoot
	}
	if !s.booted() {
		return ErrNoSystemd
	}
	return nil
}

func (s *Systemd) run(ctx context.Context, name string, args ...string) error {
	out, err := s.Run(ctx, name, args...)
	if err != nil {
		return fmt.Errorf("service: %s %s: %s (%w)", name, args[0], strings.TrimSpace(out), err)
	}
	return nil
}

func (s *Systemd) systemctl(ctx context.Context, args ...string) error {
	return s.run(ctx, "systemctl", args...)
}

// hiddenFromTheUnit reports whether path lies in a directory that the unit's ProtectHome hides from the engine.
func hiddenFromTheUnit(path string) bool {
	clean := filepath.Clean(path)
	for _, dir := range []string{"/home", "/root", "/run/user"} {
		if clean == dir || strings.HasPrefix(clean, dir+"/") {
			return true
		}
	}
	return false
}

// Install sets up the taraengine user, its directories and, when there is none, its configuration, writes the unit
// for binary and runs it: a first install, and an upgrade of a service that is enabled, enable and restart it. A
// service an admin disabled stays disabled, and moves to the new binary when it still runs. On a system without
// systemd it says so and succeeds, so that a package installs in a container.
func (s *Systemd) Install(ctx context.Context, binary string) (string, error) {
	if s.EUID != 0 {
		return "", ErrNeedsRoot
	}
	if hiddenFromTheUnit(binary) {
		return "", fmt.Errorf("the service could not run %s: its unit hides /home, /root and /run/user; "+
			"install the binary elsewhere, such as /usr/local/bin", binary)
	}
	if !s.booted() {
		return noteNoSystemd, nil
	}
	unit, err := RenderUnit(binary)
	if err != nil {
		return "", err
	}
	if err := s.ensureUser(ctx); err != nil {
		return "", err
	}
	if err := s.ensureDirs(ctx); err != nil {
		return "", err
	}
	existed := exists(s.unitPath())
	if err := writeAtomic(s.unitPath(), unit, 0o644); err != nil {
		return "", err
	}
	if err := s.systemctl(ctx, "daemon-reload"); err != nil {
		return "", err
	}
	if existed && !s.enabled(ctx) {
		if !s.active(ctx) {
			return noteStopped, nil
		}
		return noteRestarted, s.systemctl(ctx, "restart", UnitName)
	}
	if err := s.systemctl(ctx, "enable", UnitName); err != nil {
		return "", err
	}
	if err := s.systemctl(ctx, "restart", UnitName); err != nil {
		return "", err
	}
	if existed {
		return noteRestarted, nil
	}
	return noteInstalled, nil
}

// ensureUser creates the system user when it is missing and puts it in the GPU groups the system has.
func (s *Systemd) ensureUser(ctx context.Context) error {
	if _, err := s.Run(ctx, "getent", "passwd", serviceUser); err != nil {
		if err := s.run(ctx, "useradd", "--system", "--user-group", "--home-dir", "/var/lib/taraengine",
			"--no-create-home", "--shell", s.nologin(), serviceUser); err != nil {
			return err
		}
	}
	for _, group := range []string{"video", "render"} {
		if _, err := s.Run(ctx, "getent", "group", group); err != nil {
			continue
		}
		if err := s.run(ctx, "usermod", "-a", "-G", group, serviceUser); err != nil {
			return err
		}
	}
	return nil
}

// nologin is the shell that refuses a login on this system.
func (s *Systemd) nologin() string {
	for _, shell := range []string{"/usr/sbin/nologin", "/sbin/nologin"} {
		if exists(s.path(shell)) {
			return shell
		}
	}
	return "/bin/false"
}

// ensureDirs creates the data and configuration directories with their owners, and the configuration when there is
// none. A configuration that is there is the admin's and is never touched.
func (s *Systemd) ensureDirs(ctx context.Context) error {
	if err := s.mkdirOwned(ctx, "var/lib/taraengine", serviceUser+":"+serviceUser); err != nil {
		return err
	}
	if err := s.mkdirOwned(ctx, "etc/taraengine", "root:"+serviceUser); err != nil {
		return err
	}
	config := s.path("etc/taraengine/config.yaml")
	if exists(config) {
		return nil
	}
	err := os.WriteFile(config, LinuxConfig(), 0o640) //nolint:gosec // G306: the service user reads it
	if err == nil {
		err = os.Chmod(config, 0o640) //nolint:gosec // G302: the service user reads it, whatever the umask
	}
	if err != nil {
		return fmt.Errorf("service: %w", err)
	}
	return s.run(ctx, "chown", "root:"+serviceUser, config)
}

// mkdirOwned creates a directory with its owner. One it creates gets 0750 whatever the umask, so that the service user
// can read it; one that is there keeps the mode an admin gave it.
func (s *Systemd) mkdirOwned(ctx context.Context, rel, owner string) error {
	dir := s.path(rel)
	created := !exists(dir)
	err := os.MkdirAll(dir, 0o750)
	if err == nil && created {
		err = os.Chmod(dir, 0o750) //nolint:gosec // G302: the service user reads it, whatever the umask
	}
	if err != nil {
		return fmt.Errorf("service: %w", err)
	}
	return s.run(ctx, "chown", owner, dir)
}

// enabled reports whether systemd starts the service at boot. is-enabled exits with 1 for anything but enabled.
func (s *Systemd) enabled(ctx context.Context) bool {
	out, _ := s.Run(ctx, "systemctl", "is-enabled", UnitName)
	return strings.TrimSpace(out) == "enabled"
}

// active reports whether the service runs. is-active exits with 3 for anything but active.
func (s *Systemd) active(ctx context.Context) bool {
	_, err := s.Run(ctx, "systemctl", "is-active", UnitName)
	return err == nil
}

// Start enables the service and starts it.
func (s *Systemd) Start(ctx context.Context) error {
	if err := s.installed(); err != nil {
		return err
	}
	return s.systemctl(ctx, "enable", "--now", UnitName)
}

// Stop stops the service and disables it, so that it stays stopped across reboots until a start.
func (s *Systemd) Stop(ctx context.Context) error {
	if err := s.installed(); err != nil {
		return err
	}
	return s.systemctl(ctx, "disable", "--now", UnitName)
}

// Restart restarts the service when it runs, and otherwise starts it as a start does, so that it runs at boot again,
// as a restart of a stopped agent does on macOS.
func (s *Systemd) Restart(ctx context.Context) error {
	if err := s.installed(); err != nil {
		return err
	}
	if !s.active(ctx) {
		return s.systemctl(ctx, "enable", "--now", UnitName)
	}
	return s.systemctl(ctx, "restart", UnitName)
}

// installed refuses what ready refuses, and a service that is not installed.
func (s *Systemd) installed() error {
	if err := s.ready(); err != nil {
		return err
	}
	if !exists(s.unitPath()) {
		return ErrNotInstalled
	}
	return nil
}

// Uninstall stops and disables the service and removes the unit. The user, the configuration and the data stay.
func (s *Systemd) Uninstall(ctx context.Context) (string, error) {
	if err := s.installed(); err != nil {
		return "", err
	}
	if err := s.systemctl(ctx, "disable", "--now", UnitName); err != nil {
		return "", err
	}
	if err := os.Remove(s.unitPath()); err != nil {
		return "", fmt.Errorf("service: %w", err)
	}
	if err := s.systemctl(ctx, "daemon-reload"); err != nil {
		return "", err
	}
	return "removed the taraengine service; the user taraengine, /etc/taraengine and /var/lib/taraengine stay, and " +
		"`sudo userdel taraengine && sudo rm -rf /etc/taraengine /var/lib/taraengine` removes them", nil
}

// State reads the service's state from the unit file and systemctl. Anyone may read it.
func (s *Systemd) State(ctx context.Context) (State, error) {
	if !s.booted() {
		return State{}, ErrNoSystemd
	}
	st := State{Installed: exists(s.unitPath()), Definition: s.unitPath(),
		Logs: []string{"journalctl -u taraengine", "/var/lib/taraengine/logs/taraengine.log"}}
	st.Enabled = st.Installed && s.enabled(ctx)
	out, err := s.Run(ctx, "systemctl", "show", UnitName, "-p", "LoadState,ActiveState,MainPID,ExecMainStatus")
	if err != nil {
		return st, fmt.Errorf("service: systemctl show: %s (%w)", strings.TrimSpace(out), err)
	}
	props := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if key, value, ok := strings.Cut(line, "="); ok {
			props[key] = value
		}
	}
	st.Loaded = props["LoadState"] == "loaded"
	st.Running = props["ActiveState"] == "active"
	if st.Running {
		st.PID, _ = strconv.Atoi(props["MainPID"])
	}
	st.LastExit = props["ExecMainStatus"]
	return st, nil
}
