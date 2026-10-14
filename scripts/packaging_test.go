package scripts

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// maintainerScript runs packaging/<name> with the arguments dpkg or rpm would give, with a fake taraengine that logs
// its command lines and a fake rm that logs instead of removing. FAKE_ENGINE_FAILS makes the engine fail.
func maintainerScript(t *testing.T, name string, env []string, args ...string) (int, string) {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "log")
	for file, body := range map[string]string{
		"taraengine": "#!/bin/sh\necho \"taraengine $*\" >> \"$LOG\"\n[ -z \"${FAKE_ENGINE_FAILS:-}\" ]\n",
		"rm":         "#!/bin/sh\necho \"rm $*\" >> \"$LOG\"\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, file), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("sh", append([]string{filepath.Join("..", "packaging", name)}, args...)...)
	cmd.Env = append([]string{"PATH=" + dir + ":" + os.Getenv("PATH"), "LOG=" + log,
		"TARAENGINE_BIN=" + filepath.Join(dir, "taraengine")}, env...)
	out, err := cmd.CombinedOutput()
	logged, _ := os.ReadFile(log)
	code := 0
	if exit, ok := err.(*exec.ExitError); ok {
		code = exit.ExitCode()
	} else if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	return code, strings.TrimSpace(string(logged))
}

func TestPostinstallInstallsTheServiceOnAnInstallAndAnUpgrade(t *testing.T) {
	for _, args := range [][]string{{"configure"}, {"configure", "0.2.0"}, {"1"}, {"2"}} {
		if code, log := maintainerScript(t, "postinstall.sh", nil, args...); code != 0 || log != "taraengine service install" {
			t.Errorf("%v: exit %d, %q", args, code, log)
		}
	}
	for _, args := range [][]string{{"abort-upgrade", "0.2.0"}, {"abort-remove"}, {}} {
		if code, log := maintainerScript(t, "postinstall.sh", nil, args...); code != 0 || log != "" {
			t.Errorf("%v: exit %d, %q", args, code, log)
		}
	}
	if code, _ := maintainerScript(t, "postinstall.sh", []string{"FAKE_ENGINE_FAILS=1"}, "configure"); code == 0 {
		t.Error("a service that cannot be installed fails the configuration, so the admin sees it")
	}
}

func TestPreremoveUninstallsTheServiceOnARemovalOnly(t *testing.T) {
	for _, args := range [][]string{{"remove"}, {"0"}} {
		if code, log := maintainerScript(t, "preremove.sh", nil, args...); code != 0 || log != "taraengine service uninstall" {
			t.Errorf("%v: exit %d, %q", args, code, log)
		}
	}
	for _, args := range [][]string{{"upgrade", "0.3.1"}, {"1"}, {"deconfigure"}} {
		if code, log := maintainerScript(t, "preremove.sh", nil, args...); code != 0 || log != "" {
			t.Errorf("an upgrade keeps the service: %v: exit %d, %q", args, code, log)
		}
	}
	if code, _ := maintainerScript(t, "preremove.sh", []string{"FAKE_ENGINE_FAILS=1"}, "remove"); code != 0 {
		t.Error("a service that is already gone does not stop the removal")
	}
}

func TestPostremoveClearsTheDataOnADebianPurgeOnly(t *testing.T) {
	if code, log := maintainerScript(t, "postremove.sh", nil, "purge"); code != 0 || log != "rm -rf /etc/taraengine /var/lib/taraengine" {
		t.Fatalf("exit %d, %q", code, log)
	}
	for _, args := range [][]string{{"remove"}, {"0"}, {"1"}, {"upgrade"}} {
		if code, log := maintainerScript(t, "postremove.sh", nil, args...); code != 0 || log != "" {
			t.Errorf("%v: exit %d, %q", args, code, log)
		}
	}
}

// The packages depend on the OpenMP library that llama.cpp's Linux builds link, which a minimal Ubuntu lacks, so
// apt and dnf install it with taraengine.
func TestThePackagesDependOnTheOpenMPLibrary(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", ".goreleaser.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Nfpms []struct {
			Overrides map[string]struct {
				Dependencies []string `yaml:"dependencies"`
			} `yaml:"overrides"`
		} `yaml:"nfpms"`
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Nfpms) != 1 {
		t.Fatalf("want one package definition, got %d", len(cfg.Nfpms))
	}
	for format, want := range map[string]string{"deb": "libgomp1", "rpm": "libgomp"} {
		if got := cfg.Nfpms[0].Overrides[format].Dependencies; !slices.Contains(got, want) {
			t.Errorf("the %s depends on %v, want %s among them", format, got, want)
		}
	}
}
