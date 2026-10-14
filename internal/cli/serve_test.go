package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tara-vision/taraengine/internal/backend/llamaserver"
	"github.com/tara-vision/taraengine/internal/llamatest"
	"github.com/tara-vision/taraengine/internal/pull"
	"github.com/tara-vision/taraengine/internal/registry"
	"github.com/tara-vision/taraengine/internal/version"
)

func TestServeAttachModeServesTheAPI(t *testing.T) {
	dataDir := t.TempDir()
	fake := llamatest.New(t, llamatest.Options{APIKey: "k", PresetPath: llamaserver.PresetPath(dataDir)})
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	listen := l.Addr().String()
	_ = l.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		root := NewRootCommand()
		root.SetArgs([]string{"serve", "--listen", listen, "--data-dir", dataDir, "--attach", fake.URL(), "--attach-key", "k"})
		done <- root.ExecuteContext(ctx)
	}()
	var resp *http.Response
	var err error
	for i := 0; i < 100; i++ {
		resp, err = http.Get(fmt.Sprintf("http://%s/api/version", listen))
		if err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("api did not come up: %v", err)
	}
	_ = resp.Body.Close()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve returned %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not stop on cancel")
	}
	if _, err := os.Stat(filepath.Join(dataDir, "logs", "taraengine.log")); err != nil {
		t.Fatal("daemon log missing")
	}
	if _, err := os.Stat(llamaserver.PresetPath(dataDir)); err != nil {
		t.Fatal("presets missing")
	}
}

func TestServeWithoutARuntimeExplains(t *testing.T) {
	root := NewRootCommand()
	root.SetArgs([]string{"serve", "--data-dir", t.TempDir(), "--listen", "127.0.0.1:0"})
	err := root.ExecuteContext(context.Background())
	if err == nil || !strings.Contains(err.Error(), "taraengine runtime install") {
		t.Fatalf("got %v", err)
	}
}

// freeAddr is a loopback address nobody listens on right now.
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().String()
}

// serving is a `taraengine serve` command running against an attached fake router.
type serving struct {
	listen  string
	dataDir string
	fake    *llamatest.Server
	stop    func() error              // cancels serve and waits for it to return
	wait    func(time.Duration) error // waits for serve to return by itself, as after a signal
}

func (s *serving) host() string { return "--host=http://" + s.listen }

// startServe runs the serve command in attach mode until the test ends. prepare, when not nil, sets up the data
// directory before the daemon reads it; extra flags come after the standard ones.
func startServe(t *testing.T, router llamatest.Options, prepare func(dataDir string), extra ...string) *serving {
	t.Helper()
	dataDir := t.TempDir()
	if prepare != nil {
		prepare(dataDir)
	}
	router.APIKey, router.PresetPath = "k", llamaserver.PresetPath(dataDir)
	fake := llamatest.New(t, router)
	listen := freeAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	var result error
	go func() {
		root := NewRootCommand()
		args := []string{"serve", "--listen", listen, "--data-dir", dataDir, "--attach", fake.URL(), "--attach-key", "k"}
		root.SetArgs(append(args, extra...))
		result = root.ExecuteContext(ctx)
		close(finished)
	}()
	wait := func(d time.Duration) error {
		select {
		case <-finished:
			return result
		case <-time.After(d):
			return errors.New("serve did not stop")
		}
	}
	stop := func() error {
		cancel()
		return wait(10 * time.Second)
	}
	t.Cleanup(func() { _ = stop() })
	probe := &http.Client{Timeout: 5 * time.Second}
	for i := 0; !healthOK(probe, "http://"+listen); i++ {
		select {
		case <-finished:
			t.Fatalf("serve ended before it came up: %v", result)
		default:
		}
		if i >= 200 {
			t.Fatal("the api did not come up")
		}
		time.Sleep(25 * time.Millisecond)
	}
	return &serving{listen: listen, dataDir: dataDir, fake: fake, stop: stop, wait: wait}
}

// putModel places every file of a catalog model where a finished pull would have left it.
func putModel(t *testing.T, dataDir, name string) pull.Store {
	t.Helper()
	reg, err := registry.Load()
	if err != nil {
		t.Fatal(err)
	}
	entry, ok := reg.Find(name)
	if !ok {
		t.Fatalf("%s is not in the registry", name)
	}
	store := pull.Store{Dir: filepath.Join(dataDir, "models")}
	for _, f := range entry.Files() {
		path := store.Path(entry.HFRepo, f.File)
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return store
}

func get(t *testing.T, url string) (*http.Response, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp, string(body)
}

// wantPresets checks the preset file the daemon rendered for a pulled model and all its siblings.
func wantPresets(t *testing.T, dataDir string, store pull.Store, entry registry.Entry) {
	t.Helper()
	presets, err := os.ReadFile(llamaserver.PresetPath(dataDir))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"[gemma4-e4b]\n", "c = 32768\n", "model = " + store.Path(entry.HFRepo, entry.File) + "\n",
		"mmproj = " + store.Path(entry.HFRepo, entry.MMProj.File) + "\n",
		"model-draft = " + store.Path(entry.HFRepo, entry.MTP.File) + "\n", "spec-type = draft-mtp\n",
	} {
		if !strings.Contains(string(presets), want) {
			t.Errorf("the preset file lacks %q:\n%s", want, presets)
		}
	}
}

// wantProxiedModels checks that /v1/models comes back through the proxy with catalog names and the version header.
func wantProxiedModels(t *testing.T, listen string) {
	t.Helper()
	resp, body := get(t, "http://"+listen+"/v1/models")
	if resp.StatusCode != http.StatusOK || resp.Header.Get("X-Taraengine-Version") != version.Version {
		t.Fatalf("/v1/models: %d %v", resp.StatusCode, resp.Header)
	}
	var listed struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &listed); err != nil || len(listed.Data) != 1 || listed.Data[0].ID != "gemma4:e4b" {
		t.Fatalf("the proxy answers with catalog names: %v %s", err, body)
	}
}

func TestServeThroughTheCommandLineFromListToRemove(t *testing.T) {
	var store pull.Store
	s := startServe(t, llamatest.Options{ModelsMax: 1}, func(dataDir string) { store = putModel(t, dataDir, "gemma4:e4b") })
	host := s.host()
	reg, _ := registry.Load()
	entry, _ := reg.Find("gemma4:e4b")
	wantPresets(t, s.dataDir, store, entry)

	out, err := run(t, "list", host)
	if err != nil || lineWith(out, "gemma4:e4b", "pulled") == "" {
		t.Fatalf("list: %v\n%s", err, out)
	}
	out, err = run(t, "load", "gemma4:e4b", host)
	if err != nil || out != "loaded gemma4:e4b at 32768 tokens of context\n" {
		t.Fatalf("load: %v %q", err, out)
	}
	if out, err = run(t, "ps", host); err != nil || lineWith(out, "gemma4:e4b", "32768") == "" {
		t.Fatalf("ps: %v\n%s", err, out)
	}
	wantProxiedModels(t, s.listen)
	out, err = run(t, "doctor", host, "--data-dir", s.dataDir)
	if err != nil || !strings.Contains(out, "Daemon: taraengine "+version.Version+" on ") || !strings.Contains(out, "Loaded: gemma4:e4b at 32768 tokens") {
		t.Fatalf("doctor: %v\n%s", err, out)
	}
	if out, err = run(t, "unload", "gemma4:e4b", host); err != nil || out != "unloaded gemma4:e4b\n" {
		t.Fatalf("unload: %v %q", err, out)
	}
	if out, err = run(t, "rm", "gemma4:e4b", host); err != nil || out != "removed gemma4:e4b\n" {
		t.Fatalf("rm: %v %q", err, out)
	}
	if store.Exists(entry.HFRepo, entry.File) {
		t.Fatal("rm leaves the files on disk")
	}
	if presets, _ := os.ReadFile(llamaserver.PresetPath(s.dataDir)); strings.Contains(string(presets), "gemma4-e4b") {
		t.Fatalf("a removed model leaves the preset file:\n%s", presets)
	}
	if err := s.stop(); err != nil {
		t.Fatalf("serve returned %v", err)
	}
}

func TestServeKeepsTheRoutersOwnEndpointsOutOfReach(t *testing.T) {
	s := startServe(t, llamatest.Options{}, nil)
	before := len(s.fake.Requests())
	for _, path := range []string{"/v1/%2e%2e/models", "/v1/../models", "/v1//../props"} {
		if resp, _ := get(t, "http://"+s.listen+path); resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s answered %d", path, resp.StatusCode)
		}
	}
	if after := len(s.fake.Requests()); after != before {
		t.Fatalf("a dot segment reached the router: %v", s.fake.Requests()[before:])
	}
	dropIdleConnections()
}

func TestServeReadsItsConfigFileAndTheFlagsWin(t *testing.T) {
	fileDir := t.TempDir()
	cfgFile := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cfgFile, []byte("listen: 127.0.0.1:1\ndata_dir: "+fileDir+"\ncontext: 2048\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := startServe(t, llamatest.Options{}, func(dataDir string) { putModel(t, dataDir, "gemma4:e4b") }, "--config", cfgFile)
	if err := s.stop(); err != nil {
		t.Fatal(err)
	}
	presets, err := os.ReadFile(llamaserver.PresetPath(s.dataDir))
	if err != nil || !strings.Contains(string(presets), "c = 2048\n") {
		t.Fatalf("the config file's context decides the window: %v\n%s", err, presets)
	}
	if _, err := os.Stat(filepath.Join(s.dataDir, "logs", "taraengine.log")); err != nil {
		t.Fatalf("the --data-dir flag decides where the daemon works: %v", err)
	}
	if entries, err := os.ReadDir(fileDir); err != nil || len(entries) != 0 {
		t.Fatalf("the config's data_dir was used although a flag named another: %v %v", entries, err)
	}
}

func TestServeNamesWhoHoldsThePortAndTouchesNothing(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	dataDir := t.TempDir()
	_, err = run(t, "serve", "--listen", l.Addr().String(), "--data-dir", dataDir, "--attach", "http://127.0.0.1:1")
	if err == nil || !strings.Contains(err.Error(), "listen "+l.Addr().String()+": bind: address already in use") {
		t.Fatalf("%v", err)
	}
	if strings.Contains(err.Error(), "listen tcp") {
		t.Fatalf("the cause is said once: %v", err)
	}
	for _, name := range []string{"presets.ini", "state.json", "child.key"} {
		if _, statErr := os.Stat(filepath.Join(dataDir, name)); statErr == nil {
			t.Errorf("a daemon that cannot listen must not touch %s", name)
		}
	}
}

func TestRunLsofNamesTheProcessThatListens(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	_, port, _ := net.SplitHostPort(l.Addr().String())
	if _, err := runLsof(context.Background(), port); err != nil {
		t.Skipf("lsof cannot answer here: %v", err)
	}
	if got := portHolder(context.Background(), l.Addr().String(), runLsof); !strings.HasPrefix(got, fmt.Sprintf(" (held by PID %d ", os.Getpid())) {
		t.Fatalf("the holder of the port is this test process (%d), got %q", os.Getpid(), got)
	}
}

func TestServeRefusesWhatItCannotRun(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "a-file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		args []string
		want string
	}{
		{"an address without a port", []string{"--listen", "nonsense"}, "listen"},
		{"a config file that is not there", []string{"--config", filepath.Join(dir, "missing.yaml")}, "missing.yaml"},
		{"a data directory that is a file", []string{"--data-dir", file}, "logging:"},
		{"a router address that is no URL", []string{"--attach", "nonsense"}, "must look like http://host:port"},
		{"a router that does not answer", []string{"--attach", "http://127.0.0.1:1"}, "backend is not running"},
		{"a runtime directory that is no runtime", []string{"--runtime-dir", dir}, "runtime_dir " + dir},
	} {
		t.Run(c.name, func(t *testing.T) {
			args := append([]string{"serve", "--listen", "127.0.0.1:0", "--data-dir", t.TempDir()}, c.args...)
			if _, err := run(t, args...); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v, want it to mention %q", err, c.want)
			}
		})
	}
}

func TestServeFlagsAreTheDocumentedOnesAndTheTestOnesAreHidden(t *testing.T) {
	cmd, _, err := NewRootCommand().Find([]string{"serve"})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"listen", "data-dir", "runtime-dir", "service"} {
		if f := cmd.Flags().Lookup(name); f == nil || f.Hidden {
			t.Errorf("--%s must be a public flag: %v", name, f)
		}
	}
	for _, name := range []string{"attach", "attach-key"} {
		if f := cmd.Flags().Lookup(name); f == nil || !f.Hidden {
			t.Errorf("--%s must exist and be hidden: %v", name, f)
		}
	}
}

func TestPortHolder(t *testing.T) {
	answer := func(out string, err error) func(context.Context, string) ([]byte, error) {
		return func(context.Context, string) ([]byte, error) { return []byte(out), err }
	}
	for _, c := range []struct {
		name   string
		listen string
		lsof   func(context.Context, string) ([]byte, error)
		want   string
	}{
		{"one holder", "127.0.0.1:8272", answer("p123\ncnginx\n", nil), " (held by PID 123 nginx)"},
		{"the first of several holders", ":8272", answer("p123\ncnginx\np456\ncother\n", nil), " (held by PID 123 nginx)"},
		{"a holder without a name", "[::1]:8272", answer("p77\n", nil), " (held by PID 77)"},
		{"nobody found", "127.0.0.1:8272", answer("", nil), ""},
		{"lsof is missing", "127.0.0.1:8272", answer("", errors.New("executable file not found")), ""},
		{"lsof warns and still answers", "127.0.0.1:8272", answer("p9\ncnginx\n", errors.New("exit status 1")), " (held by PID 9 nginx)"},
		{"an address that does not parse", "nonsense", answer("p1\ncx\n", nil), ""},
	} {
		if got := portHolder(context.Background(), c.listen, c.lsof); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
	var asked string
	portHolder(context.Background(), "127.0.0.1:9999", func(_ context.Context, port string) ([]byte, error) {
		asked = port
		return nil, nil
	})
	if asked != "9999" {
		t.Fatalf("lsof was asked about port %q", asked)
	}
}
