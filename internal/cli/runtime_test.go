package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/tara-vision/taraengine/internal/runtime"
)

func fakeRuntimeTarball(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range []struct{ name, body string }{{"llama-bX/llama-server", "#!/bin/sh\n"}, {"llama-bX/LICENSE", "MIT"}} {
		if err := tw.WriteHeader(&tar.Header{Name: e.name, Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(e.body))}); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write([]byte(e.body))
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

// release serves the fake tarball where the installer looks for the pinned build. A chunked answer carries no
// length, so the installer cannot show progress for it.
func release(t *testing.T, lock runtime.Lock, tgz []byte, chunked bool) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/"+lock.Build+"/llama-"+lock.Build+"-bin-") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if chunked {
			_, _ = w.Write(tgz[:len(tgz)/2])
			w.(http.Flusher).Flush()
			_, _ = w.Write(tgz[len(tgz)/2:])
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(tgz)))
		_, _ = w.Write(tgz)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// lockFor writes a lock that pins the fake tarball for every platform of the embedded lock.
func lockFor(t *testing.T, lock runtime.Lock, tgz []byte) string {
	t.Helper()
	sum := sha256.Sum256(tgz)
	path := filepath.Join(t.TempDir(), "lock.yaml")
	body := "build: " + lock.Build + "\nassets:\n"
	for key, a := range lock.Assets {
		body += "  " + key + ":\n    asset: " + a.Asset + "\n    sha256: " + hex.EncodeToString(sum[:]) + "\n"
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRuntimeStatusInstallUpdate(t *testing.T) {
	tgz := fakeRuntimeTarball(t)
	lock, err := runtime.LoadLock()
	if err != nil {
		t.Fatal(err)
	}
	base, lockPath := release(t, lock, tgz, false), lockFor(t, lock, tgz)
	dataDir := t.TempDir()
	out, err := run(t, "runtime", "status", "--data-dir", dataDir)
	if err != nil || !strings.Contains(out, "not installed") {
		t.Fatalf("status before: %v\n%s", err, out)
	}
	out, err = run(t, "runtime", "install", "--data-dir", dataDir, "--release-base", base, "--lock", lockPath, "--no-cudart")
	if err != nil || !strings.Contains(out, "installed "+lock.Build) {
		t.Fatalf("install: %v\n%s", err, out)
	}
	out, err = run(t, "runtime", "status", "--data-dir", dataDir)
	if err != nil || !strings.Contains(out, "verified") {
		t.Fatalf("status after: %v\n%s", err, out)
	}
	out, err = run(t, "runtime", "status", "--data-dir", dataDir, "--dir-only")
	if err != nil || strings.TrimSpace(out) != filepath.Join(dataDir, "runtime", lock.Build) {
		t.Fatalf("dir-only: %v\n%s", err, out)
	}
	out, err = run(t, "runtime", "update", "--data-dir", dataDir, "--release-base", base, "--lock", lockPath, "--no-cudart")
	if err != nil || !strings.Contains(out, "already at "+lock.Build) {
		t.Fatalf("update: %v\n%s", err, out)
	}
}

func TestRuntimeStatusOfAnEmptyDataDirSuggestsTheInstall(t *testing.T) {
	lock, _ := runtime.LoadLock()
	dataDir := t.TempDir()
	out, err := run(t, "runtime", "status", "--data-dir", dataDir)
	want := "runtime " + lock.Build + " not installed, run `taraengine runtime install`\n"
	if err != nil || out != want {
		t.Fatalf("%v\n%q, want %q", err, out, want)
	}
	out, err = run(t, "runtime", "status", "--data-dir", dataDir, "--dir-only")
	if err != nil || strings.TrimSpace(out) != filepath.Join(dataDir, "runtime", lock.Build) {
		t.Fatalf("the directory is printed even before the install: %v %q", err, out)
	}
}

func TestRuntimeInstallShowsProgressAndWhereItWent(t *testing.T) {
	tgz := fakeRuntimeTarball(t)
	lock, _ := runtime.LoadLock()
	dataDir := t.TempDir()
	out, err := run(t, "runtime", "install", "--data-dir", dataDir, "--release-base", release(t, lock, tgz, false),
		"--lock", lockFor(t, lock, tgz), "--no-cudart")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 || lines[0] != "downloading 0.0 MB / 0.0 MB" {
		t.Fatalf("one progress line for a small file, then the result:\n%s", out)
	}
	want := " in " + filepath.Join(dataDir, "runtime", lock.Build)
	if !strings.HasPrefix(lines[1], "installed "+lock.Build+" for ") || !strings.HasSuffix(lines[1], want) {
		t.Fatalf("result line %q, want it to end with %q", lines[1], want)
	}
}

func TestRuntimeInstallWithoutAKnownLengthShowsNoProgress(t *testing.T) {
	tgz := fakeRuntimeTarball(t)
	lock, _ := runtime.LoadLock()
	out, err := run(t, "runtime", "install", "--data-dir", t.TempDir(), "--release-base", release(t, lock, tgz, true),
		"--lock", lockFor(t, lock, tgz), "--no-cudart")
	if err != nil || strings.Contains(out, "downloading") || !strings.Contains(out, "installed "+lock.Build) {
		t.Fatalf("%v\n%s", err, out)
	}
}

func TestRuntimeUpdateInstallsWhatIsMissing(t *testing.T) {
	tgz := fakeRuntimeTarball(t)
	lock, _ := runtime.LoadLock()
	flags := []string{"--data-dir", t.TempDir(), "--release-base", release(t, lock, tgz, false), "--lock", lockFor(t, lock, tgz), "--no-cudart"}
	out, err := run(t, append([]string{"runtime", "update"}, flags...)...)
	if err != nil || !strings.Contains(out, "installed "+lock.Build) {
		t.Fatalf("%v\n%s", err, out)
	}
	if out, err = run(t, append([]string{"runtime", "update"}, flags...)...); err != nil || !strings.Contains(out, "already at "+lock.Build) {
		t.Fatalf("%v\n%s", err, out)
	}
}

func TestRuntimeStatusSpotsATamperedBinary(t *testing.T) {
	tgz := fakeRuntimeTarball(t)
	lock, _ := runtime.LoadLock()
	dataDir := t.TempDir()
	flags := []string{"--data-dir", dataDir, "--release-base", release(t, lock, tgz, false), "--lock", lockFor(t, lock, tgz), "--no-cudart"}
	if _, err := run(t, append([]string{"runtime", "install"}, flags...)...); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(dataDir, "runtime", lock.Build)
	if err := os.WriteFile(runtime.Binary(dir), []byte("tampered"), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := run(t, "runtime", "status", "--data-dir", dataDir)
	if err != nil || !strings.Contains(out, "installed in "+dir+" but NOT verified: ") || !strings.Contains(out, "does not match the manifest") {
		t.Fatalf("%v\n%s", err, out)
	}
}

func TestRuntimeInstallReportsWhatWentWrong(t *testing.T) {
	tgz := fakeRuntimeTarball(t)
	lock, _ := runtime.LoadLock()
	base := release(t, lock, tgz, false)
	good := lockFor(t, lock, tgz)
	missing := filepath.Join(t.TempDir(), "missing.yaml")
	garbage := filepath.Join(t.TempDir(), "garbage.yaml")
	if err := os.WriteFile(garbage, []byte("build: not-a-build\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	other := lockFor(t, lock, []byte("not the tarball"))
	for _, c := range []struct {
		name string
		args []string
		want string
	}{
		{"a mirror without the asset", []string{"--release-base", base + "/nowhere", "--lock", good}, "GitHub answered 404"},
		{"a tarball that is not the pinned one", []string{"--release-base", base, "--lock", other}, "checksum mismatch"},
		{"a lock file that is not there", []string{"--release-base", base, "--lock", missing}, "missing.yaml"},
		{"a lock file that is no lock", []string{"--release-base", base, "--lock", garbage}, "not-a-build"},
		{"a config file that is not there", []string{"--release-base", base, "--lock", good, "--config", missing}, "missing.yaml"},
	} {
		t.Run(c.name, func(t *testing.T) {
			dataDir := t.TempDir()
			args := append([]string{"runtime", "install", "--data-dir", dataDir, "--no-cudart"}, c.args...)
			if _, err := run(t, args...); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v, want it to mention %q", err, c.want)
			}
			if _, err := os.Stat(filepath.Join(dataDir, "runtime", lock.Build)); err == nil {
				t.Fatal("a failed install leaves no install behind")
			}
		})
	}
}

func TestRuntimeCommandsTakeTheDataDirectoryFromTheConfig(t *testing.T) {
	lock, _ := runtime.LoadLock()
	fromConfig, fromFlag := t.TempDir(), t.TempDir()
	cfgFile := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cfgFile, []byte("data_dir: "+fromConfig+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := run(t, "runtime", "status", "--dir-only", "--config", cfgFile)
	if err != nil || strings.TrimSpace(out) != filepath.Join(fromConfig, "runtime", lock.Build) {
		t.Fatalf("config: %v %q", err, out)
	}
	out, err = run(t, "runtime", "status", "--dir-only", "--config", cfgFile, "--data-dir", fromFlag)
	if err != nil || strings.TrimSpace(out) != filepath.Join(fromFlag, "runtime", lock.Build) {
		t.Fatalf("the flag wins: %v %q", err, out)
	}
}

func TestRuntimeAsksForTheCudaBundleOnlyWhenTheMachineLacksIt(t *testing.T) {
	has := func(major int) bool { return major == 12 }
	for _, c := range []struct {
		name  string
		flags runtimeFlags
		major int
		want  bool
	}{
		{"the flag forces it", runtimeFlags{cudart: true}, 0, true},
		{"the flag forces it even where the machine has CUDA", runtimeFlags{cudart: true}, 12, true},
		{"a machine without CUDA 13 needs it", runtimeFlags{}, 13, true},
		{"a machine with CUDA 12 does not", runtimeFlags{}, 12, false},
		{"a build without CUDA needs none", runtimeFlags{}, 0, false},
		{"the other flag refuses it", runtimeFlags{noCudart: true}, 13, false},
	} {
		if got := c.flags.wantsCudart(c.major, has); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestRuntimeCudartFlagsExcludeEachOther(t *testing.T) {
	_, err := run(t, "runtime", "install", "--data-dir", t.TempDir(), "--cudart", "--no-cudart")
	if err == nil || !strings.Contains(err.Error(), "cudart") {
		t.Fatalf("%v", err)
	}
}

func TestRuntimeMirrorAndLockFlagsStayOutOfTheHelp(t *testing.T) {
	root := NewRootCommand()
	cmd, _, err := root.Find([]string{"runtime", "install"})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"release-base", "lock"} {
		if f := cmd.InheritedFlags().Lookup(name); f == nil || !f.Hidden {
			t.Errorf("--%s must exist and be hidden: %v", name, f)
		}
	}
	if f := cmd.InheritedFlags().Lookup("data-dir"); f == nil || f.Hidden {
		t.Errorf("--data-dir is a public flag: %v", f)
	}
}

func TestRuntimeStatusReportsAConfigOrLockThatCannotBeRead(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.yaml")
	for _, args := range [][]string{
		{"runtime", "status", "--config", missing},
		{"runtime", "status", "--data-dir", t.TempDir(), "--lock", missing},
	} {
		if _, err := run(t, args...); err == nil || !strings.Contains(err.Error(), "missing.yaml") {
			t.Errorf("%v: %v", args, err)
		}
	}
}

func TestRuntimeCommandsSayWhenTheEmbeddedLockIsBroken(t *testing.T) {
	previous := pinnedLock
	pinnedLock = func() (runtime.Lock, error) { return runtime.Lock{}, errors.New("lock is broken") }
	t.Cleanup(func() { pinnedLock = previous })
	for _, args := range [][]string{{"runtime", "status"}, {"runtime", "install"}, {"runtime", "update"}} {
		if _, err := run(t, append(args, "--data-dir", t.TempDir())...); err == nil || err.Error() != "lock is broken" {
			t.Errorf("%v: %v", args, err)
		}
	}
}

func withPlatform(t *testing.T, p runtime.Platform, err error) {
	t.Helper()
	previous := hostPlatform
	hostPlatform = func(context.Context) (runtime.Platform, error) { return p, err }
	t.Cleanup(func() { hostPlatform = previous })
}

func TestRuntimeInstallRefusesAMachineThatHasNoBuild(t *testing.T) {
	withPlatform(t, runtime.Platform{}, errors.New("runtime: plan9/386 is not supported in this release"))
	tgz := fakeRuntimeTarball(t)
	lock, _ := runtime.LoadLock()
	_, err := run(t, "runtime", "install", "--data-dir", t.TempDir(), "--release-base", release(t, lock, tgz, false), "--lock", lockFor(t, lock, tgz))
	if err == nil || !strings.Contains(err.Error(), "not supported in this release") {
		t.Fatalf("%v", err)
	}
}

func TestRuntimeInstallOnACudaMachineChecksForTheCudaRuntime(t *testing.T) {
	withPlatform(t, runtime.Platform{OS: "linux", Arch: "amd64", Accel: "cuda12"}, nil)
	tgz := fakeRuntimeTarball(t)
	lock, _ := runtime.LoadLock()
	out, err := run(t, "runtime", "install", "--data-dir", t.TempDir(), "--release-base", release(t, lock, tgz, false), "--lock", lockFor(t, lock, tgz))
	if err != nil || !strings.Contains(out, "installed "+lock.Build+" for linux/amd64/cuda12 in ") {
		t.Fatalf("%v\n%s", err, out)
	}
}
