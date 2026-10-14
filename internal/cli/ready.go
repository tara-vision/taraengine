package cli

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/tara-vision/taraengine/pkg/api"
	"github.com/tara-vision/taraengine/pkg/client"
)

// How long a command waits for an engine that is starting, how often it asks, and how long it bears an engine that
// does not answer at all before it gives up: one that cannot bind its port, for one, never answers again. They are
// variables only so that a test can wait less.
var (
	startWait    = 10 * time.Minute
	pollEvery    = time.Second
	refusedGrace = 20 * time.Second
)

// connect resolves the daemon's address and key and makes sure the engine there is ready for the command. Progress
// goes to the command's stderr.
func (f *globalFlags) connect(cmd *cobra.Command) (*globalFlags, error) {
	r, err := f.resolved()
	if err != nil {
		return nil, err
	}
	if err := r.ready(cmd.Context(), cmd.ErrOrStderr(), f.host != ""); err != nil {
		return nil, err
	}
	return r, nil
}

// ready asks the engine for its health once. An engine that is starting is waited for. An engine that does not
// answer at the address the user did not name with --host is the local service's business: ready starts the macOS
// agent that is installed and not loaded, unless the user stopped it, or else says what fits the service's state.
// Any other answer is left to the command, which reports what it meets.
func (f *globalFlags) ready(ctx context.Context, w io.Writer, hostGiven bool) error {
	h, err := f.client().Health(ctx)
	switch {
	case err == nil && h.Status == api.HealthStarting:
		return f.awaitEngine(ctx, w)
	case err == nil, !client.IsConnectionError(err):
		return nil
	case hostGiven:
		return f.explain(err)
	}
	return f.startOrExplain(ctx, w, err)
}

// startOrExplain starts the macOS agent on first use, or explains with the service's state why nothing answers. An
// agent that is not loaded is started, and so is one that ended cleanly, which its KeepAlive leaves down; one that
// crashed is not started behind the user's back.
func (f *globalFlags) startOrExplain(ctx context.Context, w io.Writer, cause error) error {
	m, err := newServiceManager()
	if err != nil {
		return f.explain(cause)
	}
	st, err := m.State(ctx)
	if err != nil {
		return f.explain(cause)
	}
	switch {
	case hostOS == "darwin" && st.Installed && st.Enabled && (!st.Loaded || !st.Running && st.LastExit == "0"):
		if err := m.Start(ctx); err != nil {
			return err
		}
		if _, err := fmt.Fprintln(w, "started the taraengine service; it also starts at every login"); err != nil {
			return err
		}
		return f.awaitEngine(ctx, w)
	case st.Installed && !st.Enabled:
		return fmt.Errorf("taraengine is not running at %s: the service is stopped; "+
			"start it with `taraengine service start`", f.host)
	case st.Installed:
		return fmt.Errorf("taraengine is not running at %s: the service did not start; "+
			"`taraengine service status` shows why", f.host)
	}
	return fmt.Errorf("taraengine is not running at %s: start it with `taraengine service install`, "+
		"or `taraengine serve` in a terminal", f.host)
}

// awaitEngine polls the engine's health until it is no longer starting, and prints every new step of a starting
// engine. It gives up at startWait, after refusedGrace without any answer, at an answer that is an error, or when
// ctx ends.
func (f *globalFlags) awaitEngine(ctx context.Context, w io.Writer) error {
	c := f.client()
	deadline := time.Now().Add(startWait)
	answered := time.Now()
	last := ""
	for {
		h, err := c.Health(ctx)
		switch {
		case err == nil && h.Status != api.HealthStarting:
			return nil
		case err == nil:
			answered = time.Now()
			if h.Detail != last {
				last = h.Detail
				if _, err := fmt.Fprintf(w, "taraengine is starting: %s\n", h.Detail); err != nil {
					return err
				}
			}
		case !client.IsConnectionError(err):
			return f.explain(err)
		case time.Since(answered) > refusedGrace:
			return fmt.Errorf("taraengine stopped answering at %s; `taraengine service status` shows why", f.host)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("taraengine did not get ready within %s at %s; `taraengine service status` shows why",
				startWait, f.host)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollEvery):
		}
	}
}
