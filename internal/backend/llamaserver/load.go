package llamaserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/tara-vision/taraengine/internal/backend"
	"github.com/tara-vision/taraengine/internal/state"
)

// The status values of the router that the backend acts on.
const (
	routerUnloaded = "unloaded"
	routerLoading  = "loading"
	routerLoaded   = "loaded"
	routerSleeping = "sleeping"
)

// Load brings the model up at the requested context, waits for it, records the outcome, then reads
// the props and measures the memory. Loads are serialized. opts.Draft needs nothing from the backend:
// the draft sibling of a pulled model is always part of its preset.
func (b *Backend) Load(ctx context.Context, name string, opts backend.LoadOptions) error {
	b.opMu.Lock()
	defer b.opMu.Unlock()
	preset, faulty, known := b.lookup(name)
	if !known {
		return fmt.Errorf("%w: %s", backend.ErrUnknownModel, name)
	}
	if faulty && !opts.Force {
		return fmt.Errorf("%w: %s", backend.ErrFaulty, name)
	}
	want := preset.Context
	if opts.Context > 0 {
		want = opts.Context
	}
	current, err := b.routerStatus(ctx, name)
	if err != nil {
		return err
	}
	switch {
	case want != preset.Context:
		return b.loadAtNewContext(ctx, name, current, want)
	case current.Status == routerLoaded:
		b.recordMissing(ctx, name, false, slog.LevelWarn)
		return nil
	}
	return b.bringUp(ctx, name, current, want)
}

// loadAtNewContext loads the model at a window other than the one its preset carries. The window is
// remembered and rendered into the preset first, because the router reads it from there. If the load
// then does not happen, whatever the reason, the model's earlier override comes back, so that one refused
// window is not tried again by every load after it.
func (b *Backend) loadAtNewContext(ctx context.Context, name string, current RouterModel, window int) error {
	prior := b.overrideOf(name)
	if err := b.commitState(func(s state.State) state.State { return s.WithOverride(name, window) }); err != nil {
		return err // nothing was changed
	}
	err := b.applyContext(ctx, name, window)
	if err == nil {
		err = b.bringUp(ctx, name, current, window)
	}
	if err != nil {
		return errors.Join(err, b.restoreContext(ctx, name, prior, err))
	}
	return nil
}

// bringUp unloads the model if it is running, has the router load it, waits, and records the outcome.
func (b *Backend) bringUp(ctx context.Context, name string, current RouterModel, window int) error {
	if current.Status == routerLoaded || current.Status == routerSleeping {
		if err := b.unload(ctx, name); err != nil {
			return err
		}
	}
	if err := b.client.Load(ctx, SectionID(name)); err != nil {
		return err
	}
	final, err := b.awaitLoaded(ctx, name)
	if err != nil {
		return err
	}
	return b.finishLoad(ctx, name, final, window)
}

// finishLoad records how the load ended. A failed load counts against the model; a good one clears the
// count and caches the model's props and memory. A state that cannot be saved is reported together with
// the outcome, never instead of it. The failure names the exit code and the window the model ran at.
func (b *Backend) finishLoad(ctx context.Context, name string, final RouterModel, window int) error {
	if final.Failed {
		failure := fmt.Errorf("%w: %s exited with exit code %d at %s",
			backend.ErrLoadFailed, name, final.ExitCode, windowLabel(window))
		saved := b.updateState(func(s state.State) state.State { return s.MarkFailedLoad(name) })
		return errors.Join(failure, saved)
	}
	saved := b.updateState(func(s state.State) state.State { return s.ClearFailures(name).WithLastLoaded(name) })
	b.record(ctx, name, final.Status == routerSleeping)
	return saved
}

// windowLabel says which context window a load ran at; a preset without one leaves it to the model.
func windowLabel(window int) string {
	if window > 0 {
		return "context " + strconv.Itoa(window)
	}
	return "the model's default context"
}

// applyContext has the router re-read the preset file and checks that its own copy of the model's
// section shows the new window.
func (b *Backend) applyContext(ctx context.Context, name string, window int) error {
	if err := b.refresh(ctx); err != nil {
		return err
	}
	after, err := b.routerStatus(ctx, name)
	if err != nil {
		return err
	}
	if !presetHasContext(after.Preset, window) {
		return fmt.Errorf("llamaserver: the router did not pick up the context %d for %s; restart taraengine", window, name)
	}
	return nil
}

// priorOverride is the context override a model had before a load changed it, if it had one.
type priorOverride struct {
	window int
	set    bool
}

func (b *Backend) overrideOf(name string) priorOverride {
	b.mu.Lock()
	defer b.mu.Unlock()
	window, set := b.st.Overrides[name]
	return priorOverride{window: window, set: set}
}

// putBack is s with the model's override as it was before the load: the old window, or none. State has
// no way to drop an override, so the copy of it that WithOverride returns, which nobody else holds, is edited.
func (p priorOverride) putBack(s state.State, name string) state.State {
	if p.set {
		return s.WithOverride(name, p.window)
	}
	out := s.WithOverride(name, 0)
	delete(out.Overrides, name)
	return out
}

// restoreContext undoes loadAtNewContext's change of window: the earlier override comes back in state and
// in the preset file, and the router re-reads the file. It runs where nobody can cancel it, because a
// caller that gave up is one of the reasons to be here; the load such a caller left behind is stopped
// first, so the model does not come up at the window being taken back. Whatever fails, apart from that
// courtesy, is returned for the load's error.
func (b *Backend) restoreContext(ctx context.Context, name string, prior priorOverride, cause error) error {
	calm := context.WithoutCancel(ctx)
	if errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded) {
		if err := b.unload(calm, name); err != nil {
			b.opts.Logger.Warn("could not stop the load that was given up", "model", name, "error", err)
		}
	}
	saved := b.updateState(func(s state.State) state.State { return prior.putBack(s, name) })
	return errors.Join(saved, b.refresh(calm))
}

// presetHasContext reports whether the router's copy of a preset section carries the window. The router
// echoes long option names, so a context written as c comes back as ctx-size.
func presetHasContext(preset string, window int) bool {
	want := strconv.Itoa(window)
	for _, line := range strings.Split(preset, "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		if k := strings.TrimSpace(key); (k == "ctx-size" || k == "c") && strings.TrimSpace(value) == want {
			return true
		}
	}
	return false
}

// routerStatus is the router's entry for a catalog model.
func (b *Backend) routerStatus(ctx context.Context, name string) (RouterModel, error) {
	list, err := b.client.Models(ctx, false)
	if err != nil {
		return RouterModel{}, err
	}
	id := SectionID(name)
	for _, m := range list {
		if m.ID == id {
			return m, nil
		}
	}
	return RouterModel{}, fmt.Errorf("%w: %s", backend.ErrUnknownModel, name)
}

// awaitLoaded polls until the model is loaded or has failed, or the load timeout passes; on a
// timeout the load is cancelled with an unload so the next load starts clean. A model the router lists
// as unloaded again, without a failure, after it was seen loading has been dropped, as another load
// that evicts it does, and the wait ends there instead of holding every other operation until the
// timeout.
func (b *Backend) awaitLoaded(ctx context.Context, name string) (RouterModel, error) {
	deadline := time.Now().Add(b.opts.LoadTimeout)
	seenLoading := false
	for {
		m, err := b.routerStatus(ctx, name)
		if err != nil {
			return m, err
		}
		switch {
		case m.Status == routerLoaded || m.Failed:
			return m, nil
		case m.Status == routerLoading:
			seenLoading = true
		case seenLoading && m.Status == routerUnloaded:
			return m, fmt.Errorf("llamaserver: the router stopped loading %s before it was up; "+
				"another load may have taken its place", name)
		}
		if time.Now().After(deadline) {
			if err := b.client.Unload(context.WithoutCancel(ctx), SectionID(name)); err != nil {
				b.opts.Logger.Warn("could not cancel the load that timed out", "model", name, "error", err)
			}
			return m, fmt.Errorf("%w: %s after %s", backend.ErrLoadTimeout, name, b.opts.LoadTimeout)
		}
		select {
		case <-ctx.Done():
			return m, ctx.Err()
		case <-time.After(b.opts.Poll):
		}
	}
}
