package scripts

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
	env           []string
	release       *installRelease
	installDir    string
	log           string
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
	} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("bash", "../install.sh")
	cmd.Env = append([]string{
		"PATH=" + strings.TrimPrefix(r.pathFirst+":"+bin+":"+os.Getenv("PATH"), ":"), "HOME=" + t.TempDir(), "LOG=" + r.log,
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
