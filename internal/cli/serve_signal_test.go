package cli

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/tara-vision/taraengine/internal/backend/llamaserver"
	"github.com/tara-vision/taraengine/internal/llamatest"
)

// captureStdout points os.Stdout at a file for the rest of the test, so that Execute, which writes to the real
// standard output, leaves the test log clean.
func captureStdout(t *testing.T) {
	t.Helper()
	f, err := os.Create(filepath.Join(t.TempDir(), "stdout.txt"))
	if err != nil {
		t.Fatal(err)
	}
	previous := os.Stdout
	os.Stdout = f
	t.Cleanup(func() {
		os.Stdout = previous
		_ = f.Close()
	})
}

// withArgs makes os.Args the command line `taraengine args...` for the rest of the test.
func withArgs(t *testing.T, args ...string) {
	t.Helper()
	previous := os.Args
	os.Args = append([]string{"taraengine"}, args...)
	t.Cleanup(func() { os.Args = previous })
}

// hangupIgnored makes the test process ignore SIGHUP, as it does under nohup, or take it by default, for the rest of
// the test and with the default restored after it. signal.Reset would not do for the restore: it leaves a signal
// that was ignored ignored.
func hangupIgnored(t *testing.T, ignored bool) {
	t.Helper()
	set := func(ignored bool) {
		if ignored {
			signal.Ignore(syscall.SIGHUP)
			return
		}
		taken := make(chan os.Signal, 1)
		signal.Notify(taken, syscall.SIGHUP) // Notify ends the ignoring, and the handler it installs stays
		signal.Stop(taken)
	}
	set(ignored)
	t.Cleanup(func() { set(false) })
}

func TestServeStopsOnAHangupAsOnATermination(t *testing.T) {
	hangupIgnored(t, false)
	s := startServe(t, llamatest.Options{}, nil)
	if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	if err := s.wait(10 * time.Second); err != nil {
		t.Fatalf("a hangup must shut the daemon down cleanly, so that it does not leave its child behind: %v", err)
	}
}

func TestExecuteTurnsASignalIntoTheInterruptionOfAPull(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM} {
		t.Run(sig.String(), func(t *testing.T) {
			captureStdout(t)
			arrived := make(chan struct{}, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, "data: {\"stage\":\"resolving\",\"file\":\"m.gguf\"}\n\n")
				w.(http.Flusher).Flush()
				arrived <- struct{}{}
				<-r.Context().Done() // the stream stays open until the client goes away
			}))
			t.Cleanup(srv.Close)
			withArgs(t, "pull", "m", "--host="+srv.URL)
			done := make(chan error, 1)
			go func() { done <- Execute() }()
			select {
			case <-arrived:
			case <-time.After(5 * time.Second):
				t.Fatal("the pull never reached the server")
			}
			if err := syscall.Kill(os.Getpid(), sig); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if err == nil || err.Error() != interrupted {
					t.Fatalf("got %v, want %q", err, interrupted)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("the pull did not stop on the signal")
			}
		})
	}
}

func TestExecuteLetsServeShutDownOnASignalWithoutTheTwoFighting(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM} {
		t.Run(sig.String(), func(t *testing.T) {
			captureStdout(t)
			dataDir := t.TempDir()
			fake := llamatest.New(t, llamatest.Options{APIKey: "k", PresetPath: llamaserver.PresetPath(dataDir)})
			listen := freeAddr(t)
			withArgs(t, "serve", "--listen", listen, "--data-dir", dataDir, "--attach", fake.URL(), "--attach-key", "k")
			done := make(chan error, 1)
			go func() { done <- Execute() }()
			probe := &http.Client{Timeout: 5 * time.Second}
			for i := 0; !healthOK(probe, "http://"+listen); i++ {
				if i >= 200 {
					t.Fatal("the api did not come up")
				}
				time.Sleep(25 * time.Millisecond)
			}
			if err := syscall.Kill(os.Getpid(), sig); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("a shutdown on a signal is a clean exit, got %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("serve did not stop on the signal")
			}
		})
	}
}

func TestServeKeepsRunningOnAHangupThatWasIgnoredAtStart(t *testing.T) {
	// `nohup taraengine serve &` starts the daemon with SIGHUP ignored, and the terminal that closes must not stop it.
	hangupIgnored(t, true)
	s := startServe(t, llamatest.Options{}, nil)
	if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	if err := s.wait(500 * time.Millisecond); err == nil || err.Error() != "serve did not stop" {
		t.Fatalf("a hangup that nohup ignores stopped the daemon: %v", err)
	}
	resp, err := http.Get("http://" + s.listen + "/api/health")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("the daemon stopped answering: %v", err)
	}
	_ = resp.Body.Close()
}

func TestTheSignalsAreLetGoOfAsSoonAsTheFirstOneArrives(t *testing.T) {
	released := make(chan struct{}, 4)
	var fire context.CancelFunc
	previous := notifyContext
	notifyContext = func(parent context.Context, _ ...os.Signal) (context.Context, context.CancelFunc) {
		ctx, cancel := context.WithCancel(parent)
		fire = cancel
		return ctx, func() {
			released <- struct{}{}
			cancel()
		}
	}
	t.Cleanup(func() { notifyContext = previous })
	ctx, stop := untilSignal(context.Background(), os.Interrupt)
	select {
	case <-released:
		t.Fatal("the signals were let go of before any arrived")
	case <-time.After(50 * time.Millisecond):
	}
	fire() // the first signal
	select {
	case <-released:
	case <-time.After(5 * time.Second):
		t.Fatal("after the first signal the next one must end the process in the usual way, so the signals are let go of")
	}
	<-ctx.Done()
	stop()
}
