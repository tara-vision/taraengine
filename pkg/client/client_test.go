package client_test

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tara-vision/taraengine/internal/api"
	"github.com/tara-vision/taraengine/internal/backend"
	"github.com/tara-vision/taraengine/internal/backend/backendtest"
	"github.com/tara-vision/taraengine/internal/catalog"
	"github.com/tara-vision/taraengine/internal/config"
	"github.com/tara-vision/taraengine/internal/machine"
	"github.com/tara-vision/taraengine/internal/pull"
	"github.com/tara-vision/taraengine/internal/registry"
	apitypes "github.com/tara-vision/taraengine/pkg/api"
	"github.com/tara-vision/taraengine/pkg/client"
)

// fetcher resolves every file to the sha256 it was given, and downloads eight bytes.
type fetcher struct{ sha256 string }

func (f fetcher) Resolve(_ context.Context, repo, file string) (pull.Artifact, error) {
	return pull.Artifact{Repo: repo, File: file, Size: 8, SHA256: f.sha256}, nil
}

func (fetcher) Fetch(_ context.Context, _ pull.Artifact, dest string, onProgress func(pull.Progress)) error {
	_ = os.MkdirAll(filepath.Dir(dest), 0o755)
	onProgress(pull.Progress{Stage: "downloading", Bytes: 4, Total: 8})
	onProgress(pull.Progress{Stage: "done", Bytes: 8, Total: 8})
	return os.WriteFile(dest, []byte("12345678"), 0o600)
}

// brokenFetcher resolves like fetcher and then fails every download.
type brokenFetcher struct{ fetcher }

func (brokenFetcher) Fetch(context.Context, pull.Artifact, string, func(pull.Progress)) error {
	return errors.New("disk full")
}

// glmSHA256 is the sha256 the registry pins for glm-4.7-flash, the model the tests pull. The server refuses
// a file whose sha256 differs from the pin, so the fetcher must answer with the registry's value.
func glmSHA256(t *testing.T) string {
	t.Helper()
	reg, err := registry.Load()
	if err != nil {
		t.Fatal(err)
	}
	entry, ok := reg.Find("glm-4.7-flash")
	if !ok {
		t.Fatal("glm-4.7-flash is not in the registry")
	}
	return entry.SHA256
}

// serverWith runs the api server over the fake backend, with the API key "secret".
func serverWith(t *testing.T, f api.Fetcher) (*httptest.Server, *backendtest.Fake) {
	t.Helper()
	reg, err := registry.Load()
	if err != nil {
		t.Fatal(err)
	}
	store := pull.Store{Dir: filepath.Join(t.TempDir(), "models")}
	cfg := config.Default()
	cfg.APIKey = "secret"
	fake := backendtest.New()
	h := api.New(api.Deps{
		Backend: fake,
		Catalog: catalog.Catalog{Registry: reg, Store: store, Index: &catalog.Index{Path: filepath.Join(store.Dir, "index.json")}},
		Config:  cfg,
		Fetcher: f,
		Machine: func(context.Context) machine.Info { return machine.Info{OS: "darwin", Tier: "48"} },
		Runtime: func() apitypes.RuntimeStatus {
			return apitypes.RuntimeStatus{Installed: true, Verified: true, Build: "bTEST"}
		},
		Version: "0.1.0-test", Build: "bTEST", Platform: "darwin/arm64",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv, fake
}

// server is serverWith with the fetcher that answers with the registry's pin for glm-4.7-flash.
func server(t *testing.T) (*httptest.Server, *backendtest.Fake) {
	t.Helper()
	return serverWith(t, fetcher{sha256: glmSHA256(t)})
}

// raw starts a server that answers every request with one status, content type and body.
func raw(t *testing.T, status int, contentType, body string) *client.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return client.New(srv.URL, "")
}

//nolint:gocyclo // one linear scenario of the API, kept as one function so the sequence reads top to bottom
func TestClientRoundTrips(t *testing.T) {
	srv, fake := server(t)
	c := client.New(srv.URL, "secret")
	ctx := context.Background()
	if h, err := c.Health(ctx); err != nil || h.Status != "ok" {
		t.Fatalf("health: %+v %v", h, err)
	}
	if v, err := c.Version(ctx); err != nil || v.Version != "0.1.0-test" {
		t.Fatalf("version: %+v %v", v, err)
	}
	var events []apitypes.PullEvent
	if err := c.Pull(ctx, "glm-4.7-flash", func(e apitypes.PullEvent) { events = append(events, e) }); err != nil {
		t.Fatalf("pull: %v", err)
	}
	if len(events) < 2 || events[len(events)-1].Stage != "done" || events[len(events)-1].File != "" {
		t.Fatalf("pull events: %+v", events)
	}
	fake.SetModels(backend.ModelStatus{Name: "glm-4.7-flash", Status: backend.StatusPulled})
	m, err := c.Load(ctx, "glm-4.7-flash", 8192, false)
	if err != nil || m.Status != "loaded" || m.Context != 8192 {
		t.Fatalf("load: %+v %v", m, err)
	}
	if list, err := c.Loaded(ctx); err != nil || len(list) != 1 {
		t.Fatalf("loaded: %+v %v", list, err)
	}
	if all, err := c.Models(ctx); err != nil || len(all) != 8 {
		t.Fatalf("models: %d %v", len(all), err)
	}
	if d, err := c.Model(ctx, "glm-4.7-flash"); err != nil || d.Preset["c"] != "8192" {
		t.Fatalf("model: %+v %v", d, err)
	}
	if err := c.Unload(ctx, "glm-4.7-flash"); err != nil {
		t.Fatalf("unload: %v", err)
	}
	if doc, err := c.Doctor(ctx); err != nil || doc.Machine.Tier != "48" || doc.Recommendation["48"] != "qwen3.6:35b" {
		t.Fatalf("doctor: %+v %v", doc, err)
	}
	if err := c.Delete(ctx, "glm-4.7-flash"); err != nil {
		t.Fatalf("delete: %v", err)
	}
}

func TestClientErrors(t *testing.T) {
	srv, fake := server(t)
	c := client.New(srv.URL, "secret")
	ctx := context.Background()
	var apiErr *client.Error
	if _, err := c.Model(ctx, "absent"); !errors.As(err, &apiErr) || apiErr.Status != http.StatusNotFound {
		t.Fatalf("404: %v", err)
	}
	if err := c.Delete(ctx, "absent"); !errors.As(err, &apiErr) || apiErr.Status != http.StatusNotFound {
		t.Fatalf("delete of an absent model: %v", err)
	}
	fake.SetModels(backend.ModelStatus{Name: "gemma4:12b", Status: backend.StatusFaulty})
	fake.Fail("load", backend.ErrFaulty)
	if _, err := c.Load(ctx, "gemma4:12b", 0, false); !errors.As(err, &apiErr) || apiErr.Status != http.StatusConflict || apiErr.Hint == "" {
		t.Fatalf("409: %v", err)
	}
	wrong := client.New(srv.URL, "nope")
	if _, err := wrong.Version(ctx); !errors.As(err, &apiErr) || apiErr.Status != http.StatusUnauthorized {
		t.Fatalf("401: %v", err)
	}
	down := client.New("http://127.0.0.1:1", "")
	if _, err := down.Version(ctx); err == nil || !client.IsConnectionError(err) {
		t.Fatalf("connection error not classified: %v", err)
	}
}

func TestErrorRendersTheHintInParentheses(t *testing.T) {
	withHint := &client.Error{Status: http.StatusConflict, Message: "faulty", Hint: "try again later"}
	if got := withHint.Error(); got != "faulty (try again later)" {
		t.Fatalf("with a hint: %q", got)
	}
	bare := &client.Error{Status: http.StatusNotFound, Message: "unknown model"}
	if got := bare.Error(); got != "unknown model" {
		t.Fatalf("without a hint: %q", got)
	}
}

func TestPullRefusalsArriveBeforeAnyEvent(t *testing.T) {
	srv, _ := server(t)
	c := client.New(srv.URL, "secret")
	var events []apitypes.PullEvent
	onEvent := func(e apitypes.PullEvent) { events = append(events, e) }
	var apiErr *client.Error
	if err := c.Pull(context.Background(), "absent", onEvent); !errors.As(err, &apiErr) || apiErr.Status != http.StatusNotFound {
		t.Fatalf("unknown catalog name: %v", err)
	}
	local := filepath.Join(t.TempDir(), "glm-4.7-flash.gguf")
	if err := c.Pull(context.Background(), local, onEvent); !errors.As(err, &apiErr) || apiErr.Status != http.StatusConflict {
		t.Fatalf("catalog name given as a local file: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("refusals sent %d events: %+v", len(events), events)
	}
}

func TestPullFailureEndsWithAnErrorEvent(t *testing.T) {
	srv, _ := serverWith(t, brokenFetcher{fetcher{sha256: glmSHA256(t)}})
	c := client.New(srv.URL, "secret")
	var events []apitypes.PullEvent
	err := c.Pull(context.Background(), "glm-4.7-flash", func(e apitypes.PullEvent) { events = append(events, e) })
	var apiErr *client.Error
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusBadGateway || apiErr.Message != "disk full" {
		t.Fatalf("failed pull: %v", err)
	}
	if len(events) == 0 || events[len(events)-1].Stage != "error" {
		t.Fatalf("events: %+v", events)
	}
}

func TestPullAcceptsANilCallback(t *testing.T) {
	srv, _ := server(t)
	if err := client.New(srv.URL, "secret").Pull(context.Background(), "glm-4.7-flash", nil); err != nil {
		t.Fatalf("pull without a callback: %v", err)
	}
}

func TestConnectionFailuresAreClassified(t *testing.T) {
	ctx := context.Background()
	down := client.New("http://127.0.0.1:1", "")
	if err := down.Pull(ctx, "glm-4.7-flash", nil); !client.IsConnectionError(err) {
		t.Fatalf("refused pull: %v", err)
	}
	bad := client.New("http://[::1", "")
	if _, err := bad.Health(ctx); err == nil || client.IsConnectionError(err) {
		t.Fatalf("malformed address: %v", err)
	}
	if err := bad.Pull(ctx, "glm-4.7-flash", nil); err == nil || client.IsConnectionError(err) {
		t.Fatalf("malformed address on a pull: %v", err)
	}
	if client.IsConnectionError(nil) || client.IsConnectionError(&client.Error{Status: http.StatusNotFound}) {
		t.Fatal("no error or an answer was classified as a connection error")
	}
}

func TestCanceledCallIsNotAConnectionError(t *testing.T) {
	srv, _ := server(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := client.New(srv.URL, "secret").Version(ctx)
	if err == nil || client.IsConnectionError(err) {
		t.Fatalf("canceled call: %v", err)
	}
}

func TestClientReadsPlainTextErrors(t *testing.T) {
	var apiErr *client.Error
	_, err := raw(t, http.StatusInternalServerError, "text/plain", "boom\n").Version(context.Background())
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusInternalServerError || apiErr.Message != "boom" || apiErr.Hint != "" {
		t.Fatalf("plain text error: %v", err)
	}
}

func TestClientRejectsUndecodableAnswers(t *testing.T) {
	ctx := context.Background()
	if _, err := raw(t, http.StatusOK, "application/json", "not json").Version(ctx); err == nil || !strings.Contains(err.Error(), "decode /api/version") {
		t.Fatalf("undecodable answer: %v", err)
	}
	var events []apitypes.PullEvent
	err := raw(t, http.StatusOK, "text/event-stream", "data: {oops}\n\n").Pull(ctx, "glm-4.7-flash", func(e apitypes.PullEvent) { events = append(events, e) })
	if err == nil || !strings.Contains(err.Error(), "pull event") || len(events) != 0 {
		t.Fatalf("malformed event: %v, events %+v", err, events)
	}
}

func TestPullWithoutTheOverallDoneEventIsIncomplete(t *testing.T) {
	perFile := `data: {"stage":"done","file":"GLM-4.7-Flash-Q4_K.gguf","bytes":8,"total":8}` + "\n\n"
	cases := []struct {
		name        string
		contentType string
		body        string
		events      int
	}{
		{"empty stream", "text/event-stream", "", 0},
		{"per-file done only", "text/event-stream", perFile, 1},
		{"html answer", "text/html", "<html><body>not a stream</body></html>", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var events []apitypes.PullEvent
			onEvent := func(e apitypes.PullEvent) { events = append(events, e) }
			err := raw(t, http.StatusOK, tc.contentType, tc.body).Pull(context.Background(), "glm-4.7-flash", onEvent)
			if !errors.Is(err, client.ErrIncompleteStream) || len(events) != tc.events {
				t.Fatalf("err %v, events %+v", err, events)
			}
		})
	}
}

func TestPullReturnsAtTheOverallDoneEvent(t *testing.T) {
	body := `data: {"stage":"done"}` + "\n\n" + "data: {oops}\n\n"
	var events []apitypes.PullEvent
	onEvent := func(e apitypes.PullEvent) { events = append(events, e) }
	err := raw(t, http.StatusOK, "text/event-stream", body).Pull(context.Background(), "glm-4.7-flash", onEvent)
	if err != nil || len(events) != 1 {
		t.Fatalf("an event after the overall done was read: %v, events %+v", err, events)
	}
}

func TestPullAcceptsAnEventStreamWithParameters(t *testing.T) {
	body := `data: {"stage":"done"}` + "\n\n"
	if err := raw(t, http.StatusOK, "text/event-stream; charset=utf-8", body).Pull(context.Background(), "glm-4.7-flash", nil); err != nil {
		t.Fatalf("an event stream with a charset: %v", err)
	}
}

func TestPullWrapsAScannerFailure(t *testing.T) {
	oversized := "data: " + strings.Repeat("x", 1<<20) + "\n\n"
	err := raw(t, http.StatusOK, "text/event-stream", oversized).Pull(context.Background(), "glm-4.7-flash", nil)
	if !errors.Is(err, bufio.ErrTooLong) || errors.Is(err, client.ErrIncompleteStream) ||
		!strings.HasPrefix(err.Error(), "reading the pull stream: ") {
		t.Fatalf("an event beyond the bound: %v", err)
	}
}

// hijackResult returns what the handler reported about its hijack, and fails the test if the handler never reported.
func hijackResult(t *testing.T, hijacked <-chan error) error {
	t.Helper()
	select {
	case err := <-hijacked:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("the handler never reached the hijack")
		return nil
	}
}

func TestPullTruncatedStreamIsIncomplete(t *testing.T) {
	hijacked := make(chan error, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: {\"stage\":\"resolving\"}\n\n")
		_, _ = io.WriteString(w, "data: {\"stage\":\"done\",\"file\":\"GLM-4.7-Flash-Q4_K.gguf\",\"bytes\":8,\"total\":8}\n\n")
		rc := http.NewResponseController(w)
		_ = rc.Flush()
		conn, _, err := rc.Hijack()
		hijacked <- err
		if err == nil {
			_ = conn.Close() // no terminating chunk follows, so the client reads a truncated body
		}
	}))
	t.Cleanup(srv.Close)
	var events []apitypes.PullEvent
	err := client.New(srv.URL, "").Pull(context.Background(), "glm-4.7-flash", func(e apitypes.PullEvent) {
		events = append(events, e)
	})
	if herr := hijackResult(t, hijacked); herr != nil {
		t.Fatalf("the handler could not hijack the connection: %v", herr)
	}
	if !errors.Is(err, client.ErrIncompleteStream) || !errors.Is(err, io.ErrUnexpectedEOF) || len(events) != 2 {
		t.Fatalf("a truncated stream: %v, events %+v", err, events)
	}
}

func TestResetAfterTheAnswerIsNotAConnectionError(t *testing.T) {
	seen := make(chan struct{})
	hijacked := make(chan error, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rc := http.NewResponseController(w)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: {\"stage\":\"resolving\"}\n\n")
		_ = rc.Flush()
		select {
		case <-seen:
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
		conn, _, err := rc.Hijack()
		hijacked <- err
		if err != nil {
			return
		}
		if tcp, ok := conn.(*net.TCPConn); ok {
			_ = tcp.SetLinger(0) // a reset, as a daemon that dies mid-pull leaves the connection
		}
		_ = conn.Close()
	}))
	t.Cleanup(srv.Close)
	first := true
	err := client.New(srv.URL, "").Pull(context.Background(), "glm-4.7-flash", func(apitypes.PullEvent) {
		if first {
			first = false
			close(seen)
		}
	})
	if herr := hijackResult(t, hijacked); herr != nil {
		t.Fatalf("the handler could not hijack the connection: %v", herr)
	}
	var opErr *net.OpError
	if !errors.As(err, &opErr) || opErr.Op != "read" {
		t.Fatalf("a reset in the stream is a read error: %v", err)
	}
	if !errors.Is(err, client.ErrIncompleteStream) || client.IsConnectionError(err) {
		t.Fatalf("a reset in the stream: %v", err)
	}
}

func TestConnectionErrorIsADialFailure(t *testing.T) {
	ctx := context.Background()
	t.Run("closed port", func(t *testing.T) {
		if _, err := client.New("http://127.0.0.1:1", "").Health(ctx); !client.IsConnectionError(err) {
			t.Fatalf("a closed port is a dial failure: %v", err)
		}
	})
	t.Run("dial timeout", func(t *testing.T) {
		srv, _ := server(t)
		c := client.New(srv.URL, "secret")
		// A dialer that gives up at once: the dial times out before any packet is sent.
		dialer := &net.Dialer{Timeout: time.Nanosecond}
		c.HTTP = &http.Client{Transport: &http.Transport{DialContext: dialer.DialContext}}
		if _, err := c.Version(ctx); !client.IsConnectionError(err) {
			t.Fatalf("a dial that times out is a connection failure: %v", err)
		}
	})
	t.Run("deadline during the answer", func(t *testing.T) {
		reached := make(chan struct{}, 1)
		release := make(chan struct{})
		srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			reached <- struct{}{}
			select {
			case <-release:
			case <-r.Context().Done():
			}
		}))
		t.Cleanup(srv.Close)
		t.Cleanup(func() { close(release) })
		deadline, cancel := context.WithTimeout(ctx, 2*time.Second) // room for a slow runner to get the request there
		defer cancel()
		_, err := client.New(srv.URL, "").Version(deadline)
		select {
		case <-reached:
		default:
			t.Fatalf("the request never reached the server, so the dial did not complete: %v", err)
		}
		if !errors.Is(err, context.DeadlineExceeded) || client.IsConnectionError(err) {
			t.Fatalf("a deadline during the answer is not a connection error: %v", err)
		}
	})
	t.Run("cancellation", func(t *testing.T) {
		srv, _ := server(t)
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := client.New(srv.URL, "secret").Version(canceled); !errors.Is(err, context.Canceled) || client.IsConnectionError(err) {
			t.Fatalf("a cancellation is not a connection error: %v", err)
		}
	})
}

func TestFailureTextFallsBackToTheStatus(t *testing.T) {
	ctx := context.Background()
	t.Run("error event without text", func(t *testing.T) {
		var apiErr *client.Error
		errorEvent := raw(t, http.StatusOK, "text/event-stream", "data: {\"stage\":\"error\"}\n\n")
		if err := errorEvent.Pull(ctx, "glm-4.7-flash", nil); !errors.As(err, &apiErr) || apiErr.Message != "Bad Gateway" {
			t.Fatalf("error event without text: %v", err)
		}
	})
	t.Run("empty error answer", func(t *testing.T) {
		var apiErr *client.Error
		if _, err := raw(t, http.StatusInternalServerError, "text/plain", "").Version(ctx); !errors.As(err, &apiErr) || apiErr.Message != "Internal Server Error" {
			t.Fatalf("empty error answer: %v", err)
		}
	})
	t.Run("status without text", func(t *testing.T) {
		var apiErr *client.Error
		if _, err := raw(t, 599, "text/plain", " \n").Version(ctx); !errors.As(err, &apiErr) || apiErr.Message != "HTTP status 599" {
			t.Fatalf("status without text: %v", err)
		}
	})
}
