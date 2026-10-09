package cli

import (
	"context"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tara-vision/taraengine/internal/runtime"
	"github.com/tara-vision/taraengine/pkg/api"
	"github.com/tara-vision/taraengine/pkg/client"
)

// wantNoInstallAdvice fails the test when a message about a configured runtime_dir sends the reader to the installer,
// which works under the data directory and cannot repair a directory the configuration names.
func wantNoInstallAdvice(t *testing.T, where, message, dir string) {
	t.Helper()
	if strings.Contains(message, "taraengine runtime install") {
		t.Errorf("%s: the installer cannot fix a configured directory: %q", where, message)
	}
	if !strings.Contains(message, "runtime_dir "+dir) || !strings.Contains(message, "check runtime_dir in the config") {
		t.Errorf("%s: the message names the directory and the config key: %q", where, message)
	}
}

// doctorRuntime is what the daemon wired for the configuration reports about its runtime through the API.
func doctorRuntime(t *testing.T, cfgRuntimeDir string) api.RuntimeStatus {
	t.Helper()
	dataDir := t.TempDir() // holds no runtime of its own
	cfg := attachedConfig(dataDir)
	cfg.RuntimeDir = cfgRuntimeDir
	wired, _ := wireAttached(t, dataDir, cfg, debugLogger(io.Discard))
	srv := httptest.NewServer(wired.Handler)
	t.Cleanup(srv.Close)
	doc, err := client.New(srv.URL, "").Doctor(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return doc.Runtime
}

func TestTheAPIReportsAVerifiedRuntimeDirAsTheRuntime(t *testing.T) {
	lock, _ := runtime.LoadLock()
	dir := installRuntime(t, t.TempDir())
	st := doctorRuntime(t, dir)
	if !st.Installed || !st.Verified || st.Dir != dir || st.Build != lock.Build || st.Error != "" {
		t.Fatalf("the daemon runs from %s, so that is the runtime it reports: %+v", dir, st)
	}
}

func TestTheAPIReportsAMissingRuntimeDirWithoutSendingYouToTheInstaller(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nope")
	st := doctorRuntime(t, dir)
	if st.Installed || st.Verified || st.Dir != dir {
		t.Fatalf("%+v", st)
	}
	wantNoInstallAdvice(t, "the API", st.Error, dir)
}

func TestDoctorReportsAVerifiedRuntimeDirAsTheRuntime(t *testing.T) {
	lock, _ := runtime.LoadLock()
	dir := installRuntime(t, t.TempDir())
	cfgFile := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cfgFile, []byte("runtime_dir: "+dir+"\ndata_dir: "+t.TempDir()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := run(t, "doctor", "--host=http://127.0.0.1:1", "--config", cfgFile)
	if want := "Runtime: " + lock.Build + " verified in " + dir + "\n"; err != nil || !strings.Contains(out, want) {
		t.Fatalf("%v\nmissing %q in\n%s", err, want, out)
	}
}

func TestDoctorReportsAMissingRuntimeDirWithoutSendingYouToTheInstaller(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nope")
	cfgFile := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cfgFile, []byte("runtime_dir: "+dir+"\ndata_dir: "+t.TempDir()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := run(t, "doctor", "--host=http://127.0.0.1:1", "--config", cfgFile)
	if err != nil {
		t.Fatal(err)
	}
	line := lineWith(out, "Runtime: ")
	if !strings.HasPrefix(line, "Runtime: runtime_dir "+dir) {
		t.Fatalf("no build is known, so the line starts with the problem: %q", line)
	}
	wantNoInstallAdvice(t, "doctor", line, dir)
}

func TestConfiguredRuntimeSaysWhatIsWrongInItsOwnWords(t *testing.T) {
	lock, _ := runtime.LoadLock()
	verified := func(t *testing.T) string { return installRuntime(t, t.TempDir()) }
	for _, c := range []struct {
		name      string
		prepare   func(t *testing.T) string
		installed bool
		build     string
		problem   string // what follows the directory in the message; empty for a runtime that verifies
	}{
		{"a runtime that verifies", verified, true, lock.Build, ""},
		{"a directory that is not there", func(t *testing.T) string { return filepath.Join(t.TempDir(), "nope") }, false, "",
			"has no manifest.json"},
		{"a manifest that cannot be read", func(t *testing.T) string {
			dir := t.TempDir()
			writeText(t, filepath.Join(dir, "manifest.json"), "{not json")
			return dir
		}, false, "", "has an unreadable manifest.json (runtime: manifest.json: "},
		{"a manifest without its binary", func(t *testing.T) string {
			dir := verified(t)
			if err := os.Remove(runtime.Binary(dir)); err != nil {
				t.Fatal(err)
			}
			return dir
		}, true, lock.Build, "has a manifest.json but no llama-server"},
		{"a binary that is not the one in the manifest", func(t *testing.T) string {
			dir := verified(t)
			writeText(t, runtime.Binary(dir), "tampered")
			return dir
		}, true, lock.Build, "holds a llama-server that does not match its manifest.json"},
		{"a binary that cannot be read", func(t *testing.T) string {
			dir := verified(t)
			if err := os.Remove(runtime.Binary(dir)); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(runtime.Binary(dir), 0o750); err != nil {
				t.Fatal(err)
			}
			return dir
		}, true, lock.Build, "holds a llama-server that cannot be read (read "},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := c.prepare(t)
			st := configuredRuntime(dir)
			if st.Installed != c.installed || st.Verified != (c.problem == "") || st.Build != c.build ||
				st.Dir != dir || st.Binary != runtime.Binary(dir) {
				t.Fatalf("%+v", st)
			}
			if c.problem == "" {
				if st.Err != "" {
					t.Fatalf("a runtime that verifies has nothing to complain about: %q", st.Err)
				}
				return
			}
			if !strings.HasPrefix(st.Err, "runtime_dir "+dir+" "+c.problem) || !strings.HasSuffix(st.Err, "; check runtime_dir in the config") {
				t.Fatalf("got %q, want it to say %q about %s", st.Err, c.problem, dir)
			}
			wantNoInstallAdvice(t, c.name, st.Err, dir)
		})
	}
}

func TestRuntimeStatusFollowsTheConfiguredDirectoryAndOtherwiseThePinnedBuild(t *testing.T) {
	lock, _ := runtime.LoadLock()
	pinned := t.TempDir()
	pinnedDir := installRuntime(t, pinned)
	inst := &runtime.Installer{Dir: runtimeRoot(pinned)}
	own := installRuntime(t, t.TempDir())
	if st := runtimeStatus("", inst, lock); !st.Verified || st.Dir != pinnedDir {
		t.Fatalf("without a runtime_dir the pinned build under the data directory is the runtime: %+v", st)
	}
	if st := runtimeStatus(own, inst, lock); !st.Verified || st.Dir != own {
		t.Fatalf("a runtime_dir replaces it, whatever the data directory holds: %+v", st)
	}
	if st := runtimeStatus(filepath.Join(t.TempDir(), "nope"), inst, lock); st.Verified || st.Dir == pinnedDir {
		t.Fatalf("a runtime_dir that does not verify is not covered by a pinned build that does: %+v", st)
	}
}

func TestRuntimeStatusFollowsTheConfiguredRuntimeDirectory(t *testing.T) {
	lock, _ := runtime.LoadLock()
	pinned := t.TempDir()
	pinnedDir := installRuntime(t, pinned)
	own := installRuntime(t, t.TempDir())
	cfg := configFile(t, "runtime_dir: "+own)

	out, err := run(t, "runtime", "status", "--config", cfg, "--data-dir", t.TempDir())
	if want := "runtime " + lock.Build + " verified in " + own + "\n"; err != nil || out != want {
		t.Fatalf("a runtime_dir is the runtime, whatever the data directory holds:\n%q, want %q (%v)", out, want, err)
	}
	if out, err = run(t, "runtime", "status", "--config", cfg, "--data-dir", t.TempDir(), "--dir-only"); err != nil || out != own+"\n" {
		t.Fatalf("the directory that is printed is the configured one: %q %v", out, err)
	}

	missing := filepath.Join(t.TempDir(), "nope")
	out, err = run(t, "runtime", "status", "--config", configFile(t, "runtime_dir: "+missing), "--data-dir", pinned)
	if err != nil || strings.Contains(out, pinnedDir) || strings.Contains(out, "verified in") || strings.Contains(out, "  ") {
		t.Fatalf("a runtime_dir that does not verify is not covered by a pinned build that does: %q %v", out, err)
	}
	wantNoInstallAdvice(t, "runtime status", strings.TrimSpace(out), missing)
	if !strings.HasPrefix(out, "runtime_dir "+missing+" has no manifest.json") {
		t.Fatalf("no build is known, so the line starts with the problem: %q", out)
	}

	broken := installRuntime(t, t.TempDir())
	if err := os.Remove(runtime.Binary(broken)); err != nil {
		t.Fatal(err)
	}
	out, err = run(t, "runtime", "status", "--config", configFile(t, "runtime_dir: "+broken))
	if want := "runtime " + lock.Build + " installed in " + broken + " but NOT verified: runtime_dir " + broken +
		" has a manifest.json but no llama-server; check runtime_dir in the config\n"; err != nil || out != want {
		t.Fatalf("a directory with a manifest and no binary:\n%q, want %q (%v)", out, want, err)
	}

	out, err = run(t, "runtime", "status", "--config", configFile(t, "data_dir: "+pinned))
	if want := "runtime " + lock.Build + " verified in " + pinnedDir + "\n"; err != nil || out != want {
		t.Fatalf("without a runtime_dir the pinned build under the data directory is the runtime:\n%q, want %q (%v)", out, want, err)
	}
}
