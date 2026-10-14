package cli

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tara-vision/taraengine/internal/backend/llamaserver"
	"github.com/tara-vision/taraengine/internal/llamatest"
)

// serveWired serves an already wired daemon on ln until ctx ends, the way serve does once it has switched to the full
// handler.
func serveWired(ctx context.Context, ln net.Listener, wired *Wired, logger *slog.Logger, grace time.Duration) error {
	srv, failed := startHTTP(ln, wired.Handler, wired.Config.DataDir, logger)
	return await(ctx, srv, failed, wired.Backend, logger, grace)
}

// brokenListener fails to accept at once, so that the HTTP server built on it stops by itself.
type brokenListener struct{}

func (brokenListener) Accept() (net.Conn, error) { return nil, errors.New("accept failed") }
func (brokenListener) Close() error              { return nil }
func (brokenListener) Addr() net.Addr            { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1} }

func TestServeWiredReportsAServerThatStopsOnItsOwn(t *testing.T) {
	dataDir := t.TempDir()
	wired, _ := wireAttached(t, dataDir, attachedConfig(dataDir), debugLogger(io.Discard))
	err := serveWired(context.Background(), brokenListener{}, wired, debugLogger(io.Discard), time.Second)
	if err == nil || !strings.Contains(err.Error(), "serve: accept failed") {
		t.Fatalf("%v", err)
	}
}

func TestServeWiredClosesWhatOutlivesTheGraceAndStillReturnsCleanly(t *testing.T) {
	dataDir := t.TempDir()
	putModel(t, dataDir, "gemma4:e4b")
	fake := llamatest.New(t, llamatest.Options{
		APIKey: "k", PresetPath: llamaserver.PresetPath(dataDir), ChatDelay: 500 * time.Millisecond,
	})
	var logs syncBuffer
	logger := debugLogger(&logs)
	wired, err := Wire(context.Background(), attachedConfig(dataDir), WireOptions{Attach: fake.URL(), AttachKey: "k", Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- serveWired(ctx, ln, wired, logger, 100*time.Millisecond) }()

	body := `{"model":"gemma4:e4b","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	resp, err := http.Post("http://"+ln.Addr().String()+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	reader := bufio.NewReader(resp.Body)
	if line, err := reader.ReadString('\n'); err != nil || !strings.HasPrefix(line, "data: ") {
		t.Fatalf("the stream did not start: %q %v", line, err)
	}

	start := time.Now()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("a shutdown on request is a clean exit: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serveOn waited for a stream that takes seconds to finish")
	}
	if took := time.Since(start); took > 1500*time.Millisecond {
		t.Fatalf("shutdown took %s although the grace is 100ms", took)
	}
	rest, _ := io.ReadAll(reader)
	if strings.Contains(string(rest), "[DONE]") {
		t.Fatalf("the stream ran to its end instead of being closed:\n%s", rest)
	}
	for _, want := range []string{"taraengine listening", "taraengine stopping", "still running"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("the log lacks %q:\n%s", want, logs.String())
		}
	}
}

func TestServeWiredStopsAnIdleDaemonAtOnce(t *testing.T) {
	dataDir := t.TempDir()
	var logs syncBuffer
	wired, _ := wireAttached(t, dataDir, attachedConfig(dataDir), debugLogger(&logs))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serveWired(ctx, ln, wired, debugLogger(&logs), time.Minute) }()
	if resp, _ := get(t, "http://"+ln.Addr().String()+"/api/health"); resp.StatusCode != http.StatusOK {
		t.Fatalf("health: %d", resp.StatusCode)
	}
	http.DefaultClient.CloseIdleConnections()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("an idle daemon must not wait out the grace")
	}
	if strings.Contains(logs.String(), "still running") {
		t.Fatalf("nothing was running:\n%s", logs.String())
	}
}
