package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/tara-vision/taraengine/internal/selfupdate"
	"github.com/tara-vision/taraengine/internal/service"
	"github.com/tara-vision/taraengine/internal/version"
)

// updateWorld is the release side of an update: a release page that leads to latest, and its assets with sums.
type updateWorld struct {
	latest   string
	assets   map[string]string
	commands [][]string // what the update ran with the terminal attached
}

// withUpdate runs this binary as running, installed at exe, with world as the release side and run answering the
// package managers.
func withUpdate(t *testing.T, running, exe string, world *updateWorld, run selfupdate.Runner) {
	t.Helper()
	sums := ""
	for name, body := range world.assets {
		sum := sha256.Sum256([]byte(body))
		sums += hex.EncodeToString(sum[:]) + "  " + name + "\n"
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch name := filepath.Base(r.URL.Path); {
		case r.URL.Path == "/releases/latest":
			http.Redirect(w, r, "/releases/tag/v"+world.latest, http.StatusFound)
		case strings.HasPrefix(r.URL.Path, "/releases/tag/"):
			_, _ = w.Write([]byte("<html>"))
		case name == "checksums.txt":
			_, _ = w.Write([]byte(sums))
		case world.assets[name] != "":
			_, _ = w.Write([]byte(world.assets[name]))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	previous := []any{version.Version, newFinder, newDownloader, updateRunner, runInteractive, executable, hostArch}
	t.Cleanup(func() {
		version.Version = previous[0].(string)
		newFinder = previous[1].(func() *selfupdate.Finder)
		newDownloader = previous[2].(func() *selfupdate.Downloader)
		updateRunner = previous[3].(selfupdate.Runner)
		runInteractive = previous[4].(func(context.Context, string, ...string) error)
		executable = previous[5].(func() (string, error))
		hostArch = previous[6].(string)
	})
	version.Version = running
	newFinder = func() *selfupdate.Finder {
		return &selfupdate.Finder{HTTP: srv.Client(), Releases: srv.URL + "/releases/latest", API: srv.URL + "/none",
			Site: srv.URL + "/none"}
	}
	newDownloader = func() *selfupdate.Downloader {
		return &selfupdate.Downloader{HTTP: srv.Client(), Base: srv.URL + "/download"}
	}
	updateRunner = run
	runInteractive = func(_ context.Context, name string, args ...string) error {
		world.commands = append(world.commands, append([]string{name}, args...))
		return nil
	}
	executable = func() (string, error) { return exe, nil }
	hostArch = "amd64"
}

// ownsNothing is a system on which no package manager owns the binary.
func ownsNothing(context.Context, string, ...string) (string, error) {
	return "", errors.New("not owned")
}

// installedBinaryFile writes an old binary into a directory of its own and returns its path.
func installedBinaryFile(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "taraengine")
	if err := os.WriteFile(path, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestUpdateRefusesADevelopmentBuild(t *testing.T) {
	withUpdate(t, "dev", installedBinaryFile(t), &updateWorld{latest: "0.3.1"}, ownsNothing)
	if _, err := run(t, "update"); err == nil || !strings.Contains(err.Error(), "development build") {
		t.Fatalf("got %v", err)
	}
}

func TestUpdateSaysWhenNothingIsNewer(t *testing.T) {
	withUpdate(t, "0.3.1", installedBinaryFile(t), &updateWorld{latest: "0.3.1"}, ownsNothing)
	if out, err := run(t, "update"); err != nil || strings.TrimSpace(out) != "taraengine 0.3.1 is the latest release" {
		t.Fatalf("%v %q", err, out)
	}
	withUpdate(t, "0.3.0-rc.1", installedBinaryFile(t), &updateWorld{latest: "0.2.0"}, ownsNothing)
	out, err := run(t, "update")
	if err != nil || strings.TrimSpace(out) != "taraengine 0.3.0-rc.1 is newer than the latest release, 0.2.0" {
		t.Fatalf("a release candidate is never taken back to an older stable: %v %q", err, out)
	}
}

func TestUpdateCheckOnlyReports(t *testing.T) {
	exe := installedBinaryFile(t)
	withUpdate(t, "0.3.0-rc.2", exe, &updateWorld{latest: "0.3.0"}, ownsNothing)
	out, err := run(t, "update", "--check")
	if err != nil || strings.TrimSpace(out) != "taraengine 0.3.0 is out (this is 0.3.0-rc.2); `taraengine update` installs it" {
		t.Fatalf("%v %q", err, out)
	}
	if got, _ := os.ReadFile(exe); string(got) != "old" {
		t.Fatal("a check changes nothing")
	}
}

func TestUpdateReplacesAFileInstallAndRestartsTheService(t *testing.T) {
	exe := installedBinaryFile(t)
	world := &updateWorld{latest: "0.3.1", assets: map[string]string{"taraengine-linux-amd64": "new"}}
	withUpdate(t, "0.3.0", exe, world, ownsNothing)
	m := &fakeManager{state: service.State{Installed: true, Enabled: true, Running: true}}
	withManager(t, m, "linux", 0)
	out, err := run(t, "update")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if got, _ := os.ReadFile(exe); string(got) != "new" {
		t.Fatalf("the binary is replaced: %q", got)
	}
	for _, want := range []string{"taraengine 0.3.0 -> 0.3.1 in " + exe,
		"restarting the taraengine service on the new binary; a generation in progress is cut off",
		"restarted the taraengine service"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	if !slices.Contains(m.calls, "restart") {
		t.Fatalf("the service runs the new binary: %v", m.calls)
	}
}

func TestUpdateLeavesAStoppedServiceStopped(t *testing.T) {
	exe := installedBinaryFile(t)
	withUpdate(t, "0.3.0", exe, &updateWorld{latest: "0.3.1", assets: map[string]string{"taraengine-linux-amd64": "new"}}, ownsNothing)
	for _, st := range []service.State{{Installed: true}, {}} {
		m := &fakeManager{state: st}
		withManager(t, m, "linux", 0)
		if out, err := run(t, "update"); err != nil || strings.Contains(out, "restart") || slices.Contains(m.calls, "restart") {
			t.Fatalf("%+v: %v %q %v", st, err, out, m.calls)
		}
	}
	withManager(t, &fakeManager{stateErr: service.ErrNoSystemd}, "linux", 0)
	if _, err := run(t, "update"); err != nil {
		t.Fatalf("a system without a service manager has nothing to restart: %v", err)
	}
	previous := newServiceManager
	newServiceManager = func() (service.Manager, error) { return nil, errors.New("none") }
	t.Cleanup(func() { newServiceManager = previous })
	if _, err := run(t, "update"); err != nil {
		t.Fatalf("nor a system without one at all: %v", err)
	}
}

func TestUpdateSaysWhatTheRestartNeeds(t *testing.T) {
	withUpdate(t, "0.3.0", installedBinaryFile(t), &updateWorld{latest: "0.3.1", assets: map[string]string{"taraengine-linux-amd64": "new"}}, ownsNothing)
	withManager(t, &fakeManager{state: service.State{Installed: true, Enabled: true}, err: service.ErrNeedsRoot}, "linux", 1000)
	if _, err := run(t, "update"); err == nil || !strings.Contains(err.Error(), "sudo taraengine service restart") {
		t.Fatalf("got %v", err)
	}
	withManager(t, &fakeManager{state: service.State{Installed: true, Enabled: true}, err: errors.New("service: kickstart failed")}, "linux", 0)
	if _, err := run(t, "update"); err == nil || !strings.Contains(err.Error(), "kickstart failed") {
		t.Fatalf("got %v", err)
	}
}

func TestUpdateInADirectoryItCannotWriteAsksForSudo(t *testing.T) {
	exe := installedBinaryFile(t)
	withUpdate(t, "0.3.0", exe, &updateWorld{latest: "0.3.1", assets: map[string]string{"taraengine-linux-amd64": "new"}}, ownsNothing)
	withManager(t, &fakeManager{}, "linux", 1000)
	if err := os.Chmod(filepath.Dir(exe), 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Dir(exe), 0o700) })
	if _, err := run(t, "update"); err == nil || !strings.Contains(err.Error(), "run `sudo taraengine update`") {
		t.Fatalf("got %v", err)
	}
	if got, _ := os.ReadFile(exe); string(got) != "old" {
		t.Fatal("the old binary stays")
	}
}

func TestUpdateReportsADownloadThatFails(t *testing.T) {
	withUpdate(t, "0.3.0", installedBinaryFile(t), &updateWorld{latest: "0.3.1"}, ownsNothing)
	withManager(t, &fakeManager{}, "linux", 0)
	if _, err := run(t, "update"); err == nil || !strings.Contains(err.Error(), "no checksum for taraengine-linux-amd64") {
		t.Fatalf("got %v", err)
	}
}

func TestUpdateHandsACaskToHomebrew(t *testing.T) {
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	exe := filepath.Join(dir, "Caskroom", "taraengine", "0.3.0", "taraengine-darwin-arm64")
	if err := os.MkdirAll(filepath.Dir(exe), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exe, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	world := &updateWorld{latest: "0.3.1"}
	withUpdate(t, "0.3.0", exe, world, ownsNothing)
	withManager(t, &fakeManager{}, "darwin", 501)
	out, err := run(t, "update")
	brew := filepath.Join(dir, "bin", "brew")
	if err != nil || !strings.Contains(out, "running "+brew+" upgrade --cask tara-vision/taracode/taraengine") ||
		!slices.Equal(world.commands[0], []string{brew, "upgrade", "--cask", "tara-vision/taracode/taraengine"}) {
		t.Fatalf("%v %q %v", err, out, world.commands)
	}
	withManager(t, &fakeManager{}, "darwin", 0)
	if _, err := run(t, "update"); err == nil || !strings.Contains(err.Error(), "without sudo") {
		t.Fatalf("Homebrew refuses root: %v", err)
	}
}

func TestUpdateInstallsADebOrAnRPM(t *testing.T) {
	for _, c := range []struct{ owner, tool, flag, ext string }{{"dpkg-query", "dpkg", "-i", "deb"}, {"rpm", "rpm", "-U", "rpm"}} {
		t.Run(c.ext, func(t *testing.T) {
			exe := installedBinaryFile(t)
			pkg := "taraengine_0.3.1_linux_amd64." + c.ext
			world := &updateWorld{latest: "0.3.1", assets: map[string]string{pkg: "a package"}}
			owner := func(_ context.Context, name string, _ ...string) (string, error) {
				if name == c.owner {
					return "taraengine", nil
				}
				return "", errors.New("not owned")
			}
			withUpdate(t, "0.3.0", exe, world, owner)
			var seen string
			runInteractive = func(_ context.Context, name string, args ...string) error {
				world.commands = append(world.commands, append([]string{name}, args...))
				data, _ := os.ReadFile(args[len(args)-1])
				seen = string(data)
				return nil
			}
			withManager(t, &fakeManager{}, "linux", 1000)
			if _, err := run(t, "update"); err == nil || !strings.Contains(err.Error(), "sudo taraengine update") {
				t.Fatalf("a package is updated as root: %v", err)
			}
			withManager(t, &fakeManager{}, "linux", 0)
			out, err := run(t, "update")
			if err != nil || len(world.commands) != 1 || world.commands[0][0] != c.tool || world.commands[0][1] != c.flag ||
				filepath.Base(world.commands[0][2]) != pkg || seen != "a package" || !strings.Contains(out, "the package restarts the service") {
				t.Fatalf("%v %q %v %q", err, out, world.commands, seen)
			}
			if _, err := os.Stat(world.commands[0][2]); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("the downloaded package is removed afterwards")
			}
		})
	}
}

func TestUpdateReportsWhatGoesWrongOnTheWay(t *testing.T) {
	withUpdate(t, "0.3.0", installedBinaryFile(t), &updateWorld{latest: "0.3.1"}, ownsNothing)
	newFinder = func() *selfupdate.Finder {
		return &selfupdate.Finder{HTTP: http.DefaultClient, Releases: "http://127.0.0.1:1", API: "http://127.0.0.1:1", Site: "http://127.0.0.1:1"}
	}
	if _, err := run(t, "update"); err == nil || !strings.Contains(err.Error(), "no release could be found") {
		t.Fatalf("got %v", err)
	}
	withUpdate(t, "0.3.0", installedBinaryFile(t), &updateWorld{latest: "0.3.1"}, ownsNothing)
	executable = func() (string, error) { return "", errors.New("no executable") }
	if _, err := run(t, "update"); err == nil || !strings.Contains(err.Error(), "no executable") {
		t.Fatalf("got %v", err)
	}
	withUpdate(t, "0.3.0", installedBinaryFile(t), &updateWorld{latest: "0.3.1"},
		func(_ context.Context, name string, _ ...string) (string, error) {
			if name == "dpkg-query" {
				return "taraengine", nil
			}
			return "", errors.New("not owned")
		})
	withManager(t, &fakeManager{}, "linux", 0)
	if _, err := run(t, "update"); err == nil || !strings.Contains(err.Error(), "no checksum") {
		t.Fatalf("a package whose download fails: %v", err)
	}
}

func TestUpdateManagerOnMacOSUnderSudoIsTheUsersAgent(t *testing.T) {
	withManager(t, &fakeManager{}, "darwin", 0)
	previous := lookupUserID
	t.Cleanup(func() { lookupUserID = previous })
	lookupUserID = func(uid string) (*user.User, error) { return &user.User{Uid: uid, HomeDir: "/Users/someone"}, nil }
	t.Setenv("SUDO_UID", "501")
	m, err := updateManager()
	agent, ok := m.(*service.Launchd)
	if err != nil || !ok || agent.UID != 501 || agent.Home != "/Users/someone" || agent.Sandboxed {
		t.Fatalf("%+v %v", m, err)
	}
	lookupUserID = func(string) (*user.User, error) { return nil, errors.New("unknown user") }
	if _, err := updateManager(); err == nil {
		t.Fatal("a user that cannot be looked up")
	}
	t.Setenv("SUDO_UID", "not a number")
	lookupUserID = func(uid string) (*user.User, error) { return &user.User{Uid: uid, HomeDir: "/x"}, nil }
	if _, err := updateManager(); err == nil {
		t.Fatal("a user id that is no number")
	}
	t.Setenv("SUDO_UID", "")
	if m, err := updateManager(); err != nil || m == nil {
		t.Fatalf("without sudo, the system's manager: %v", err)
	}
}

func TestRunInteractiveRunsTheCommandWithTheTerminal(t *testing.T) {
	if err := runInteractive(context.Background(), "true"); err != nil {
		t.Fatal(err)
	}
	if err := runInteractive(context.Background(), "false"); err == nil || !strings.Contains(err.Error(), "false") {
		t.Fatalf("a command that fails is named: %v", err)
	}
}

func TestUpdateStopsWhenItCannotPrintOrUnpack(t *testing.T) {
	quiet := &printer{w: &failingWriter{err: errors.New("stdout is closed")}}
	withManager(t, &fakeManager{}, "darwin", 501)
	if err := updateHomebrew(context.Background(), quiet, selfupdate.Install{Prefix: "/opt/homebrew"}); err == nil ||
		!strings.Contains(err.Error(), "stdout is closed") {
		t.Fatalf("got %v", err)
	}
	pkg := "taraengine_0.3.1_linux_amd64.deb"
	withUpdate(t, "0.3.0", installedBinaryFile(t), &updateWorld{latest: "0.3.1", assets: map[string]string{pkg: "p"}}, ownsNothing)
	withManager(t, &fakeManager{}, "linux", 0)
	latest, _ := selfupdate.ParseVersion("0.3.1")
	quiet = &printer{w: &failingWriter{err: errors.New("stdout is closed")}}
	if err := updatePackage(context.Background(), quiet, selfupdate.Install{Channel: selfupdate.ChannelDeb}, latest); err == nil ||
		!strings.Contains(err.Error(), "stdout is closed") {
		t.Fatalf("got %v", err)
	}
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "gone"))
	if err := updatePackage(context.Background(), &printer{w: &strings.Builder{}}, selfupdate.Install{Channel: selfupdate.ChannelDeb}, latest); err == nil ||
		!strings.HasPrefix(err.Error(), "update: ") {
		t.Fatalf("a temporary directory that cannot be made: %v", err)
	}
}
