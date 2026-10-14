package cli

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tara-vision/taraengine/internal/service"
)

// fastWaits makes the waits of a command short enough for a test.
func fastWaits(t *testing.T, wait, grace time.Duration) {
	t.Helper()
	previousWait, previousPoll, previousGrace := startWait, pollEvery, refusedGrace
	startWait, pollEvery, refusedGrace = wait, time.Millisecond, grace
	t.Cleanup(func() { startWait, pollEvery, refusedGrace = previousWait, previousPoll, previousGrace })
}

// startingEngine answers health with the details in order, one per poll, then ok; the models route answers an empty
// list.
func startingEngine(t *testing.T, details ...string) string {
	t.Helper()
	var polls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/api/health" {
			_, _ = w.Write([]byte(`[]`))
			return
		}
		if i := int(polls.Add(1)) - 1; i < len(details) {
			_, _ = w.Write([]byte(`{"status":"starting","backend":"llama.cpp","detail":"` + details[i] + `"}`))
			return
		}
		_, _ = w.Write([]byte(`{"status":"ok","backend":"llama.cpp"}`))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestACommandWaitsForAStartingEngineAndShowsItsProgress(t *testing.T) {
	fastWaits(t, time.Minute, time.Minute)
	url := startingEngine(t, "installing runtime b11514 (10%)", "installing runtime b11514 (10%)",
		"installing runtime b11514 (60%)", "starting llama-server")
	out, err := run(t, "list", "--host="+url)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{"taraengine is starting: installing runtime b11514 (10%)\n" +
		"taraengine is starting: installing runtime b11514 (60%)\ntaraengine is starting: starting llama-server\n", "NAME"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in\n%s", want, out)
		}
	}
}

func TestTheWaitEndsAtTheDeadline(t *testing.T) {
	fastWaits(t, 50*time.Millisecond, time.Minute)
	details := make([]string, 100000)
	for i := range details {
		details[i] = "installing runtime b11514 (1%)"
	}
	_, err := run(t, "ps", "--host="+startingEngine(t, details...))
	if err == nil || !strings.Contains(err.Error(), "did not get ready within") || !strings.Contains(err.Error(), "service status") {
		t.Fatalf("got %v", err)
	}
}

func TestTheWaitEndsWhenTheEngineStopsAnswering(t *testing.T) {
	fastWaits(t, time.Minute, 50*time.Millisecond)
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// The daemon dies after its first answer, as one that cannot bind its port would: the answer goes out on a
		// connection that is closed after it, and the listener closes, so that the next poll is refused.
		w.Header().Set("Connection", "close")
		_, _ = w.Write([]byte(`{"status":"starting","backend":"llama.cpp","detail":"checking runtime"}`))
		go func() { _ = srv.Listener.Close() }()
	}))
	t.Cleanup(srv.Close)
	_, err := run(t, "rm", "m", "--host="+srv.URL)
	if err == nil || !strings.Contains(err.Error(), "stopped answering") || !strings.Contains(err.Error(), "service status") {
		t.Fatalf("got %v", err)
	}
}

func TestTheWaitEndsWithTheCommand(t *testing.T) {
	fastWaits(t, time.Minute, time.Minute)
	f := &globalFlags{host: startingEngine(t, "checking runtime", "checking runtime", "checking runtime")}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := f.awaitEngine(ctx, &strings.Builder{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}

func TestReadyLetsTheCommandReportWhatItCannotRead(t *testing.T) {
	f := &globalFlags{host: stub(t, http.StatusOK, "<html>a proxy page</html>")}
	if err := f.ready(context.Background(), &strings.Builder{}, true); err != nil {
		t.Fatalf("an answer that is no health is the command's to report: %v", err)
	}
	f = &globalFlags{host: stub(t, http.StatusBadGateway, `{"error":{"message":"bad gateway"}}`)}
	if err := f.awaitEngine(context.Background(), &strings.Builder{}); err == nil || !strings.Contains(err.Error(), "bad gateway") {
		t.Fatalf("a wait that meets an error reports it: %v", err)
	}
}

// configListeningOn writes a config whose listen address is addr and returns its path.
func configListeningOn(t *testing.T, addr string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("listen: "+addr+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestWithoutAHostTheHintFollowsTheService(t *testing.T) {
	config := configListeningOn(t, "127.0.0.1:1")
	for _, c := range []struct {
		name  string
		state service.State
		err   error
		want  string
	}{
		{"not installed", service.State{}, nil, "start it with `taraengine service install`, or `taraengine serve` in a terminal"},
		{"stopped", service.State{Installed: true}, nil, "the service is stopped; start it with `taraengine service start`"},
		{"failing", service.State{Installed: true, Enabled: true, Loaded: true}, nil,
			"the service did not start; `taraengine service status` shows why"},
		{"no manager to ask", service.State{}, service.ErrNoSystemd, "start it with `taraengine serve`"},
	} {
		t.Run(c.name, func(t *testing.T) {
			withManager(t, &fakeManager{state: c.state, stateErr: c.err}, "linux", 1000)
			_, err := run(t, "list", "--config", config)
			if err == nil || !strings.Contains(err.Error(), "taraengine is not running at http://127.0.0.1:1: "+c.want) {
				t.Fatalf("got %v", err)
			}
		})
	}
	previous := newServiceManager
	newServiceManager = func() (service.Manager, error) { return nil, errors.New("no manager") }
	t.Cleanup(func() { newServiceManager = previous })
	if _, err := run(t, "list", "--config", config); err == nil || !strings.Contains(err.Error(), "taraengine serve") {
		t.Fatalf("without a manager the plain hint: %v", err)
	}
}

// agentManager is a manager whose Start brings an engine up on addr, as launchd starts the agent.
type agentManager struct {
	*fakeManager
	addr    string
	started *httptest.Server
	t       *testing.T
}

func (a *agentManager) Start(ctx context.Context) error {
	if err := a.fakeManager.Start(ctx); err != nil {
		return err
	}
	ln, err := net.Listen("tcp", a.addr)
	if err != nil {
		a.t.Fatal(err)
	}
	a.started = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/health" {
			_, _ = w.Write([]byte(`{"status":"ok","backend":"llama.cpp"}`))
			return
		}
		_, _ = w.Write([]byte(`[]`))
	}))
	a.started.Listener = ln
	a.started.Start()
	a.t.Cleanup(a.started.Close)
	return nil
}

func TestTheFirstCommandStartsTheMacOSAgent(t *testing.T) {
	fastWaits(t, time.Minute, time.Minute)
	addr := freeAddr(t)
	agent := &agentManager{fakeManager: &fakeManager{state: service.State{Installed: true, Enabled: true}}, addr: addr, t: t}
	withManager(t, agent.fakeManager, "darwin", 501)
	newServiceManager = func() (service.Manager, error) { return agent, nil }
	out, err := run(t, "list", "--config", configListeningOn(t, addr))
	if err != nil || !strings.Contains(out, "started the taraengine service; it also starts at every login") ||
		!strings.Contains(out, "NAME") || agent.started == nil {
		t.Fatalf("%v\n%s", err, out)
	}
}

func TestTheFirstUseStartLeavesAStoppedOrForeignAgentAlone(t *testing.T) {
	config := configListeningOn(t, "127.0.0.1:1")
	stopped := &fakeManager{state: service.State{Installed: true}}
	withManager(t, stopped, "darwin", 501)
	if _, err := run(t, "list", "--config", config); err == nil || !strings.Contains(err.Error(), "service start") {
		t.Fatalf("a stopped agent is not started behind the user's back: %v", err)
	}
	if strings.Contains(strings.Join(stopped.calls, " "), "start") {
		t.Fatalf("no start: %v", stopped.calls)
	}
	loaded := &fakeManager{state: service.State{Installed: true, Enabled: true, Loaded: true}}
	withManager(t, loaded, "darwin", 501)
	if _, err := run(t, "list", "--config", config); err == nil || !strings.Contains(err.Error(), "did not start") {
		t.Fatalf("a loaded agent that does not answer is a failure, not a start: %v", err)
	}
	failing := &fakeManager{state: service.State{Installed: true, Enabled: true}, err: errors.New("service: bootstrap failed")}
	withManager(t, failing, "darwin", 501)
	if _, err := run(t, "list", "--config", config); err == nil || !strings.Contains(err.Error(), "bootstrap failed") {
		t.Fatalf("a start that fails says so: %v", err)
	}
}

func TestTheWaitStopsWhenItCannotShowItsProgress(t *testing.T) {
	fastWaits(t, time.Minute, time.Minute)
	f := &globalFlags{host: startingEngine(t, "checking runtime")}
	if err := f.awaitEngine(context.Background(), &failingWriter{err: errors.New("write refused")}); err == nil || !strings.Contains(err.Error(), "write refused") {
		t.Fatalf("got %v", err)
	}
	addr := freeAddr(t)
	agent := &agentManager{fakeManager: &fakeManager{state: service.State{Installed: true, Enabled: true}}, addr: addr, t: t}
	withManager(t, agent.fakeManager, "darwin", 501)
	newServiceManager = func() (service.Manager, error) { return agent, nil }
	g := &globalFlags{host: "http://" + addr}
	if err := g.startOrExplain(context.Background(), &failingWriter{err: errors.New("write refused")}, errors.New("refused")); err == nil ||
		!strings.Contains(err.Error(), "write refused") {
		t.Fatalf("got %v", err)
	}
}

func TestAStopEndsTheWaitBetweenPolls(t *testing.T) {
	fastWaits(t, time.Minute, time.Minute)
	pollEvery = time.Minute
	f := &globalFlags{host: startingEngine(t, "checking runtime", "checking runtime")}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	if err := f.awaitEngine(ctx, &strings.Builder{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}
