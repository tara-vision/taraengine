package llamaserver

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tara-vision/taraengine/internal/backend"
	"github.com/tara-vision/taraengine/internal/llamatest"
)

// fakeProcess is the supervised child as the backend sees it. No process runs; the scripted router
// plays the part of the router the child would start.
type fakeProcess struct {
	mu        sync.Mutex
	opts      SupervisorOptions
	startErr  error
	pid       int
	running   bool
	unhealthy bool
	starts    int
	stops     int
}

func (p *fakeProcess) Start(context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.starts++
	if p.startErr != nil {
		return p.startErr
	}
	p.running = true
	return nil
}

func (p *fakeProcess) Stop(context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stops++
	p.running = false
	return nil
}

func (p *fakeProcess) PID() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.pid
}

func (p *fakeProcess) Running() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.running
}

func (p *fakeProcess) Unhealthy() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.unhealthy
}

func (p *fakeProcess) set(running, unhealthy bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.running, p.unhealthy = running, unhealthy
}

func (p *fakeProcess) counts() (starts, stops int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.starts, p.stops
}

func (p *fakeProcess) options() SupervisorOptions {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.opts
}

// supervised is a backend in spawn mode over a fake process and a scripted router.
type supervised struct {
	b          *Backend
	proc       *fakeProcess
	fake       *llamatest.Server
	probe      *fakeProbe
	dataDir    string
	runtimeDir string
}

func portOfURL(t *testing.T, raw string) int {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatal(err)
	}
	return port
}

func newSupervised(t *testing.T) *supervised {
	t.Helper()
	dataDir, runtimeDir := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(runtimeDir, "llama-server"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	fake := llamatest.New(t, llamatest.Options{APIKey: "secret", PresetPath: PresetPath(dataDir), ModelsMax: 1})
	probe := &fakeProbe{mem: backend.Memory{Bytes: 5, Source: "measured"}}
	proc := &fakeProcess{pid: 7777}
	b, err := New(Options{
		RuntimeDir: runtimeDir, DataDir: dataDir, Port: portOfURL(t, fake.URL()), APIKey: "secret", Build: "bTEST",
		SleepIdleSeconds: 900, Poll: 10 * time.Millisecond, State: testStore(dataDir), Probe: probe, Logger: quietLogger(),
		Models: func(context.Context, map[string]int) ([]PresetModel, error) {
			return []PresetModel{{Name: "a", ModelPath: "/m/a.gguf", Context: 4096}}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	b.newProcess = func(o SupervisorOptions) childProcess {
		proc.mu.Lock()
		proc.opts = o
		proc.mu.Unlock()
		return proc
	}
	return &supervised{b: b, proc: proc, fake: fake, probe: probe, dataDir: dataDir, runtimeDir: runtimeDir}
}

func TestStartGivesTheChildItsOwnEnvironmentAndAPIDFile(t *testing.T) {
	t.Setenv("LD_LIBRARY_PATH", "")
	t.Setenv("DYLD_LIBRARY_PATH", "/opt/lib")
	t.Setenv("TARAENGINE_API_KEY", "the-daemons-own-key")
	s := newSupervised(t)
	if err := s.b.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	o := s.proc.options()
	wantEnv := []string{
		"LD_LIBRARY_PATH=" + s.runtimeDir, "DYLD_LIBRARY_PATH=" + s.runtimeDir + string(os.PathListSeparator) + "/opt/lib",
		"LLAMA_CACHE=" + CachePath(s.dataDir),
	}
	if len(o.Env) < 3 || !slices.Equal(o.Env[len(o.Env)-3:], wantEnv) || strings.Contains(strings.Join(o.Env, " "), "the-daemons-own-key") {
		t.Fatalf("env\n got %v\nwant it to end in %v, with no key of the daemon's", o.Env, wantEnv)
	}
	if entries, err := os.ReadDir(CachePath(s.dataDir)); err != nil || len(entries) != 0 {
		t.Fatalf("the router's cache is an empty directory of the engine's: %v %v", entries, err)
	}
	if info, err := os.Stat(CachePath(s.dataDir)); err != nil || info.Mode().Perm()&^0o750 != 0 {
		t.Fatalf("cache directory %v: %v", info, err)
	}
	if o.PIDFile != PIDPath(s.dataDir) {
		t.Fatalf("pid file %q", o.PIDFile)
	}
}

func TestStartSpawnsTheChildWithAKeyFile(t *testing.T) {
	s := newSupervised(t)
	keyPath := KeyPath(s.dataDir)
	if err := os.WriteFile(keyPath, []byte("stale\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := s.b.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	if starts, _ := s.proc.counts(); starts != 1 {
		t.Fatalf("starts %d", starts)
	}
	o := s.proc.options()
	wantArgs := []string{
		"--models-preset", PresetPath(s.dataDir),
		"--host", "127.0.0.1", "--port", strconv.Itoa(s.b.opts.Port),
		"--api-key-file", keyPath,
		"--models-max", "1", "--sleep-idle-seconds", "900",
		"--no-webui", "--log-verbosity", "2",
	}
	if !slices.Equal(o.Args, wantArgs) {
		t.Fatalf("args\n got %v\nwant %v", o.Args, wantArgs)
	}
	if o.Binary != filepath.Join(s.runtimeDir, "llama-server") || o.LogPath != LogPath(s.dataDir) {
		t.Fatalf("binary %q, log %q", o.Binary, o.LogPath)
	}
	if o.Health == nil || o.Health(ctx) != nil {
		t.Fatal("the supervisor's health probe must reach the router with the child's key")
	}
	if strings.Contains(strings.Join(o.Args, " "), "secret") {
		t.Fatalf("the key must not show on the command line: %v", o.Args)
	}
	data, err := os.ReadFile(keyPath)
	if err != nil || string(data) != "secret\n" {
		t.Fatalf("key file %q: %v", data, err)
	}
	if info, err := os.Stat(keyPath); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("the key file must be private, even when an older file was looser: %v %v", info, err)
	}
	if info, err := os.Stat(filepath.Dir(LogPath(s.dataDir))); err != nil || !info.IsDir() {
		t.Fatalf("log directory: %v %v", info, err)
	}
	if a := statusOf(t, s.b, "a"); a.Status != backend.StatusPulled {
		t.Fatalf("a: %+v", a)
	}
	if err := s.b.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if _, stops := s.proc.counts(); stops != 1 {
		t.Fatalf("stops %d", stops)
	}
}

func TestLoadMeasuresTheSupervisedProcessTree(t *testing.T) {
	s := newSupervised(t)
	if err := s.b.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.b.Load(context.Background(), "a", backend.LoadOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(s.probe.pids) != 1 || s.probe.pids[0] != 7777 {
		t.Fatalf("the probe must measure the supervised child, got %v", s.probe.pids)
	}
}

func TestStartFailures(t *testing.T) {
	broken := errors.New("catalog unreadable")
	rows := []struct {
		name   string
		mutate func(t *testing.T, s *supervised)
		want   string
		starts int
		stops  int
	}{
		{"the state file is corrupt", func(t *testing.T, s *supervised) { writeTestFile(t, s.b.opts.State.Path, "{") }, "state", 0, 0},
		{"the catalog cannot be read", func(_ *testing.T, s *supervised) {
			s.b.opts.Models = func(context.Context, map[string]int) ([]PresetModel, error) { return nil, broken }
		}, "catalog unreadable", 0, 0},
		{"the presets are refused", func(_ *testing.T, s *supervised) {
			s.b.opts.Models = func(context.Context, map[string]int) ([]PresetModel, error) {
				return []PresetModel{{Name: "a", ModelPath: "/m/a.gguf", Extra: map[string]string{"x=y": "1"}}}, nil
			}
		}, "preset key", 0, 0},
		{"the data directory cannot be created", func(t *testing.T, s *supervised) {
			blocker := filepath.Join(t.TempDir(), "file")
			writeTestFile(t, blocker, "x")
			s.b.opts.DataDir = filepath.Join(blocker, "data")
		}, "not a directory", 0, 0},
		{"the runtime binary is missing", func(t *testing.T, s *supervised) {
			if err := os.Remove(filepath.Join(s.runtimeDir, "llama-server")); err != nil {
				t.Fatal(err)
			}
		}, "llama-server", 0, 0},
		{"the log directory cannot be created", func(t *testing.T, s *supervised) { writeTestFile(t, filepath.Join(s.dataDir, "logs"), "x") }, "not a directory", 0, 0},
		{"the cache directory cannot be created", func(t *testing.T, s *supervised) { writeTestFile(t, CachePath(s.dataDir), "x") }, "not a directory", 0, 0},
		{"the key file cannot be written", func(t *testing.T, s *supervised) {
			if err := os.Mkdir(KeyPath(s.dataDir), 0o750); err != nil {
				t.Fatal(err)
			}
		}, "child key", 0, 0},
		{"the child will not start", func(_ *testing.T, s *supervised) {
			s.proc.startErr = errors.New("exited before it became healthy")
		}, "exited before it became healthy", 1, 0},
		{"the router refuses the first sync", func(_ *testing.T, s *supervised) { s.b.client.APIKey = "wrong" }, "401", 1, 1},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			s := newSupervised(t)
			row.mutate(t, s)
			err := s.b.Start(context.Background())
			if err == nil || !strings.Contains(err.Error(), row.want) {
				t.Fatalf("want an error with %q, got %v", row.want, err)
			}
			if starts, stops := s.proc.counts(); starts != row.starts || stops != row.stops {
				t.Fatalf("child starts %d and stops %d, want %d and %d: a failed start leaves no child behind", starts, stops, row.starts, row.stops)
			}
		})
	}
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestHealthFollowsTheChild(t *testing.T) {
	s := newSupervised(t)
	ctx := context.Background()
	if err := s.b.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if h, err := s.b.Health(ctx); err != nil || !h.OK || h.Backend != "llama.cpp" || h.Build != "bTEST" {
		t.Fatalf("running: %+v %v", h, err)
	}
	s.proc.set(false, false)
	if h, _ := s.b.Health(ctx); h.OK || !strings.Contains(h.Detail, "not running") {
		t.Fatalf("not running: %+v", h)
	}
	s.proc.set(false, true)
	if h, _ := s.b.Health(ctx); h.OK || !strings.Contains(h.Detail, "five times") {
		t.Fatalf("unhealthy: %+v", h)
	}
	s.proc.set(true, false)
	s.fake.Close()
	if h, _ := s.b.Health(ctx); h.OK || h.Detail == "" {
		t.Fatalf("router gone: %+v", h)
	}
}

func TestProxyAnswers503WhileTheChildIsDown(t *testing.T) {
	s := newSupervised(t)
	if err := s.b.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	s.proc.set(false, false)
	rec := httptest.NewRecorder()
	s.b.Proxy().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "not running") || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("%d %v %s", rec.Code, rec.Header(), rec.Body.String())
	}
	if n := countRequests(s.fake, "GET /v1/models"); n != 0 {
		t.Fatalf("nothing may reach a router that is being restarted, saw %d", n)
	}
	s.proc.set(true, false)
	rec = httptest.NewRecorder()
	s.b.Proxy().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("after the restart: %d %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Taraengine-Version"); got != "" {
		t.Fatalf("no version configured, so no header: %q", got)
	}
}

func TestStopWithoutAChildIsQuiet(t *testing.T) {
	dataDir := t.TempDir()
	b, err := New(Options{DataDir: dataDir, Attach: "http://127.0.0.1:1", State: testStore(dataDir), Models: emptyCatalog, SleepIdleSeconds: 900})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Stop(context.Background()); err != nil {
		t.Fatalf("stopping an attached backend must be a no-op: %v", err)
	}
}

// TestStartDrivesTheRealSupervisor starts the test binary as the child: started under the name
// llama-server it runs in the helper mode of TestMain, serves /health and nothing else, so the router
// sync that follows fails and the start is undone. Whatever happens, the cleanup stops the supervisor
// and waits until every child it started is gone.
func TestStartDrivesTheRealSupervisor(t *testing.T) {
	dataDir, runtimeDir := t.TempDir(), t.TempDir()
	if err := os.Symlink(os.Args[0], filepath.Join(runtimeDir, "llama-server")); err != nil {
		t.Fatal(err)
	}
	rec := &logRecorder{}
	b, err := New(Options{
		RuntimeDir: runtimeDir, DataDir: dataDir, State: testStore(dataDir), Models: emptyCatalog, Logger: slog.New(rec), SleepIdleSeconds: 900,
	})
	if err != nil {
		t.Fatal(err)
	}
	var sup *Supervisor
	build := b.newProcess // the real constructor, wrapped only to keep hold of what it builds
	b.newProcess = func(o SupervisorOptions) childProcess {
		built := build(o)
		sup = built.(*Supervisor)
		t.Cleanup(func() {
			stopAndWait(t, sup)
			for _, pid := range rec.startedPIDs() {
				awaitProcessGone(t, pid, 10*time.Second)
			}
		})
		return built
	}
	err = b.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("the helper has no /models, so the sync must fail with its 404: %v", err)
	}
	if sup == nil || sup.Running() || b.supervisor() != nil {
		t.Fatal("a failed start must not leave the child running, nor keep it")
	}
	log, err := os.ReadFile(LogPath(dataDir))
	if err != nil || !strings.Contains(string(log), "helper started") {
		t.Fatalf("the child's output must reach the log file: %q %v", log, err)
	}
	if _, err := os.Stat(PIDPath(dataDir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the stop that undid the start removes the pid file: %v", err)
	}
}

func TestStartRefusesASecondStart(t *testing.T) {
	s := newSupervised(t)
	ctx := context.Background()
	if err := s.b.Start(ctx); err != nil {
		t.Fatal(err)
	}
	err := s.b.Start(ctx)
	if err == nil || !strings.Contains(err.Error(), "already started") {
		t.Fatalf("a second start must be refused: %v", err)
	}
	if starts, _ := s.proc.counts(); starts != 1 || !s.proc.Running() {
		t.Fatalf("the first child must be left alone: starts %d, running %v", starts, s.proc.Running())
	}
}

func TestStopThenStartBuildsAFreshChild(t *testing.T) {
	s := newSupervised(t)
	var procs []*fakeProcess
	s.b.newProcess = func(o SupervisorOptions) childProcess {
		p := &fakeProcess{pid: 7000 + len(procs), opts: o}
		procs = append(procs, p)
		return p
	}
	ctx := context.Background()
	if err := s.b.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.b.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if s.b.supervisor() != nil {
		t.Fatal("a stopped backend holds no supervisor")
	}
	if err := s.b.Start(ctx); err != nil {
		t.Fatalf("a stopped backend can start again: %v", err)
	}
	if len(procs) != 2 || s.b.supervisor() != childProcess(procs[1]) {
		t.Fatalf("the second start needs a supervisor of its own, got %d", len(procs))
	}
	if _, stops := procs[0].counts(); stops != 1 {
		t.Fatalf("the first child was stopped %d times", stops)
	}
	if starts, _ := procs[1].counts(); starts != 1 {
		t.Fatalf("the second child was started %d times", starts)
	}
}

func TestAChildThatFailedToStartCanBeTriedAgain(t *testing.T) {
	s := newSupervised(t)
	s.proc.startErr = errors.New("exited before it became healthy")
	ctx := context.Background()
	if err := s.b.Start(ctx); err == nil {
		t.Fatal("the failing child must fail the start")
	}
	if s.b.supervisor() != nil {
		t.Fatal("a child that never ran must not be kept as the backend's")
	}
	s.proc.startErr = nil
	if err := s.b.Start(ctx); err != nil {
		t.Fatalf("the retry: %v", err)
	}
	if starts, _ := s.proc.counts(); starts != 2 {
		t.Fatalf("starts %d", starts)
	}
}

func TestKeyFileReplacesALinkInsteadOfFollowingIt(t *testing.T) {
	s := newSupervised(t)
	victim := filepath.Join(t.TempDir(), "victim")
	writeTestFile(t, victim, "precious\n")
	if err := os.Symlink(victim, KeyPath(s.dataDir)); err != nil {
		t.Fatal(err)
	}
	if err := s.b.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(victim); string(data) != "precious\n" {
		t.Fatalf("the file behind a link at the key path must not be written: %q", data)
	}
	info, err := os.Lstat(KeyPath(s.dataDir))
	if err != nil || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o600 {
		t.Fatalf("the key path must hold a private file of its own: %v %v", info, err)
	}
	if key, _ := os.ReadFile(KeyPath(s.dataDir)); string(key) != "secret\n" {
		t.Fatalf("key %q", key)
	}
}

func TestKeyFileLeavesNoTemporaryFiles(t *testing.T) {
	s := newSupervised(t)
	leftovers := func() []string {
		names, _ := filepath.Glob(filepath.Join(s.dataDir, ".child.key-*"))
		return names
	}
	ctx := context.Background()
	if err := s.b.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.b.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if names := leftovers(); len(names) != 0 {
		t.Fatalf("after a good write: %v", names)
	}
	if err := os.Remove(KeyPath(s.dataDir)); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(KeyPath(s.dataDir), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := s.b.Start(ctx); err == nil || !strings.Contains(err.Error(), "child key") {
		t.Fatalf("a key file that cannot be replaced must fail the start: %v", err)
	}
	if names := leftovers(); len(names) != 0 {
		t.Fatalf("after a refused rename: %v", names)
	}
}

func TestKeyFileNeedsItsDirectory(t *testing.T) {
	err := writeKeyFile(filepath.Join(t.TempDir(), "absent", "child.key"), "k")
	if err == nil || !strings.Contains(err.Error(), "child key") {
		t.Fatalf("got %v", err)
	}
}
