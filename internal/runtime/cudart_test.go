package runtime

import (
	"archive/tar"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// layoutFor reads the recorded entry names of a release archive, the output of
//
//	curl -sL <asset url> | tar -tzf -
//
// one name per line in archive order, a directory with its trailing slash. The files are named after the asset, so a
// lock that pins another build finds no layout until someone records it.
func layoutFor(t *testing.T, asset string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "cudart", asset+".txt"))
	if err != nil {
		t.Fatalf("no recorded layout for %s: list the archive with `tar -tzf` into testdata/cudart/%s.txt: %v",
			asset, asset, err)
	}
	return strings.Fields(string(data)) // the names hold no spaces
}

// tarballOf builds an archive with the given entry names: a directory for a name with a trailing slash, otherwise a
// small regular file. The recorded layouts are of archives of hundreds of megabytes; the names are what Unpack acts on.
func tarballOf(t *testing.T, names []string) []byte {
	t.Helper()
	entries := make([]tarEntry, 0, len(names))
	for _, name := range names {
		if strings.HasSuffix(name, "/") {
			entries = append(entries, tarEntry{name: name, typ: tar.TypeDir, mode: 0o755})
			continue
		}
		entries = append(entries, tarEntry{name: name, typ: tar.TypeReg, body: "content of " + name})
	}
	return makeTarGz(t, entries)
}

// platformOf is the platform behind a lock key such as linux/amd64/cuda12.
func platformOf(t *testing.T, key string) Platform {
	t.Helper()
	parts := strings.Split(key, "/")
	if len(parts) != 3 {
		t.Fatalf("lock key %q is not os/arch/accel", key)
	}
	return Platform{OS: parts[0], Arch: parts[1], Accel: parts[2]}
}

func TestEveryCudartAssetOfTheLockHasARecordedLayoutThatInstallCanUse(t *testing.T) {
	lock, err := LoadLock()
	if err != nil {
		t.Fatal(err)
	}
	if len(lock.Cudart) == 0 {
		t.Fatal("the lock pins no cudart bundle")
	}
	for key, asset := range lock.Cudart {
		t.Run(key, func(t *testing.T) {
			p := platformOf(t, key)
			if p.CUDAMajor() == 0 {
				t.Fatalf("%s pins a cudart bundle but names no CUDA major", key)
			}
			names := layoutFor(t, asset.Asset)
			top := strings.TrimSuffix(asset.Asset, ".tar.gz") + "/"
			if names[0] != top {
				t.Fatalf("first entry %q, want the top-level directory %q", names[0], top)
			}
			for _, name := range names {
				if !strings.HasPrefix(name, top) {
					t.Errorf("entry %q is outside the top-level directory %q", name, top)
				}
			}
			want := top + "libcudart.so." + strconv.Itoa(p.CUDAMajor())
			if !containsName(names, want) {
				t.Errorf("the layout has no %s: %v", want, names)
			}
		})
	}
}

func containsName(names []string, want string) bool {
	for _, name := range names {
		if name == want {
			return true
		}
	}
	return false
}

// bundleLock is the embedded lock with the checksums of the stand-in archives, so that Install runs the real names
// and the real layouts through the whole install without the hundreds of megabytes.
func bundleLock(lock Lock, key string, server, bundle []byte) Lock {
	return Lock{
		Build:  lock.Build,
		Assets: map[string]Asset{key: {Asset: lock.Assets[key].Asset, SHA256: sumOf(server)}},
		Cudart: map[string]Asset{key: {Asset: lock.Cudart[key].Asset, SHA256: sumOf(bundle)}},
	}
}

func TestInstallUnpacksEveryRecordedCudartLayout(t *testing.T) {
	lock, err := LoadLock()
	if err != nil {
		t.Fatal(err)
	}
	for key, asset := range lock.Cudart {
		t.Run(key, func(t *testing.T) {
			p := platformOf(t, key)
			server := runtimeTarball(t, "llama-"+lock.Build)
			bundle := tarballOf(t, layoutFor(t, asset.Asset))
			srv := assetServer(t, lock.Build, map[string][]byte{lock.Assets[key].Asset: server, asset.Asset: bundle})
			inst := &Installer{Dir: t.TempDir(), Client: srv.Client(), BaseURL: srv.URL}
			dir, err := inst.Install(context.Background(), bundleLock(lock, key, server, bundle), p, true)
			if err != nil {
				t.Fatalf("install: %v", err)
			}
			major := strconv.Itoa(p.CUDAMajor())
			for _, lib := range []string{"libcudart.so." + major, "libcublas.so." + major, "libcublasLt.so." + major} {
				if info, err := os.Stat(filepath.Join(dir, lib)); err != nil || !info.Mode().IsRegular() {
					t.Errorf("%s is not in the installed runtime: %v", lib, err)
				}
			}
			if _, err := Verify(dir); err != nil {
				t.Errorf("the installed runtime does not verify: %v", err)
			}
		})
	}
}

// cudart12 is the platform whose bundle the failure tests alter.
const cudart12 = "linux/amd64/cuda12"

// bundleTop is the top-level directory of the CUDA 12 bundle of the lock, with its trailing slash.
func bundleTop(t *testing.T) string {
	t.Helper()
	lock, err := LoadLock()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSuffix(lock.Cudart[cudart12].Asset, ".tar.gz") + "/"
}

// installBundle installs the stand-in server tarball and a bundle with the given entry names for the CUDA 12
// platform, and returns the installer, the install directory and the error.
func installBundle(t *testing.T, names []string) (*Installer, string, error) {
	t.Helper()
	return installBundleArchive(t, tarballOf(t, names))
}

// installBundleEntries is installBundle for a bundle whose entries are given in full.
func installBundleEntries(t *testing.T, entries []tarEntry) (*Installer, string, error) {
	t.Helper()
	return installBundleArchive(t, makeTarGz(t, entries))
}

func installBundleArchive(t *testing.T, bundle []byte) (*Installer, string, error) {
	t.Helper()
	lock, err := LoadLock()
	if err != nil {
		t.Fatal(err)
	}
	server := runtimeTarball(t, "llama-"+lock.Build)
	srv := assetServer(t, lock.Build, map[string][]byte{
		lock.Assets[cudart12].Asset: server, lock.Cudart[cudart12].Asset: bundle,
	})
	inst := &Installer{Dir: t.TempDir(), Client: srv.Client(), BaseURL: srv.URL}
	dir, err := inst.Install(context.Background(), bundleLock(lock, cudart12, server, bundle), platformOf(t, cudart12), true)
	return inst, dir, err
}

func TestInstallRefusesABundleThatKeepsItsLibrariesAtTheTopLevel(t *testing.T) {
	inst, _, err := installBundle(t, []string{"libcublas.so.12", "libcudart.so.12", "libcublasLt.so.12"})
	if err == nil || !strings.Contains(err.Error(), "libcublas.so.12") || !strings.Contains(err.Error(), "top-level directory") {
		t.Fatalf("want the loose library named, got %v", err)
	}
	if entries, _ := os.ReadDir(inst.Dir); len(entries) != 0 {
		t.Fatalf("a refused install left %v behind", entries)
	}
}

func TestInstallRefusesABundleWithoutTheCudartLibrary(t *testing.T) {
	top := bundleTop(t)
	inst, dir, err := installBundle(t, []string{top, top + "libcublas.so.12", top + "libcublasLt.so.12"})
	if err == nil || !strings.Contains(err.Error(), "libcudart.so.12") {
		t.Fatalf("want the missing library named, got %v", err)
	}
	if _, serr := os.Stat(dir); !os.IsNotExist(serr) {
		t.Fatal("an install without the library was swapped into place")
	}
	if entries, _ := os.ReadDir(inst.Dir); len(entries) != 0 {
		t.Fatalf("a refused install left %v behind", entries)
	}
}

func TestInstallRefusesABundleWhoseCudartLibraryIsNotAFile(t *testing.T) {
	top := bundleTop(t)
	_, _, err := installBundle(t, []string{top, top + "libcublas.so.12", top + "libcudart.so.12/"})
	if err == nil || !strings.Contains(err.Error(), "libcudart.so.12") {
		t.Fatalf("want the library named, got %v", err)
	}
}

func TestInstallAcceptsABundleWhoseCudartLibraryIsALinkToAFile(t *testing.T) {
	lock, err := LoadLock()
	if err != nil {
		t.Fatal(err)
	}
	top := bundleTop(t)
	server := runtimeTarball(t, "llama-"+lock.Build)
	bundle := makeTarGz(t, []tarEntry{
		{name: top, typ: tar.TypeDir, mode: 0o755},
		{name: top + "libcudart.so.12.8.90", typ: tar.TypeReg, body: "cudart"},
		{name: top + "libcudart.so.12", typ: tar.TypeSymlink, link: "libcudart.so.12.8.90"},
	})
	srv := assetServer(t, lock.Build, map[string][]byte{
		lock.Assets[cudart12].Asset: server, lock.Cudart[cudart12].Asset: bundle,
	})
	inst := &Installer{Dir: t.TempDir(), Client: srv.Client(), BaseURL: srv.URL}
	if _, err := inst.Install(context.Background(), bundleLock(lock, cudart12, server, bundle), platformOf(t, cudart12), true); err != nil {
		t.Fatalf("install: %v", err)
	}
}

func TestInstallAcceptsABundleThatRepeatsTheLicenseOfTheServer(t *testing.T) {
	top := bundleTop(t)
	bundle := []tarEntry{
		{name: top, typ: tar.TypeDir, mode: 0o755},
		{name: top + "LICENSE", typ: tar.TypeReg, body: "MIT"}, // the stand-in server tarball carries the same bytes
		{name: top + "libcudart.so.12", typ: tar.TypeReg, body: "cudart"},
	}
	inst, dir, err := installBundleEntries(t, bundle)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(dir, "LICENSE")); err != nil || string(got) != "MIT" {
		t.Fatalf("LICENSE %q, %v", got, err)
	}
	if _, err := Verify(dir); err != nil {
		t.Fatalf("the installed runtime does not verify: %v (in %s)", err, inst.Dir)
	}
}

func TestInstallRefusesABundleWhoseFileDiffersFromTheServers(t *testing.T) {
	top := bundleTop(t)
	bundle := []tarEntry{
		{name: top, typ: tar.TypeDir, mode: 0o755},
		{name: top + "LICENSE", typ: tar.TypeReg, body: "something else"},
		{name: top + "libcudart.so.12", typ: tar.TypeReg, body: "cudart"},
	}
	inst, _, err := installBundleEntries(t, bundle)
	if err == nil || !strings.Contains(err.Error(), "refusing to replace") {
		t.Fatalf("want the clash named, got %v", err)
	}
	if entries, _ := os.ReadDir(inst.Dir); len(entries) != 0 {
		t.Fatalf("a refused install left %v behind", entries)
	}
}
