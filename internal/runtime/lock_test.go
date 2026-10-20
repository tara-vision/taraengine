package runtime

import (
	"strings"
	"testing"
)

func TestEmbeddedLock(t *testing.T) {
	l, err := LoadLock()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if l.Build != "b11750" {
		t.Fatalf("build %s", l.Build)
	}
	for _, key := range []string{"darwin/arm64", "linux/amd64/cuda13", "linux/amd64/cuda12", "linux/amd64/cpu", "linux/arm64/cuda13", "linux/arm64/cpu"} {
		if _, ok := l.Assets[key]; !ok {
			t.Errorf("asset %s missing", key)
		}
	}
	for _, key := range []string{"linux/amd64/cuda13", "linux/amd64/cuda12", "linux/arm64/cuda13"} {
		if _, ok := l.Cudart[key]; !ok {
			t.Errorf("cudart %s missing", key)
		}
	}
	a := l.Assets["darwin/arm64"]
	if got := l.URL(a); got != "https://github.com/ggml-org/llama.cpp/releases/download/b11750/llama-b11750-bin-macos-arm64.tar.gz" {
		t.Fatalf("url %s", got)
	}
	if _, err := l.Asset(Platform{OS: "windows", Arch: "amd64", Accel: "cpu"}); err == nil {
		t.Fatal("unknown platform must fail")
	}
	if _, ok := l.CudartFor(Platform{OS: "darwin", Arch: "arm64", Accel: "metal"}); ok {
		t.Fatal("macOS has no cudart bundle")
	}
}

func TestParseLockRejects(t *testing.T) {
	good := "build: b1\nassets:\n  darwin/arm64:\n    asset: llama-b1-bin-macos-arm64.tar.gz\n    sha256: " + strings.Repeat("a", 64) + "\n"
	if _, err := ParseLock([]byte(good)); err != nil {
		t.Fatalf("good lock refused: %v", err)
	}
	cases := []struct{ name, body, want string }{
		{"bad build", strings.Replace(good, "build: b1", "build: latest", 1), "build"},
		{"bad sha", strings.Replace(good, strings.Repeat("a", 64), "abc", 1), "sha256"},
		{"asset of another build", strings.Replace(good, "llama-b1-bin", "llama-b2-bin", 1), "does not belong"},
		{"build is a prefix of the asset build", strings.Replace(strings.Replace(good, "build: b1", "build: b1175", 1), "llama-b1-bin", "llama-b11750-bin", 1), "does not belong"},
		{"build b1 against an asset of b11750", strings.Replace(good, "llama-b1-bin", "llama-b11750-bin", 1), "does not belong"},
		{"asset name with a slash", strings.Replace(good, "llama-b1-bin", "sub/llama-b1-bin", 1), "plain file name"},
		{"asset name that climbs", strings.Replace(good, "llama-b1-bin", "../llama-b1-bin", 1), "plain file name"},
		{"asset name with dot dot inside", strings.Replace(good, "llama-b1-bin", "llama-b1-bin..", 1), "plain file name"},
		{"asset name with a backslash", strings.Replace(good, "llama-b1-bin", "llama-b1-bin\\", 1), "plain file name"},
		{"cudart name with a slash", good + "cudart:\n  linux/amd64/cuda12:\n    asset: x/cudart-llama-b1-bin.tar.gz\n    sha256: " +
			strings.Repeat("a", 64) + "\n", "plain file name"},
		{"no assets", "build: b1\n", "no assets"},
		{"not yaml", "build: [", "parse"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseLock([]byte(tc.body)); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestAssetForSupportedPlatform(t *testing.T) {
	l, err := LoadLock()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	a, err := l.Asset(Platform{OS: "darwin", Arch: "arm64", Accel: "metal"})
	if err != nil {
		t.Fatalf("darwin/arm64 asset: %v", err)
	}
	if want := l.Assets["darwin/arm64"]; a != want {
		t.Fatalf("asset %+v, want the lock's darwin/arm64 entry %+v", a, want)
	}
}

func TestEverySelectedPlatformHasAnAsset(t *testing.T) {
	l, err := LoadLock()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	picks := []struct{ goos, goarch, driver string }{
		{"darwin", "arm64", ""},
		{"linux", "amd64", "580.65.06"},
		{"linux", "amd64", "570.133"},
		{"linux", "amd64", ""},
		{"linux", "arm64", "580.1"},
		{"linux", "arm64", ""},
	}
	for _, pick := range picks {
		p, err := Select(pick.goos, pick.goarch, pick.driver)
		if err != nil {
			t.Fatalf("select %s/%s: %v", pick.goos, pick.goarch, err)
		}
		if _, err := l.Asset(p); err != nil {
			t.Errorf("%s: %v", p.Key(), err)
		}
	}
}
