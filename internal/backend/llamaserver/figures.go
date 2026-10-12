package llamaserver

import (
	"context"
	"errors"
	"log/slog"

	"github.com/tara-vision/taraengine/internal/backend"
	"github.com/tara-vision/taraengine/internal/measure"
)

// want says which figures of a running model a recording reads.
type want struct {
	props  bool
	memory bool
}

// figures is what one recording found. A figure it could not have is nil; noMemory says that this run of the model
// has no memory figure to give, so that no recording asks for one again until the model stops.
type figures struct {
	props    *backend.Props
	memory   *backend.Memory
	noMemory bool
}

// record caches the figures of a model that a load just brought up, in place of whatever an earlier run left, so a
// figure never outlives the process it described. A model already asleep gets its props only: its child keeps a
// fraction of the memory of a run. Neither figure is the load's to fail.
func (b *Backend) record(ctx context.Context, name string, asleep bool) {
	got := b.readFigures(ctx, name, want{props: true, memory: !asleep}, slog.LevelWarn)
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.props, name)
	delete(b.memory, name)
	delete(b.noMemory, name)
	b.keepLocked(name, got)
}

// recordMissing caches the figures a running model lacks and never replaces one it has, as for a model that a
// request brought up on its own. A sleeping model gets its props only. Every recording runs under opMu, so no
// other one caches a figure between the look at what is missing and the keep.
func (b *Backend) recordMissing(ctx context.Context, name string, asleep bool, level slog.Level) {
	b.mu.Lock()
	need := b.missingLocked(name, asleep)
	b.mu.Unlock()
	if !need.props && !need.memory {
		return
	}
	got := b.readFigures(ctx, name, need, level)
	b.mu.Lock()
	defer b.mu.Unlock()
	b.keepLocked(name, got)
}

// missingLocked says which figures of a running model are due: the props until they are cached, and the memory of
// a model that is up until it is cached or a recording found that this run has none. The caller holds b.mu.
func (b *Backend) missingLocked(name string, asleep bool) want {
	_, hasProps := b.props[name]
	_, hasMemory := b.memory[name]
	return want{props: !hasProps, memory: !asleep && !hasMemory && !b.noMemory[name]}
}

// keepLocked caches what a recording found. The caller holds b.mu.
func (b *Backend) keepLocked(name string, got figures) {
	if got.props != nil {
		b.props[name] = *got.props
	}
	if got.memory != nil {
		b.memory[name] = *got.memory
	}
	if got.noMemory {
		b.noMemory[name] = true
	}
}

// readFigures reads the figures need asks for: the props, with autoload off so that the read never starts a model
// the router stopped meanwhile, and the memory of the process tree. A failure is logged at level and leaves the
// figure out.
func (b *Backend) readFigures(ctx context.Context, name string, need want, level slog.Level) figures {
	var got figures
	if need.props {
		props, err := b.client.Props(ctx, SectionID(name))
		if err != nil {
			b.opts.Logger.Log(ctx, level, "props unavailable", "model", name, "error", err)
		} else {
			got.props = &props
		}
	}
	if need.memory {
		got.memory, got.noMemory = b.measureMemory(ctx, name, level)
	}
	return got
}

// measureMemory measures the process tree of the supervised child, or of the attached router. none reports that
// this run has no figure to give: there is no probe or no process to measure, or the probe sees no memory held by
// the tree, as when nvidia-smi cannot see the router's processes from inside a container. A probe that breaks, or a
// caller that gives up, is not final, and the next recording tries again.
func (b *Backend) measureMemory(ctx context.Context, name string, level slog.Level) (mem *backend.Memory, none bool) {
	pid := b.opts.AttachPID
	if sup := b.supervisor(); sup != nil {
		pid = sup.PID()
	}
	if b.opts.Probe == nil || pid <= 0 {
		return nil, true
	}
	got, err := b.opts.Probe.Measure(ctx, pid)
	switch {
	case errors.Is(err, measure.ErrNoFigure):
		return nil, true
	case err != nil:
		b.opts.Logger.Log(ctx, level, "memory probe failed", "model", name, "error", err)
		return nil, false
	}
	return &got, false
}
