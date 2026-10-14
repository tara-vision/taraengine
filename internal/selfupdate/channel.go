package selfupdate

import (
	"context"
	"path/filepath"
	"strings"
)

// Channel is how the running binary was installed, which decides how an update reaches it.
type Channel string

// The channels an update knows.
const (
	ChannelFile     Channel = "file"     // install.sh or a person put the binary there
	ChannelHomebrew Channel = "homebrew" // the cask tara-vision/taracode/taraengine
	ChannelDeb      Channel = "deb"
	ChannelRPM      Channel = "rpm"
)

// Runner runs a command and returns what it printed.
type Runner func(ctx context.Context, name string, args ...string) (string, error)

// Install is where the running binary lives and how it came there. Prefix is Homebrew's prefix for a cask.
type Install struct {
	Channel Channel
	Path    string
	Prefix  string
}

// DetectInstall tells the channel of the binary at exe from where it resolves to: a Homebrew Caskroom, or on Linux a
// file that dpkg or rpm owns. Anything else was put there as a file.
func DetectInstall(ctx context.Context, run Runner, goos, exe string) Install {
	resolved, err := filepath.EvalSymlinks(exe)
	if err != nil {
		resolved = exe
	}
	if prefix, _, found := strings.Cut(resolved, "/Caskroom/taraengine/"); found {
		return Install{Channel: ChannelHomebrew, Path: resolved, Prefix: prefix}
	}
	if goos == "linux" {
		if _, err := run(ctx, "dpkg-query", "-S", resolved); err == nil {
			return Install{Channel: ChannelDeb, Path: resolved}
		}
		if _, err := run(ctx, "rpm", "-qf", resolved); err == nil {
			return Install{Channel: ChannelRPM, Path: resolved}
		}
	}
	return Install{Channel: ChannelFile, Path: resolved}
}
