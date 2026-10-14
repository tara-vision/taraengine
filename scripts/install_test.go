package scripts

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// fakeEngine is the binary the fake release serves: it logs every command line and answers version and service
// install the way taraengine does. FAKE_SERVICE_FAILS makes the service install fail.
const fakeEngine = `#!/bin/sh
echo "taraengine $*" >> "$LOG"
case "$1 $2" in
"version "*) echo "taraengine 0.3.1" ;;
"service install")
  if [ -n "${FAKE_SERVICE_FAILS:-}" ]; then echo "service: useradd failed" >&2; exit 1; fi
  echo "installed and started the taraengine service" ;;
esac
`

// installRun is one run of install.sh against a fake release, on a fake system.
type installRun struct {
	os, arch, uid string // what uname -s, uname -m and id -u answer
	pathFirst     string // a directory that comes before everything else on PATH
	// sealed puts on PATH, after the stand-ins, only the real tools that install.sh uses, so that no package manager of
	// the machine that runs the tests shows through
	sealed     bool
	env        []string
	release    *installRelease
	installDir string
	log        string
}

// installRelease is the release side: a release page leading to tag, and the assets of every tag it serves.
type installRelease struct {
	tag        string
	assets     map[string]string // asset name to body, for every tag
	wrongSum   bool              // checksums.txt lists a sum that does not match
	noSums     bool              // there is no checksums.txt
	noRedirect bool              // the release page leads nowhere
	apiTag     string            // what the API names; empty fails
	siteTag    string            // what the site names; empty fails
}

func (r *installRelease) serve(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		p := req.URL.Path
		switch {
		case p == "/releases/latest" && !r.noRedirect && r.tag != "":
			http.Redirect(w, req, "/releases/tag/"+r.tag, http.StatusFound)
		case strings.HasPrefix(p, "/releases/tag/"):
			_, _ = w.Write([]byte("<html>"))
		case p == "/api" && r.apiTag != "":
			_, _ = w.Write([]byte(`{"tag_name": "` + r.apiTag + `"}`))
		case p == "/site" && r.siteTag != "":
			_, _ = w.Write([]byte(`{"version":"` + r.siteTag + `","name":"taraengine"}`))
		case strings.HasPrefix(p, "/releases/download/") && strings.HasSuffix(p, "/checksums.txt") && !r.noSums:
			for name, body := range r.assets {
				sum := sha256.Sum256([]byte(body))
				hexSum := hex.EncodeToString(sum[:])
				if r.wrongSum {
					hexSum = strings.Repeat("0", 64)
				}
				_, _ = w.Write([]byte(hexSum + "  " + name + "\n"))
			}
		case strings.HasPrefix(p, "/releases/download/") && r.assets[filepath.Base(p)] != "":
			_, _ = w.Write([]byte(r.assets[filepath.Base(p)]))
		default:
			http.NotFound(w, req)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// newInstallRun is a run on Linux amd64 by a user that is not root, against a release of v0.3.1 with every binary.
func newInstallRun(t *testing.T) *installRun {
	t.Helper()
	assets := map[string]string{}
	for _, a := range []string{"taraengine-linux-amd64", "taraengine-linux-arm64", "taraengine-darwin-arm64"} {
		assets[a] = fakeEngine
	}
	return &installRun{os: "Linux", arch: "x86_64", uid: "1000", installDir: t.TempDir(),
		release: &installRelease{tag: "v0.3.1", assets: assets}, log: filepath.Join(t.TempDir(), "log")}
}

// run runs install.sh and returns its exit status, its output, and the log of the commands it ran.
func (r *installRun) run(t *testing.T) (int, string, string) {
	t.Helper()
	srv := r.release.serve(t)
	bin := t.TempDir()
	for name, body := range map[string]string{
		"uname": "#!/bin/sh\ncase \"$1\" in -s) echo \"$FAKE_OS\" ;; -m) echo \"$FAKE_ARCH\" ;; esac\n",
		"id":    "#!/bin/sh\n[ \"$1\" = -u ] && echo \"$FAKE_UID\"\n",
		"sudo":  "#!/bin/sh\necho \"sudo $*\" >> \"$LOG\"\nchmod u+w \"$INSTALL_DIR\" 2>/dev/null\n[ \"$1\" = -u ] && shift 2\nexec \"$@\"\n",
		// The loader finds libgomp unless FAKE_NO_OPENMP is set and no package manager has installed it since; the
		// listing goes on with FAKE_LIBS_AFTER more libraries, as ldconfig lists them after libgomp.
		"ldconfig": "#!/bin/sh\nif [ -z \"$FAKE_NO_OPENMP\" ] || [ -f \"$LOG.openmp\" ]; then echo \"\tlibgomp.so.1 (libc6,x86-64) => /lib/x86_64-linux-gnu/libgomp.so.1\"; fi\n" +
			"i=0\nwhile [ \"$i\" -lt \"${FAKE_LIBS_AFTER:-0}\" ]; do echo \"\tlibcublasLt.so.$i (libc6,x86-64) => /usr/local/cuda/lib64/libcublasLt.so.$i\"; i=$((i+1)); done\n",
	} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	path := bin + ":" + os.Getenv("PATH")
	if r.sealed {
		path = bin + ":" + realTools(t)
	}
	cmd := exec.Command("bash", "../install.sh")
	cmd.Env = append([]string{
		"PATH=" + strings.TrimPrefix(r.pathFirst+":"+path, ":"), "HOME=" + t.TempDir(), "LOG=" + r.log,
		"FAKE_OS=" + r.os, "FAKE_ARCH=" + r.arch, "FAKE_UID=" + r.uid, "INSTALL_DIR=" + r.installDir,
		"TARAENGINE_RELEASES=" + srv.URL + "/releases", "TARAENGINE_API=" + srv.URL + "/api",
		"TARAENGINE_SITE=" + srv.URL + "/site",
	}, r.env...)
	out, err := cmd.CombinedOutput()
	logged, _ := os.ReadFile(r.log)
	if exit, ok := err.(*exec.ExitError); ok {
		return exit.ExitCode(), string(out), string(logged)
	}
	if err != nil {
		t.Fatal(err)
	}
	return 0, string(out), string(logged)
}

func TestInstallShInstallsTheLatestReleaseAndStartsTheService(t *testing.T) {
	r := newInstallRun(t)
	r.sealed = true // no taraengine of the machine that runs the tests shows through
	code, out, log := r.run(t)
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if got, _ := os.ReadFile(filepath.Join(r.installDir, "taraengine")); string(got) != fakeEngine {
		t.Fatalf("the verified binary is installed: %q", got)
	}
	for _, want := range []string{"v0.3.1", "taraengine 0.3.1", "taraengine pull gemma4:12b",
		"taraengine is not in PATH. Add " + r.installDir + " to your PATH."} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	if !strings.Contains(log, "sudo "+filepath.Join(r.installDir, "taraengine")+" service install") {
		t.Fatalf("on Linux the system service is installed through sudo:\n%s", log)
	}
}

func TestInstallShOnAMacInstallsTheAgentAsTheUser(t *testing.T) {
	r := newInstallRun(t)
	r.os, r.arch = "Darwin", "arm64"
	code, out, log := r.run(t)
	if code != 0 || !strings.Contains(log, "taraengine service install") || strings.Contains(log, "sudo") {
		t.Fatalf("exit %d, the agent belongs to the user:\n%s\n%s", code, out, log)
	}
	r = newInstallRun(t)
	r.os, r.arch, r.uid = "Darwin", "arm64", "0"
	r.env = []string{"SUDO_USER=someone"}
	code, out, log = r.run(t)
	bin := filepath.Join(r.installDir, "taraengine")
	if code != 0 || !strings.Contains(log, "sudo -u someone "+bin+" service install") {
		t.Fatalf("exit %d, a script run under sudo installs the agent for the user who ran it:\n%s\n%s", code, out, log)
	}
}

func TestInstallShOnAMacAsRootWithoutAUserSkipsTheAgent(t *testing.T) {
	r := newInstallRun(t)
	r.os, r.arch, r.uid = "Darwin", "arm64", "0"
	code, out, log := r.run(t)
	if code != 0 || strings.Contains(log, "service install") || !strings.Contains(out, "taraengine service install") {
		t.Fatalf("exit %d, root without a user to install for says what to run:\n%s\n%s", code, out, log)
	}
}

func TestInstallShAsRootOnLinuxNeedsNoSudo(t *testing.T) {
	r := newInstallRun(t)
	r.uid = "0"
	code, out, log := r.run(t)
	if code != 0 || strings.Contains(log, "sudo") || !strings.Contains(log, "taraengine service install") {
		t.Fatalf("exit %d:\n%s\n%s", code, out, log)
	}
}

func TestInstallShWithoutTheService(t *testing.T) {
	r := newInstallRun(t)
	r.env = []string{"TARAENGINE_NO_SERVICE=1"}
	code, out, log := r.run(t)
	if code != 0 || strings.Contains(log, "service install") || !strings.Contains(out, "taraengine serve") {
		t.Fatalf("exit %d, the binary alone, and how to run it:\n%s\n%s", code, out, log)
	}
}

func TestInstallShInstallsTheVersionItIsAskedFor(t *testing.T) {
	r := newInstallRun(t)
	r.env = []string{"TARAENGINE_VERSION=v0.3.0-rc.1"}
	r.release.tag = ""
	code, out, _ := r.run(t)
	if code != 0 || !strings.Contains(out, "v0.3.0-rc.1") {
		t.Fatalf("exit %d\n%s", code, out)
	}
}

func TestInstallShRefusesWhatItCannotVerify(t *testing.T) {
	for name, change := range map[string]func(*installRelease){
		"a sum that does not match": func(r *installRelease) { r.wrongSum = true },
		"no checksums.txt":          func(r *installRelease) { r.noSums = true },
		"no sum for the binary":     func(r *installRelease) { delete(r.assets, "taraengine-linux-amd64"); r.assets["other"] = "x" },
	} {
		t.Run(name, func(t *testing.T) {
			r := newInstallRun(t)
			change(r.release)
			code, out, _ := r.run(t)
			if code == 0 || !strings.Contains(out, "Error:") {
				t.Fatalf("exit %d\n%s", code, out)
			}
			if _, err := os.Stat(filepath.Join(r.installDir, "taraengine")); err == nil {
				t.Fatal("nothing is installed")
			}
		})
	}
}

func TestInstallShRefusesASystemItHasNoBuildFor(t *testing.T) {
	for _, c := range []struct{ os, arch, want string }{
		{"Darwin", "x86_64", "Apple Silicon"}, {"FreeBSD", "amd64", "Unsupported operating system"},
		{"Linux", "riscv64", "Unsupported architecture"},
	} {
		r := newInstallRun(t)
		r.os, r.arch = c.os, c.arch
		if code, out, _ := r.run(t); code == 0 || !strings.Contains(out, c.want) {
			t.Errorf("%s/%s: exit %d\n%s", c.os, c.arch, code, out)
		}
	}
}

func TestInstallShFindsTheLatestReleaseWhereItCan(t *testing.T) {
	r := newInstallRun(t)
	r.release.noRedirect, r.release.apiTag = true, "v0.3.1"
	if code, out, _ := r.run(t); code != 0 || !strings.Contains(out, "v0.3.1") {
		t.Fatalf("the API: exit %d\n%s", code, out)
	}
	r = newInstallRun(t)
	r.release.noRedirect, r.release.siteTag = true, "0.3.1"
	if code, out, _ := r.run(t); code != 0 || !strings.Contains(out, "v0.3.1") {
		t.Fatalf("the site, whose version has no v: exit %d\n%s", code, out)
	}
	r = newInstallRun(t)
	r.release.noRedirect = true
	if code, out, _ := r.run(t); code == 0 || !strings.Contains(out, "Could not determine the version") {
		t.Fatalf("nothing names a release: exit %d\n%s", code, out)
	}
}

func TestInstallShUsesSudoForADirectoryItCannotWrite(t *testing.T) {
	r := newInstallRun(t)
	r.installDir = filepath.Join(t.TempDir(), "bin")
	if err := os.Mkdir(r.installDir, 0o500); err != nil {
		t.Fatal(err)
	}
	code, out, log := r.run(t)
	if code != 0 || !strings.Contains(log, "sudo mv ") {
		t.Fatalf("exit %d:\n%s\n%s", code, out, log)
	}
	r = newInstallRun(t)
	r.installDir = filepath.Join(t.TempDir(), "missing", "bin")
	if code, out, log := r.run(t); code != 0 || !strings.Contains(log, "sudo mkdir -p "+r.installDir) {
		t.Fatalf("a missing directory is made: exit %d:\n%s\n%s", code, out, log)
	}
}

func TestInstallShWarnsAboutAnotherTaraengineFirstOnThePath(t *testing.T) {
	r := newInstallRun(t)
	other := t.TempDir()
	if err := os.WriteFile(filepath.Join(other, "taraengine"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	r.pathFirst = other
	code, out, _ := r.run(t)
	if code != 0 || !strings.Contains(out, "Another taraengine is first in PATH: "+filepath.Join(other, "taraengine")) {
		t.Fatalf("exit %d\n%s", code, out)
	}
}

func TestInstallShSaysWhatToRetryWhenTheServiceFails(t *testing.T) {
	r := newInstallRun(t)
	r.env = []string{"FAKE_SERVICE_FAILS=1"}
	code, out, _ := r.run(t)
	if code == 0 || !strings.Contains(out, "the binary is installed") || !strings.Contains(out, "taraengine service install") {
		t.Fatalf("exit %d\n%s", code, out)
	}
}

func TestInstallShParses(t *testing.T) {
	if out, err := exec.Command("bash", "-n", "../install.sh").CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}

func TestInstallShPutsInAFileOnlyItsOwnerChangesWhateverTheUmask(t *testing.T) {
	previous := syscall.Umask(0o077) // a hardened shell's umask
	t.Cleanup(func() { syscall.Umask(previous) })
	for name, needsRoot := range map[string]bool{"a directory the user writes": false, "a directory that needs root": true} {
		t.Run(name, func(t *testing.T) {
			r := newInstallRun(t)
			r.env = append(r.env, "TARAENGINE_NO_SERVICE=1")
			if needsRoot {
				r.installDir = filepath.Join(t.TempDir(), "bin")
				if err := os.Mkdir(r.installDir, 0o500); err != nil {
					t.Fatal(err)
				}
			}
			code, out, log := r.run(t)
			bin, staged := filepath.Join(r.installDir, "taraengine"), filepath.Join(r.installDir, ".taraengine.new")
			info, err := os.Stat(bin)
			if code != 0 || err != nil || info.Mode().Perm() != 0o755 {
				t.Fatalf("exit %d, %v: the binary is 0755 whatever the umask, so the service user can run it:\n%s", code, err, out)
			}
			if _, err := os.Stat(staged); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("the copy is renamed into place: %v", err)
			}
			if needsRoot && (!strings.Contains(log, "sudo install -m 0755 ") || !strings.Contains(log, "sudo mv -f "+staged+" "+bin)) {
				t.Fatalf("root writes the copy, so root owns the file that root and the service run, and renames it into place:\n%s", log)
			}
		})
	}
}

// realTools is a directory of links to the real tools that install.sh and its stand-ins use, and nothing else.
func realTools(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, tool := range []string{"awk", "chmod", "curl", "env", "grep", "head", "install", "mkdir", "mktemp", "mv", "rm",
		"sed", "sha256sum", "shasum", "touch"} {
		if found, err := exec.LookPath(tool); err == nil {
			if err := os.Symlink(found, filepath.Join(dir, tool)); err != nil {
				t.Fatal(err)
			}
		}
	}
	return dir
}

// packageManager is a stand-in for a package manager that logs how it was called and exits with code. An install
// that it answers with 0, or with FAKE_PM_INSTALLS set, puts the library where the ldconfig stand-in finds it; an
// apt-get update exits with FAKE_UPDATE_EXIT.
func packageManager(t *testing.T, name string, code int) string {
	t.Helper()
	dir := t.TempDir()
	body := "#!/bin/sh\necho \"" + name + " $*\" >> \"$LOG\"\n" +
		"case \"$*\" in *update*) exit \"${FAKE_UPDATE_EXIT:-0}\" ;; esac\n" +
		"if [ " + strconv.Itoa(code) + " = 0 ] || [ -n \"$FAKE_PM_INSTALLS\" ]; then touch \"$LOG.openmp\"; fi\n" +
		"exit " + strconv.Itoa(code) + "\n"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// A Linux system without libgomp gets it from its package manager before anything else is installed: llama.cpp's
// Linux builds need it, and a minimal Ubuntu has none.
func TestInstallShInstallsTheOpenMPLibraryWithAptGet(t *testing.T) {
	r := newInstallRun(t)
	r.sealed, r.pathFirst = true, packageManager(t, "apt-get", 0)
	r.env = append(r.env, "FAKE_NO_OPENMP=1")
	code, out, log := r.run(t)
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if !strings.Contains(out, "Installing libgomp1, the OpenMP library that llama.cpp needs") {
		t.Errorf("the run says what it installs:\n%s", out)
	}
	update := strings.Index(log, "sudo apt-get update -qq\n")
	install := strings.Index(log, "sudo env DEBIAN_FRONTEND=noninteractive apt-get -o DPkg::Lock::Timeout=120 install -y -qq libgomp1\n")
	binary := strings.Index(log, "taraengine version")
	if update < 0 || install < update || binary < install {
		t.Fatalf("apt-get updates, installs libgomp1, and only then the binary comes:\n%s", log)
	}
}

func TestInstallShInstallsTheOpenMPLibraryWithDnf(t *testing.T) {
	r := newInstallRun(t)
	r.sealed, r.pathFirst = true, packageManager(t, "dnf", 0)
	r.env = append(r.env, "FAKE_NO_OPENMP=1")
	code, out, log := r.run(t)
	if code != 0 || !strings.Contains(log, "sudo dnf install -y -q libgomp\n") || !strings.Contains(out, "Installing libgomp,") {
		t.Fatalf("exit %d\n%s\n%s", code, out, log)
	}
}

func TestInstallShInstallsTheOpenMPLibraryWithZypper(t *testing.T) {
	r := newInstallRun(t)
	r.sealed, r.pathFirst = true, packageManager(t, "zypper", 0)
	r.env = append(r.env, "FAKE_NO_OPENMP=1")
	code, out, log := r.run(t)
	if code != 0 || !strings.Contains(log, "sudo zypper --non-interactive install libgomp1\n") {
		t.Fatalf("exit %d\n%s\n%s", code, out, log)
	}
}

func TestInstallShStopsWhenTheOpenMPLibraryCannotBeInstalled(t *testing.T) {
	r := newInstallRun(t)
	r.sealed, r.pathFirst = true, packageManager(t, "dnf", 1)
	r.env = append(r.env, "FAKE_NO_OPENMP=1")
	code, out, log := r.run(t)
	if code != 1 || !strings.Contains(out, "Could not install libgomp, the OpenMP library that llama.cpp needs") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if strings.Contains(log, "taraengine ") {
		t.Fatalf("nothing is installed when the library cannot be:\n%s", log)
	}
}

func TestInstallShStopsWithoutAPackageManagerItKnows(t *testing.T) {
	r := newInstallRun(t)
	r.sealed = true
	r.env = append(r.env, "FAKE_NO_OPENMP=1")
	code, out, log := r.run(t)
	if code != 1 || !strings.Contains(out, "taraengine needs libgomp.so.1, the OpenMP library of GCC") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if strings.Contains(log, "taraengine ") {
		t.Fatalf("nothing is installed without the library:\n%s", log)
	}
}

func TestInstallShLeavesTheOpenMPLibraryToTheMacAndToASystemThatHasIt(t *testing.T) {
	for name, r := range map[string]*installRun{"linux with libgomp": newInstallRun(t), "macos": newInstallRun(t)} {
		r.sealed, r.pathFirst = true, packageManager(t, "apt-get", 0)
		if name == "macos" {
			r.os, r.arch = "Darwin", "arm64"
			r.env = append(r.env, "FAKE_NO_OPENMP=1")
		}
		code, out, log := r.run(t)
		if code != 0 || strings.Contains(log, "apt-get") {
			t.Fatalf("%s: exit %d, no package manager called\n%s\n%s", name, code, out, log)
		}
	}
}

// A release before v0.3.0 has no service commands, so the script refuses it unless only the binary is asked for.
func TestInstallShRefusesAReleaseWithoutTheServiceCommands(t *testing.T) {
	for _, env := range [][]string{{"TARAENGINE_VERSION=v0.2.0"}, nil} {
		r := newInstallRun(t)
		r.env = append(r.env, env...)
		if env == nil {
			r.release.tag = "v0.2.0" // the latest release is the older one
		}
		code, out, log := r.run(t)
		if code != 1 || !strings.Contains(out, "taraengine v0.2.0 has no service commands, which came with v0.3.0: set TARAENGINE_VERSION to v0.3.0 or later") {
			t.Fatalf("%v: exit %d\n%s", env, code, out)
		}
		if strings.Contains(log, "taraengine ") {
			t.Fatalf("%v: nothing is installed:\n%s", env, log)
		}
	}
	r := newInstallRun(t)
	r.env = append(r.env, "TARAENGINE_VERSION=v0.2.0", "TARAENGINE_NO_SERVICE=1")
	if code, out, _ := r.run(t); code != 0 {
		t.Fatalf("the binary alone may be any release: exit %d\n%s", code, out)
	}
}

// The Linux unit hides /home, /root and /run/user from the service, so a binary there could never start.
func TestInstallShRefusesADirectoryTheLinuxServiceCannotReach(t *testing.T) {
	for _, dir := range []string{"/home/someone/.local/bin", "/root/bin", "/run/user/1000/bin"} {
		r := newInstallRun(t)
		r.installDir = dir
		code, out, log := r.run(t)
		if code != 1 || !strings.Contains(out, "the service could not run "+dir+"/taraengine") {
			t.Fatalf("%s: exit %d\n%s", dir, code, out)
		}
		if log != "" {
			t.Fatalf("%s: nothing runs:\n%s", dir, log)
		}
	}
}

// ldconfig lists libgomp before the many libraries named after it, and the check reads the whole listing: a reader
// that stopped at the match would end ldconfig with SIGPIPE, which pipefail takes for a missing library.
func TestInstallShFindsTheOpenMPLibraryInALongListing(t *testing.T) {
	r := newInstallRun(t)
	r.sealed, r.pathFirst = true, packageManager(t, "apt-get", 0)
	r.env = append(r.env, "FAKE_LIBS_AFTER=2000") // about 150 KB after the line, past a pipe's buffer
	code, out, log := r.run(t)
	if code != 0 || strings.Contains(log, "apt-get") {
		t.Fatalf("a listed libgomp needs no package manager: exit %d\n%s\n%s", code, out, log)
	}
}

// A package manager that installed the library and still failed, as zypper does when another repository cannot be
// refreshed, lets the installation go on.
func TestInstallShGoesOnWhenThePackageManagerFailsAfterInstallingTheLibrary(t *testing.T) {
	for name, code := range map[string]int{"apt-get": 100, "dnf": 1, "zypper": 106} {
		r := newInstallRun(t)
		r.sealed, r.pathFirst = true, packageManager(t, name, code)
		r.env = append(r.env, "FAKE_NO_OPENMP=1", "FAKE_PM_INSTALLS=1")
		if exit, out, log := r.run(t); exit != 0 || !strings.Contains(log, name+" ") {
			t.Fatalf("%s %d: exit %d\n%s\n%s", name, code, exit, out, log)
		}
	}
}

func TestInstallShGoesOnWhenTheAptGetUpdateFails(t *testing.T) {
	r := newInstallRun(t)
	r.sealed, r.pathFirst = true, packageManager(t, "apt-get", 0)
	r.env = append(r.env, "FAKE_NO_OPENMP=1", "FAKE_UPDATE_EXIT=100")
	if code, out, log := r.run(t); code != 0 || !strings.Contains(log, "install -y -qq libgomp1") {
		t.Fatalf("exit %d\n%s\n%s", code, out, log)
	}
}

func TestInstallShAsRootInstallsTheOpenMPLibraryWithoutSudo(t *testing.T) {
	r := newInstallRun(t)
	r.sealed, r.pathFirst, r.uid = true, packageManager(t, "apt-get", 0), "0"
	r.env = append(r.env, "FAKE_NO_OPENMP=1")
	code, out, log := r.run(t)
	if code != 0 || !strings.Contains(log, "apt-get -o DPkg::Lock::Timeout=120 install -y -qq libgomp1") || strings.Contains(log, "sudo") {
		t.Fatalf("root runs the package manager itself: exit %d\n%s\n%s", code, out, log)
	}
}

// Every tag from v0.3.0 on installs with the service; a tag before it, with or without its v, does not.
func TestInstallShReadsTheTagsAroundTheServiceFloor(t *testing.T) {
	for tag, refused := range map[string]bool{"v0.3.0-rc.1": false, "v0.10.0": false, "v1.0.0": false, "0.2.0": true, "v0.2.9-rc.1": true} {
		r := newInstallRun(t)
		r.env = append(r.env, "TARAENGINE_VERSION="+tag)
		code, out, _ := r.run(t)
		if refused != (code == 1 && strings.Contains(out, "has no service commands")) || (!refused && code != 0) {
			t.Errorf("%s: exit %d\n%s", tag, code, out)
		}
	}
}
