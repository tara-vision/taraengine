package cli

import (
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/tara-vision/taraengine/internal/service"
)

// TestMain keeps every test away from the developer's own taraengine configuration and keys: no config file at the
// default location, no API key and no Hugging Face token in the environment. A test that needs one sets it itself. No
// test drives the machine's own launchd or systemd either: a test that needs a service manager sets a fake one.
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "taraengine-cli-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	for name, value := range map[string]string{"XDG_CONFIG_HOME": home, "TARAENGINE_API_KEY": "", "TARAENGINE_HF_TOKEN": ""} {
		_ = os.Setenv(name, value)
	}
	newServiceManager = func() (service.Manager, error) { return nil, errors.New("no service manager in the tests") }
	code := m.Run()
	_ = os.RemoveAll(home)
	os.Exit(code)
}
