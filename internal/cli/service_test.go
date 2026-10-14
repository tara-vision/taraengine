package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/tara-vision/taraengine/internal/service"
)

// fakeManager is a service manager that records what it is asked and answers with what the test set.
type fakeManager struct {
	state    service.State
	stateErr error
	note     string
	err      error
	calls    []string
}

func (f *fakeManager) Install(_ context.Context, binary string) (string, error) {
	f.calls = append(f.calls, "install "+binary)
	return f.note, f.err
}

func (f *fakeManager) Uninstall(context.Context) (string, error) {
	f.calls = append(f.calls, "uninstall")
	return f.note, f.err
}

func (f *fakeManager) Start(context.Context) error { f.calls = append(f.calls, "start"); return f.err }
func (f *fakeManager) Stop(context.Context) error  { f.calls = append(f.calls, "stop"); return f.err }
func (f *fakeManager) Restart(context.Context) error {
	f.calls = append(f.calls, "restart")
	return f.err
}

func (f *fakeManager) State(context.Context) (service.State, error) {
	f.calls = append(f.calls, "state")
	return f.state, f.stateErr
}

// withManager hands the service command f in place of the system's manager, on a system of the given name run by
// the given user.
func withManager(t *testing.T, f *fakeManager, goos string, euid int) {
	t.Helper()
	previousManager, previousOS, previousEUID := newServiceManager, hostOS, processEUID
	newServiceManager = func() (service.Manager, error) { return f, nil }
	hostOS, processEUID = goos, func() int { return euid }
	t.Cleanup(func() { newServiceManager, hostOS, processEUID = previousManager, previousOS, previousEUID })
}

func TestServiceInstallPrintsWhatTheManagerDid(t *testing.T) {
	f := &fakeManager{note: "installed and started the taraengine service"}
	withManager(t, f, "linux", 0)
	out, err := run(t, "service", "install", "--binary", "/usr/local/bin/taraengine")
	if err != nil || strings.TrimSpace(out) != f.note || !slices.Equal(f.calls, []string{"install /usr/local/bin/taraengine"}) {
		t.Fatalf("%v %q %v", err, out, f.calls)
	}
	f.err = errors.New("service: useradd failed")
	if _, err := run(t, "service", "install", "--binary", "/usr/local/bin/taraengine"); err == nil ||
		!strings.Contains(err.Error(), "useradd failed") {
		t.Fatalf("got %v", err)
	}
}

func TestServiceInstallNamesTheBinaryOnThePathWhenItIsThisOne(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "Caskroom", "taraengine", "0.3.0", "taraengine-darwin-arm64")
	link := filepath.Join(dir, "bin", "taraengine")
	other := filepath.Join(dir, "elsewhere", "taraengine")
	for _, p := range []string{target, other} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(p), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	previousExe, previousLook := executable, lookPath
	t.Cleanup(func() { executable, lookPath = previousExe, previousLook })
	executable = func() (string, error) { return target, nil }
	for _, c := range []struct{ onPath, want string }{{link, link}, {other, target}, {"", target}} {
		lookPath = func(string) (string, error) {
			if c.onPath == "" {
				return "", errors.New("not found")
			}
			return c.onPath, nil
		}
		if got, err := installedBinary(""); err != nil || got != c.want {
			t.Errorf("on PATH %q: got %q %v, want %q", c.onPath, got, err, c.want)
		}
	}
	if got, _ := installedBinary("relative/taraengine"); !filepath.IsAbs(got) {
		t.Errorf("a --binary is made absolute: %q", got)
	}
	executable = func() (string, error) { return "", errors.New("no executable") }
	if _, err := installedBinary(""); err == nil {
		t.Error("an executable that cannot be found is an error")
	}
}

func TestServiceStartStopRestartUninstall(t *testing.T) {
	f := &fakeManager{note: "removed the taraengine service"}
	withManager(t, f, "linux", 0)
	for _, c := range []struct{ cmd, call, out string }{
		{"start", "start", "started the taraengine service"},
		{"stop", "stop", "stopped the taraengine service; it stays stopped until `taraengine service start`"},
		{"restart", "restart", "restarted the taraengine service"},
		{"uninstall", "uninstall", "removed the taraengine service"},
	} {
		f.calls = nil
		out, err := run(t, "service", c.cmd)
		if err != nil || strings.TrimSpace(out) != c.out || !slices.Equal(f.calls, []string{c.call}) {
			t.Errorf("%s: %v %q %v", c.cmd, err, out, f.calls)
		}
	}
	f.err = service.ErrNotInstalled
	for _, cmd := range []string{"start", "stop", "restart", "uninstall"} {
		if _, err := run(t, "service", cmd); !errors.Is(err, service.ErrNotInstalled) {
			t.Errorf("%s: %v", cmd, err)
		}
	}
}

func TestServiceCommandsRefuseRootOnMacOS(t *testing.T) {
	f := &fakeManager{state: service.State{}}
	withManager(t, f, "darwin", 0)
	for _, cmd := range []string{"install", "uninstall", "start", "stop", "restart"} {
		if _, err := run(t, "service", cmd); err == nil || !strings.Contains(err.Error(), "without sudo") {
			t.Errorf("%s: %v", cmd, err)
		}
	}
	if len(f.calls) != 0 {
		t.Fatalf("nothing reached the manager: %v", f.calls)
	}
	if _, err := run(t, "service", "status", "--host=http://127.0.0.1:1"); ExitCode(err) != 3 {
		t.Fatalf("status may run as root: %v", err)
	}
}

func TestServiceStatusOfARunningEngine(t *testing.T) {
	srv, _ := daemon(t)
	f := &fakeManager{state: service.State{Installed: true, Loaded: true, Enabled: true, Running: true, PID: 4242,
		Definition: "/etc/systemd/system/taraengine.service", Logs: []string{"journalctl -u taraengine"}}}
	withManager(t, f, "linux", 1000)
	out, err := run(t, "service", "status", "--host="+srv.URL)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{
		"Service: installed (/etc/systemd/system/taraengine.service), enabled, running (pid 4242)",
		"Engine: ok, taraengine 0.1.0-test", "at " + srv.URL,
		"Logs: journalctl -u taraengine",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
}

func TestServiceStatusOfAServiceThatDoesNotRun(t *testing.T) {
	f := &fakeManager{state: service.State{Installed: true, Loaded: true, Enabled: true, LastExit: "1",
		Definition: "/Users/x/Library/LaunchAgents/vision.tara.taraengine.plist",
		Logs:       []string{"/Users/x/Library/Logs/taraengine/service.log"}}}
	withManager(t, f, "darwin", 501)
	dataDir := t.TempDir()
	out, err := run(t, "service", "status", "--host=http://127.0.0.1:1", "--data-dir", dataDir)
	if ExitCode(err) != 3 || err.Error() != "" {
		t.Fatalf("a service that does not run ends with 3 and says nothing more: %v", err)
	}
	for _, want := range []string{
		"enabled, not running (last exit 1)", "Engine: not answering at http://127.0.0.1:1",
		"Logs: /Users/x/Library/Logs/taraengine/service.log, " + filepath.Join(dataDir, "logs", "taraengine.log"),
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
}

func TestServiceStatusOfAStoppedOrMissingService(t *testing.T) {
	f := &fakeManager{state: service.State{Installed: true, Definition: "/etc/systemd/system/taraengine.service"}}
	withManager(t, f, "linux", 1000)
	out, err := run(t, "service", "status", "--host=http://127.0.0.1:1")
	if ExitCode(err) != 3 || !strings.Contains(out, "stopped, not running") {
		t.Fatalf("%v\n%s", err, out)
	}
	f.state = service.State{}
	out, err = run(t, "service", "status", "--host=http://127.0.0.1:1")
	if ExitCode(err) != 3 || !strings.Contains(out, "Service: not installed; `taraengine service install` installs it") {
		t.Fatalf("%v\n%s", err, out)
	}
}

func TestServiceStatusOfAStartingEngine(t *testing.T) {
	url := stub(t, http.StatusOK, `{"status":"starting","backend":"llama.cpp","detail":"installing runtime b11514 (40%)"}`)
	f := &fakeManager{state: service.State{Installed: true, Loaded: true, Enabled: true, Running: true, PID: 7}}
	withManager(t, f, "linux", 1000)
	out, err := run(t, "service", "status", "--host="+url)
	if err != nil || !strings.Contains(out, "Engine: starting: installing runtime b11514 (40%)") {
		t.Fatalf("%v\n%s", err, out)
	}
}

func TestServiceStatusReportsAStateItCannotRead(t *testing.T) {
	f := &fakeManager{stateErr: service.ErrNoSystemd}
	withManager(t, f, "linux", 1000)
	if _, err := run(t, "service", "status"); !errors.Is(err, service.ErrNoSystemd) || ExitCode(err) != 1 {
		t.Fatalf("got %v", err)
	}
}

func TestServiceCommandsReportAManagerTheyCannotBuild(t *testing.T) {
	previous := newServiceManager
	newServiceManager = func() (service.Manager, error) { return nil, errors.New("service: plan9 has none") }
	t.Cleanup(func() { newServiceManager = previous })
	for _, cmd := range []string{"install", "uninstall", "start", "stop", "restart", "status"} {
		if _, err := run(t, "service", cmd); err == nil || !strings.Contains(err.Error(), "plan9 has none") {
			t.Errorf("%s: %v", cmd, err)
		}
	}
}

func TestSystemManagerFitsTheSystem(t *testing.T) {
	systems := map[string]string{"linux": "*service.Systemd"}
	if _, err := user.Current(); err == nil { // the macOS agent reads the user database; its failures are the service package's
		systems["darwin"] = "*service.Launchd"
	}
	for goos, want := range systems {
		m, err := systemManager(goos)
		if err != nil || fmt.Sprintf("%T", m) != want {
			t.Errorf("%s: %T %v", goos, m, err)
		}
	}
	if _, err := systemManager("plan9"); err == nil || !strings.Contains(err.Error(), "plan9") {
		t.Errorf("got %v", err)
	}
}

func TestExitCode(t *testing.T) {
	for _, c := range []struct {
		err  error
		want int
	}{
		{nil, 0}, {errors.New("x"), 1}, {&ExitError{Code: 3}, 3}, {fmt.Errorf("wrapped: %w", &ExitError{Code: 4}), 4},
	} {
		if got := ExitCode(c.err); got != c.want {
			t.Errorf("%v: %d, want %d", c.err, got, c.want)
		}
	}
	if (&ExitError{Code: 3, Message: "m"}).Error() != "m" {
		t.Error("an ExitError says its message")
	}
}

func TestServiceStatusOfAnEngineThatAnswersOddly(t *testing.T) {
	f := &fakeManager{state: service.State{Installed: true, Loaded: true, Enabled: true, Running: true, PID: 7}}
	withManager(t, f, "linux", 1000)
	degraded := stub(t, http.StatusOK, `{"status":"degraded","backend":"llama.cpp","detail":"the backend did not answer"}`)
	out, err := run(t, "service", "status", "--host="+degraded)
	if err != nil || !strings.Contains(out, "Engine: degraded (the backend did not answer)") {
		t.Fatalf("%v\n%s", err, out)
	}
	broken := stub(t, http.StatusInternalServerError, `{"error":{"message":"boom"}}`)
	out, err = run(t, "service", "status", "--host="+broken)
	if ExitCode(err) != 3 || !strings.Contains(out, "did not answer as expected: boom") {
		t.Fatalf("%v\n%s", err, out)
	}
}

func TestServiceStatusReportsAConfigItCannotRead(t *testing.T) {
	withManager(t, &fakeManager{}, "linux", 1000)
	out, err := run(t, "service", "status", "--config", filepath.Join(t.TempDir(), "missing.yaml"))
	if ExitCode(err) != 1 || !strings.Contains(out, "missing.yaml") {
		t.Fatalf("got %v\n%s", err, out)
	}
}

func TestServiceInstallReportsAnExecutableItCannotFind(t *testing.T) {
	withManager(t, &fakeManager{}, "linux", 0)
	previous := executable
	executable = func() (string, error) { return "", errors.New("no executable") }
	t.Cleanup(func() { executable = previous })
	if _, err := run(t, "service", "install"); err == nil || !strings.Contains(err.Error(), "no executable") {
		t.Fatalf("got %v", err)
	}
}

func TestTheDefaultManagerFollowsTheHostSystem(t *testing.T) {
	previous := hostOS
	hostOS = "plan9"
	t.Cleanup(func() { hostOS = previous })
	if _, err := hostServiceManager(); err == nil || !strings.Contains(err.Error(), "plan9") {
		t.Fatalf("got %v", err)
	}
}

func TestServiceStatusShowsTheServiceWhenTheConfigurationIsBroken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("listen: [\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	withManager(t, &fakeManager{state: service.State{Installed: true, Enabled: true, Loaded: true, LastExit: "1",
		Definition: "/agents/vision.tara.taraengine.plist", Logs: []string{"/logs/service.log"}}}, "darwin", 501)
	out, err := run(t, "service", "status", "--config", path)
	if ExitCode(err) != 1 {
		t.Fatalf("a configuration that cannot be read ends status with 1: %v\n%s", err, out)
	}
	for _, want := range []string{
		"Service: installed (/agents/vision.tara.taraengine.plist), enabled, not running (last exit 1)\n",
		"Engine: cannot read the configuration: ",
		"Logs: /logs/service.log, ",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q: the case status is for is a service that a broken configuration keeps down\n%s", want, out)
		}
	}
}

func TestServiceStatusReportsWhatItCannotPrint(t *testing.T) {
	withManager(t, &fakeManager{state: service.State{Installed: true}}, "linux", 1000)
	boom := errors.New("stdout is closed")
	err := runWithContext(context.Background(), t, &failingWriter{err: boom}, "service", "status", "--config",
		filepath.Join(t.TempDir(), "missing.yaml"))
	if !errors.Is(err, boom) {
		t.Fatalf("got %v, want the write error", err)
	}
}
