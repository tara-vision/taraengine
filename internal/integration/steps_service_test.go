//go:build integration

package integration

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/tara-vision/taraengine/internal/config"
	"github.com/tara-vision/taraengine/internal/registry"
	"github.com/tara-vision/taraengine/internal/runtime"
	"github.com/tara-vision/taraengine/pkg/client"
)

// staleBuild is a runtime build an earlier version left in the data directory.
const staleBuild = "b00001"

// TestServiceModeFetchesItsRuntime runs the daemon the way a service manager does: serve --service on a data directory
// that has no runtime, only a build an earlier version left. The daemon answers at once that it is starting, fetches
// the build this binary pins from the real upstream release, removes the old one, and then serves the test model.
func TestServiceModeFetchesItsRuntime(t *testing.T) {
	s := newServiceScenario(t, requireEnvironment(t), "stories260k")
	runSteps(t, s, []step{
		{"runtime", checkFetchedRuntime},
		{"pull", checkPull},
		{"load", checkLoad},
		{"chat", checkChat},
		{"stop", checkStopLoaded},
	})
}

// newServiceScenario is newScenario for a daemon that brings its own runtime: no runtime_dir in the config, a stale
// build in the data directory, and a listen address of its own, since --service keeps the listening line off stderr.
func newServiceScenario(t *testing.T, env environment, name string) *scenario {
	t.Helper()
	dataDir := filepath.Join(t.TempDir(), "Application Support", "taraengine")
	stale := filepath.Join(dataDir, "runtime", staleBuild)
	if err := os.MkdirAll(stale, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stale, "llama-server"), []byte("#!/bin/sh\n"), 0o750); err != nil {
		t.Fatal(err)
	}
	entry := testEntry(name)
	writeYAML(t, filepath.Join(dataDir, "registry.yaml"), registry.Registry{Entries: []registry.Entry{entry}})
	cfg := config.Default()
	cfg.Listen = freeLoopback(t)
	cfg.DataDir = dataDir
	cfg.Context = contextWindow
	cfg.LoadTimeout = loadTimeoutSeconds
	cfg.PresetGlobal = map[string]string{"n-gpu-layers": "0"}
	configPath := filepath.Join(dataDir, "config.yaml")
	writeYAML(t, configPath, cfg)
	d := startDaemonWith(t, env, dataDir, configPath, "http://"+cfg.Listen, "--service")
	return &scenario{env: env, d: d, c: client.New(d.baseURL, ""), entry: entry, dataDir: dataDir}
}

// freeLoopback is a loopback address nobody listens on right now.
func freeLoopback(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().String()
}

func checkFetchedRuntime(t *testing.T, s *scenario) {
	if len(s.d.seen) == 0 || s.d.seen[0] != "starting" {
		t.Fatalf("the daemon answers that it is starting before it has a runtime; health said %v", s.d.seen)
	}
	lock, err := runtime.LoadLock()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(s.dataDir, "runtime")
	if st := (&runtime.Installer{Dir: root}).Status(lock); !st.Verified {
		t.Fatalf("the pinned build is in the data directory and verifies: %+v", st)
	}
	if _, err := os.Stat(filepath.Join(root, staleBuild)); !os.IsNotExist(err) {
		t.Fatalf("the build the binary no longer pins is removed: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), callTimeout)
	defer cancel()
	if v, err := s.c.Version(ctx); err != nil || v.Runtime.Build != lock.Build {
		t.Fatalf("the daemon runs the fetched build: %+v %v", v, err)
	}
}
