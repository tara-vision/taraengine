package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tara-vision/taraengine/pkg/client"
)

const interrupted = "the pull was interrupted (run the same pull again to resume): context canceled"

// runWithContext is run for a command that the caller can cancel, with an output that can be read while it runs.
func runWithContext(ctx context.Context, t *testing.T, out io.Writer, args ...string) error {
	t.Helper()
	root := NewRootCommand()
	root.SetOut(out)
	root.SetErr(out)
	root.SetArgs(args)
	return root.ExecuteContext(ctx)
}

func TestPullCanceledBeforeItStartsIsInterruptedWhetherOrNotTheDaemonIsThere(t *testing.T) {
	srv, _ := daemon(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for name, host := range map[string]string{"a daemon that is up": srv.URL, "a daemon that is down": "http://127.0.0.1:1"} {
		var out syncBuffer
		err := runWithContext(ctx, t, &out, "pull", "gemma4:12b", "--host="+host)
		if err == nil || err.Error() != interrupted || !errors.Is(err, context.Canceled) {
			t.Errorf("%s: got %v", name, err)
		}
		if out.String() != "" {
			t.Errorf("%s: nothing was pulled, so nothing is shown: %q", name, out.String())
		}
	}
}

func TestPullCanceledMidStreamIsInterruptedAndNotAnEarlyEnd(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: {\"stage\":\"resolving\",\"file\":\"m.gguf\"}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done() // the stream stays open until the client goes away
	}))
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var out syncBuffer
	done := make(chan error, 1)
	go func() { done <- runWithContext(ctx, t, &out, "pull", "m", "--host="+srv.URL) }()
	for deadline := time.Now().Add(5 * time.Second); !strings.Contains(out.String(), "resolving m.gguf"); {
		if time.Now().After(deadline) {
			t.Fatalf("the stream never started: %q", out.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err == nil || err.Error() != interrupted || !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the pull did not stop when its context was canceled")
	}
}

func TestPullFailureChecksCancellationBeforeTheIncompleteStream(t *testing.T) {
	f := &globalFlags{host: "http://127.0.0.1:8272"}
	both := fmt.Errorf("%w: reading the pull stream: %w", client.ErrIncompleteStream, context.Canceled)
	if err := f.pullFailure(both); err == nil || err.Error() != interrupted {
		t.Fatalf("an interrupted pull matches both, and is the caller's doing: %v", err)
	}
	early := fmt.Errorf("%w: reading the pull stream: %w", client.ErrIncompleteStream, io.ErrUnexpectedEOF)
	if err := f.pullFailure(early); err == nil || !strings.Contains(err.Error(), "the stream ended before the pull finished") {
		t.Fatalf("a stream cut by the other side is an early end: %v", err)
	}
}
