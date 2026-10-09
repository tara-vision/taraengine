package config

import (
	"path/filepath"
	"testing"
)

func TestDataDirPerOS(t *testing.T) {
	if got := dataDir("darwin", "/xdg", "/Users/me"); got != filepath.Join("/Users/me", "Library", "Application Support", "taraengine") {
		t.Errorf("darwin: %s", got)
	}
	if got := dataDir("linux", "/xdg", "/home/me"); got != "/xdg/taraengine" {
		t.Errorf("linux with XDG_DATA_HOME: %s", got)
	}
	if got := dataDir("linux", "", "/home/me"); got != "/home/me/.local/share/taraengine" {
		t.Errorf("linux without XDG_DATA_HOME: %s", got)
	}
}

func TestDefaultConfigPathHonorsXDG(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/cfg")
	if got := DefaultConfigPath(); got != "/cfg/taraengine/config.yaml" {
		t.Errorf("got %s", got)
	}
	t.Setenv("XDG_CONFIG_HOME", "")
	if got := DefaultConfigPath(); filepath.Base(got) != "config.yaml" {
		t.Errorf("got %s", got)
	}
}

func TestHomeDirFallsBackToTheCurrentDirectory(t *testing.T) {
	t.Setenv("HOME", "")
	if got := homeDir(); got != "." {
		t.Errorf("homeDir() = %q, want %q", got, ".")
	}
}
