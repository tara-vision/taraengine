package selfupdate

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// maxAsset caps one download; the largest asset, a package, is a few tens of megabytes.
const maxAsset = 1 << 30

// Downloader fetches the assets of a release and checks each against the release's checksums.txt.
type Downloader struct {
	HTTP *http.Client
	Base string
}

// DefaultDownloader fetches from the releases on GitHub.
func DefaultDownloader() *Downloader {
	return &Downloader{HTTP: &http.Client{Timeout: 30 * time.Minute},
		Base: "https://github.com/tara-vision/taraengine/releases/download"}
}

// AssetName is the binary of a release for a system.
func AssetName(goos, goarch string) string { return "taraengine-" + goos + "-" + goarch }

// PackageName is the deb or rpm of a release for a Linux architecture.
func PackageName(v Version, goarch, ext string) string {
	return "taraengine_" + v.String() + "_linux_" + goarch + "." + ext
}

// Fetch downloads the asset of release v into dest and keeps it only when its sha256 is the one checksums.txt lists
// for it. The check is the one install.sh makes: the sums come from the same release, over HTTPS.
func (d *Downloader) Fetch(ctx context.Context, v Version, asset, dest string) error {
	base := d.Base + "/v" + v.String() + "/"
	want, err := d.checksum(ctx, base+"checksums.txt", asset)
	if err != nil {
		return err
	}
	resp, err := getOK(ctx, d.HTTP, base+asset)
	if err != nil {
		return fmt.Errorf("selfupdate: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	f, err := os.OpenFile(dest, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600) //nolint:gosec // dest is ours, beside the binary
	if err != nil {
		return fmt.Errorf("selfupdate: %w", err)
	}
	hash := sha256.New()
	_, err = io.Copy(io.MultiWriter(f, hash), io.LimitReader(resp.Body, maxAsset))
	err = errors.Join(err, f.Close())
	if got := hex.EncodeToString(hash.Sum(nil)); err == nil && got != want {
		err = fmt.Errorf("checksum mismatch for %s: checksums.txt says %s, the download is %s", asset, want, got)
	}
	if err != nil {
		_ = os.Remove(dest)
		return fmt.Errorf("selfupdate: %w", err)
	}
	return nil
}

// checksum is the sha256 checksums.txt lists for asset.
func (d *Downloader) checksum(ctx context.Context, url, asset string) (string, error) {
	resp, err := getOK(ctx, d.HTTP, url)
	if err != nil {
		return "", fmt.Errorf("selfupdate: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	lines := bufio.NewScanner(io.LimitReader(resp.Body, maxAnswer))
	for lines.Scan() {
		sum, name, ok := strings.Cut(lines.Text(), "  ")
		if ok && name == asset && len(sum) == sha256.Size*2 {
			return sum, nil
		}
	}
	return "", fmt.Errorf("selfupdate: no checksum for %s in the release's checksums.txt", asset)
}

// Replace puts a new binary at path: fetch writes it next to path, it is made executable and renamed over path, so a
// failure at any step leaves the old binary as it was. A directory this process cannot write to answers with an error
// that matches os.ErrPermission.
func Replace(path string, fetch func(dest string) error) error {
	tmp := filepath.Join(filepath.Dir(path), ".taraengine-update-"+strconv.Itoa(os.Getpid()))
	err := fetch(tmp)
	if err == nil {
		err = os.Chmod(tmp, 0o755) //nolint:gosec // G302: a binary on PATH is executable by everyone
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("selfupdate: %w", err)
	}
	return nil
}
