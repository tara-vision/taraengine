package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// fakeLaunchctl is launchd as launchctl shows it, for one agent: loaded or not, disabled or not, running with a pid.
// A subcommand listed in fail fails the way launchctl does. It records every command.
type fakeLaunchctl struct {
	loaded, disabled, running bool
	lastExit                  string // what print says after "last exit code ="
	fail                      map[string]bool
	calls                     []string
	// slowExit makes the next bootout answer as launchctl does while the job is still exiting: 36, "Operation now in
	// progress". The job then stays loaded for lingers more prints.
	slowExit bool
	lingers  int
	exiting  int
}

const fakeTarget = "gui/501/" + Label

// notFound is what launchctl prints for an agent it does not have, with the exit status 113.
const notFound = `Could not find service "` + Label + `" in domain for user gui: 501`

var errExit113 = errors.New("exit status 113")

func (f *fakeLaunchctl) run(_ context.Context, name string, args ...string) (string, error) {
	call := strings.Join(append([]string{name}, args...), " ")
	f.calls = append(f.calls, call)
	if name != "launchctl" || len(args) < 2 {
		return "", fmt.Errorf("unexpected command %q", call)
	}
	if f.fail[args[0]] {
		if args[0] == "bootstrap" {
			return "Bootstrap failed: 5: Input/output error", errors.New("exit status 5")
		}
		return args[0] + " failed", errors.New("exit status 1")
	}
	handle, ok := map[string]func(args []string) (string, error){
		"print": f.print, "print-disabled": f.printDisabled, "enable": f.enable, "disable": f.disable,
		"bootstrap": f.bootstrap, "bootout": f.bootout, "kickstart": f.kickstart,
	}[args[0]]
	if !ok {
		return "", fmt.Errorf("unexpected command %q", call)
	}
	return handle(args)
}

func (f *fakeLaunchctl) print(args []string) (string, error) {
	if f.exiting > 0 {
		if f.exiting--; f.exiting == 0 {
			f.loaded, f.running = false, false
		}
	}
	if args[1] != fakeTarget || !f.loaded {
		return "Bad request.\n" + notFound, errExit113
	}
	state, pid := "not running", ""
	if f.running {
		state, pid = "running", "\tpid = 4242\n"
	}
	return fakeTarget + " = {\n\tstate = " + state + "\n" + pid + "\tlast exit code = " + f.lastExit +
		"\n\tendpoints = {\n\t\tstate = active\n\t}\n}\n", nil
}

func (f *fakeLaunchctl) printDisabled([]string) (string, error) {
	value := "enabled"
	if f.disabled {
		value = "disabled"
	}
	return "\tdisabled services = {\n\t\t\"com.example.other\" => disabled\n\t\t\"" + Label + "\" => " + value + "\n\t}\n", nil
}

func (f *fakeLaunchctl) enable([]string) (string, error)  { f.disabled = false; return "", nil }
func (f *fakeLaunchctl) disable([]string) (string, error) { f.disabled = true; return "", nil }

func (f *fakeLaunchctl) bootstrap([]string) (string, error) {
	if f.loaded {
		return "Bootstrap failed: 37: Operation already in progress", errors.New("exit status 37")
	}
	f.loaded, f.running = true, true
	return "", nil
}

func (f *fakeLaunchctl) bootout([]string) (string, error) {
	if !f.loaded {
		return "Boot-out failed: 3: No such process", errors.New("exit status 3")
	}
	if f.slowExit {
		f.slowExit, f.exiting = false, f.lingers+1
		return "Boot-out failed: 36: Operation now in progress", errors.New("exit status 36")
	}
	f.loaded, f.running = false, false
	return "", nil
}

func (f *fakeLaunchctl) kickstart([]string) (string, error) {
	if !f.loaded {
		return notFound, errExit113
	}
	f.running = true
	return "", nil
}

// newLaunchd is a manager for uid 501 with a home of its own and the fake behind it.
func newLaunchd(t *testing.T, fake *fakeLaunchctl, sandboxed bool) *Launchd {
	t.Helper()
	return &Launchd{Run: fake.run, UID: 501, Home: t.TempDir(), Sandboxed: sandboxed}
}

func readPlist(t *testing.T, l *Launchd) string {
	t.Helper()
	data, err := os.ReadFile(l.PlistPath())
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestLaunchdPathsComeFromTheHome(t *testing.T) {
	l := &Launchd{UID: 501, Home: "/Users/someone"}
	if l.PlistPath() != "/Users/someone/Library/LaunchAgents/vision.tara.taraengine.plist" ||
		l.LogPath() != "/Users/someone/Library/Logs/taraengine/service.log" {
		t.Fatalf("%s %s", l.PlistPath(), l.LogPath())
	}
}

func TestLaunchdInstallWritesTheAgentAndStartsIt(t *testing.T) {
	fake := &fakeLaunchctl{}
	l := newLaunchd(t, fake, false)
	note, err := l.Install(context.Background(), "/usr/local/bin/taraengine")
	if err != nil || note != "installed and started the taraengine service" {
		t.Fatalf("%q %v", note, err)
	}
	if !strings.Contains(readPlist(t, l), "<string>/usr/local/bin/taraengine</string>") {
		t.Fatal("the plist runs the binary")
	}
	if info, err := os.Stat(l.PlistPath()); err != nil || info.Mode().Perm() != 0o644 {
		t.Fatalf("the plist is readable by launchd: %v %v", info, err)
	}
	if info, err := os.Stat(filepath.Dir(l.LogPath())); err != nil || !info.IsDir() {
		t.Fatalf("the log directory exists, launchd does not create it: %v", err)
	}
	if !fake.loaded || !fake.running || !slices.Contains(fake.calls, "launchctl bootstrap gui/501 "+l.PlistPath()) {
		t.Fatalf("bootstrapped: %v", fake.calls)
	}
}

func TestLaunchdInstallRestartsALoadedAgentOnTheNewBinary(t *testing.T) {
	fake := &fakeLaunchctl{}
	l := newLaunchd(t, fake, false)
	if _, err := l.Install(context.Background(), "/usr/local/bin/taraengine"); err != nil {
		t.Fatal(err)
	}
	fake.calls = nil
	note, err := l.Install(context.Background(), "/usr/local/bin/taraengine")
	if err != nil || note != "restarted the taraengine service" {
		t.Fatalf("%q %v", note, err)
	}
	if !slices.Equal(fake.calls, []string{
		"launchctl print-disabled gui/501", "launchctl print gui/501/" + Label, "launchctl kickstart -k gui/501/" + Label,
	}) {
		t.Fatalf("an unchanged agent is restarted in place: %v", fake.calls)
	}
}

func TestLaunchdInstallReloadsAnAgentWhoseBinaryMoved(t *testing.T) {
	fake := &fakeLaunchctl{}
	l := newLaunchd(t, fake, false)
	if _, err := l.Install(context.Background(), "/usr/local/bin/taraengine"); err != nil {
		t.Fatal(err)
	}
	note, err := l.Install(context.Background(), "/opt/homebrew/bin/taraengine")
	if err != nil || note != "restarted the taraengine service" {
		t.Fatalf("%q %v", note, err)
	}
	if !slices.Contains(fake.calls, "launchctl bootout gui/501/"+Label) || !fake.loaded ||
		!strings.Contains(readPlist(t, l), "/opt/homebrew/bin/taraengine") {
		t.Fatalf("a changed definition is booted out and loaded again: %v", fake.calls)
	}
}

func TestLaunchdInstallKeepsAStoppedAgentStopped(t *testing.T) {
	fake := &fakeLaunchctl{disabled: true}
	l := newLaunchd(t, fake, false)
	note, err := l.Install(context.Background(), "/usr/local/bin/taraengine")
	if err != nil || note != "the taraengine service is stopped; `taraengine service start` starts it" {
		t.Fatalf("%q %v", note, err)
	}
	if fake.loaded || !fake.disabled {
		t.Fatalf("a stop is remembered through an install: %v", fake.calls)
	}
	if !strings.Contains(readPlist(t, l), "/usr/local/bin/taraengine") {
		t.Fatal("the definition is still rewritten")
	}
}

func TestLaunchdInstallInHomebrewsSandbox(t *testing.T) {
	t.Run("a first install cannot load the agent and says when it starts", func(t *testing.T) {
		fake := &fakeLaunchctl{fail: map[string]bool{"bootstrap": true}}
		l := newLaunchd(t, fake, true)
		note, err := l.Install(context.Background(), "/opt/homebrew/bin/taraengine")
		if err != nil || !strings.Contains(note, "starts at your next login, or with the first taraengine command") {
			t.Fatalf("%q %v", note, err)
		}
	})
	// In the sandbox launchctl cannot restart the agent: a kickstart there blocks the upgrade. Homebrew links the new
	// binary before this step, and the agent's KeepAlive on that path brings it back on the new binary by itself.
	t.Run("an upgrade leaves the restart to launchd", func(t *testing.T) {
		fake := &fakeLaunchctl{loaded: true, running: true, fail: map[string]bool{"bootstrap": true}}
		l := newLaunchd(t, fake, true)
		note, err := l.Install(context.Background(), "/opt/homebrew/bin/taraengine")
		if err != nil || note != "the taraengine service restarts on the new binary" {
			t.Fatalf("%q %v", note, err)
		}
		if !slices.Equal(fake.calls, []string{"launchctl print-disabled gui/501", "launchctl print gui/501/" + Label}) {
			t.Fatalf("only reads, no restart: %v", fake.calls)
		}
	})
	t.Run("outside the sandbox a failed load is an error", func(t *testing.T) {
		fake := &fakeLaunchctl{fail: map[string]bool{"bootstrap": true}}
		l := newLaunchd(t, fake, false)
		if _, err := l.Install(context.Background(), "/usr/local/bin/taraengine"); err == nil ||
			!strings.Contains(err.Error(), "Input/output error") {
			t.Fatalf("got %v", err)
		}
	})
}

func TestLaunchdInstallReportsWhatItCannotWrite(t *testing.T) {
	fake := &fakeLaunchctl{}
	l := newLaunchd(t, fake, false)
	if err := os.WriteFile(filepath.Join(l.Home, "Library"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Install(context.Background(), "/usr/local/bin/taraengine"); err == nil ||
		!strings.HasPrefix(err.Error(), "service: ") {
		t.Fatalf("got %v", err)
	}
	l = newLaunchd(t, fake, false)
	if err := os.MkdirAll(l.PlistPath(), 0o755); err != nil { // a directory where the plist goes
		t.Fatal(err)
	}
	if _, err := l.Install(context.Background(), "/usr/local/bin/taraengine"); err == nil {
		t.Fatal("a plist that cannot be put in place is an error")
	}
}

func TestLaunchdStartStopRestart(t *testing.T) {
	fake := &fakeLaunchctl{}
	l := newLaunchd(t, fake, false)
	ctx := context.Background()
	if err := l.Start(ctx); !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("start before install: %v", err)
	}
	if _, err := l.Install(ctx, "/usr/local/bin/taraengine"); err != nil {
		t.Fatal(err)
	}
	if err := l.Stop(ctx); err != nil || fake.loaded || !fake.disabled {
		t.Fatalf("stop boots the agent out and disables it: %v %v", err, fake.calls)
	}
	if err := l.Stop(ctx); err != nil {
		t.Fatalf("a second stop is no error: %v", err)
	}
	if err := l.Restart(ctx); err != nil || !fake.loaded || fake.disabled {
		t.Fatalf("restart of a stopped agent starts it: %v %v", err, fake.calls)
	}
	fake.calls = nil
	if err := l.Restart(ctx); err != nil || !slices.Contains(fake.calls, "launchctl kickstart -k gui/501/"+Label) {
		t.Fatalf("restart of a loaded agent kickstarts it: %v %v", err, fake.calls)
	}
	fake.calls = nil
	if err := l.Start(ctx); err != nil || !slices.Contains(fake.calls, "launchctl kickstart gui/501/"+Label) {
		t.Fatalf("start of a loaded agent kickstarts it: %v %v", err, fake.calls)
	}
}

// fastGone makes the wait for an exiting job short enough for a test.
func fastGone(t *testing.T) {
	t.Helper()
	previousWait, previousPoll := goneWait, gonePoll
	goneWait, gonePoll = 200*time.Millisecond, time.Millisecond
	t.Cleanup(func() { goneWait, gonePoll = previousWait, previousPoll })
}

// installedAgent is an agent installed and running on the fake.
func installedAgent(t *testing.T) (*Launchd, *fakeLaunchctl) {
	t.Helper()
	fake := &fakeLaunchctl{}
	l := newLaunchd(t, fake, false)
	if _, err := l.Install(context.Background(), "/usr/local/bin/taraengine"); err != nil {
		t.Fatal(err)
	}
	fake.calls = nil
	return l, fake
}

func TestLaunchdStopIsRememberedWhileTheJobIsStillExiting(t *testing.T) {
	fastGone(t)
	l, fake := installedAgent(t)
	fake.slowExit, fake.lingers = true, 2
	if err := l.Stop(context.Background()); err != nil || fake.loaded || !fake.disabled {
		t.Fatalf("a stop waits for the job to exit and is remembered: %v %v", err, fake.calls)
	}
	disable, bootout := slices.Index(fake.calls, "launchctl disable "+fakeTarget), slices.Index(fake.calls, "launchctl bootout "+fakeTarget)
	if disable < 0 || bootout < 0 || disable > bootout {
		t.Fatalf("the stop is written before the job is booted out: %v", fake.calls)
	}
}

func TestLaunchdStopIsRememberedWhenTheBootoutFails(t *testing.T) {
	l, fake := installedAgent(t)
	fake.fail = map[string]bool{"bootout": true}
	if err := l.Stop(context.Background()); err == nil || !strings.Contains(err.Error(), "launchctl bootout") || !fake.disabled {
		t.Fatalf("a stop that cannot unload the job still keeps it from the next login: %v %v", err, fake.calls)
	}
}

func TestLaunchdStopSaysWhenTheJobDoesNotExit(t *testing.T) {
	fastGone(t)
	l, fake := installedAgent(t)
	fake.slowExit, fake.lingers = true, 1<<30
	if err := l.Stop(context.Background()); err == nil || !strings.Contains(err.Error(), "still exiting") {
		t.Fatalf("got %v", err)
	}
	fake.slowExit, fake.lingers, fake.exiting = true, 1<<30, 0
	fake.loaded = true
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := l.Uninstall(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("the wait ends with the command: %v", err)
	}
}

func TestLaunchdReloadAndUninstallWaitForTheOldJob(t *testing.T) {
	fastGone(t)
	l, fake := installedAgent(t)
	fake.slowExit, fake.lingers = true, 2
	note, err := l.Install(context.Background(), "/opt/homebrew/bin/taraengine")
	if err != nil || note != noteRestarted || !fake.loaded {
		t.Fatalf("a moved binary is loaded once the old job is gone: %q %v %v", note, err, fake.calls)
	}
	fake.slowExit, fake.lingers = true, 2
	if _, err := l.Uninstall(context.Background()); err != nil || fake.loaded || l.installed() {
		t.Fatalf("an uninstall waits for the job before it removes the plist: %v %v", err, fake.calls)
	}
}

func TestLaunchdStartReportsALoadThatFails(t *testing.T) {
	fake := &fakeLaunchctl{}
	l := newLaunchd(t, fake, false)
	if _, err := l.Install(context.Background(), "/usr/local/bin/taraengine"); err != nil {
		t.Fatal(err)
	}
	if err := l.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	fake.fail = map[string]bool{"bootstrap": true}
	if err := l.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "Input/output error") {
		t.Fatalf("got %v", err)
	}
}

func TestLaunchdUninstallRemovesTheAgentAndForgetsTheStop(t *testing.T) {
	fake := &fakeLaunchctl{}
	l := newLaunchd(t, fake, false)
	ctx := context.Background()
	if _, err := l.Install(ctx, "/usr/local/bin/taraengine"); err != nil {
		t.Fatal(err)
	}
	if err := l.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	note, err := l.Uninstall(ctx)
	if err != nil || !strings.Contains(note, "removed the taraengine service") {
		t.Fatalf("%q %v", note, err)
	}
	if _, err := os.Stat(l.PlistPath()); !errors.Is(err, os.ErrNotExist) || fake.loaded || fake.disabled {
		t.Fatalf("the plist is gone, the agent unloaded and the stop forgotten: %v %v", err, fake.calls)
	}
	if _, err := l.Uninstall(ctx); !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("a second uninstall: %v", err)
	}
}

func TestLaunchdUninstallReportsAPlistItCannotRemove(t *testing.T) {
	fake := &fakeLaunchctl{}
	l := newLaunchd(t, fake, false)
	if _, err := l.Install(context.Background(), "/usr/local/bin/taraengine"); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(l.PlistPath())
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if _, err := l.Uninstall(context.Background()); err == nil || !strings.HasPrefix(err.Error(), "service: ") {
		t.Fatalf("got %v", err)
	}
}

func TestLaunchdState(t *testing.T) {
	fake := &fakeLaunchctl{lastExit: "(never exited)"}
	l := newLaunchd(t, fake, false)
	ctx := context.Background()
	st, err := l.State(ctx)
	if err != nil || st.Installed || st.Loaded || st.Running {
		t.Fatalf("nothing installed: %+v %v", st, err)
	}
	if _, err := l.Install(ctx, "/usr/local/bin/taraengine"); err != nil {
		t.Fatal(err)
	}
	st, err = l.State(ctx)
	want := State{Installed: true, Loaded: true, Enabled: true, Running: true, PID: 4242, LastExit: "(never exited)",
		Definition: l.PlistPath(), Logs: []string{l.LogPath()}}
	if err != nil || !stateEqual(st, want) {
		t.Fatalf("got %+v, want %+v (%v)", st, want, err)
	}
	fake.running, fake.lastExit = false, "1"
	if st, _ := l.State(ctx); st.Running || st.PID != 0 || st.LastExit != "1" {
		t.Fatalf("a loaded agent that exited: %+v", st)
	}
	if err := l.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if st, _ := l.State(ctx); !st.Installed || st.Enabled || st.Loaded {
		t.Fatalf("a stopped agent: %+v", st)
	}
}

func TestLaunchdStateReportsADisabledListItCannotRead(t *testing.T) {
	l := newLaunchd(t, &fakeLaunchctl{}, false)
	l.Run = func(context.Context, string, ...string) (string, error) { return "", errors.New("launchctl is gone") }
	if _, err := l.State(context.Background()); err == nil || !strings.Contains(err.Error(), "launchctl is gone") {
		t.Fatalf("got %v", err)
	}
	if _, err := l.Install(context.Background(), "/usr/local/bin/taraengine"); err == nil {
		t.Fatal("an install that cannot read the disabled list fails")
	}
}

func stateEqual(a, b State) bool {
	return a.Installed == b.Installed && a.Loaded == b.Loaded && a.Enabled == b.Enabled && a.Running == b.Running &&
		a.PID == b.PID && a.LastExit == b.LastExit && a.Definition == b.Definition && slices.Equal(a.Logs, b.Logs)
}

func TestNewLaunchdTakesTheHomeFromTheUserDatabase(t *testing.T) {
	previous := currentUser
	t.Cleanup(func() { currentUser = previous })
	currentUser = func() (*user.User, error) { return &user.User{Uid: "501", HomeDir: "/Users/someone"}, nil }
	t.Setenv("HOME", t.TempDir())
	l, err := NewLaunchd(ExecRunner)
	if err != nil {
		t.Fatal(err)
	}
	if l.Home != "/Users/someone" || !l.Sandboxed || l.UID != 501 {
		t.Fatalf("the home is the user's, not $HOME, and the difference marks a sandbox: %+v", l)
	}
	t.Setenv("HOME", "/Users/someone")
	if l, _ := NewLaunchd(ExecRunner); l.Sandboxed {
		t.Fatal("a HOME that is the user's home is no sandbox")
	}
}

func TestLaunchdReportsWhatLaunchctlRefuses(t *testing.T) {
	ctx := context.Background()
	installed := func(t *testing.T, fail string) (*Launchd, *fakeLaunchctl) {
		t.Helper()
		fake := &fakeLaunchctl{}
		l := newLaunchd(t, fake, false)
		if _, err := l.Install(ctx, "/usr/local/bin/taraengine"); err != nil {
			t.Fatal(err)
		}
		fake.fail = map[string]bool{fail: true}
		return l, fake
	}
	t.Run("a bootout that fails stops the reload of a moved binary", func(t *testing.T) {
		l, _ := installed(t, "bootout")
		if _, err := l.Install(ctx, "/opt/homebrew/bin/taraengine"); err == nil || !strings.Contains(err.Error(), "bootout failed") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("start", func(t *testing.T) {
		l, _ := installed(t, "enable")
		if err := l.Start(ctx); err == nil || !strings.Contains(err.Error(), "launchctl enable") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("stop", func(t *testing.T) {
		l, _ := installed(t, "bootout")
		if err := l.Stop(ctx); err == nil || !strings.Contains(err.Error(), "launchctl bootout") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("stop that cannot be written", func(t *testing.T) {
		l, fake := installed(t, "disable")
		if err := l.Stop(ctx); err == nil || !strings.Contains(err.Error(), "launchctl disable") || !fake.loaded {
			t.Fatalf("a stop that cannot be remembered leaves the job running: %v", err)
		}
	})
	t.Run("uninstall that cannot unload", func(t *testing.T) {
		l, _ := installed(t, "bootout")
		if _, err := l.Uninstall(ctx); err == nil || !strings.Contains(err.Error(), "launchctl bootout") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("uninstall that cannot forget the stop", func(t *testing.T) {
		l, _ := installed(t, "enable")
		if _, err := l.Uninstall(ctx); err == nil || !strings.Contains(err.Error(), "launchctl enable") {
			t.Fatalf("got %v", err)
		}
	})
}

func TestLaunchdInstallReportsAPlistItCannotWrite(t *testing.T) {
	l := newLaunchd(t, &fakeLaunchctl{}, false)
	if err := os.MkdirAll(l.PlistPath()+".tmp", 0o755); err != nil { // a directory where the temporary file goes
		t.Fatal(err)
	}
	if _, err := l.Install(context.Background(), "/usr/local/bin/taraengine"); err == nil ||
		!strings.HasPrefix(err.Error(), "service: ") {
		t.Fatalf("got %v", err)
	}
}

func TestNewLaunchdReportsAUserItCannotLookUp(t *testing.T) {
	previous := currentUser
	t.Cleanup(func() { currentUser = previous })
	currentUser = func() (*user.User, error) { return nil, errors.New("no user database") }
	if _, err := NewLaunchd(ExecRunner); err == nil || !strings.Contains(err.Error(), "no user database") {
		t.Fatalf("got %v", err)
	}
	currentUser = func() (*user.User, error) { return &user.User{Uid: "S-1-5-21", HomeDir: "/x"}, nil }
	if _, err := NewLaunchd(ExecRunner); err == nil || !strings.Contains(err.Error(), "user id") {
		t.Fatalf("got %v", err)
	}
}
