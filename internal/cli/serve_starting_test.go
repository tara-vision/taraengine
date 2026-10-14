package cli

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tara-vision/taraengine/internal/backend/llamaserver"
	"github.com/tara-vision/taraengine/internal/config"
	"github.com/tara-vision/taraengine/internal/llamatest"
	"github.com/tara-vision/taraengine/pkg/api"
)

// healthOf is the health a daemon at base reports, or the zero value when nothing answers.
func healthOf(probe *http.Client, base string) api.Health {
	resp, err := probe.Get(base + "/api/health")
	if err != nil {
		return api.Health{}
	}
	defer func() { _ = resp.Body.Close() }()
	var h api.Health
	_ = json.NewDecoder(resp.Body).Decode(&h)
	return h
}

// healthOK reports whether the daemon at base is up and wired.
func healthOK(probe *http.Client, base string) bool {
	return healthOf(probe, base).Status == api.HealthOK
}

// holdWiring makes serve wait in its starting state until release is closed, and tells entered when it got there.
func holdWiring(t *testing.T) (entered <-chan struct{}, release chan<- struct{}) {
	t.Helper()
	in, out := make(chan struct{}), make(chan struct{})
	previous := wireDaemon
	wireDaemon = func(ctx context.Context, cfg config.Config, opts WireOptions) (*Wired, error) {
		close(in)
		select {
		case <-out:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return previous(ctx, cfg, opts)
	}
	t.Cleanup(func() { wireDaemon = previous })
	return in, out
}

// runServe runs the serve command in attach mode with the extra flags and returns where it listens, its data
// directory, a cancel function and the channel that carries what serve returned.
func runServe(t *testing.T, prepare func(dataDir string), extra ...string) (string, string, context.CancelFunc, <-chan error) {
	t.Helper()
	dataDir := t.TempDir()
	if prepare != nil {
		prepare(dataDir)
	}
	fake := llamatest.New(t, llamatest.Options{APIKey: "k", PresetPath: llamaserver.PresetPath(dataDir)})
	listen := freeAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		root := NewRootCommand()
		args := []string{"serve", "--listen", listen, "--data-dir", dataDir, "--attach", fake.URL(), "--attach-key", "k"}
		root.SetArgs(append(args, extra...))
		done <- root.ExecuteContext(ctx)
	}()
	t.Cleanup(cancel)
	return listen, dataDir, cancel, done
}

// eventually polls until cond holds, for at most five seconds.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for i := 0; !cond(); i++ {
		if i >= 200 {
			t.Fatalf("%s did not happen", what)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func TestServeAnswersStartingUntilTheDaemonIsWired(t *testing.T) {
	entered, release := holdWiring(t)
	listen, _, cancel, done := runServe(t, nil)
	base := "http://" + listen
	select {
	case <-entered:
	case err := <-done:
		t.Fatalf("serve ended before it wired: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("serve never began to wire")
	}
	probe := &http.Client{Timeout: 5 * time.Second}
	if h := healthOf(probe, base); h.Status != api.HealthStarting || h.Detail != "starting llama-server" {
		t.Fatalf("while it wires, health says %+v", h)
	}
	resp, body := get(t, base+"/api/models")
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") != "5" ||
		!strings.Contains(body, "taraengine is starting: starting llama-server") {
		t.Fatalf("while it wires, the models route answers %d %q %s", resp.StatusCode, resp.Header.Get("Retry-After"), body)
	}
	close(release)
	eventually(t, "the switch to the full handler", func() bool { return healthOK(probe, base) })
	if resp, body := get(t, base+"/api/models"); resp.StatusCode != http.StatusOK {
		t.Fatalf("once wired, the models route answers: %d %s", resp.StatusCode, body)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("serve returned %v", err)
	}
}

func TestServeRefusesADataDirectoryAnotherDaemonHolds(t *testing.T) {
	dataDir := t.TempDir()
	lock, err := llamaserver.LockDataDir(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lock.Close() })
	listen := freeAddr(t)
	_, err = run(t, "serve", "--listen", listen, "--data-dir", dataDir, "--attach", "http://127.0.0.1:1")
	if err == nil || !strings.Contains(err.Error(), "in use by another taraengine") {
		t.Fatalf("got %v", err)
	}
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		t.Fatalf("serve freed its address when it gave up: %v", err)
	}
	_ = ln.Close()
}

func TestServeLetsGoOfTheLockWhenWiringFails(t *testing.T) {
	dataDir := t.TempDir()
	_, err := run(t, "serve", "--listen", "127.0.0.1:0", "--data-dir", dataDir, "--attach", "http://127.0.0.1:1")
	if err == nil || !strings.Contains(err.Error(), "backend is not running") {
		t.Fatalf("got %v", err)
	}
	lock, err := llamaserver.LockDataDir(dataDir)
	if err != nil {
		t.Fatalf("a serve that failed to wire holds no lock: %v", err)
	}
	_ = lock.Close()
}

func TestAStopWhileStartingEndsServeCleanly(t *testing.T) {
	entered, _ := holdWiring(t)
	_, _, cancel, done := runServe(t, nil)
	<-entered
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("a stop asked for while starting is no failure: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not stop while starting")
	}
}

// captureStderr points os.Stderr at a file for the rest of the test and returns the file's path.
func captureStderr(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stderr.txt")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	previous := os.Stderr
	os.Stderr = f
	t.Cleanup(func() {
		os.Stderr = previous
		_ = f.Close()
	})
	return path
}

func TestServeServiceKeepsInfoLinesOffStderr(t *testing.T) {
	stderr := captureStderr(t)
	listen, dataDir, cancel, done := runServe(t, nil, "--service")
	probe := &http.Client{Timeout: 5 * time.Second}
	eventually(t, "the daemon coming up", func() bool { return healthOK(probe, "http://"+listen) })
	cancel()
	if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("serve returned %v", err)
	}
	shown, _ := os.ReadFile(stderr)
	if strings.Contains(string(shown), "taraengine listening") {
		t.Fatalf("a service keeps info lines off stderr: %q", shown)
	}
	logged, err := os.ReadFile(filepath.Join(dataDir, "logs", "taraengine.log"))
	if err != nil || !strings.Contains(string(logged), "taraengine listening") {
		t.Fatalf("the log file keeps them: %v %q", err, logged)
	}
}

func TestProgressSaysTheLastStepAndNothingBeforeTheFirst(t *testing.T) {
	var p progress
	if got := p.detail(); got != "" {
		t.Fatalf("no step yet, got %q", got)
	}
	p.set(stepChecking)
	p.set(stepStarting)
	if got := p.detail(); got != stepStarting {
		t.Fatalf("the last step set is the one reported, got %q", got)
	}
}
