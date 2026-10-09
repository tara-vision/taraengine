package cli

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/tara-vision/taraengine/internal/version"
)

func TestVersionPrintsTheVersion(t *testing.T) {
	previous := version.Version
	version.Version = "1.2.3"
	t.Cleanup(func() { version.Version = previous })

	var out bytes.Buffer
	root := NewRootCommand()
	root.SetOut(&out)
	root.SetArgs([]string{"version"})
	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if got, want := out.String(), "taraengine 1.2.3\n"; got != want {
		t.Fatalf("output %q, want %q", got, want)
	}
}

func TestRootHasTheGlobalFlags(t *testing.T) {
	root := NewRootCommand()
	for _, name := range []string{"host", "api-key", "config"} {
		if root.PersistentFlags().Lookup(name) == nil {
			t.Errorf("flag --%s missing", name)
		}
	}
	// The default is the listen address of the config file, which only the command knows once it has read the file.
	host := root.PersistentFlags().Lookup("host")
	if host.DefValue != "" || !strings.Contains(host.Usage, "listen address of the config") ||
		!strings.Contains(host.Usage, "http://127.0.0.1:8272") {
		t.Errorf("--host default %q, usage %q", host.DefValue, host.Usage)
	}
	if key := root.PersistentFlags().Lookup("api-key"); !strings.Contains(key.Usage, "TARAENGINE_API_KEY") ||
		!strings.Contains(key.Usage, "api_key of the config") {
		t.Errorf("--api-key usage %q", key.Usage)
	}
}

func TestExecuteReturnsTheErrorOfAnUnknownCommand(t *testing.T) {
	previous := os.Args
	os.Args = []string{"taraengine", "no-such-command"}
	t.Cleanup(func() { os.Args = previous })
	if err := Execute(); err == nil {
		t.Fatal("execute: want an error for an unknown command")
	}
}
