package llamaserver

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tara-vision/taraengine/internal/backend"
	"github.com/tara-vision/taraengine/internal/llamatest"
	"github.com/tara-vision/taraengine/internal/measure"
)

// probeFunc is a probe made of a function, for the tests that act while a recording measures.
type probeFunc func(ctx context.Context, pid int) (backend.Memory, error)

func (f probeFunc) Measure(ctx context.Context, pid int) (backend.Memory, error) { return f(ctx, pid) }

// readResult is what one read of the model list gave.
type readResult struct {
	models []backend.ModelStatus
	err    error
}

// readAsync reads the model list on another goroutine.
func readAsync(ctx context.Context, b *Backend) <-chan readResult {
	out := make(chan readResult, 1)
	go func() {
		models, err := b.Models(ctx)
		out <- readResult{models: models, err: err}
	}()
	return out
}

// awaitRead waits for a read for at most d.
func awaitRead(t *testing.T, read <-chan readResult, d time.Duration, what string) []backend.ModelStatus {
	t.Helper()
	select {
	case r := <-read:
		if r.err != nil {
			t.Fatalf("%s: %v", what, r.err)
		}
		return r.models
	case <-time.After(d):
		t.Fatalf("%s did not answer within %s", what, d)
		return nil
	}
}

// modelIn is the named model of a list.
func modelIn(t *testing.T, models []backend.ModelStatus, name string) backend.ModelStatus {
	t.Helper()
	for _, m := range models {
		if m.Name == name {
			return m
		}
	}
	t.Fatalf("%s is not listed in %+v", name, models)
	return backend.ModelStatus{}
}

// requireLockFree fails the test while an operation or a recording holds the backend.
func requireLockFree(t *testing.T, b *Backend) {
	t.Helper()
	if !b.opMu.TryLock() {
		t.Fatal("the operations lock is still held")
	}
	b.opMu.Unlock()
}

// awaitSignal waits for one value on ch for at most two seconds.
func awaitSignal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatalf("%s did not happen within two seconds", what)
	}
}

func TestAReadRecordsTheFiguresOfAModelARequestBroughtUp(t *testing.T) {
	h := newHarness(t, 0)
	h.fake.SetStatus("a", "loaded") // a chat request brought it up on its own
	a := statusOf(t, h.b, "a")
	if a.Status != backend.StatusLoaded || a.Memory == nil || a.Memory.Bytes != 1<<30 || a.Props == nil ||
		a.Props.ContextLength != 4096 {
		t.Fatalf("the read that finds the model records its figures and shows them: %+v", a)
	}
	if n := countRequests(h.fake, "GET /props?model=a&autoload=false"); n != 1 {
		t.Fatalf("the props are read once, with autoload off: %d\n%v", n, h.fake.Requests())
	}
	statusOf(t, h.b, "a")
	if len(h.probe.pids) != 1 {
		t.Fatalf("figures already recorded are not measured again: %v", h.probe.pids)
	}
}

func TestAModelThatStopsWhileItIsRecordedShowsNoFigures(t *testing.T) {
	h := newHarnessWith(t, 0, func(fake *llamatest.Server, o *Options) {
		o.Probe = probeFunc(func(context.Context, int) (backend.Memory, error) {
			fake.SetStatus("a", "unloaded") // evicted between the first look at the router and the measure
			return backend.Memory{Bytes: 1 << 30, Source: "measured"}, nil
		})
	})
	h.fake.SetStatus("a", "loaded")
	if a := statusOf(t, h.b, "a"); a.Status != backend.StatusPulled || a.Memory != nil || a.Props != nil {
		t.Fatalf("the answer comes from a second look at the router, so a model gone meanwhile shows none: %+v", a)
	}
}

func TestAReadWhileALoadRunsRecordsNothingAndAnswersAtOnce(t *testing.T) {
	gate := &gateProbe{entered: make(chan struct{}, 2), release: make(chan struct{})}
	h := newHarnessWith(t, 0, func(_ *llamatest.Server, o *Options) { o.Probe = gate })
	done := make(chan error, 1)
	go func() { done <- h.b.Load(context.Background(), "b", backend.LoadOptions{}) }()
	awaitSignal(t, gate.entered, "the load's measure") // the load measures b while it holds the operations lock
	h.fake.SetStatus("a", "loaded")
	models := awaitRead(t, readAsync(context.Background(), h.b), 2*time.Second, "a read during a load")
	if a := modelIn(t, models, "a"); a.Status != backend.StatusLoaded || a.Memory != nil || a.Props != nil {
		t.Fatalf("a read during a load answers with what it has: %+v", a)
	}
	if n := countRequests(h.fake, "GET /props?model=a"); n != 0 {
		t.Fatalf("a read during a load records nothing: %d props reads of a", n)
	}
	close(gate.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if a := statusOf(t, h.b, "a"); a.Memory == nil || a.Props == nil {
		t.Fatalf("the next read records what the first one could not: %+v", a)
	}
}

func TestTwoReadsAtOnceMeasureTheModelOnce(t *testing.T) {
	gate := &gateProbe{entered: make(chan struct{}, 2), release: make(chan struct{})}
	h := newHarnessWith(t, 0, func(_ *llamatest.Server, o *Options) { o.Probe = gate })
	h.fake.SetStatus("a", "loaded")
	first := readAsync(context.Background(), h.b)
	awaitSignal(t, gate.entered, "the first read's measure") // the first read records, holding the operations lock
	models := awaitRead(t, readAsync(context.Background(), h.b), 2*time.Second, "the second read")
	if a := modelIn(t, models, "a"); a.Memory != nil {
		t.Fatalf("the second read answers at once, before the first has a figure: %+v", a)
	}
	close(gate.release)
	if a := modelIn(t, awaitRead(t, first, 2*time.Second, "the first read"), "a"); a.Memory == nil || a.Memory.Bytes != 1 {
		t.Fatalf("the first read shows the figure it recorded: %+v", a)
	}
	select {
	case <-gate.entered:
		t.Fatal("the model was measured twice")
	default:
	}
}

func TestAFailedProbeKeepsThePropsAlreadyRecorded(t *testing.T) {
	h := newHarness(t, 0)
	h.probe.err = errors.New("ps exploded")
	if err := h.b.Load(context.Background(), "a", backend.LoadOptions{}); err != nil {
		t.Fatal(err)
	}
	if a := statusOf(t, h.b, "a"); a.Props == nil || a.Memory != nil {
		t.Fatalf("a: %+v", a)
	}
	if len(h.probe.pids) != 2 {
		t.Fatalf("the read tries the probe again: %v", h.probe.pids)
	}
	if n := countRequests(h.fake, "GET /props"); n != 1 {
		t.Fatalf("props already recorded are kept and not read again: %d reads", n)
	}
}

func TestAMemoryFigureThatCannotBeHadIsProbedOncePerRunOfTheModel(t *testing.T) {
	h := newHarness(t, 0)
	h.probe.err = measure.ErrNoFigure
	h.fake.SetStatus("a", "loaded")
	for range 3 {
		statusOf(t, h.b, "a")
	}
	if len(h.probe.pids) != 1 {
		t.Fatalf("one probe per run of the model, not one per read: %v", h.probe.pids)
	}
	h.fake.SetStatus("a", "unloaded")
	statusOf(t, h.b, "a")
	h.fake.SetStatus("a", "loaded")
	for range 2 {
		statusOf(t, h.b, "a")
	}
	if len(h.probe.pids) != 2 {
		t.Fatalf("the next run is probed once more: %v", h.probe.pids)
	}
}

func TestWithoutAProcessToMeasureAReadRecordsThePropsOnce(t *testing.T) {
	h := newHarness(t, 0)
	h.b.opts.AttachPID = 0 // attach mode as serve wires it: there is no process to measure
	h.fake.SetStatus("a", "loaded")
	for range 2 {
		if a := statusOf(t, h.b, "a"); a.Props == nil || a.Memory != nil {
			t.Fatalf("a: %+v", a)
		}
	}
	if n := countRequests(h.fake, "GET /props"); n != 1 || len(h.probe.pids) != 0 {
		t.Fatalf("the props are read once and nothing is probed: %d props reads, probes %v", n, h.probe.pids)
	}
}

func TestAProbeThatHangsPastTheDeadlineLetsGoOfTheLock(t *testing.T) {
	var hang atomic.Bool
	hang.Store(true)
	var calls atomic.Int32
	h := newHarnessWith(t, 0, func(_ *llamatest.Server, o *Options) {
		o.Probe = probeFunc(func(ctx context.Context, _ int) (backend.Memory, error) {
			calls.Add(1)
			if hang.Load() {
				<-ctx.Done()
				return backend.Memory{}, ctx.Err()
			}
			return backend.Memory{Bytes: 1 << 30, Source: "measured"}, nil
		})
	})
	h.b.recordWait = 100 * time.Millisecond
	h.fake.SetStatus("a", "loaded")
	began := time.Now()
	statusOf(t, h.b, "a")
	if took := time.Since(began); took > 2*time.Second {
		t.Fatalf("the read took %s; its recording has a deadline", took)
	}
	requireLockFree(t, h.b)
	hang.Store(false)
	if a := statusOf(t, h.b, "a"); a.Memory == nil || calls.Load() != 2 {
		t.Fatalf("the figure the deadline cut off stays due for the next read: %+v, %d probes", a, calls.Load())
	}
}

func TestAReaderThatGivesUpLeavesTheFigureDue(t *testing.T) {
	var hang atomic.Bool
	hang.Store(true)
	entered := make(chan struct{}, 1)
	h := newHarnessWith(t, 0, func(_ *llamatest.Server, o *Options) {
		o.Probe = probeFunc(func(ctx context.Context, _ int) (backend.Memory, error) {
			if hang.Load() {
				entered <- struct{}{}
				<-ctx.Done()
				return backend.Memory{}, ctx.Err()
			}
			return backend.Memory{Bytes: 1 << 30, Source: "measured"}, nil
		})
	})
	h.fake.SetStatus("a", "loaded")
	ctx, cancel := context.WithCancel(context.Background())
	read := readAsync(ctx, h.b)
	awaitSignal(t, entered, "the read's measure")
	cancel() // the client hung up in the middle of the recording
	awaitRead(t, read, 2*time.Second, "a read whose caller gave up")
	requireLockFree(t, h.b)
	hang.Store(false)
	if a := statusOf(t, h.b, "a"); a.Memory == nil {
		t.Fatalf("the figure stays due for the next read: %+v", a)
	}
}

func TestASleepingModelGetsItsPropsButNoMemory(t *testing.T) {
	h := newHarness(t, 0)
	h.fake.SetStatus("a", "sleeping") // first seen asleep, as after the engine restarts with an attached router
	a := statusOf(t, h.b, "a")
	if a.Status != backend.StatusSleeping || a.Props == nil || a.Memory != nil || len(h.probe.pids) != 0 {
		t.Fatalf("a sleeping model keeps a fraction of a run's memory, so only its props are recorded: %+v, probes %v",
			a, h.probe.pids)
	}
	h.fake.SetStatus("a", "loaded") // a request woke it
	if a := statusOf(t, h.b, "a"); a.Memory == nil || len(h.probe.pids) != 1 {
		t.Fatalf("once it runs, its memory is recorded: %+v, probes %v", a, h.probe.pids)
	}
}
