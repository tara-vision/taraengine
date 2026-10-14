package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tara-vision/taraengine/internal/backend/llamaserver"
	"github.com/tara-vision/taraengine/internal/config"
	"github.com/tara-vision/taraengine/internal/runtime"
)

// selfRelease is a release server for the fake runtime that refuses the first failures requests with a 500, and the
// lock that pins its tarball. It counts the requests it got.
type selfRelease struct {
	lock     runtime.Lock
	requests atomic.Int32
}

func newSelfRelease(t *testing.T, failures int32) *selfRelease {
	t.Helper()
	tgz := fakeRuntimeTarball(t)
	embedded, err := runtime.LoadLock()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(lockFor(t, embedded, tgz))
	if err != nil {
		t.Fatal(err)
	}
	lock, err := runtime.ParseLock(data)
	if err != nil {
		t.Fatal(err)
	}
	r := &selfRelease{lock: lock}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if r.requests.Add(1) <= failures {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(tgz)))
		_, _ = w.Write(tgz)
	}))
	t.Cleanup(srv.Close)
	previousLock, previousBase := pinnedLock, serviceReleaseBase
	pinnedLock = func() (runtime.Lock, error) { return lock, nil }
	serviceReleaseBase = srv.URL
	t.Cleanup(func() { pinnedLock, serviceReleaseBase = previousLock, previousBase })
	return r
}

// recordSleeps makes the retry waits return at once, records each, and returns what the detail said at each. stopAt,
// when above zero, ends the run at that wait as a stop would.
func recordSleeps(t *testing.T, steps *progress, stopAt int) (waits *[]time.Duration, details *[]string) {
	t.Helper()
	var w []time.Duration
	var d []string
	previous := sleepFor
	sleepFor = func(_ context.Context, wait time.Duration) error {
		w = append(w, wait)
		d = append(d, steps.detail())
		if len(w) == stopAt {
			return context.Canceled
		}
		return nil
	}
	t.Cleanup(func() { sleepFor = previous })
	return &w, &d
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func serviceConfig(dataDir string) config.Config {
	cfg := config.Default()
	cfg.DataDir = dataDir
	return cfg
}

func TestEnsureRuntimeInstallsThePinnedBuildAndRemovesTheOthers(t *testing.T) {
	r := newSelfRelease(t, 0)
	dataDir := t.TempDir()
	root := runtimeRoot(dataDir)
	for _, dir := range []string{"b10000", "b10000.old", "custom"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	steps := &progress{}
	if err := ensureRuntime(context.Background(), serviceConfig(dataDir), steps, quiet()); err != nil {
		t.Fatal(err)
	}
	if st := (&runtime.Installer{Dir: root}).Status(r.lock); !st.Verified {
		t.Fatalf("the pinned build is installed and verified: %+v", st)
	}
	if got := steps.detail(); got != "installing runtime "+r.lock.Build+" (100%)" {
		t.Fatalf("the detail followed the download, last %q", got)
	}
	entries, _ := os.ReadDir(root)
	var left []string
	for _, e := range entries {
		left = append(left, e.Name())
	}
	if want := []string{r.lock.Build, "custom"}; !slices.Equal(left, want) {
		t.Fatalf("left %v, want %v", left, want)
	}
}

func TestEnsureRuntimeWithAVerifiedBuildDownloadsNothing(t *testing.T) {
	r := newSelfRelease(t, 0)
	dataDir := t.TempDir()
	if err := ensureRuntime(context.Background(), serviceConfig(dataDir), &progress{}, quiet()); err != nil {
		t.Fatal(err)
	}
	before := r.requests.Load()
	if err := ensureRuntime(context.Background(), serviceConfig(dataDir), &progress{}, quiet()); err != nil {
		t.Fatal(err)
	}
	if after := r.requests.Load(); after != before {
		t.Fatalf("a verified build is not fetched again: %d requests, then %d", before, after)
	}
}

func TestEnsureRuntimeRetriesOnTheScheduleAndSaysWhy(t *testing.T) {
	r := newSelfRelease(t, 5)
	steps := &progress{}
	waits, details := recordSleeps(t, steps, 0)
	if err := ensureRuntime(context.Background(), serviceConfig(t.TempDir()), steps, quiet()); err != nil {
		t.Fatal(err)
	}
	want := []time.Duration{10 * time.Second, 30 * time.Second, 60 * time.Second, 300 * time.Second, 300 * time.Second}
	if !slices.Equal(*waits, want) {
		t.Fatalf("waits %v, want %v", *waits, want)
	}
	first := (*details)[0]
	if !strings.HasPrefix(first, "runtime download failed: ") || !strings.HasSuffix(first, "; retrying in 10 s") ||
		strings.Contains(first, "\n") {
		t.Fatalf("the detail of the first wait says why and when, on one line: %q", first)
	}
	if last := (*details)[4]; !strings.HasSuffix(last, "; retrying in 300 s") {
		t.Fatalf("the waits stay at five minutes: %q", last)
	}
	if got := r.requests.Load(); got != 6 {
		t.Fatalf("five failures and one success make six requests, got %d", got)
	}
}

func TestEnsureRuntimeKeepsTheWorkingBuildWhenTheNewOneFails(t *testing.T) {
	newSelfRelease(t, 1000)
	dataDir := t.TempDir()
	old := filepath.Join(runtimeRoot(dataDir), "b10000")
	if err := os.MkdirAll(old, 0o750); err != nil {
		t.Fatal(err)
	}
	steps := &progress{}
	recordSleeps(t, steps, 1)
	err := ensureRuntime(context.Background(), serviceConfig(dataDir), steps, quiet())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("a stop during the wait ends the run: %v", err)
	}
	if _, err := os.Stat(old); err != nil {
		t.Fatalf("no build is removed before the pinned one verifies: %v", err)
	}
}

func TestEnsureRuntimeLeavesAConfiguredRuntimeDirAlone(t *testing.T) {
	r := newSelfRelease(t, 0)
	dataDir := t.TempDir()
	old := filepath.Join(runtimeRoot(dataDir), "b10000")
	if err := os.MkdirAll(old, 0o750); err != nil {
		t.Fatal(err)
	}
	cfg := serviceConfig(dataDir)
	cfg.RuntimeDir = t.TempDir()
	if err := ensureRuntime(context.Background(), cfg, &progress{}, quiet()); err != nil {
		t.Fatal(err)
	}
	if r.requests.Load() != 0 {
		t.Fatal("a configured runtime_dir is the operator's: nothing is fetched")
	}
	if _, err := os.Stat(old); err != nil {
		t.Fatalf("and nothing is removed: %v", err)
	}
}

func TestEnsureRuntimeSaysWhenTheEmbeddedLockIsBroken(t *testing.T) {
	previous := pinnedLock
	pinnedLock = func() (runtime.Lock, error) { return runtime.Lock{}, errors.New("runtime: lock: broken") }
	t.Cleanup(func() { pinnedLock = previous })
	if err := ensureRuntime(context.Background(), serviceConfig(t.TempDir()), &progress{}, quiet()); err == nil ||
		!strings.Contains(err.Error(), "broken") {
		t.Fatalf("got %v", err)
	}
}

func TestEnsureRuntimeLogsABuildItCannotRemove(t *testing.T) {
	newSelfRelease(t, 0)
	dataDir := t.TempDir()
	root := runtimeRoot(dataDir)
	if err := ensureRuntime(context.Background(), serviceConfig(dataDir), &progress{}, quiet()); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "b10000", "lib"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o700) })
	var logs strings.Builder
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	if err := ensureRuntime(context.Background(), serviceConfig(dataDir), &progress{}, logger); err != nil {
		t.Fatalf("a build that cannot go is no reason to stop: %v", err)
	}
	if !strings.Contains(logs.String(), "could not remove") {
		t.Fatalf("it is logged: %s", logs.String())
	}
}

func TestEnsureRuntimeReportsAMachineWithoutABuild(t *testing.T) {
	newSelfRelease(t, 0)
	withPlatform(t, runtime.Platform{}, errors.New("runtime: plan9/386 is not supported in this release"))
	steps := &progress{}
	_, details := recordSleeps(t, steps, 1)
	err := ensureRuntime(context.Background(), serviceConfig(t.TempDir()), steps, quiet())
	if !errors.Is(err, context.Canceled) || !strings.Contains((*details)[0], "not supported in this release") {
		t.Fatalf("the detail says why: %v %v", err, *details)
	}
}

func TestSleepForWaitsOrEndsWithTheContext(t *testing.T) {
	if err := sleepFor(context.Background(), time.Millisecond); err != nil {
		t.Fatalf("a short wait ends by itself: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sleepFor(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("a stop ends a wait at once: %v", err)
	}
}

func TestStartFetchesTheRuntimeOnlyForAServiceThatIsNotAttached(t *testing.T) {
	r := newSelfRelease(t, 0)
	errWired := errors.New("wired")
	var verified bool
	previous := wireDaemon
	wireDaemon = func(_ context.Context, cfg config.Config, _ WireOptions) (*Wired, error) {
		verified = (&runtime.Installer{Dir: runtimeRoot(cfg.DataDir)}).Status(r.lock).Verified
		return nil, errWired
	}
	t.Cleanup(func() { wireDaemon = previous })
	for _, c := range []struct {
		name  string
		opts  WireOptions
		fetch bool
	}{
		{"a daemon run by hand", WireOptions{}, false},
		{"an attached service", WireOptions{Service: true, Attach: "http://127.0.0.1:1"}, false},
		{"a service", WireOptions{Service: true}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			verified = false
			if _, err := start(context.Background(), serviceConfig(t.TempDir()), c.opts, &progress{}); !errors.Is(err, errWired) {
				t.Fatalf("got %v", err)
			}
			if verified != c.fetch {
				t.Fatalf("runtime installed before wiring: %v, want %v", verified, c.fetch)
			}
		})
	}
}

func TestEnsureRuntimeOnACudaMachineChecksForTheCudaRuntime(t *testing.T) {
	r := newSelfRelease(t, 0)
	withPlatform(t, runtime.Platform{OS: "linux", Arch: "amd64", Accel: "cuda12"}, nil)
	dataDir := t.TempDir()
	if err := ensureRuntime(context.Background(), serviceConfig(dataDir), &progress{}, quiet()); err != nil {
		t.Fatal(err)
	}
	if st := (&runtime.Installer{Dir: runtimeRoot(dataDir)}).Status(r.lock); !st.Verified {
		t.Fatalf("installed for a CUDA machine: %+v", st)
	}
}

func TestStartLetsGoOfTheLockWhenTheRuntimeStepFails(t *testing.T) {
	previous := pinnedLock
	pinnedLock = func() (runtime.Lock, error) { return runtime.Lock{}, errors.New("runtime: lock: broken") }
	t.Cleanup(func() { pinnedLock = previous })
	dataDir := t.TempDir()
	if _, err := start(context.Background(), serviceConfig(dataDir), WireOptions{Service: true}, &progress{}); err == nil {
		t.Fatal("a runtime step that fails ends the start")
	}
	lock, err := llamaserver.LockDataDir(dataDir)
	if err != nil {
		t.Fatalf("the lock was let go of: %v", err)
	}
	_ = lock.Close()
}

func TestStartRefusesADataDirectoryAnotherDaemonHoldsBeforeItFetchesOrRemovesAnything(t *testing.T) {
	r := newSelfRelease(t, 0)
	dataDir := t.TempDir()
	stale := filepath.Join(runtimeRoot(dataDir), "b10000")
	if err := os.MkdirAll(stale, 0o750); err != nil {
		t.Fatal(err)
	}
	held, err := llamaserver.LockDataDir(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = held.Close() })
	_, err = start(context.Background(), serviceConfig(dataDir), WireOptions{Service: true}, &progress{})
	if err == nil || !strings.Contains(err.Error(), "in use by another taraengine") {
		t.Fatalf("got %v", err)
	}
	if n := r.requests.Load(); n != 0 {
		t.Fatalf("a second daemon downloaded %d times before it had the lock", n)
	}
	if _, err := os.Stat(stale); err != nil {
		t.Fatalf("a second daemon removed a build under the first one: %v", err)
	}
}

// cudaRelease serves the fake runtime and a CUDA runtime bundle that holds libcudart.so.12, and pins both for
// linux/amd64/cuda12 under the asset names of the embedded lock. It records the paths it was asked for.
func cudaRelease(t *testing.T) (runtime.Lock, *[]string) {
	t.Helper()
	embedded, err := runtime.LoadLock()
	if err != nil {
		t.Fatal(err)
	}
	key := runtime.Platform{OS: "linux", Arch: "amd64", Accel: "cuda12"}.Key()
	server, bundle := fakeRuntimeTarball(t), cudartTarball(t)
	body := "build: " + embedded.Build + "\nassets:\n  " + key + ":\n    asset: " + embedded.Assets[key].Asset +
		"\n    sha256: " + sha256Hex(server) + "\ncudart:\n  " + key + ":\n    asset: " + embedded.Cudart[key].Asset +
		"\n    sha256: " + sha256Hex(bundle) + "\n"
	lock, err := runtime.ParseLock([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	var asked []string
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked = append(asked, r.URL.Path)
		mu.Unlock()
		body := server
		if strings.HasSuffix(r.URL.Path, "/"+embedded.Cudart[key].Asset) {
			body = bundle
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	previousLock, previousBase := pinnedLock, serviceReleaseBase
	pinnedLock = func() (runtime.Lock, error) { return lock, nil }
	serviceReleaseBase = srv.URL
	t.Cleanup(func() { pinnedLock, serviceReleaseBase = previousLock, previousBase })
	return lock, &asked
}

func cudartTarball(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	body := "ELF"
	if err := tw.WriteHeader(&tar.Header{Name: "cudart/libcudart.so.12", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body))}); err != nil {
		t.Fatal(err)
	}
	_, _ = tw.Write([]byte(body))
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func TestServiceModeBringsTheCUDARuntimeAMachineLacks(t *testing.T) {
	lock, asked := cudaRelease(t)
	withPlatform(t, runtime.Platform{OS: "linux", Arch: "amd64", Accel: "cuda12"}, nil)
	dataDir := t.TempDir()
	if err := ensureRuntime(context.Background(), serviceConfig(dataDir), &progress{}, quiet()); err != nil {
		t.Fatal(err)
	}
	key := runtime.Platform{OS: "linux", Arch: "amd64", Accel: "cuda12"}.Key()
	if !slices.Contains(*asked, "/"+lock.Build+"/"+lock.Cudart[key].Asset) {
		t.Fatalf("the CUDA runtime bundle was not fetched: %v", *asked)
	}
	if _, err := os.Stat(filepath.Join(runtimeRoot(dataDir), lock.Build, "libcudart.so.12")); err != nil {
		t.Fatalf("the CUDA runtime did not land beside llama-server: %v", err)
	}
}
