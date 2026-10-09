package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"

	"github.com/tara-vision/taraengine/internal/api"
	"github.com/tara-vision/taraengine/internal/backend"
	"github.com/tara-vision/taraengine/internal/machine"
	"github.com/tara-vision/taraengine/internal/pull"
	"github.com/tara-vision/taraengine/internal/runtime"
)

// installRuntime lays down what `runtime install` leaves behind for the pinned build: a binary and a manifest that
// matches it. The binary is a stub, nothing runs it.
func installRuntime(t *testing.T, dataDir string) string {
	t.Helper()
	lock, err := runtime.LoadLock()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(dataDir, "runtime", lock.Build)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(runtime.Binary(dir), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	sum, err := pull.SHA256File(runtime.Binary(dir))
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := json.Marshal(runtime.Manifest{Build: lock.Build, Key: "test", BinarySHA256: sum})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), manifest, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// withMachine makes doctor see a host other than the one the tests run on.
func withMachine(t *testing.T, info machine.Info) {
	t.Helper()
	previous := detectMachine
	detectMachine = func(context.Context) machine.Info { return info }
	t.Cleanup(func() { detectMachine = previous })
}

func TestDoctorDescribesTheHost(t *testing.T) {
	for _, c := range []struct {
		name string
		info machine.Info
		want string
	}{
		{"a Mac with unified memory", machine.Info{OS: "darwin", Arch: "arm64", GPU: "Apple M4 Max", GPUMemoryBytes: 128 << 30, UnifiedMemory: true, RAMBytes: 128 << 30, Tier: "48"},
			"Machine: darwin/arm64, Apple M4 Max, 128.0 GiB RAM, tier 48\n"},
		{"a Linux host with an NVIDIA card", machine.Info{OS: "linux", Arch: "amd64", GPU: "NVIDIA GeForce RTX 5090", GPUMemoryBytes: 32 << 30, RAMBytes: 48 << 30, Tier: "48"},
			"Machine: linux/amd64, NVIDIA GeForce RTX 5090 (32.0 GiB), 48.0 GiB RAM, tier 48\n"},
		{"a Linux host without a card", machine.Info{OS: "linux", Arch: "arm64", RAMBytes: 16 << 30, Tier: "16"},
			"Machine: linux/arm64, no NVIDIA GPU found, 16.0 GiB RAM, tier 16\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			withMachine(t, c.info)
			out, err := run(t, "doctor", "--host=http://127.0.0.1:1", "--data-dir", t.TempDir())
			if err != nil || !strings.HasPrefix(out, c.want) {
				t.Fatalf("%v\n%s\nwant it to start with\n%s", err, out, c.want)
			}
		})
	}
}

func TestDoctorReadsTheRealHostByDefault(t *testing.T) {
	out, err := run(t, "doctor", "--host=http://127.0.0.1:1", "--data-dir", t.TempDir())
	line := lineWith(out, "Machine: ")
	if err != nil || !strings.Contains(line, goruntime.GOOS+"/"+goruntime.GOARCH+", ") || !strings.Contains(line, " RAM, tier ") {
		t.Fatalf("%v\n%s", err, out)
	}
}

func TestDoctorShowsAVerifiedRuntimeAndTheDaemon(t *testing.T) {
	d := startDaemon(t, nil)
	dataDir := t.TempDir()
	dir := installRuntime(t, dataDir)
	lock, _ := runtime.LoadLock()
	out, err := run(t, "doctor", "--host="+d.srv.URL, "--data-dir", dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if want := "Runtime: " + lock.Build + " verified in " + dir + "\n"; !strings.Contains(out, want) {
		t.Fatalf("missing %q in\n%s", want, out)
	}
	if want := "Daemon: taraengine 0.1.0-test on linux/amd64/cuda12, runtime bTEST, at " + d.srv.URL + "\n"; !strings.Contains(out, want) {
		t.Fatalf("missing %q in\n%s", want, out)
	}
}

func TestDoctorListsWhatIsLoadedAndRecommendsForThisTier(t *testing.T) {
	withMachine(t, machine.Info{OS: "linux", Arch: "amd64", RAMBytes: 48 << 30, Tier: "48"})
	d := startDaemon(t, nil)
	d.install(t, "gemma4:12b")
	d.fake.SetModels(backend.ModelStatus{Name: "gemma4:12b", Status: backend.StatusLoaded, Context: 8192})
	out, err := run(t, "doctor", "--host="+d.srv.URL, "--data-dir", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Loaded: gemma4:12b at 8192 tokens\n", "Recommended for this tier: qwen3.6:35b\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
}

func TestDoctorSaysWhenTheDaemonRefusesTheKey(t *testing.T) {
	d := startDaemon(t, func(deps *api.Deps) { deps.Config.APIKey = "sesame" })
	out, err := run(t, "doctor", "--host="+d.srv.URL, "--data-dir", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	daemonLine := lineWith(out, "Daemon: ")
	if want := "Daemon: at " + d.srv.URL + ", but it did not answer as expected: missing or wrong API key " +
		"(" + keyHint + ")"; daemonLine != want {
		t.Fatalf("a daemon that refuses the key is running, and the hint is the CLI's own:\n%s\nwant %q", out, want)
	}
	out, err = run(t, "doctor", "--host="+d.srv.URL, "--data-dir", t.TempDir(), "--api-key", "sesame")
	if err != nil || !strings.Contains(out, "Daemon: taraengine 0.1.0-test") {
		t.Fatalf("with the key: %v\n%s", err, out)
	}
}

func TestDoctorSaysWhenTheRuntimeIsTooOldForAModel(t *testing.T) {
	dataDir := t.TempDir()
	override := "entries:\n  - name: newest\n    hf_repo: org/newest\n    file: newest.gguf\n    sha256: " +
		strings.Repeat("a", 64) + "\n    size: 10\n    tier: small\n    min_runtime: b99999\n"
	if err := os.WriteFile(filepath.Join(dataDir, "registry.yaml"), []byte(override), 0o600); err != nil {
		t.Fatal(err)
	}
	lock, _ := runtime.LoadLock()
	out, err := run(t, "doctor", "--host=http://127.0.0.1:1", "--data-dir", dataDir)
	want := "Model newest needs runtime b99999 or newer; this taraengine pins " + lock.Build + "\n"
	if err != nil || !strings.Contains(out, want) {
		t.Fatalf("%v\nmissing %q in\n%s", err, want, out)
	}
}

func TestDoctorReportsABrokenRegistryOverride(t *testing.T) {
	dataDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dataDir, "registry.yaml"), []byte("entries: [nonsense"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := run(t, "doctor", "--host=http://127.0.0.1:1", "--data-dir", dataDir)
	if err != nil || !strings.Contains(lineWith(out, "Registry: "), "registry: parse") {
		t.Fatalf("%v\n%s", err, out)
	}
}

func TestDoctorShowsTheEndOfTheLogOfAModelThatFailedOrIsFaulty(t *testing.T) {
	d := startDaemon(t, nil)
	d.install(t, "gemma4:12b")
	d.install(t, "glm-4.7-flash")
	d.install(t, "gemma4:e4b")
	d.install(t, "gemma4:26b")
	d.fake.SetModels(
		backend.ModelStatus{Name: "gemma4:e4b", Status: backend.StatusFailed},
		backend.ModelStatus{
			Name: "gemma4:12b", Status: backend.StatusFailed, Failed: true, ExitCode: 137,
			LogTail: "loading tensors\nCUDA out of memory",
		},
		backend.ModelStatus{Name: "gemma4:26b", Status: backend.StatusFaulty},
		backend.ModelStatus{Name: "glm-4.7-flash", Status: backend.StatusFaulty, Failed: true, ExitCode: 139, LogTail: "segfault"},
	)
	out, err := run(t, "doctor", "--host="+d.srv.URL, "--data-dir", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	want := "Model gemma4:e4b: failed\nModel gemma4:12b: failed, exit code 137\n  loading tensors\n  CUDA out of memory\n" +
		"Model gemma4:26b: faulty after two failed loads, retry with `taraengine load gemma4:26b --force`\n" +
		"Model glm-4.7-flash: faulty after two failed loads, exit code 139, retry with `taraengine load glm-4.7-flash --force`\n  segfault\n"
	if !strings.Contains(out, want) {
		t.Fatalf("missing\n%s\nin\n%s", want, out)
	}
}

func TestDoctorStaysQuietAboutHealthyModels(t *testing.T) {
	d := startDaemon(t, nil)
	d.install(t, "gemma4:12b")
	d.fake.SetModels(backend.ModelStatus{Name: "gemma4:12b", Status: backend.StatusLoaded, Context: 8192})
	out, err := run(t, "doctor", "--host="+d.srv.URL, "--data-dir", t.TempDir())
	if err != nil || strings.Contains(out, "Model gemma4:12b") {
		t.Fatalf("%v\n%s", err, out)
	}
}

func TestDoctorTakesTheDataDirectoryFromTheConfigFile(t *testing.T) {
	dataDir := t.TempDir()
	dir := installRuntime(t, dataDir)
	cfgFile := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cfgFile, []byte("data_dir: "+dataDir+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := run(t, "doctor", "--host=http://127.0.0.1:1", "--config", cfgFile)
	if err != nil || !strings.Contains(out, "verified in "+dir) {
		t.Fatalf("%v\n%s", err, out)
	}
}

func TestDoctorRefusesAConfigFileThatIsNotThere(t *testing.T) {
	out, err := run(t, "doctor", "--host=http://127.0.0.1:1", "--config", filepath.Join(t.TempDir(), "missing.yaml"))
	if err == nil || !strings.Contains(err.Error(), "missing.yaml") {
		t.Fatalf("%v", err)
	}
	if !strings.Contains(out, "Machine: ") {
		t.Fatalf("what the host knows is still shown:\n%s", out)
	}
}

func TestDoctorSurvivesABackendThatCannotListItsModels(t *testing.T) {
	d := startDaemon(t, nil)
	d.fake.Fail("models", errors.New("the router does not answer"))
	out, err := run(t, "doctor", "--host="+d.srv.URL, "--data-dir", t.TempDir())
	if err != nil || !strings.Contains(out, "Daemon: taraengine 0.1.0-test") {
		t.Fatalf("%v\n%s", err, out)
	}
	if strings.Contains(out, "Loaded:") || strings.Contains(out, "Model ") {
		t.Fatalf("no model can be named when the backend cannot say:\n%s", out)
	}
}

func TestDoctorSaysWhenTheEmbeddedLockIsBroken(t *testing.T) {
	previous := pinnedLock
	pinnedLock = func() (runtime.Lock, error) { return runtime.Lock{}, errors.New("lock is broken") }
	t.Cleanup(func() { pinnedLock = previous })
	if _, err := run(t, "doctor", "--host=http://127.0.0.1:1", "--data-dir", t.TempDir()); err == nil || err.Error() != "lock is broken" {
		t.Fatalf("%v", err)
	}
}

func TestDoctorKeepsBlankLogLinesButNeverEndsALineInWhitespace(t *testing.T) {
	d := startDaemon(t, nil)
	d.install(t, "gemma4:12b")
	d.fake.SetModels(backend.ModelStatus{
		Name: "gemma4:12b", Status: backend.StatusFailed, Failed: true, ExitCode: 1,
		LogTail: "first\n\nthird  \n\t\nlast\r\n\n",
	})
	out, err := run(t, "doctor", "--host="+d.srv.URL, "--data-dir", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	want := "Model gemma4:12b: failed, exit code 1\n  first\n\n  third\n\n  last\n"
	if !strings.Contains(out, want) {
		t.Fatalf("missing %q in\n%q", want, out)
	}
	for _, line := range strings.Split(out, "\n") {
		if line != strings.TrimRight(line, " \t\r") {
			t.Errorf("the line %q ends in whitespace", line)
		}
	}
}

func TestDoctorRecommendsForTheTierOfTheDaemonItAsks(t *testing.T) {
	withMachine(t, machine.Info{OS: "linux", Arch: "amd64", RAMBytes: 16 << 30, Tier: "16"})
	d := startDaemon(t, nil) // its machine is a tier 48 host
	out, err := run(t, "doctor", "--host="+d.srv.URL, "--data-dir", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Machine: linux/amd64, no NVIDIA GPU found, 16.0 GiB RAM, tier 16\n") {
		t.Fatalf("the machine line is this host's:\n%s", out)
	}
	if !strings.Contains(out, "Recommended for this tier: qwen3.6:35b\n") || strings.Contains(out, "gemma4:12b") {
		t.Fatalf("the recommendation is for the machine the daemon runs on, not the one doctor runs on:\n%s", out)
	}
}

func TestDoctorSaysNothingOfATierThatTheDaemonDoesNotReport(t *testing.T) {
	withMachine(t, machine.Info{OS: "linux", Arch: "amd64", RAMBytes: 16 << 30, Tier: "16"})
	d := startDaemon(t, func(deps *api.Deps) {
		deps.Machine = func(context.Context) machine.Info { return machine.Info{} }
	})
	out, err := run(t, "doctor", "--host="+d.srv.URL, "--data-dir", t.TempDir())
	if err != nil || strings.Contains(out, "Recommended") {
		t.Fatalf("a daemon that cannot place its machine recommends nothing, and not for the tier of this host: %v\n%s", err, out)
	}
}

func TestDoctorRecommendsByTheLocalTierWhenNoDaemonAnswers(t *testing.T) {
	withMachine(t, machine.Info{OS: "linux", Arch: "amd64", RAMBytes: 32 << 30, Tier: "32"})
	out, err := run(t, "doctor", "--host=http://127.0.0.1:1", "--data-dir", t.TempDir())
	if err != nil || !strings.Contains(out, "Daemon: not running") || !strings.Contains(out, "Recommended for this tier: glm-4.7-flash\n") {
		t.Fatalf("%v\n%s", err, out)
	}
	withMachine(t, machine.Info{OS: "linux", Arch: "amd64", RAMBytes: 1 << 30, Tier: "tiny"})
	if out, err = run(t, "doctor", "--host=http://127.0.0.1:1", "--data-dir", t.TempDir()); err != nil || strings.Contains(out, "Recommended") {
		t.Fatalf("no model for the tier of this host: %v\n%s", err, out)
	}
}

func TestDoctorRecommendsNothingFromACatalogItCannotRead(t *testing.T) {
	withMachine(t, machine.Info{OS: "linux", Arch: "amd64", RAMBytes: 32 << 30, Tier: "32"})
	dataDir := t.TempDir()
	writeText(t, filepath.Join(dataDir, "registry.yaml"), "entries: [")
	out, err := run(t, "doctor", "--host=http://127.0.0.1:1", "--data-dir", dataDir)
	if err != nil || !strings.Contains(out, "Registry: ") || strings.Contains(out, "Recommended") {
		t.Fatalf("%v\n%s", err, out)
	}
}

func TestDoctorReturnsTheErrorOfAContextThatIsDone(t *testing.T) {
	withMachine(t, machine.Info{OS: "linux", Arch: "amd64", RAMBytes: 32 << 30, Tier: "32"})
	d := startDaemon(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var out strings.Builder
	root := NewRootCommand()
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"doctor", "--host=" + d.srv.URL, "--data-dir", t.TempDir()})
	err := root.ExecuteContext(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("doctor was interrupted, and says so: %v\n%s", err, out.String())
	}
	if strings.Contains(out.String(), "Daemon:") {
		t.Fatalf("a call that was cut off is no word about the daemon:\n%s", out.String())
	}
}
