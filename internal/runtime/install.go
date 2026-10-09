package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/tara-vision/taraengine/internal/pull"
)

// Manifest records what an installed runtime directory holds, written at install time.
type Manifest struct {
	Build         string    `json:"build"`
	Key           string    `json:"key"`
	TarballSHA256 string    `json:"tarball_sha256"`
	BinarySHA256  string    `json:"binary_sha256"`
	InstalledAt   time.Time `json:"installed_at"`
}

// Status is what `taraengine runtime status` and doctor show.
type Status struct {
	Installed bool
	Verified  bool
	Build     string
	Dir       string
	Binary    string
	Err       string
}

// Installer downloads, verifies and unpacks the pinned build into Dir/<build>.
type Installer struct {
	Dir        string
	Client     *http.Client
	BaseURL    string // the release base; tests point it at a local server
	OnProgress func(pull.Progress)
}

const manifestName = "manifest.json"

// renamePath is the rename the swap uses. Tests replace it to fail a chosen step.
var renamePath = os.Rename

// maxDownloadBytes caps one download. It is a variable so tests can lower it.
var maxDownloadBytes int64 = 4 << 30

// Binary is the server binary inside a runtime directory.
func Binary(dir string) string { return filepath.Join(dir, "llama-server") }

// Install fetches the platform's tarball (and the CUDA runtime bundle when asked and pinned), verifies
// each against the lock, unpacks them into a staging directory, checks that the bundle brought the CUDA
// runtime library, records the manifest and swaps the result into place. Any failure removes the staging
// directory and leaves the old install as it was. An old install that a crashed swap left in the aside
// directory is put back first.
func (i *Installer) Install(ctx context.Context, lock Lock, p Platform, withCudart bool) (string, error) {
	asset, err := lock.Asset(p)
	if err != nil {
		return "", err
	}
	dir := filepath.Join(i.Dir, lock.Build)
	tmp := dir + ".partial"
	aside := dir + ".old"
	_ = os.RemoveAll(tmp)
	if err := settleAside(dir, aside); err != nil {
		return "", err
	}
	if err := os.MkdirAll(tmp, 0o750); err != nil {
		return "", fmt.Errorf("runtime: %w", err)
	}
	if err := i.stage(ctx, lock, asset, p, tmp, withCudart); err != nil {
		_ = os.RemoveAll(tmp)
		return "", err
	}
	if err := swap(tmp, dir, aside); err != nil {
		_ = os.RemoveAll(tmp)
		return "", err
	}
	return dir, nil
}

// settleAside deals with what a swap that crashed left in the aside directory. A swap moves the old
// install to aside, moves the new one into place and only then removes aside, so a crash between the two
// renames leaves the old install in aside and nothing at dir. Then aside is the only good copy, and it goes
// back before anything else happens, so that an install that fails does not lose it. With an install at
// dir, aside is the leftover of a swap that did its work and is removed.
func settleAside(dir, aside string) error {
	if _, err := os.Lstat(aside); err != nil {
		return nil // no leftover
	}
	if _, err := os.Lstat(dir); errors.Is(err, os.ErrNotExist) {
		if err := renamePath(aside, dir); err != nil {
			return fmt.Errorf("runtime: restoring the install from %s: %w", aside, err)
		}
		return nil
	}
	_ = os.RemoveAll(aside) // best effort; the swap that follows needs the name
	return nil
}

// swap moves the staged install into dir. The old install moves to aside first, so a failed final
// rename can put it back, and the aside directory is removed once the swap has succeeded.
func swap(tmp, dir, aside string) error {
	moved := true
	if err := renamePath(dir, aside); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("runtime: %w", err)
		}
		moved = false
	}
	if err := renamePath(tmp, dir); err != nil {
		if moved {
			if rerr := renamePath(aside, dir); rerr != nil {
				return fmt.Errorf("runtime: %w; restoring the old install from %s failed: %v", err, aside, rerr)
			}
		}
		return fmt.Errorf("runtime: %w", err)
	}
	if moved {
		_ = os.RemoveAll(aside) // best effort; the next install removes a leftover
	}
	return nil
}

func (i *Installer) stage(ctx context.Context, lock Lock, asset Asset, p Platform, tmp string, withCudart bool) error {
	if err := i.fetchAndUnpack(ctx, lock, asset, tmp); err != nil {
		return err
	}
	if ca, ok := lock.CudartFor(p); ok && withCudart {
		if err := i.fetchAndUnpack(ctx, lock, ca, tmp); err != nil {
			return err
		}
		if err := requireCudart(tmp, p); err != nil {
			return err
		}
	}
	binSum, err := pull.SHA256File(Binary(tmp))
	if err != nil {
		return fmt.Errorf("runtime: %s holds no llama-server: %w", asset.Asset, err)
	}
	m := Manifest{
		Build: lock.Build, Key: p.Key(), TarballSHA256: asset.SHA256, BinarySHA256: binSum,
		InstalledAt: time.Now().UTC(),
	}
	// MarshalIndent cannot fail: a Manifest holds strings and a time.
	data, _ := json.MarshalIndent(m, "", "  ")
	// The manifest is public data and readable by everyone, like the rest of the runtime directory.
	if err := os.WriteFile(filepath.Join(tmp, manifestName), data, 0o644); err != nil { //nolint:gosec // see above
		return fmt.Errorf("runtime: %w", err)
	}
	return nil
}

// requireCudart checks that the bundle put the CUDA runtime library of the build's major into dir. The check
// is there because an unpack that lets an archive install nothing would report a bundle that is not there,
// and the first sign would be a model that quietly runs on the CPU.
func requireCudart(dir string, p Platform) error {
	lib := "libcudart.so." + strconv.Itoa(p.CUDAMajor())
	if info, err := os.Stat(filepath.Join(dir, lib)); err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("runtime: the CUDA bundle did not provide %s, so the build could not run on the GPU", lib)
	}
	return nil
}

func (i *Installer) fetchAndUnpack(ctx context.Context, lock Lock, a Asset, dest string) error {
	base := i.BaseURL
	if base == "" {
		base = releaseBase
	}
	tgz := filepath.Join(i.Dir, a.Asset+".download")
	if err := i.download(ctx, base+"/"+lock.Build+"/"+a.Asset, tgz); err != nil {
		return err
	}
	defer func() { _ = os.Remove(tgz) }()
	f, err := os.Open(tgz) //nolint:gosec // path built from the lock's asset name
	if err != nil {
		return fmt.Errorf("runtime: %w", err)
	}
	defer func() { _ = f.Close() }()
	// One open file serves both steps, so the bytes that are unpacked are the bytes that were hashed. The hash
	// reads through ReadAt and leaves the offset of f at the start for Unpack.
	sum, err := pull.SHA256Reader(io.NewSectionReader(f, 0, math.MaxInt64))
	if err != nil {
		return fmt.Errorf("runtime: %w", err)
	}
	if sum != a.SHA256 {
		return fmt.Errorf("runtime: %s: checksum mismatch: got %s, want %s", a.Asset, sum, a.SHA256)
	}
	return Unpack(f, dest)
}

// download fetches url into dest. A download that fails removes the partial file, and a body over
// maxDownloadBytes is refused. The context is the only time bound.
func (i *Installer) download(ctx context.Context, url, dest string) (err error) {
	client := i.Client
	if client == nil {
		client = &http.Client{}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("runtime: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("runtime: download %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("runtime: download %s: GitHub answered %d", url, resp.StatusCode)
	}
	if resp.ContentLength > maxDownloadBytes {
		return fmt.Errorf("runtime: download %s: %d bytes is over the %d byte cap", url, resp.ContentLength, maxDownloadBytes)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o750); err != nil {
		return fmt.Errorf("runtime: %w", err)
	}
	f, err := os.Create(dest) //nolint:gosec // path built from the lock's asset name
	if err != nil {
		return fmt.Errorf("runtime: %w", err)
	}
	defer func() {
		_ = f.Close()
		if err != nil {
			_ = os.Remove(dest)
		}
	}()
	return i.copyBody(f, resp.Body, resp.ContentLength, url)
}

// copyBody copies body into dst in blocks and reports the running total as progress. total is the length the
// server announced, or -1. A body that grows past maxDownloadBytes is refused whatever it announced.
func (i *Installer) copyBody(dst io.Writer, body io.Reader, total int64, url string) error {
	var done int64
	buf := make([]byte, 1<<20)
	for {
		n, rerr := body.Read(buf)
		if n > 0 {
			if done+int64(n) > maxDownloadBytes {
				return fmt.Errorf("runtime: download %s: body is over the %d byte cap", url, maxDownloadBytes)
			}
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return fmt.Errorf("runtime: %w", werr)
			}
			done += int64(n)
			if i.OnProgress != nil {
				i.OnProgress(pull.Progress{Stage: "downloading", Bytes: done, Total: total})
			}
		}
		if errors.Is(rerr, io.EOF) {
			return nil
		}
		if rerr != nil {
			return fmt.Errorf("runtime: download %s: %w", url, rerr)
		}
	}
}

// Verify reads a runtime directory's manifest and re-hashes llama-server against it.
func Verify(dir string) (Manifest, error) {
	data, err := os.ReadFile(filepath.Join(dir, manifestName)) //nolint:gosec // dir is the caller's runtime directory
	if err != nil {
		return Manifest{}, err
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return Manifest{}, fmt.Errorf("runtime: %s: %w", manifestName, err)
	}
	sum, err := pull.SHA256File(Binary(dir))
	if err != nil {
		return m, fmt.Errorf("runtime: %w", err)
	}
	if sum != m.BinarySHA256 {
		return m, fmt.Errorf("runtime: llama-server in %s does not match the manifest, run `taraengine runtime install`", dir)
	}
	return m, nil
}

// Status describes the lock's build on this machine: absent, installed, or installed and verified. A
// manifest that is present while a check fails is reported as installed, with the check's error.
func (i *Installer) Status(lock Lock) Status {
	dir := filepath.Join(i.Dir, lock.Build)
	st := Status{Build: lock.Build, Dir: dir, Binary: Binary(dir)}
	if _, err := os.Stat(filepath.Join(dir, manifestName)); errors.Is(err, os.ErrNotExist) {
		st.Err = "not installed, run `taraengine runtime install`"
		return st
	}
	st.Installed = true
	if _, err := Verify(dir); err != nil {
		st.Err = err.Error()
		return st
	}
	st.Verified = true
	return st
}
