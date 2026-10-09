// Package runtime installs and verifies the pinned upstream llama.cpp build this release runs on.
package runtime

import (
	_ "embed"
	"fmt"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

//go:embed runtime.lock.yaml
var embeddedLock []byte

const releaseBase = "https://github.com/ggml-org/llama.cpp/releases/download"

// Asset is one release tarball: its file name and sha256. Untested marks a platform we build and pin
// without a machine to run it on.
type Asset struct {
	Asset    string `yaml:"asset"`
	SHA256   string `yaml:"sha256"`
	Untested bool   `yaml:"untested,omitempty"`
}

// Lock pins one upstream build: the server tarball per platform key, and the CUDA runtime bundle per
// CUDA platform key for machines without the toolkit.
type Lock struct {
	Build  string           `yaml:"build"`
	Assets map[string]Asset `yaml:"assets"`
	Cudart map[string]Asset `yaml:"cudart"`
}

var (
	buildPattern = regexp.MustCompile(`^b[0-9]+$`)
	sha256Hex    = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// LoadLock parses the embedded lock.
func LoadLock() (Lock, error) { return ParseLock(embeddedLock) }

// ParseLock validates a lock: a build like b11514, at least one asset, every asset named for that
// build as a whole token (so b1 never matches b11514) with a sha256. An asset name is a plain file
// name: it becomes part of a URL and of a path in the data directory, so a slash, a backslash or ".."
// in it is refused.
func ParseLock(data []byte) (Lock, error) {
	var l Lock
	if err := yaml.Unmarshal(data, &l); err != nil {
		return Lock{}, fmt.Errorf("runtime: parse lock: %w", err)
	}
	if !buildPattern.MatchString(l.Build) {
		return Lock{}, fmt.Errorf("runtime: lock build %q is not an upstream build number", l.Build)
	}
	if len(l.Assets) == 0 {
		return Lock{}, fmt.Errorf("runtime: lock has no assets")
	}
	for _, group := range []map[string]Asset{l.Assets, l.Cudart} {
		for key, a := range group {
			if strings.ContainsAny(a.Asset, "/\\") || strings.Contains(a.Asset, "..") {
				return Lock{}, fmt.Errorf("runtime: asset %q for %s is not a plain file name", a.Asset, key)
			}
			if !strings.Contains(a.Asset, "-"+l.Build+"-") {
				return Lock{}, fmt.Errorf("runtime: asset %s for %s does not belong to build %s", a.Asset, key, l.Build)
			}
			if !sha256Hex.MatchString(a.SHA256) {
				return Lock{}, fmt.Errorf("runtime: asset %s has no valid sha256", a.Asset)
			}
		}
	}
	return l, nil
}

// URL is where the asset is downloaded from.
func (l Lock) URL(a Asset) string { return releaseBase + "/" + l.Build + "/" + a.Asset }

// Asset is the server tarball for a platform.
func (l Lock) Asset(p Platform) (Asset, error) {
	a, ok := l.Assets[p.Key()]
	if !ok {
		return Asset{}, fmt.Errorf("runtime: build %s has no asset for %s", l.Build, p.Key())
	}
	return a, nil
}

// CudartFor is the CUDA runtime bundle for a platform, when one exists.
func (l Lock) CudartFor(p Platform) (Asset, bool) {
	a, ok := l.Cudart[p.Key()]
	return a, ok
}
