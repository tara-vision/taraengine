package selfupdate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// owns is a package manager that owns the files of its set: dpkg-query and rpm answer for them alone.
func owns(tool string, files ...string) Runner {
	return func(_ context.Context, name string, args ...string) (string, error) {
		if name != tool {
			return "", errors.New("not found")
		}
		for _, f := range files {
			if args[len(args)-1] == f {
				return "taraengine: " + f, nil
			}
		}
		return "", errors.New("no package owns it")
	}
}

func writeExe(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestDetectChannel(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cask := filepath.Join(dir, "homebrew", "Caskroom", "taraengine", "0.3.0", "taraengine-darwin-arm64")
	link := filepath.Join(dir, "homebrew", "bin", "taraengine")
	plain := filepath.Join(dir, "usr", "local", "bin", "taraengine")
	packaged := filepath.Join(dir, "usr", "bin", "taraengine")
	for _, p := range []string{cask, plain, packaged} {
		writeExe(t, p)
	}
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(cask, link); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, c := range []struct {
		name, goos, exe string
		run             Runner
		want            Install
	}{
		{"a cask", "darwin", link, owns("none"), Install{Channel: ChannelHomebrew, Path: cask, Prefix: filepath.Join(dir, "homebrew")}},
		{"install.sh on a Mac", "darwin", plain, owns("none"), Install{Channel: ChannelFile, Path: plain}},
		{"a deb", "linux", packaged, owns("dpkg-query", packaged), Install{Channel: ChannelDeb, Path: packaged}},
		{"an rpm", "linux", packaged, owns("rpm", packaged), Install{Channel: ChannelRPM, Path: packaged}},
		{"install.sh on Linux", "linux", plain, owns("dpkg-query", packaged), Install{Channel: ChannelFile, Path: plain}},
		{"a path that does not resolve", "linux", filepath.Join(dir, "gone"), owns("none"),
			Install{Channel: ChannelFile, Path: filepath.Join(dir, "gone")}},
	} {
		if got := DetectInstall(ctx, c.run, c.goos, c.exe); got != c.want {
			t.Errorf("%s: %+v, want %+v", c.name, got, c.want)
		}
	}
}
