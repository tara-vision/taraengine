package cli

import (
	"fmt"
	"os"
	"testing"
)

// TestMain keeps every test away from the developer's own taraengine configuration and keys: no config file at the
// default location, no API key and no Hugging Face token in the environment. A test that needs one sets it itself.
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "taraengine-cli-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	for name, value := range map[string]string{"XDG_CONFIG_HOME": home, "TARAENGINE_API_KEY": "", "TARAENGINE_HF_TOKEN": ""} {
		_ = os.Setenv(name, value)
	}
	code := m.Run()
	_ = os.RemoveAll(home)
	os.Exit(code)
}
