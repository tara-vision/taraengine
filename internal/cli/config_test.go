package cli

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tara-vision/taraengine/internal/config"
)

// withConfigHome points the default config location at a fresh directory and returns the file it would read.
func withConfigHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", home)
	return filepath.Join(home, "taraengine", "config.yaml")
}

func TestConfigPathIsTheFlagOrTheDefaultLocation(t *testing.T) {
	def := withConfigHome(t)
	if got := configPath(&globalFlags{}); got != def || got != config.DefaultConfigPath() {
		t.Fatalf("default %q, want %q", got, def)
	}
	if got := configPath(&globalFlags{config: "/etc/taraengine.yaml"}); got != "/etc/taraengine.yaml" {
		t.Fatalf("flag %q", got)
	}
}

func TestLoadConfigMayMissTheDefaultFileButNotTheOneNamed(t *testing.T) {
	def := withConfigHome(t)
	cfg, err := loadConfig(&globalFlags{})
	if err != nil || cfg.Listen != config.Default().Listen {
		t.Fatalf("no file at the default location is the defaults: %+v %v", cfg, err)
	}
	missing := filepath.Join(t.TempDir(), "missing.yaml")
	_, err = loadConfig(&globalFlags{config: missing})
	if err == nil || !errors.Is(err, fs.ErrNotExist) || !strings.Contains(err.Error(), missing) {
		t.Fatalf("a file named with --config must exist: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(def), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(def, []byte("listen: 127.0.0.1:9999\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if cfg, err = loadConfig(&globalFlags{}); err != nil || cfg.Listen != "127.0.0.1:9999" {
		t.Fatalf("the default location is read when it exists: %+v %v", cfg, err)
	}
	named := filepath.Join(t.TempDir(), "named.yaml")
	if err := os.WriteFile(named, []byte("listen: 127.0.0.1:7777\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if cfg, err = loadConfig(&globalFlags{config: named}); err != nil || cfg.Listen != "127.0.0.1:7777" {
		t.Fatalf("the named file: %+v %v", cfg, err)
	}
}

func TestLoadConfigNamesTheKeyThatIsWrong(t *testing.T) {
	bad := filepath.Join(t.TempDir(), "bad.yaml")
	if err := os.WriteFile(bad, []byte("models_max: 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(&globalFlags{config: bad}); err == nil || !strings.Contains(err.Error(), "models_max") {
		t.Fatalf("%v", err)
	}
	if err := os.WriteFile(bad, []byte("listen: [\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(&globalFlags{config: bad}); err == nil || !strings.Contains(err.Error(), "parse") {
		t.Fatalf("%v", err)
	}
}

func TestTheEnvironmentOverridesTheConfigFile(t *testing.T) {
	withConfigHome(t)
	t.Setenv("TARAENGINE_API_KEY", "from-env")
	named := filepath.Join(t.TempDir(), "named.yaml")
	if err := os.WriteFile(named, []byte("api_key: from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if cfg, err := loadConfig(&globalFlags{config: named}); err != nil || cfg.APIKey != "from-env" {
		t.Fatalf("%+v %v", cfg, err)
	}
}
