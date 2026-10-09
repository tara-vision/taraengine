package config

import (
	"os"
	"path/filepath"
	"runtime"
)

// DefaultConfigPath is $XDG_CONFIG_HOME/taraengine/config.yaml, or ~/.config/taraengine/config.yaml.
func DefaultConfigPath() string {
	return filepath.Join(configHome(), "taraengine", "config.yaml")
}

// DefaultDataDir is ~/Library/Application Support/taraengine on macOS, and $XDG_DATA_HOME/taraengine
// or ~/.local/share/taraengine elsewhere.
func DefaultDataDir() string {
	return dataDir(runtime.GOOS, os.Getenv("XDG_DATA_HOME"), homeDir())
}

func dataDir(goos, xdgData, home string) string {
	if goos == "darwin" {
		return filepath.Join(home, "Library", "Application Support", "taraengine")
	}
	if xdgData != "" {
		return filepath.Join(xdgData, "taraengine")
	}
	return filepath.Join(home, ".local", "share", "taraengine")
}

func configHome() string {
	if v := os.Getenv("XDG_CONFIG_HOME"); v != "" {
		return v
	}
	return filepath.Join(homeDir(), ".config")
}

func homeDir() string {
	if h, err := os.UserHomeDir(); err == nil {
		return h
	}
	return "."
}
