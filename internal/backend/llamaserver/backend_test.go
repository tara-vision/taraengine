package llamaserver

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tara-vision/taraengine/internal/backend"
	"github.com/tara-vision/taraengine/internal/llamatest"
	"github.com/tara-vision/taraengine/internal/state"
)

type fakeProbe struct {
	pids []int
	mem  backend.Memory
	err  error
}

func (p *fakeProbe) Measure(_ context.Context, pid int) (backend.Memory, error) {
	p.pids = append(p.pids, pid)
	return p.mem, p.err
}

// quietLogger is a logger that drops everything, so passing tests print nothing.
func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// testStore is a state file inside dir.
func testStore(dir string) state.Store { return state.Store{Path: filepath.Join(dir, "state.json")} }

type harness struct {
	b      *Backend
	fake   *llamatest.Server
	probe  *fakeProbe
	store  state.Store
	models map[string]PresetModel
	order  []string // the order the catalog lists the models in
}

func newHarness(t *testing.T, loadDelay time.Duration) *harness {
	t.Helper()
	return newHarnessWith(t, loadDelay, nil)
}

// newHarnessWith is newHarness with a hook that may change the options, and see the scripted router,
// before the backend is built.
func newHarnessWith(t *testing.T, loadDelay time.Duration, tweak func(*llamatest.Server, *Options)) *harness {
	t.Helper()
	return newHarnessScripted(t, llamatest.Options{LoadDelay: loadDelay}, tweak)
}

// newHarnessScripted is newHarnessWith with the script of the router; its key, preset path and model
// limit are the harness's own.
func newHarnessScripted(t *testing.T, script llamatest.Options, tweak func(*llamatest.Server, *Options)) *harness {
	t.Helper()
	dataDir := t.TempDir()
	script.APIKey, script.PresetPath, script.ModelsMax = "k", PresetPath(dataDir), 1
	fake := llamatest.New(t, script)
	h := &harness{fake: fake, probe: &fakeProbe{mem: backend.Memory{Bytes: 1 << 30, Source: "measured"}}, store: testStore(dataDir)}
	h.models = map[string]PresetModel{
		"a": {Name: "a", ModelPath: "/m/a.gguf", Context: 4096},
		"b": {Name: "b", ModelPath: "/m/b.gguf", DraftPath: "/m/b-mtp.gguf", Context: 4096},
	}
	h.order = []string{"a", "b"}
	opts := Options{
		DataDir:          dataDir,
		SleepIdleSeconds: 900,
		Attach:           fake.URL(),
		AttachPID:        4242,
		APIKey:           "k",
		Build:            "bTEST",
		LoadTimeout:      2 * time.Second,
		Poll:             10 * time.Millisecond,
		State:            h.store,
		Probe:            h.probe,
		Logger:           quietLogger(),
		Models: func(_ context.Context, overrides map[string]int) ([]PresetModel, error) {
			out := []PresetModel{}
			for _, name := range h.order {
				m := h.models[name]
				if c, ok := overrides[name]; ok {
					m.Context = c
				}
				out = append(out, m)
			}
			return out, nil
		},
	}
	if tweak != nil {
		tweak(fake, &opts)
	}
	b, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = b.Stop(context.Background()) })
	h.b = b
	return h
}

func statusOf(t *testing.T, b *Backend, name string) backend.ModelStatus {
	t.Helper()
	models, err := b.Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range models {
		if m.Name == name {
			return m
		}
	}
	t.Fatalf("model %s not listed in %+v", name, models)
	return backend.ModelStatus{}
}

func TestStartWritesPresetsAndSyncs(t *testing.T) {
	h := newHarness(t, 0)
	data, err := os.ReadFile(PresetPath(h.b.opts.DataDir))
	if err != nil || !strings.Contains(string(data), "[a]\nc = 4096\nmodel = /m/a.gguf") || !strings.Contains(string(data), "spec-type = draft-mtp") {
		t.Fatalf("presets: %v\n%s", err, data)
	}
	a := statusOf(t, h.b, "a")
	if a.Status != backend.StatusPulled || a.Context != 4096 || a.Memory != nil {
		t.Fatalf("a before load: %+v", a)
	}
	health, err := h.b.Health(context.Background())
	if err != nil || !health.OK || health.Backend != "llama.cpp" || health.Build != "bTEST" {
		t.Fatalf("health: %+v %v", health, err)
	}
}

func TestLoadPollsMeasuresAndRecords(t *testing.T) {
	h := newHarness(t, 30*time.Millisecond)
	if err := h.b.Load(context.Background(), "a", backend.LoadOptions{}); err != nil {
		t.Fatalf("load: %v", err)
	}
	a := statusOf(t, h.b, "a")
	if a.Status != backend.StatusLoaded || a.Memory == nil || a.Memory.Bytes != 1<<30 || a.Props == nil || a.Props.ContextLength != 4096 || !a.Props.SupportsTools {
		t.Fatalf("a after load: %+v", a)
	}
	if len(h.probe.pids) != 1 || h.probe.pids[0] != 4242 {
		t.Fatalf("probe pids %v", h.probe.pids)
	}
	st, _ := h.store.Load()
	if st.LastLoaded != "a" || st.FailedLoads["a"] != 0 {
		t.Fatalf("state after load: %+v", st)
	}
	if err := h.b.Load(context.Background(), "a", backend.LoadOptions{}); err != nil {
		t.Fatalf("a second load of a loaded model must be a no-op: %v", err)
	}
	if n := countRequests(h.fake, "POST /models/load"); n != 1 {
		t.Fatalf("router loads %d, want 1", n)
	}
	if err := h.b.Unload(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	if a := statusOf(t, h.b, "a"); a.Status != backend.StatusPulled || a.Memory != nil {
		t.Fatalf("a after unload: %+v", a)
	}
}

func TestLoadWithANewContextRewritesThePreset(t *testing.T) {
	h := newHarness(t, 0)
	if err := h.b.Load(context.Background(), "a", backend.LoadOptions{Context: 8192}); err != nil {
		t.Fatalf("load: %v", err)
	}
	data, _ := os.ReadFile(PresetPath(h.b.opts.DataDir))
	if !strings.Contains(string(data), "[a]\nc = 8192\n") {
		t.Fatalf("preset not rewritten:\n%s", data)
	}
	if a := statusOf(t, h.b, "a"); a.Status != backend.StatusLoaded || a.Context != 8192 {
		t.Fatalf("a: %+v", a)
	}
	st, _ := h.store.Load()
	if st.Overrides["a"] != 8192 {
		t.Fatalf("override not persisted: %+v", st)
	}
	if st.LastLoaded != "a" {
		t.Fatalf("the final save must keep building on the override: %+v", st)
	}
	if err := h.b.Load(context.Background(), "a", backend.LoadOptions{Context: 2048}); err != nil {
		t.Fatalf("reload with another context: %v", err)
	}
	if n := countRequests(h.fake, "POST /models/unload"); n != 1 {
		t.Fatalf("a loaded model with another context must be unloaded first, unloads %d", n)
	}
	if st, _ := h.store.Load(); st.Overrides["a"] != 2048 {
		t.Fatalf("second override not persisted: %+v", st)
	}
}

func TestLoadFailuresMakeAModelFaulty(t *testing.T) {
	h := newHarness(t, 0)
	h.fake.FailLoad("b", 1)
	err := h.b.Load(context.Background(), "b", backend.LoadOptions{})
	if !errors.Is(err, backend.ErrLoadFailed) || !strings.Contains(err.Error(), "exit code 1") {
		t.Fatalf("first failure: %v", err)
	}
	if b := statusOf(t, h.b, "b"); b.Status != backend.StatusFailed || b.ExitCode != 1 {
		t.Fatalf("b after one failure: %+v", b)
	}
	if err := h.b.Load(context.Background(), "b", backend.LoadOptions{}); !errors.Is(err, backend.ErrLoadFailed) {
		t.Fatalf("second failure: %v", err)
	}
	if b := statusOf(t, h.b, "b"); b.Status != backend.StatusFaulty {
		t.Fatalf("b after two failures: %+v", b)
	}
	before := countRequests(h.fake, "POST /models/load")
	if err := h.b.Load(context.Background(), "b", backend.LoadOptions{}); !errors.Is(err, backend.ErrFaulty) {
		t.Fatalf("faulty model must be refused: %v", err)
	}
	if countRequests(h.fake, "POST /models/load") != before {
		t.Fatal("a refused load must not reach the router")
	}
	if err := h.b.Load(context.Background(), "b", backend.LoadOptions{Force: true}); !errors.Is(err, backend.ErrLoadFailed) {
		t.Fatalf("forced load must try again: %v", err)
	}
	if err := h.b.Load(context.Background(), "zzz", backend.LoadOptions{}); !errors.Is(err, backend.ErrUnknownModel) {
		t.Fatalf("unknown model: %v", err)
	}
}

func TestLoadTimesOut(t *testing.T) {
	h := newHarness(t, 0)
	h.b.opts.LoadTimeout = 150 * time.Millisecond
	h.fake.HangLoad("a")
	start := time.Now()
	err := h.b.Load(context.Background(), "a", backend.LoadOptions{})
	if !errors.Is(err, backend.ErrLoadTimeout) {
		t.Fatalf("want a timeout, got %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("the timeout took too long")
	}
	if n := countRequests(h.fake, "POST /models/unload"); n != 1 {
		t.Fatalf("a load that timed out must be cancelled with an unload, unloads %d", n)
	}
	h.fake.SetStatus("a", "unloaded")
	if err := h.b.Load(context.Background(), "b", backend.LoadOptions{}); err != nil {
		t.Fatalf("the daemon must stay usable after a timeout: %v", err)
	}
	if st, _ := h.store.Load(); st.FailedLoads["a"] != 0 {
		t.Fatalf("a timeout is not a failed load: %+v", st)
	}
}

func TestHealthReportsARouterThatIsGone(t *testing.T) {
	h := newHarness(t, 0)
	h.fake.Close()
	health, err := h.b.Health(context.Background())
	if err != nil || health.OK || health.Detail == "" {
		t.Fatalf("health with the router gone: %+v %v", health, err)
	}
	if strings.Contains(health.Detail, "127.0.0.1") || strings.Contains(health.Detail, "http") {
		t.Fatalf("the health route is open, so the child's address stays out of it: %q", health.Detail)
	}
	if _, err := h.b.Models(context.Background()); err == nil {
		t.Fatal("models with the router gone must fail")
	}
}

func TestNewRejectsBadOptions(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Fatal("no data dir and no models func accepted")
	}
	dataDir := t.TempDir()
	b, err := New(Options{
		DataDir:          dataDir,
		SleepIdleSeconds: 900,
		RuntimeDir:       filepath.Join(t.TempDir(), "absent"),
		State:            testStore(dataDir),
		Models:           func(context.Context, map[string]int) ([]PresetModel, error) { return nil, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "llama-server") {
		t.Fatalf("a missing runtime binary must fail the start: %v", err)
	}
}

func countRequests(s *llamatest.Server, prefix string) int {
	n := 0
	for _, r := range s.Requests() {
		if strings.HasPrefix(r, prefix) {
			n++
		}
	}
	return n
}

func TestStartCreatesAPrivateDataDirectory(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "nested", "data")
	fake := llamatest.New(t, llamatest.Options{APIKey: "k", PresetPath: PresetPath(dataDir)})
	b, err := New(Options{
		DataDir: dataDir, Attach: fake.URL(), APIKey: "k", State: testStore(dataDir), Logger: quietLogger(), SleepIdleSeconds: 900,
		Models: func(context.Context, map[string]int) ([]PresetModel, error) {
			return []PresetModel{{Name: "a", ModelPath: "/m/a.gguf"}}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Start(context.Background()); err != nil {
		t.Fatalf("a data directory that does not exist yet is created: %v", err)
	}
	info, err := os.Stat(dataDir)
	if err != nil || !info.IsDir() || info.Mode().Perm()&^0o750 != 0 {
		t.Fatalf("data directory %v: %v (it must not be open to the group's writers or to others)", info, err)
	}
	if a := statusOf(t, b, "a"); a.Status != backend.StatusPulled {
		t.Fatalf("a: %+v", a)
	}
}
