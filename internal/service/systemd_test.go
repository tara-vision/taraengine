package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
)

// fakeSystem is a Linux machine as getent, useradd, usermod, chown and systemctl show it: which users and groups
// exist, and the unit's state. A command listed in fail fails. It records every command.
type fakeSystem struct {
	users   map[string]bool
	groups  map[string]bool
	enabled string // what is-enabled prints: enabled, disabled, or "" for a unit systemd does not know
	active  bool
	exit    string // ExecMainStatus
	fail    map[string]bool
	calls   []string
}

func newFakeSystem() *fakeSystem {
	return &fakeSystem{users: map[string]bool{}, groups: map[string]bool{"video": true}, exit: "0"}
}

func (f *fakeSystem) run(_ context.Context, name string, args ...string) (string, error) {
	call := strings.Join(append([]string{name}, args...), " ")
	f.calls = append(f.calls, call)
	if f.fail[name] || (name == "systemctl" && len(args) > 0 && f.fail["systemctl "+args[0]]) {
		return name + " failed", errors.New("exit status 1")
	}
	handle, ok := map[string]func(args []string) (string, error){
		"getent": f.getent, "useradd": f.useradd, "usermod": f.ok, "chown": f.ok, "systemctl": f.systemctl,
	}[name]
	if !ok {
		return "", fmt.Errorf("unexpected command %q", call)
	}
	return handle(args)
}

func (f *fakeSystem) ok([]string) (string, error) { return "", nil }

func (f *fakeSystem) getent(args []string) (string, error) {
	known := (args[0] == "passwd" && f.users[args[1]]) || (args[0] == "group" && f.groups[args[1]])
	if !known {
		return "", errors.New("exit status 2")
	}
	return args[1] + ":x:999:999::/var/lib/taraengine:/usr/sbin/nologin\n", nil
}

func (f *fakeSystem) useradd(args []string) (string, error) {
	f.users[args[len(args)-1]] = true
	return "", nil
}

func (f *fakeSystem) systemctl(args []string) (string, error) {
	switch strings.Join(args, " ") {
	case "daemon-reload":
	case "is-enabled " + UnitName:
		if f.enabled != "enabled" {
			return f.enabled + "\n", errors.New("exit status 1") // systemctl answers 1 for anything but enabled
		}
		return "enabled\n", nil
	case "enable " + UnitName:
		f.enabled = "enabled"
	case "enable --now " + UnitName:
		f.enabled, f.active = "enabled", true
	case "disable --now " + UnitName:
		f.enabled, f.active = "disabled", false
	case "restart " + UnitName:
		f.active = true
	case "is-active " + UnitName:
		if !f.active {
			return "inactive\n", errors.New("exit status 3")
		}
		return "active\n", nil
	case "show " + UnitName + " -p LoadState,ActiveState,MainPID,ExecMainStatus":
		load, active, pid := "not-found", "inactive", "0"
		if f.enabled != "" {
			load = "loaded"
		}
		if f.active {
			active, pid = "active", "4242"
		}
		return "MainPID=" + pid + "\nExecMainStatus=" + f.exit + "\nLoadState=" + load + "\nActiveState=" + active + "\n", nil
	default:
		return "", fmt.Errorf("unexpected systemctl %q", strings.Join(args, " "))
	}
	return "", nil
}

// newSystemd is a manager over a root of its own, booted with systemd, run as root, with the fake behind it.
func newSystemd(t *testing.T, fake *fakeSystem) *Systemd {
	t.Helper()
	root := t.TempDir()
	for _, dir := range []string{"run/systemd/system", "usr/sbin"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "usr/sbin/nologin"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return &Systemd{Run: fake.run, Root: root, EUID: 0}
}

func TestSystemdInstallSetsItsModesWhateverTheUmask(t *testing.T) {
	previous := syscall.Umask(0o077) // a hardened shell's umask, which sudo keeps
	t.Cleanup(func() { syscall.Umask(previous) })
	s := newSystemd(t, newFakeSystem())
	if _, err := s.Install(context.Background(), "/usr/local/bin/taraengine"); err != nil {
		t.Fatal(err)
	}
	for rel, want := range map[string]os.FileMode{"etc/taraengine": 0o750, "var/lib/taraengine": 0o750, "etc/taraengine/config.yaml": 0o640} {
		info, err := os.Stat(s.path(rel))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != want {
			t.Fatalf("%s is %o, want %o: the service user has to read it", rel, info.Mode().Perm(), want)
		}
	}
}

func TestSystemdInstallLeavesTheModesOfDirectoriesThatAreThere(t *testing.T) {
	s := newSystemd(t, newFakeSystem())
	dir := s.path("etc/taraengine")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil { //nolint:gosec // G302: an admin's choice, which Install keeps
		t.Fatal(err)
	}
	if _, err := s.Install(context.Background(), "/usr/local/bin/taraengine"); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(dir); err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("an admin's directory keeps its mode: %v %v", info.Mode(), err)
	}
}

func TestSystemdRestartOfAStoppedServiceStartsItAtBootAgain(t *testing.T) {
	fake := newFakeSystem()
	s := newSystemd(t, fake)
	ctx := context.Background()
	if _, err := s.Install(ctx, "/usr/local/bin/taraengine"); err != nil {
		t.Fatal(err)
	}
	if err := s.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.Restart(ctx); err != nil || fake.enabled != "enabled" || !fake.active {
		t.Fatalf("a restart of a stopped service starts it and keeps it at boot, as on macOS: %v %v", err, fake.calls)
	}
	fake.calls = nil
	if err := s.Restart(ctx); err != nil || !slices.Contains(fake.calls, "systemctl restart "+UnitName) ||
		slices.Contains(fake.calls, "systemctl enable --now "+UnitName) {
		t.Fatalf("a restart of a running service restarts it: %v %v", err, fake.calls)
	}
}

func TestSystemdUpgradeMovesARunningButDisabledServiceToTheNewBinary(t *testing.T) {
	fake := newFakeSystem()
	s := newSystemd(t, fake)
	ctx := context.Background()
	if _, err := s.Install(ctx, "/usr/local/bin/taraengine"); err != nil {
		t.Fatal(err)
	}
	fake.enabled, fake.calls = "disabled", nil // an admin's systemctl disable, without --now
	note, err := s.Install(ctx, "/usr/bin/taraengine")
	if err != nil || note != noteRestarted || !slices.Contains(fake.calls, "systemctl restart "+UnitName) || fake.enabled != "disabled" {
		t.Fatalf("a disabled service that runs moves to the new binary and stays disabled: %q %v %v", note, err, fake.calls)
	}
}

func TestSystemdInstallSetsUpTheUserTheDirectoriesAndTheUnit(t *testing.T) {
	fake := newFakeSystem()
	s := newSystemd(t, fake)
	note, err := s.Install(context.Background(), "/usr/local/bin/taraengine")
	if err != nil || note != noteInstalled {
		t.Fatalf("%q %v", note, err)
	}
	for _, want := range []string{
		"useradd --system --user-group --home-dir /var/lib/taraengine --no-create-home --shell /usr/sbin/nologin taraengine",
		"usermod -a -G video taraengine",
		"chown taraengine:taraengine " + s.path("var/lib/taraengine"),
		"chown root:taraengine " + s.path("etc/taraengine"),
		"chown root:taraengine " + s.path("etc/taraengine/config.yaml"),
		"systemctl daemon-reload", "systemctl enable " + UnitName, "systemctl restart " + UnitName,
	} {
		if !slices.Contains(fake.calls, want) {
			t.Errorf("missing %q in %v", want, fake.calls)
		}
	}
	if slices.Contains(fake.calls, "usermod -a -G render taraengine") {
		t.Error("a group the system does not have is left out")
	}
	for path, mode := range map[string]os.FileMode{
		"var/lib/taraengine": 0o750, "etc/taraengine": 0o750, "etc/taraengine/config.yaml": 0o640,
		"etc/systemd/system/taraengine.service": 0o644,
	} {
		if info, err := os.Stat(s.path(path)); err != nil || info.Mode().Perm() != mode {
			t.Errorf("%s: %v %v, want %o", path, info, err, mode)
		}
	}
	unit, _ := os.ReadFile(s.path("etc/systemd/system/taraengine.service"))
	if !strings.Contains(string(unit), "ExecStart=/usr/local/bin/taraengine serve --service") {
		t.Fatalf("the unit runs the binary:\n%s", unit)
	}
	if config, _ := os.ReadFile(s.path("etc/taraengine/config.yaml")); string(config) != string(LinuxConfig()) {
		t.Fatalf("a first install writes the config: %q", config)
	}
}

func TestSystemdInstallNeverTouchesAConfigTheAdminEdited(t *testing.T) {
	fake := newFakeSystem()
	s := newSystemd(t, fake)
	if err := os.MkdirAll(s.path("etc/taraengine"), 0o750); err != nil {
		t.Fatal(err)
	}
	edited := "data_dir: /srv/taraengine\napi_key: abc\n"
	if err := os.WriteFile(s.path("etc/taraengine/config.yaml"), []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := s.Install(context.Background(), "/usr/bin/taraengine"); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := os.ReadFile(s.path("etc/taraengine/config.yaml"))
	info, _ := os.Stat(s.path("etc/taraengine/config.yaml"))
	if string(got) != edited || info.Mode().Perm() != 0o600 ||
		slices.Contains(fake.calls, "chown root:taraengine "+s.path("etc/taraengine/config.yaml")) {
		t.Fatalf("the admin's file, its mode and its owner stay: %q %v %v", got, info.Mode(), fake.calls)
	}
}

func TestSystemdInstallKeepsTheUserItFinds(t *testing.T) {
	fake := newFakeSystem()
	fake.users["taraengine"] = true
	s := newSystemd(t, fake)
	if _, err := s.Install(context.Background(), "/usr/bin/taraengine"); err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(fake.calls, func(c string) bool { return strings.HasPrefix(c, "useradd") }) {
		t.Fatalf("an existing user is kept: %v", fake.calls)
	}
}

func TestSystemdInstallRestartsAnEnabledServiceAndKeepsAStoppedOneStopped(t *testing.T) {
	fake := newFakeSystem()
	s := newSystemd(t, fake)
	ctx := context.Background()
	if _, err := s.Install(ctx, "/usr/bin/taraengine"); err != nil {
		t.Fatal(err)
	}
	note, err := s.Install(ctx, "/usr/bin/taraengine")
	if err != nil || note != noteRestarted {
		t.Fatalf("an upgrade restarts the service: %q %v", note, err)
	}
	if err := s.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	fake.calls = nil
	note, err = s.Install(ctx, "/usr/bin/taraengine")
	if err != nil || note != noteStopped || fake.active || fake.enabled != "disabled" {
		t.Fatalf("a stopped service stays stopped through an upgrade: %q %v %v", note, err, fake.calls)
	}
}

func TestSystemdInstallPicksTheNologinShellTheSystemHas(t *testing.T) {
	for _, c := range []struct{ has, want string }{
		{"sbin/nologin", "/sbin/nologin"}, {"", "/bin/false"},
	} {
		fake := newFakeSystem()
		s := newSystemd(t, fake)
		if err := os.Remove(s.path("usr/sbin/nologin")); err != nil {
			t.Fatal(err)
		}
		if c.has != "" {
			if err := os.MkdirAll(filepath.Dir(s.path(c.has)), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(s.path(c.has), []byte("x"), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := s.Install(context.Background(), "/usr/bin/taraengine"); err != nil {
			t.Fatal(err)
		}
		if !slices.ContainsFunc(fake.calls, func(call string) bool { return strings.Contains(call, "--shell "+c.want+" ") }) {
			t.Errorf("want the shell %s in %v", c.want, fake.calls)
		}
	}
}

func TestSystemdNeedsRootToChangeTheService(t *testing.T) {
	s := newSystemd(t, newFakeSystem())
	s.EUID = 1000
	ctx := context.Background()
	if _, err := s.Install(ctx, "/usr/bin/taraengine"); !errors.Is(err, ErrNeedsRoot) {
		t.Errorf("install: %v", err)
	}
	if _, err := s.Uninstall(ctx); !errors.Is(err, ErrNeedsRoot) {
		t.Errorf("uninstall: %v", err)
	}
	for name, op := range map[string]func(context.Context) error{"start": s.Start, "stop": s.Stop, "restart": s.Restart} {
		if err := op(ctx); !errors.Is(err, ErrNeedsRoot) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := s.State(ctx); err != nil {
		t.Errorf("anyone may read the state: %v", err)
	}
}

func TestSystemdWithoutSystemd(t *testing.T) {
	fake := newFakeSystem()
	s := newSystemd(t, fake)
	if err := os.RemoveAll(s.path("run/systemd")); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	note, err := s.Install(ctx, "/usr/bin/taraengine")
	if err != nil || note != noteNoSystemd || len(fake.calls) != 0 {
		t.Fatalf("a package installs in a container without systemd: %q %v %v", note, err, fake.calls)
	}
	if err := s.Start(ctx); !errors.Is(err, ErrNoSystemd) {
		t.Errorf("start: %v", err)
	}
	if _, err := s.State(ctx); !errors.Is(err, ErrNoSystemd) {
		t.Errorf("state: %v", err)
	}
}

func TestSystemdStartStopRestartUninstall(t *testing.T) {
	fake := newFakeSystem()
	s := newSystemd(t, fake)
	ctx := context.Background()
	if err := s.Start(ctx); !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("start before install: %v", err)
	}
	if _, err := s.Uninstall(ctx); !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("uninstall before install: %v", err)
	}
	if _, err := s.Install(ctx, "/usr/bin/taraengine"); err != nil {
		t.Fatal(err)
	}
	if err := s.Stop(ctx); err != nil || fake.active || fake.enabled != "disabled" {
		t.Fatalf("stop disables: %v %v", err, fake.calls)
	}
	if err := s.Start(ctx); err != nil || !fake.active || fake.enabled != "enabled" {
		t.Fatalf("start enables: %v %v", err, fake.calls)
	}
	fake.calls = nil
	if err := s.Restart(ctx); err != nil || !slices.Equal(fake.calls, []string{"systemctl is-active " + UnitName, "systemctl restart " + UnitName}) {
		t.Fatalf("restart: %v %v", err, fake.calls)
	}
	note, err := s.Uninstall(ctx)
	if err != nil || !strings.Contains(note, "sudo userdel taraengine") || !strings.Contains(note, "/var/lib/taraengine") {
		t.Fatalf("uninstall says what stays and how it goes: %q %v", note, err)
	}
	if _, err := os.Stat(s.path("etc/systemd/system/taraengine.service")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the unit is removed: %v", err)
	}
	if !slices.Contains(fake.calls, "systemctl disable --now "+UnitName) {
		t.Fatalf("and the service stopped: %v", fake.calls)
	}
}

func TestSystemdState(t *testing.T) {
	fake := newFakeSystem()
	s := newSystemd(t, fake)
	ctx := context.Background()
	st, err := s.State(ctx)
	if err != nil || st.Installed || st.Loaded || st.Running {
		t.Fatalf("nothing installed: %+v %v", st, err)
	}
	if _, err := s.Install(ctx, "/usr/bin/taraengine"); err != nil {
		t.Fatal(err)
	}
	st, err = s.State(ctx)
	want := State{Installed: true, Loaded: true, Enabled: true, Running: true, PID: 4242, LastExit: "0",
		Definition: s.path("etc/systemd/system/taraengine.service"),
		Logs:       []string{"journalctl -u taraengine", "/var/lib/taraengine/logs/taraengine.log"}}
	if err != nil || !stateEqual(st, want) {
		t.Fatalf("got %+v, want %+v (%v)", st, want, err)
	}
	fake.active, fake.exit = false, "1"
	if st, _ := s.State(ctx); st.Running || st.PID != 0 || st.LastExit != "1" {
		t.Fatalf("a service that exited with 1: %+v", st)
	}
}

func TestSystemdReportsWhatTheToolsRefuse(t *testing.T) {
	ctx := context.Background()
	for _, fail := range []string{"useradd", "usermod", "chown", "systemctl daemon-reload", "systemctl enable",
		"systemctl restart"} {
		t.Run(fail, func(t *testing.T) {
			fake := newFakeSystem()
			fake.fail = map[string]bool{fail: true}
			s := newSystemd(t, fake)
			if _, err := s.Install(ctx, "/usr/bin/taraengine"); err == nil || !strings.Contains(err.Error(), "failed") {
				t.Fatalf("got %v", err)
			}
		})
	}
	installed := func(t *testing.T, fail string) *Systemd {
		t.Helper()
		fake := newFakeSystem()
		s := newSystemd(t, fake)
		if _, err := s.Install(ctx, "/usr/bin/taraengine"); err != nil {
			t.Fatal(err)
		}
		fake.fail = map[string]bool{fail: true}
		return s
	}
	if _, err := installed(t, "systemctl disable").Uninstall(ctx); err == nil {
		t.Error("an uninstall that cannot stop the service fails")
	}
	if _, err := installed(t, "systemctl daemon-reload").Uninstall(ctx); err == nil {
		t.Error("an uninstall that cannot reload fails")
	}
	if _, err := installed(t, "systemctl show").State(ctx); err == nil {
		t.Error("a state that cannot be read fails")
	}
}

func TestSystemdReportsWhatItCannotWrite(t *testing.T) {
	ctx := context.Background()
	for _, blocker := range []string{"var/lib", "etc/taraengine", "etc/systemd/system/taraengine.service.tmp"} {
		t.Run(blocker, func(t *testing.T) {
			s := newSystemd(t, newFakeSystem())
			path := s.path(blocker)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			var err error
			if strings.HasSuffix(blocker, ".tmp") {
				err = os.MkdirAll(path, 0o755) // a directory where the temporary file goes
			} else {
				err = os.WriteFile(path, []byte("x"), 0o600) // a file where a directory goes
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.Install(ctx, "/usr/bin/taraengine"); err == nil || !strings.HasPrefix(err.Error(), "service: ") {
				t.Fatalf("got %v", err)
			}
		})
	}
	s := newSystemd(t, newFakeSystem())
	if _, err := s.Install(ctx, "/opt/my tools/taraengine"); err == nil || !strings.Contains(err.Error(), "binary path") {
		t.Fatalf("a binary path the unit would misread: %v", err)
	}
}

func TestNewSystemdIsTheMachine(t *testing.T) {
	s := NewSystemd(ExecRunner)
	if s.Root != "/" || s.EUID != os.Geteuid() || s.Run == nil {
		t.Fatalf("%+v", s)
	}
}

func TestSystemdReportsADirectoryItCannotWriteIn(t *testing.T) {
	ctx := context.Background()
	t.Run("the unit directory", func(t *testing.T) {
		s := newSystemd(t, newFakeSystem())
		if err := os.MkdirAll(s.path("etc"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(s.path("etc/systemd"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Install(ctx, "/usr/bin/taraengine"); err == nil || !strings.HasPrefix(err.Error(), "service: ") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("the configuration directory", func(t *testing.T) {
		s := newSystemd(t, newFakeSystem())
		if err := os.MkdirAll(s.path("etc/taraengine"), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(s.path("etc/taraengine"), 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(s.path("etc/taraengine"), 0o700) })
		if _, err := s.Install(ctx, "/usr/bin/taraengine"); err == nil || !strings.HasPrefix(err.Error(), "service: ") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("the unit on uninstall", func(t *testing.T) {
		s := newSystemd(t, newFakeSystem())
		if _, err := s.Install(ctx, "/usr/bin/taraengine"); err != nil {
			t.Fatal(err)
		}
		dir := filepath.Dir(s.unitPath())
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
		if _, err := s.Uninstall(ctx); err == nil || !strings.HasPrefix(err.Error(), "service: ") {
			t.Fatalf("got %v", err)
		}
	})
}

// The unit hides /home, /root and /run/user (ProtectHome) and /tmp and /var/tmp (PrivateTmp) from the engine, so a
// binary there, or a link to one, could never start: Install refuses it before it changes anything.
func TestSystemdInstallRefusesABinaryTheUnitHides(t *testing.T) {
	for _, binary := range []string{"/home/someone/.local/bin/taraengine", "/root/bin/taraengine", "/run/user/1000/taraengine",
		"/tmp/taraengine", "/var/tmp/bin/taraengine"} {
		fake := newFakeSystem()
		s := newSystemd(t, fake)
		_, err := s.Install(context.Background(), binary)
		if err == nil || !strings.Contains(err.Error(), "the service could not run "+binary+": its unit hides /home, /root, /run/user, /tmp and /var/tmp") {
			t.Fatalf("%s: %v", binary, err)
		}
		if len(fake.calls) != 0 || exists(s.unitPath()) {
			t.Fatalf("%s: nothing is changed, got %v", binary, fake.calls)
		}
	}
}

// Without systemd there is no unit to hide anything, and Install says so for any binary, as it does in a container.
func TestSystemdInstallWithoutSystemdTakesAnyBinary(t *testing.T) {
	s := newSystemd(t, newFakeSystem())
	if err := os.RemoveAll(s.path("run/systemd/system")); err != nil {
		t.Fatal(err)
	}
	if note, err := s.Install(context.Background(), "/root/go/bin/taraengine"); err != nil || note != noteNoSystemd {
		t.Fatalf("%q %v", note, err)
	}
}

func TestLinkTargetFollowsLinksToWhereTheyEnd(t *testing.T) {
	dir := t.TempDir()
	relative, absolute, loop := filepath.Join(dir, "relative"), filepath.Join(dir, "absolute"), filepath.Join(dir, "loop")
	for link, target := range map[string]string{relative: "absolute", absolute: "/home/someone/taraengine", loop: "loop"} {
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
	}
	for path, want := range map[string]string{
		relative: "/home/someone/taraengine", absolute: "/home/someone/taraengine", loop: loop,
		"/usr/local/bin/no-such-file": "/usr/local/bin/no-such-file",
	} {
		if got := linkTarget(path); got != want {
			t.Errorf("linkTarget(%s) = %s, want %s", path, got, want)
		}
	}
}

// A link outside the hidden directories that points into one is refused as well, and the refusal says where it points.
func TestSystemdInstallRefusesALinkIntoAHiddenDirectory(t *testing.T) {
	dir := t.TempDir()
	if hiddenFromTheUnit(dir) { // Linux keeps temporary directories under /tmp, which the unit hides by itself
		shm, err := os.MkdirTemp("/dev/shm", "taraengine-test-")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(shm) })
		dir = shm
	}
	link := filepath.Join(dir, "taraengine")
	if err := os.Symlink("/home/someone/.local/bin/taraengine", link); err != nil {
		t.Fatal(err)
	}
	if hiddenFromTheUnit(link) {
		t.Fatalf("the link itself must lie outside the hidden directories: %s", link)
	}
	fake := newFakeSystem()
	s := newSystemd(t, fake)
	_, err := s.Install(context.Background(), link)
	want := "the service could not run " + link + ", a link to /home/someone/.local/bin/taraengine: its unit hides"
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("got %v, want %q", err, want)
	}
	if len(fake.calls) != 0 || exists(s.unitPath()) {
		t.Fatalf("nothing is changed, got %v", fake.calls)
	}
}
