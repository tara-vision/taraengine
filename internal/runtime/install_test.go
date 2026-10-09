package runtime

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/tara-vision/taraengine/internal/pull"
)

func sumOf(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// assetServer serves the given tarballs at /<build>/<asset> as the release base would.
func assetServer(t *testing.T, build string, assets map[string][]byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := assets[strings.TrimPrefix(r.URL.Path, "/"+build+"/")]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func testLock(serverTgz, cudartTgz []byte) Lock {
	return Lock{
		Build: "bTEST",
		Assets: map[string]Asset{
			"darwin/arm64":       {Asset: "llama-bTEST-bin-macos-arm64.tar.gz", SHA256: sumOf(serverTgz)},
			"linux/amd64/cuda12": {Asset: "llama-bTEST-bin-ubuntu-cuda-12.8-x64.tar.gz", SHA256: sumOf(serverTgz)},
		},
		Cudart: map[string]Asset{
			"linux/amd64/cuda12": {Asset: "cudart-llama-bTEST-bin-ubuntu-cuda-12.8-x64.tar.gz", SHA256: sumOf(cudartTgz)},
		},
	}
}

func TestInstallVerifyStatus(t *testing.T) { //nolint:gocyclo // the brief lifecycle test is one scenario, kept whole
	serverTgz := runtimeTarball(t, "llama-bTEST")
	cudartTgz := makeTarGz(t, []tarEntry{
		{name: "cudart-llama-bTEST-bin-ubuntu-cuda-12.8-x64/", typ: tar.TypeDir, mode: 0o755},
		{name: "cudart-llama-bTEST-bin-ubuntu-cuda-12.8-x64/libcudart.so.12", typ: tar.TypeReg, body: "cudart"},
	})
	lock := testLock(serverTgz, cudartTgz)
	srv := assetServer(t, "bTEST", map[string][]byte{
		"llama-bTEST-bin-macos-arm64.tar.gz":                 serverTgz,
		"llama-bTEST-bin-ubuntu-cuda-12.8-x64.tar.gz":        serverTgz,
		"cudart-llama-bTEST-bin-ubuntu-cuda-12.8-x64.tar.gz": cudartTgz,
	})
	var stages []string
	inst := &Installer{Dir: t.TempDir(), Client: srv.Client(), BaseURL: srv.URL, OnProgress: func(p pull.Progress) { stages = append(stages, p.Stage) }}

	if st := inst.Status(lock); st.Installed || st.Verified || st.Err == "" {
		t.Fatalf("status before install: %+v", st)
	}

	dir, err := inst.Install(context.Background(), lock, Platform{OS: "linux", Arch: "amd64", Accel: "cuda12"}, true)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if dir != filepath.Join(inst.Dir, "bTEST") {
		t.Fatalf("dir %s", dir)
	}
	for _, f := range []string{"llama-server", "LICENSE", "libcudart.so.12", "manifest.json"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("%s missing after install", f)
		}
	}
	if len(stages) == 0 || stages[0] != "downloading" {
		t.Fatalf("progress stages %v", stages)
	}
	m, err := Verify(dir)
	if err != nil || m.Build != "bTEST" || m.Key != "linux/amd64/cuda12" || m.BinarySHA256 != sumOf([]byte("#!/bin/sh\necho server\n")) {
		t.Fatalf("verify: %+v, %v", m, err)
	}
	if st := inst.Status(lock); !st.Installed || !st.Verified || st.Binary != Binary(dir) {
		t.Fatalf("status after install: %+v", st)
	}
	if _, err := os.Stat(dir + ".partial"); !os.IsNotExist(err) {
		t.Fatal("partial directory left behind")
	}

	if err := os.WriteFile(Binary(dir), []byte("tampered"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(dir); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("tamper not detected: %v", err)
	}
	if st := inst.Status(lock); !st.Installed || st.Verified {
		t.Fatalf("status after tamper: %+v", st)
	}
}

func TestInstallRejectsAChecksumMismatch(t *testing.T) {
	serverTgz := runtimeTarball(t, "llama-bTEST")
	lock := testLock(serverTgz, nil)
	lock.Assets["darwin/arm64"] = Asset{Asset: "llama-bTEST-bin-macos-arm64.tar.gz", SHA256: strings.Repeat("0", 64)}
	srv := assetServer(t, "bTEST", map[string][]byte{"llama-bTEST-bin-macos-arm64.tar.gz": serverTgz})
	inst := &Installer{Dir: t.TempDir(), Client: srv.Client(), BaseURL: srv.URL}
	_, err := inst.Install(context.Background(), lock, Platform{OS: "darwin", Arch: "arm64", Accel: "metal"}, false)
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("want checksum mismatch, got %v", err)
	}
	entries, _ := os.ReadDir(inst.Dir)
	if len(entries) != 0 {
		t.Fatalf("install dir not clean after a mismatch: %v", entries)
	}
}

func TestInstallFailsWithoutAServerBinaryOrAsset(t *testing.T) {
	noServer := makeTarGz(t, []tarEntry{{name: "llama-bTEST/LICENSE", typ: tar.TypeReg, body: "MIT"}})
	lock := testLock(noServer, nil)
	srv := assetServer(t, "bTEST", map[string][]byte{"llama-bTEST-bin-macos-arm64.tar.gz": noServer})
	inst := &Installer{Dir: t.TempDir(), Client: srv.Client(), BaseURL: srv.URL}
	if _, err := inst.Install(context.Background(), lock, Platform{OS: "darwin", Arch: "arm64", Accel: "metal"}, false); err == nil || !strings.Contains(err.Error(), "llama-server") {
		t.Fatalf("want a missing-binary error, got %v", err)
	}
	if _, err := inst.Install(context.Background(), lock, Platform{OS: "windows", Arch: "amd64", Accel: "cpu"}, false); err == nil {
		t.Fatal("unknown platform accepted")
	}
	lock.Assets["darwin/arm64"] = Asset{Asset: "llama-bTEST-bin-absent.tar.gz", SHA256: sumOf(noServer)}
	if _, err := inst.Install(context.Background(), lock, Platform{OS: "darwin", Arch: "arm64", Accel: "metal"}, false); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("want a 404 error, got %v", err)
	}
}

func TestVerifyWithoutAManifest(t *testing.T) {
	if _, err := Verify(t.TempDir()); err == nil {
		t.Fatal("no manifest must fail")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(dir); err == nil {
		t.Fatal("broken manifest must fail")
	}
}

// roundTripFunc answers every request itself, so no test reaches the network.
type roundTripFunc func(*http.Request) (*http.Response, error)

// RoundTrip implements http.RoundTripper.
func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// recorded answers a request with body, the way a release download answers.
func recorded(body []byte) (*http.Response, error) {
	rec := httptest.NewRecorder()
	_, _ = rec.Write(body)
	return rec.Result(), nil
}

func TestInstallFailsWhenTheRuntimeDirCannotBeCreated(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	inst := &Installer{Dir: blocker}
	if _, err := inst.Install(context.Background(), testLock(nil, nil), Platform{OS: "darwin", Arch: "arm64", Accel: "metal"}, false); err == nil {
		t.Fatal("install under a file accepted")
	}
}

func TestInstallFailsWhenTheCudartBundleIsMissing(t *testing.T) {
	serverTgz := runtimeTarball(t, "llama-bTEST")
	cudartTgz := makeTarGz(t, []tarEntry{
		{name: "cudart-llama-bTEST-bin-ubuntu-cuda-12.8-x64/libcudart.so.12", typ: tar.TypeReg, body: "cudart"},
	})
	srv := assetServer(t, "bTEST", map[string][]byte{"llama-bTEST-bin-ubuntu-cuda-12.8-x64.tar.gz": serverTgz})
	inst := &Installer{Dir: t.TempDir(), Client: srv.Client(), BaseURL: srv.URL}
	_, err := inst.Install(context.Background(), testLock(serverTgz, cudartTgz), Platform{OS: "linux", Arch: "amd64", Accel: "cuda12"}, true)
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("want a 404 for the cudart bundle, got %v", err)
	}
	if entries, _ := os.ReadDir(inst.Dir); len(entries) != 0 {
		t.Fatalf("install dir not clean after a failed cudart fetch: %v", entries)
	}
}

func TestInstallFailsWhenTheManifestCannotBeWritten(t *testing.T) {
	serverTgz := makeTarGz(t, []tarEntry{
		{name: "llama-bTEST/llama-server", typ: tar.TypeReg, mode: 0o755, body: "#!/bin/sh\n"},
		{name: "llama-bTEST/manifest.json/", typ: tar.TypeDir, mode: 0o755},
	})
	srv := assetServer(t, "bTEST", map[string][]byte{"llama-bTEST-bin-macos-arm64.tar.gz": serverTgz})
	inst := &Installer{Dir: t.TempDir(), Client: srv.Client(), BaseURL: srv.URL}
	if _, err := inst.Install(context.Background(), testLock(serverTgz, nil), Platform{OS: "darwin", Arch: "arm64", Accel: "metal"}, false); err == nil {
		t.Fatal("manifest written over a directory")
	}
	if _, err := os.Stat(filepath.Join(inst.Dir, "bTEST")); !os.IsNotExist(err) {
		t.Fatal("failed install left a runtime directory behind")
	}
}

func TestInstallUsesTheReleaseBaseWhenNoBaseURLIsSet(t *testing.T) {
	serverTgz := runtimeTarball(t, "llama-bTEST")
	var asked []string
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		asked = append(asked, r.URL.String())
		return recorded(serverTgz)
	})}
	inst := &Installer{Dir: t.TempDir(), Client: client}
	if _, err := inst.Install(context.Background(), testLock(serverTgz, nil), Platform{OS: "darwin", Arch: "arm64", Accel: "metal"}, false); err != nil {
		t.Fatalf("install: %v", err)
	}
	want := releaseBase + "/bTEST/llama-bTEST-bin-macos-arm64.tar.gz"
	if len(asked) != 1 || asked[0] != want {
		t.Fatalf("asked %v, want %s", asked, want)
	}
}

func TestInstallRejectsAMalformedBaseURL(t *testing.T) {
	serverTgz := runtimeTarball(t, "llama-bTEST")
	inst := &Installer{Dir: t.TempDir(), BaseURL: "http://[::1"}
	if _, err := inst.Install(context.Background(), testLock(serverTgz, nil), Platform{OS: "darwin", Arch: "arm64", Accel: "metal"}, false); err == nil {
		t.Fatal("malformed base URL accepted")
	}
}

func TestDownloadReportsAnUnreachableServer(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("connection refused")
	})}
	inst := &Installer{Dir: t.TempDir(), Client: client}
	err := inst.download(context.Background(), "http://runtime.invalid/asset", filepath.Join(inst.Dir, "asset.download"))
	if err == nil || !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("want the transport error, got %v", err)
	}
}

func TestDownloadUsesTheDefaultClientWhenNoneIsSet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("payload"))
	}))
	t.Cleanup(srv.Close)
	inst := &Installer{Dir: t.TempDir()}
	dest := filepath.Join(inst.Dir, "asset.download")
	if err := inst.download(context.Background(), srv.URL+"/asset", dest); err != nil {
		t.Fatalf("download: %v", err)
	}
	if got, err := os.ReadFile(dest); err != nil || string(got) != "payload" {
		t.Fatalf("downloaded %q, %v", got, err)
	}
}

func TestDownloadRejectsAnUnusableDestination(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("payload"))
	}))
	t.Cleanup(srv.Close)
	inst := &Installer{Dir: t.TempDir(), Client: srv.Client()}
	blocker := filepath.Join(inst.Dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := inst.download(context.Background(), srv.URL, filepath.Join(blocker, "asset")); err == nil {
		t.Fatal("destination below a file accepted")
	}
	dirDest := filepath.Join(inst.Dir, "dir")
	if err := os.Mkdir(dirDest, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := inst.download(context.Background(), srv.URL, dirDest); err == nil {
		t.Fatal("destination that is a directory accepted")
	}
}

func TestVerifyFailsWithoutAServerBinary(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, manifestName), []byte(`{"build":"bTEST"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(dir); err == nil {
		t.Fatal("manifest without llama-server verified")
	}
}

func TestDownloadReportsAShortBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "100")
		_, _ = w.Write([]byte("short"))
	}))
	t.Cleanup(srv.Close)
	inst := &Installer{Dir: t.TempDir(), Client: srv.Client()}
	if err := inst.download(context.Background(), srv.URL+"/asset", filepath.Join(inst.Dir, "asset.download")); err == nil {
		t.Fatal("a body shorter than its Content-Length accepted")
	}
	if entries, _ := os.ReadDir(inst.Dir); len(entries) != 0 {
		t.Fatalf("partial archive left behind: %v", entries)
	}
}

// cancelOnceExists cancels once path exists, which is once the body is being read, or after five seconds.
func cancelOnceExists(path string, cancel context.CancelFunc) {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
}

func TestDownloadRemovesThePartialArchiveWhenCancelled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	inst := &Installer{Dir: t.TempDir(), Client: srv.Client()}
	dest := filepath.Join(inst.Dir, "asset.download")
	go cancelOnceExists(dest, cancel)
	if err := inst.download(ctx, srv.URL+"/asset", dest); err == nil {
		t.Fatal("cancelled download accepted")
	}
	if entries, _ := os.ReadDir(inst.Dir); len(entries) != 0 {
		t.Fatalf("partial archive left behind: %v", entries)
	}
}

func TestDownloadRefusesABodyOverTheCap(t *testing.T) {
	previous := maxDownloadBytes
	maxDownloadBytes = 16
	t.Cleanup(func() { maxDownloadBytes = previous })
	declared := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "100")
		_, _ = w.Write(make([]byte, 100))
	}))
	t.Cleanup(declared.Close)
	streamed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(make([]byte, 10))
		w.(http.Flusher).Flush()
		_, _ = w.Write(make([]byte, 90))
	}))
	t.Cleanup(streamed.Close)
	for name, srv := range map[string]*httptest.Server{"declared length": declared, "streamed": streamed} {
		t.Run(name, func(t *testing.T) {
			inst := &Installer{Dir: t.TempDir(), Client: srv.Client()}
			err := inst.download(context.Background(), srv.URL+"/asset", filepath.Join(inst.Dir, "asset.download"))
			if err == nil || !strings.Contains(err.Error(), "byte cap") {
				t.Fatalf("want the cap refusal, got %v", err)
			}
			if entries, _ := os.ReadDir(inst.Dir); len(entries) != 0 {
				t.Fatalf("refused archive left behind: %v", entries)
			}
		})
	}
}

func TestStatusReportsADamagedInstallAsInstalled(t *testing.T) {
	inst := &Installer{Dir: t.TempDir()}
	dir := filepath.Join(inst.Dir, "bTEST")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, manifestName), []byte(`{"build":"bTEST"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if st := inst.Status(testLock(nil, nil)); !st.Installed || st.Verified || st.Err == "" || strings.Contains(st.Err, "not installed") {
		t.Fatalf("damaged install reported as %+v", st)
	}
}

// darwinInstaller serves the test build for darwin/arm64 and returns an installer for it.
func darwinInstaller(t *testing.T) (*Installer, Lock, Platform) {
	t.Helper()
	serverTgz := runtimeTarball(t, "llama-bTEST")
	srv := assetServer(t, "bTEST", map[string][]byte{"llama-bTEST-bin-macos-arm64.tar.gz": serverTgz})
	inst := &Installer{Dir: t.TempDir(), Client: srv.Client(), BaseURL: srv.URL}
	return inst, testLock(serverTgz, nil), Platform{OS: "darwin", Arch: "arm64", Accel: "metal"}
}

// failRename makes each rename that match reports with cause, for the rest of the test.
func failRename(t *testing.T, match func(from, to string) bool, cause error) {
	t.Helper()
	previous := renamePath
	renamePath = func(from, to string) error {
		if match(from, to) {
			return &os.LinkError{Op: "rename", Old: from, New: to, Err: cause}
		}
		return previous(from, to)
	}
	t.Cleanup(func() { renamePath = previous })
}

func TestReinstallKeepsTheOldInstallWhenTheFinalRenameFails(t *testing.T) {
	inst, lock, p := darwinInstaller(t)
	dir, err := inst.Install(context.Background(), lock, p, false)
	if err != nil {
		t.Fatalf("first install: %v", err)
	}
	failRename(t, func(from, to string) bool { return to == dir && strings.HasSuffix(from, ".partial") }, syscall.EEXIST)
	if _, err := inst.Install(context.Background(), lock, p, false); err == nil || !strings.Contains(err.Error(), syscall.EEXIST.Error()) {
		t.Fatalf("want the rename cause, got %v", err)
	}
	if _, err := Verify(dir); err != nil {
		t.Fatalf("old install not intact: %v", err)
	}
	for _, leftover := range []string{dir + ".old", dir + ".partial"} {
		if _, err := os.Stat(leftover); !os.IsNotExist(err) {
			t.Fatalf("%s left behind", leftover)
		}
	}
}

func TestReinstallKeepsTheOldInstallWhenTheOldCannotBeMovedAside(t *testing.T) {
	inst, lock, p := darwinInstaller(t)
	dir, err := inst.Install(context.Background(), lock, p, false)
	if err != nil {
		t.Fatalf("first install: %v", err)
	}
	failRename(t, func(from, _ string) bool { return from == dir }, syscall.EACCES)
	if _, err := inst.Install(context.Background(), lock, p, false); err == nil || !strings.Contains(err.Error(), syscall.EACCES.Error()) {
		t.Fatalf("want the move cause, got %v", err)
	}
	if _, err := Verify(dir); err != nil {
		t.Fatalf("old install not intact: %v", err)
	}
	if _, err := os.Stat(dir + ".partial"); !os.IsNotExist(err) {
		t.Fatal("partial directory left behind")
	}
}

func TestReinstallNamesBothCausesWhenTheRestoreFails(t *testing.T) {
	inst, lock, p := darwinInstaller(t)
	dir, err := inst.Install(context.Background(), lock, p, false)
	if err != nil {
		t.Fatalf("first install: %v", err)
	}
	failRename(t, func(_, to string) bool { return to == dir }, syscall.EEXIST)
	if _, err := inst.Install(context.Background(), lock, p, false); err == nil || !strings.Contains(err.Error(), "restoring the old install") {
		t.Fatalf("want both causes named, got %v", err)
	}
	if _, err := Verify(dir + ".old"); err != nil {
		t.Fatalf("old install not kept in the aside directory: %v", err)
	}
}

func TestReinstallRemovesAnAsideDirectoryWhenAnInstallExists(t *testing.T) {
	inst, lock, p := darwinInstaller(t)
	if _, err := inst.Install(context.Background(), lock, p, false); err != nil {
		t.Fatalf("first install: %v", err)
	}
	aside := filepath.Join(inst.Dir, "bTEST.old")
	if err := os.MkdirAll(aside, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(aside, "junk"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir, err := inst.Install(context.Background(), lock, p, false)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if _, err := os.Stat(aside); !os.IsNotExist(err) {
		t.Fatal("aside directory left behind after a successful install")
	}
	if _, err := Verify(dir); err != nil {
		t.Fatalf("install not verifiable: %v", err)
	}
}

func TestReinstallReplacesTheInstallAndLeavesNoAside(t *testing.T) {
	inst, lock, p := darwinInstaller(t)
	if _, err := inst.Install(context.Background(), lock, p, false); err != nil {
		t.Fatalf("first install: %v", err)
	}
	dir, err := inst.Install(context.Background(), lock, p, false)
	if err != nil {
		t.Fatalf("reinstall: %v", err)
	}
	if _, err := os.Stat(dir + ".old"); !os.IsNotExist(err) {
		t.Fatal("aside directory left behind after a successful reinstall")
	}
	if _, err := Verify(dir); err != nil {
		t.Fatalf("reinstalled runtime not verifiable: %v", err)
	}
}
