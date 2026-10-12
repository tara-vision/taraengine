package llamaserver

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tara-vision/taraengine/internal/backend"
	"github.com/tara-vision/taraengine/internal/llamatest"
	"github.com/tara-vision/taraengine/internal/measure"
)

// cannedReply replaces the router's answer to one call.
type cannedReply struct {
	status int
	body   string
}

func scriptedFailure(status int) *cannedReply {
	return &cannedReply{status: status, body: `{"error":{"code":` + strconv.Itoa(status) + `,"message":"scripted failure"}}`}
}

// flakyRouter fronts a scripted router. canned sees every request and the 1-based count of calls to its
// path, and may answer instead of the real router.
type flakyRouter struct {
	*httptest.Server
	mu    sync.Mutex
	calls map[string]int
}

func newFlakyRouter(t *testing.T, upstream string, canned func(r *http.Request, n int) *cannedReply) *flakyRouter {
	t.Helper()
	target, err := url.Parse(upstream)
	if err != nil {
		t.Fatal(err)
	}
	pass := httputil.NewSingleHostReverseProxy(target)
	f := &flakyRouter{calls: map[string]int{}}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.calls[r.URL.Path]++
		n := f.calls[r.URL.Path]
		f.mu.Unlock()
		if reply := canned(r, n); reply != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(reply.status)
			_, _ = w.Write([]byte(reply.body))
			return
		}
		pass.ServeHTTP(w, r)
	}))
	t.Cleanup(f.Close)
	return f
}

// behindFlaky puts a flaky router between the backend and the scripted router.
func behindFlaky(t *testing.T, canned func(r *http.Request, n int) *cannedReply) *harness {
	t.Helper()
	return newHarnessWith(t, 0, func(fake *llamatest.Server, o *Options) {
		o.Attach = newFlakyRouter(t, fake.URL(), canned).URL
	})
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

func loggingTo(logs *syncBuffer) func(*llamatest.Server, *Options) {
	return func(_ *llamatest.Server, o *Options) { o.Logger = slog.New(slog.NewTextHandler(logs, nil)) }
}

func TestColonNamesUseSectionIDsEndToEnd(t *testing.T) {
	h := newHarness(t, 0)
	withModel("gemma4:12b")(h)
	ctx := context.Background()
	if err := h.b.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(readPresets(t, h), "[gemma4-12b]\n") {
		t.Fatalf("the section must carry the router id:\n%s", readPresets(t, h))
	}
	if g := statusOf(t, h.b, "gemma4:12b"); g.Status != backend.StatusPulled {
		t.Fatalf("listed by catalog name, not by the router's id: %+v", g)
	}
	if err := h.b.Load(ctx, "gemma4:12b", backend.LoadOptions{Context: 8192}); err != nil {
		t.Fatalf("load: %v", err)
	}
	g := statusOf(t, h.b, "gemma4:12b")
	if g.Status != backend.StatusLoaded || g.Context != 8192 || g.Memory == nil || g.Props == nil || g.Props.ContextLength != 8192 {
		t.Fatalf("after load: %+v", g)
	}
	st, _ := h.store.Load()
	if st.LastLoaded != "gemma4:12b" || st.Overrides["gemma4:12b"] != 8192 {
		t.Fatalf("state is keyed by catalog name: %+v", st)
	}
	if err := h.b.Unload(ctx, "gemma4:12b"); err != nil {
		t.Fatal(err)
	}
	if g := statusOf(t, h.b, "gemma4:12b"); g.Status != backend.StatusPulled || g.Memory != nil || g.Props != nil {
		t.Fatalf("after unload: %+v", g)
	}
	for _, req := range h.fake.Requests() {
		if strings.Contains(req, "gemma4:") || strings.Contains(req, "%3A") {
			t.Errorf("the router must only ever see section ids: %s", req)
		}
	}
}

func TestModelsListsOnlyWhatTheCatalogHas(t *testing.T) {
	h := newHarness(t, 0)
	h.fake.AddModel("stranger", "/m/s.gguf")
	models, err := h.b.Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 || models[0].Name != "a" || models[1].Name != "b" {
		t.Fatalf("a model the router lists on its own is not ours: %+v", models)
	}
}

func TestModelsStatusRules(t *testing.T) {
	h := newHarness(t, 0)
	ctx := context.Background()
	for router, want := range map[string]backend.Status{
		"loading":     backend.StatusLoading,
		"loaded":      backend.StatusLoaded,
		"sleeping":    backend.StatusSleeping,
		"unloaded":    backend.StatusPulled,
		"downloading": backend.StatusPulled,
	} {
		h.fake.SetStatus("a", router)
		if got := statusOf(t, h.b, "a").Status; got != want {
			t.Errorf("router status %q became %q, want %q", router, got, want)
		}
	}
	h.fake.SetStatus("a", "unloaded")
	if err := h.b.Load(ctx, "a", backend.LoadOptions{}); err != nil {
		t.Fatal(err)
	}
	h.fake.SetStatus("a", "sleeping")
	if a := statusOf(t, h.b, "a"); a.Status != backend.StatusSleeping || a.Memory == nil || a.Props == nil {
		t.Fatalf("a sleeping model keeps its figures: %+v", a)
	}
	h.fake.SetStatus("a", "unloaded")
	if a := statusOf(t, h.b, "a"); a.Memory != nil || a.Props != nil {
		t.Fatalf("an unloaded model shows no figures: %+v", a)
	}
}

func TestUnloadRefusesUnknownModelsAndForgivesIdleOnes(t *testing.T) {
	h := newHarness(t, 0)
	if err := h.b.Unload(context.Background(), "zzz"); !errors.Is(err, backend.ErrUnknownModel) {
		t.Fatalf("unknown model: %v", err)
	}
	if n := countRequests(h.fake, "POST /models/unload"); n != 0 {
		t.Fatalf("an unknown model must not reach the router, unloads %d", n)
	}
	if err := h.b.Unload(context.Background(), "a"); err != nil {
		t.Fatalf("a model that is not running is already unloaded: %v", err)
	}
}

// gateProbe holds a load inside its critical section until the test lets it go.
type gateProbe struct {
	entered chan struct{}
	release chan struct{}
}

func (p *gateProbe) Measure(context.Context, int) (backend.Memory, error) {
	p.entered <- struct{}{}
	<-p.release
	return backend.Memory{Bytes: 1, Source: "measured"}, nil
}

func TestLoadsAreSerialized(t *testing.T) {
	gate := &gateProbe{entered: make(chan struct{}, 2), release: make(chan struct{})}
	h := newHarnessWith(t, 0, func(_ *llamatest.Server, o *Options) { o.Probe = gate })
	ctx := context.Background()
	errs := make(chan error, 2)
	go func() { errs <- h.b.Load(ctx, "a", backend.LoadOptions{}) }()
	<-gate.entered
	go func() { errs <- h.b.Load(ctx, "b", backend.LoadOptions{}) }()
	time.Sleep(150 * time.Millisecond)
	if n := countRequests(h.fake, "POST /models/load"); n != 1 {
		t.Fatalf("a second load reached the router while the first still held the lock: %d loads", n)
	}
	close(gate.release)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if n := countRequests(h.fake, "POST /models/load"); n != 2 {
		t.Fatalf("router loads %d, want 2", n)
	}
	if st, _ := h.store.Load(); st.LastLoaded != "b" {
		t.Fatalf("the second load ran second: %+v", st)
	}
}

func TestLoadStopsWaitingWhenTheContextEnds(t *testing.T) {
	h := newHarness(t, 0)
	h.b.opts.Poll = time.Minute // the context, not the poll, must end the wait
	h.fake.HangLoad("a")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err := h.b.Load(ctx, "a", backend.LoadOptions{})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want the context's error, got %v", err)
	}
	if st, _ := h.store.Load(); st.FailedLoads["a"] != 0 || st.LastLoaded != "" {
		t.Fatalf("a caller that gave up is not a failed load: %+v", st)
	}
}

func TestLoadSurvivesMissingFiguresAndProbeTrouble(t *testing.T) {
	rows := []struct {
		name     string
		mutate   func(h *harness)
		wantPIDs []int
		wantLog  string
	}{
		{"the probe has no figure", func(h *harness) { h.probe.err = measure.ErrNoFigure }, []int{4242}, ""},
		{"the probe fails", func(h *harness) { h.probe.err = errors.New("ps exploded") }, []int{4242, 4242}, "memory probe failed"},
		{"there is no probe", func(h *harness) { h.b.opts.Probe = nil }, nil, ""},
		{"there is no process to measure", func(h *harness) { h.b.opts.AttachPID = 0 }, nil, ""},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			var logs syncBuffer
			h := newHarnessWith(t, 0, loggingTo(&logs))
			row.mutate(h)
			if err := h.b.Load(context.Background(), "a", backend.LoadOptions{}); err != nil {
				t.Fatalf("a missing figure is not a failed load: %v", err)
			}
			a := statusOf(t, h.b, "a")
			if a.Status != backend.StatusLoaded || a.Memory != nil || a.Props == nil {
				t.Fatalf("loaded without a figure: %+v", a)
			}
			if len(h.probe.pids) != len(row.wantPIDs) {
				t.Fatalf("probe pids %v, want %v", h.probe.pids, row.wantPIDs)
			}
			if row.wantLog == "" && strings.Contains(logs.String(), "memory probe failed") {
				t.Errorf("no figure is not worth a warning:\n%s", logs.String())
			}
			if row.wantLog != "" && !strings.Contains(logs.String(), row.wantLog) {
				t.Errorf("expected %q in the log:\n%s", row.wantLog, logs.String())
			}
		})
	}
}

func TestLoadForgetsFiguresOfAnEarlierLoad(t *testing.T) {
	h := newHarness(t, 0)
	ctx := context.Background()
	if err := h.b.Load(ctx, "a", backend.LoadOptions{}); err != nil {
		t.Fatal(err)
	}
	// The router drops the model on its own (an eviction); the backend never saw an unload.
	h.fake.SetStatus("a", "unloaded")
	h.probe.err = errors.New("ps exploded")
	if err := h.b.Load(ctx, "a", backend.LoadOptions{}); err != nil {
		t.Fatal(err)
	}
	if a := statusOf(t, h.b, "a"); a.Status != backend.StatusLoaded || a.Memory != nil || a.Props == nil {
		t.Fatalf("a figure from the earlier load must not survive the next one: %+v", a)
	}
}

func TestLoadSurvivesPropsTrouble(t *testing.T) {
	var logs syncBuffer
	h := newHarnessWith(t, 0, func(fake *llamatest.Server, o *Options) {
		loggingTo(&logs)(fake, o)
		o.Attach = newFlakyRouter(t, fake.URL(), func(r *http.Request, _ int) *cannedReply {
			if r.URL.Path == "/props" {
				return scriptedFailure(http.StatusInternalServerError)
			}
			return nil
		}).URL
	})
	if err := h.b.Load(context.Background(), "a", backend.LoadOptions{}); err != nil {
		t.Fatalf("missing props are not a failed load: %v", err)
	}
	a := statusOf(t, h.b, "a")
	if a.Status != backend.StatusLoaded || a.Props != nil || a.Memory == nil {
		t.Fatalf("a: %+v", a)
	}
	if !strings.Contains(logs.String(), "props unavailable") {
		t.Errorf("expected a warning:\n%s", logs.String())
	}
}

// failFrom answers the nth and every later call to path with a 500.
func failFrom(path string, n int) func(*http.Request, int) *cannedReply {
	return func(r *http.Request, call int) *cannedReply {
		if r.URL.Path == path && call >= n {
			return scriptedFailure(http.StatusInternalServerError)
		}
		return nil
	}
}

// answerOnce replaces only the nth call to path with a canned 200.
func answerOnce(path string, n int, body string) func(*http.Request, int) *cannedReply {
	return func(r *http.Request, call int) *cannedReply {
		if r.URL.Path == path && call == n {
			return &cannedReply{status: http.StatusOK, body: body}
		}
		return nil
	}
}

// failPlainStatusFrom refuses the nth and later reads of the list that do not ask for a reload.
func failPlainStatusFrom(n int) func(*http.Request, int) *cannedReply {
	return func(r *http.Request, call int) *cannedReply {
		if r.URL.Path == "/models" && r.URL.Query().Get("reload") == "" && call >= n {
			return scriptedFailure(http.StatusInternalServerError)
		}
		return nil
	}
}

func loadA(opts backend.LoadOptions) func(h *harness) error {
	return func(h *harness) error { return h.b.Load(context.Background(), "a", opts) }
}

// loadAThenChangeItsContext loads a, then asks for another context, which must unload it first.
func loadAThenChangeItsContext(h *harness) error {
	if err := loadA(backend.LoadOptions{})(h); err != nil {
		return err
	}
	return loadA(backend.LoadOptions{Context: 2048})(h)
}

func TestLoadReportsRouterTrouble(t *testing.T) {
	rows := []struct {
		name   string
		canned func(r *http.Request, n int) *cannedReply
		run    func(h *harness) error
		want   string
		loads  int // loads that reached the router
	}{
		{"the status cannot be read", failFrom("/models", 2), loadA(backend.LoadOptions{}), "scripted failure", 0},
		{"the load call is refused", failFrom("/models/load", 1), loadA(backend.LoadOptions{}), "scripted failure", 0},
		{"the status cannot be read while waiting", failFrom("/models", 3), loadA(backend.LoadOptions{}), "scripted failure", 1},
		{"the router does not list the model", answerOnce("/models", 2, `{"data":[],"object":"list"}`), loadA(backend.LoadOptions{}),
			backend.ErrUnknownModel.Error(), 0},
		{"the unload before a new context is refused", failFrom("/models/unload", 1), loadAThenChangeItsContext, "scripted failure", 1},
		{"the reload for a new context is refused", failReloadsFrom(2), loadA(backend.LoadOptions{Context: 8192}), "scripted failure", 0},
		{"the status after a new context cannot be read", failPlainStatusFrom(3), loadA(backend.LoadOptions{Context: 8192}),
			"scripted failure", 0},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			h := behindFlaky(t, row.canned)
			err := row.run(h)
			if err == nil || !strings.Contains(err.Error(), row.want) {
				t.Fatalf("want an error with %q, got %v", row.want, err)
			}
			if errors.Is(err, backend.ErrLoadFailed) {
				t.Fatalf("router trouble is not a failed load: %v", err)
			}
			if n := countRequests(h.fake, "POST /models/load"); n != row.loads {
				t.Errorf("router loads %d, want %d", n, row.loads)
			}
			if st, _ := h.store.Load(); st.FailedLoads["a"] != 0 {
				t.Errorf("router trouble must not count against the model: %+v", st)
			}
		})
	}
}

func TestLoadReportsStateTrouble(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	breakState := func(h *harness) { h.b.opts.State.Path = filepath.Join(blocker, "state.json") }
	ctx := context.Background()

	t.Run("after a good load", func(t *testing.T) {
		h := newHarness(t, 0)
		breakState(h)
		err := h.b.Load(ctx, "a", backend.LoadOptions{})
		if err == nil || !strings.Contains(err.Error(), "state") {
			t.Fatalf("a state that cannot be saved must be reported: %v", err)
		}
		if a := statusOf(t, h.b, "a"); a.Status != backend.StatusLoaded || a.Memory == nil || a.Props == nil {
			t.Fatalf("the model is loaded and must be recorded all the same: %+v", a)
		}
	})
	t.Run("after a failed load", func(t *testing.T) {
		h := newHarness(t, 0)
		h.fake.FailLoad("b", 1)
		breakState(h)
		err := h.b.Load(ctx, "b", backend.LoadOptions{})
		if !errors.Is(err, backend.ErrLoadFailed) || !strings.Contains(err.Error(), "state") {
			t.Fatalf("both the failed load and the lost state must be reported: %v", err)
		}
		if b := statusOf(t, h.b, "b"); b.Status != backend.StatusFailed {
			t.Fatalf("b: %+v", b)
		}
	})
	t.Run("before a new context", func(t *testing.T) {
		h := newHarness(t, 0)
		breakState(h)
		err := h.b.Load(ctx, "a", backend.LoadOptions{Context: 8192})
		if err == nil || !strings.Contains(err.Error(), "state") {
			t.Fatalf("a new context that cannot be remembered must stop the load: %v", err)
		}
		if n := countRequests(h.fake, "POST /models/load"); n != 0 {
			t.Fatalf("router loads %d, want 0", n)
		}
	})
}

func TestLoadRefusesAContextTheRouterDoesNotPickUp(t *testing.T) {
	h := newHarnessWith(t, 0, func(_ *llamatest.Server, o *Options) {
		o.Models = func(context.Context, map[string]int) ([]PresetModel, error) {
			return []PresetModel{{Name: "a", ModelPath: "/m/a.gguf", Context: 4096}}, nil
		}
	})
	err := h.b.Load(context.Background(), "a", backend.LoadOptions{Context: 8192})
	if err == nil || !strings.Contains(err.Error(), "did not pick up the context 8192") {
		t.Fatalf("a context the preset does not carry must not be loaded: %v", err)
	}
	if n := countRequests(h.fake, "POST /models/load"); n != 0 {
		t.Fatalf("router loads %d, want 0", n)
	}
}

func TestLoadReloadsASleepingModel(t *testing.T) {
	h := newHarness(t, 0)
	ctx := context.Background()
	if err := h.b.Load(ctx, "a", backend.LoadOptions{}); err != nil {
		t.Fatal(err)
	}
	h.fake.SetStatus("a", "sleeping")
	if err := h.b.Load(ctx, "a", backend.LoadOptions{}); err != nil {
		t.Fatal(err)
	}
	if loads, unloads := countRequests(h.fake, "POST /models/load"), countRequests(h.fake, "POST /models/unload"); loads != 2 || unloads != 1 {
		t.Fatalf("a sleeping model is unloaded and loaded again: loads %d, unloads %d", loads, unloads)
	}
	if a := statusOf(t, h.b, "a"); a.Status != backend.StatusLoaded {
		t.Fatalf("a: %+v", a)
	}
}

func TestStartRendersWhatTheStateRemembers(t *testing.T) {
	h := newHarnessWith(t, 0, func(_ *llamatest.Server, o *Options) {
		writeTestFile(t, o.State.Path, `{"failed_loads":{"b":2},"last_loaded":"a","overrides":{"a":8192}}`)
	})
	if !strings.Contains(readPresets(t, h), "[a]\nc = 8192\n") {
		t.Fatalf("a context remembered from the last run belongs in the first preset file:\n%s", readPresets(t, h))
	}
	if a := statusOf(t, h.b, "a"); a.Context != 8192 {
		t.Fatalf("a: %+v", a)
	}
	if b := statusOf(t, h.b, "b"); b.Status != backend.StatusFaulty {
		t.Fatalf("a model that failed twice stays faulty across a restart: %+v", b)
	}
	if err := h.b.Load(context.Background(), "b", backend.LoadOptions{}); !errors.Is(err, backend.ErrFaulty) {
		t.Fatalf("faulty models stay refused after a restart: %v", err)
	}
}
