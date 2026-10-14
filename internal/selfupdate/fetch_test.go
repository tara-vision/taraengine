package selfupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// release serves the assets of v0.3.1 under /v0.3.1/, with a checksums.txt that lists sums for them; a name in
// wrong gets a sum that does not match.
func release(t *testing.T, assets map[string]string, wrong ...string) *Downloader {
	t.Helper()
	sums := ""
	for name, body := range assets {
		sum := sha256.Sum256([]byte(body))
		hexSum := hex.EncodeToString(sum[:])
		for _, w := range wrong {
			if w == name {
				hexSum = strings.Repeat("0", 64)
			}
		}
		sums += hexSum + "  " + name + "\n"
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/v0.3.1/")
		if name == "checksums.txt" {
			_, _ = w.Write([]byte(sums))
			return
		}
		body, ok := assets[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return &Downloader{HTTP: srv.Client(), Base: srv.URL}
}

func v031(t *testing.T) Version {
	t.Helper()
	v, err := ParseVersion("0.3.1")
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestFetchKeepsAnAssetWhoseChecksumMatches(t *testing.T) {
	d := release(t, map[string]string{"taraengine-linux-amd64": "the new binary"})
	dest := filepath.Join(t.TempDir(), "x")
	if err := d.Fetch(context.Background(), v031(t), "taraengine-linux-amd64", dest); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(dest); string(got) != "the new binary" {
		t.Fatalf("got %q", got)
	}
}

func TestFetchRefusesWhatItCannotVerify(t *testing.T) {
	d := release(t, map[string]string{"taraengine-linux-amd64": "tampered"}, "taraengine-linux-amd64")
	dest := filepath.Join(t.TempDir(), "x")
	err := d.Fetch(context.Background(), v031(t), "taraengine-linux-amd64", dest)
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("got %v", err)
	}
	if _, err := os.Stat(dest); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a download that does not match is removed")
	}
	if err := d.Fetch(context.Background(), v031(t), "taraengine-linux-arm64", dest); err == nil ||
		!strings.Contains(err.Error(), "no checksum for taraengine-linux-arm64") {
		t.Fatalf("an asset checksums.txt does not list: %v", err)
	}
	empty := release(t, map[string]string{})
	if err := empty.Fetch(context.Background(), v031(t), "checksums.txt", dest); err == nil {
		t.Fatal("checksums.txt does not vouch for itself")
	}
}

func TestFetchReportsDownloadsThatFail(t *testing.T) {
	d := release(t, map[string]string{"listed-only": "x"})
	dir := t.TempDir()
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if err := d.Fetch(context.Background(), v031(t), "listed-only", filepath.Join(dir, "x")); err == nil {
		t.Fatal("a destination that cannot be created")
	}
	gone := &Downloader{HTTP: http.DefaultClient, Base: "http://127.0.0.1:1"}
	if err := gone.Fetch(context.Background(), v031(t), "a", filepath.Join(t.TempDir(), "x")); err == nil {
		t.Fatal("a release that cannot be reached")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "checksums.txt") {
			sum := sha256.Sum256([]byte("x"))
			_, _ = w.Write([]byte(hex.EncodeToString(sum[:]) + "  a\n"))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	missing := &Downloader{HTTP: srv.Client(), Base: srv.URL}
	if err := missing.Fetch(context.Background(), v031(t), "a", filepath.Join(t.TempDir(), "x")); err == nil ||
		!strings.Contains(err.Error(), "404") {
		t.Fatalf("an asset that is listed but missing: %v", err)
	}
}

func TestReplacePutsTheNewBinaryInPlaceAtOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "taraengine")
	if err := os.WriteFile(path, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	err := Replace(path, func(dest string) error { return os.WriteFile(dest, []byte("new"), 0o600) })
	if err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	if got, _ := os.ReadFile(path); string(got) != "new" || info.Mode().Perm() != 0o755 {
		t.Fatalf("%q %v", got, info.Mode())
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatalf("nothing is left beside it: %v", entries)
	}
}

func TestReplaceLeavesTheOldBinaryWhenTheNewOneFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "taraengine")
	if err := os.WriteFile(path, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	err := Replace(path, func(dest string) error {
		_ = os.WriteFile(dest, []byte("half"), 0o600)
		return errors.New("checksum mismatch")
	})
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("got %v", err)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if got, _ := os.ReadFile(path); string(got) != "old" || len(entries) != 1 {
		t.Fatalf("the old binary stays, alone: %q %v", got, entries)
	}
}

func TestReplaceInADirectoryItCannotWriteIsAPermissionError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "taraengine")
	if err := os.WriteFile(path, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	write := func(dest string) error { return os.WriteFile(dest, []byte("new"), 0o600) }
	if err := Replace(path, write); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("got %v", err)
	}
}

func TestReplaceReportsAChmodOrRenameThatFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "taraengine")
	if err := os.Mkdir(path, 0o755); err != nil { // a directory where the binary goes cannot be renamed over
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "x"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Replace(path, func(dest string) error { return os.WriteFile(dest, []byte("new"), 0o600) }); err == nil {
		t.Fatal("a rename that fails")
	}
	if err := Replace(path, func(string) error { return nil }); err == nil {
		t.Fatal("a fetch that wrote nothing leaves nothing to make executable")
	}
}

func TestAssetNames(t *testing.T) {
	v := v031(t)
	if AssetName("linux", "arm64") != "taraengine-linux-arm64" ||
		PackageName(v, "amd64", "deb") != "taraengine_0.3.1_linux_amd64.deb" {
		t.Fatal(AssetName("linux", "arm64"), PackageName(v, "amd64", "deb"))
	}
	if d := DefaultDownloader(); d.Base != "https://github.com/tara-vision/taraengine/releases/download" || d.HTTP == nil {
		t.Fatalf("%+v", d)
	}
}
