package cli

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tara-vision/taraengine/internal/backend/llamaserver"
	"github.com/tara-vision/taraengine/internal/llamatest"
)

// bindLoopbackInstead makes serve bind a loopback port wherever its configuration says to listen, and reports which
// port, so that a daemon can run with a configuration that names an address which would open the machine to the
// network.
func bindLoopbackInstead(t *testing.T) <-chan string {
	t.Helper()
	bound := make(chan string, 1)
	previous := listenTCP
	listenTCP = func(string) (net.Listener, error) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err == nil {
			bound <- ln.Addr().String()
		}
		return ln, err
	}
	t.Cleanup(func() { listenTCP = previous })
	return bound
}

// serveLog runs the serve command with the flags until its API answers, stops it, and returns what it wrote to its
// log file.
func serveLog(t *testing.T, flags ...string) string {
	t.Helper()
	bound := bindLoopbackInstead(t)
	dataDir := t.TempDir()
	fake := llamatest.New(t, llamatest.Options{APIKey: "k", PresetPath: llamaserver.PresetPath(dataDir)})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		root := NewRootCommand()
		root.SetArgs(append([]string{"serve", "--data-dir", dataDir, "--attach", fake.URL(), "--attach-key", "k"}, flags...))
		done <- root.ExecuteContext(ctx)
	}()
	var addr string
	select {
	case addr = <-bound:
	case err := <-done:
		t.Fatalf("serve ended before it bound: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not bind")
	}
	probe := &http.Client{Timeout: 5 * time.Second}
	eventually(t, "the daemon coming up", func() bool { return healthOK(probe, "http://"+addr) })
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve returned %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not stop on cancel")
	}
	data, err := os.ReadFile(filepath.Join(dataDir, "logs", "taraengine.log"))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestServeLogsTheWarningForAnOpenListenAddressWithoutAKey(t *testing.T) {
	logged := serveLog(t, "--listen", "0.0.0.0:8272")
	for _, want := range []string{
		"level=WARN", "listening beyond localhost without an api_key; anyone on the network can use this engine",
		"listen=0.0.0.0:8272", "taraengine listening",
	} {
		if !strings.Contains(logged, want) {
			t.Errorf("the log lacks %q:\n%s", want, logged)
		}
	}
}

func TestServeLogsNoWarningWhenTheAddressIsLocalOrAKeyIsSet(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cfgFile, []byte("api_key: sekrit-daemon-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, flags := range map[string][]string{
		"a local address without a key": {"--listen", "127.0.0.1:8272"},
		"an open address with a key":    {"--listen", "0.0.0.0:8272", "--config", cfgFile},
	} {
		logged := serveLog(t, flags...)
		if strings.Contains(logged, "listening beyond localhost") {
			t.Errorf("%s: no warning was due:\n%s", name, logged)
		}
		if !strings.Contains(logged, "taraengine listening") || strings.Contains(logged, "sekrit-daemon-key") {
			t.Errorf("%s: the daemon logs that it listens, and never its key:\n%s", name, logged)
		}
	}
}
