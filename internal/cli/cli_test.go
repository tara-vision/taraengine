package cli

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/tara-vision/taraengine/internal/api"
	"github.com/tara-vision/taraengine/internal/backend"
	"github.com/tara-vision/taraengine/internal/backend/backendtest"
	"github.com/tara-vision/taraengine/internal/catalog"
	"github.com/tara-vision/taraengine/internal/config"
	"github.com/tara-vision/taraengine/internal/machine"
	"github.com/tara-vision/taraengine/internal/pull"
	"github.com/tara-vision/taraengine/internal/registry"
	apitypes "github.com/tara-vision/taraengine/pkg/api"
)

type cliFetcher struct{}

func (cliFetcher) Resolve(_ context.Context, repo, file string) (pull.Artifact, error) {
	reg, _ := registry.Load()
	for _, e := range reg.Entries {
		for _, f := range e.Files() {
			if e.HFRepo == repo && f.File == file {
				return pull.Artifact{Repo: repo, File: file, Size: 16, SHA256: f.SHA256}, nil
			}
		}
	}
	return pull.Artifact{}, os.ErrNotExist
}

func (cliFetcher) Fetch(_ context.Context, _ pull.Artifact, dest string, onProgress func(pull.Progress)) error {
	_ = os.MkdirAll(filepath.Dir(dest), 0o755)
	onProgress(pull.Progress{Stage: "downloading", Bytes: 8, Total: 16})
	onProgress(pull.Progress{Stage: "done", Bytes: 16, Total: 16})
	return os.WriteFile(dest, bytes.Repeat([]byte("x"), 16), 0o600)
}

// testDaemon is the real API server over the fake backend, with a model store of its own.
type testDaemon struct {
	srv     *httptest.Server
	fake    *backendtest.Fake
	store   pull.Store
	dataDir string
}

// startDaemon serves the API over the fake backend. tweak, when not nil, changes the dependencies first.
func startDaemon(t *testing.T, tweak func(*api.Deps)) *testDaemon {
	t.Helper()
	reg, err := registry.Load()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	store := pull.Store{Dir: filepath.Join(dir, "models")}
	cfg := config.Default()
	cfg.DataDir = dir
	fake := backendtest.New()
	deps := api.Deps{
		Backend: fake, Catalog: catalog.Catalog{Registry: reg, Store: store, Index: &catalog.Index{Path: filepath.Join(store.Dir, "index.json")}},
		Config: cfg, Fetcher: cliFetcher{},
		Machine: func(context.Context) machine.Info {
			return machine.Info{OS: "linux", GPU: "NVIDIA GeForce RTX 5090", GPUMemoryBytes: 32 << 30, RAMBytes: 48 << 30, Tier: "48"}
		},
		Runtime: func() apitypes.RuntimeStatus {
			return apitypes.RuntimeStatus{Installed: true, Verified: true, Build: "bTEST", Dir: "/rt/bTEST"}
		},
		Version: "0.1.0-test", Build: "bTEST", Platform: "linux/amd64/cuda12",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if tweak != nil {
		tweak(&deps)
	}
	srv := httptest.NewServer(api.New(deps))
	t.Cleanup(srv.Close)
	return &testDaemon{srv: srv, fake: fake, store: store, dataDir: dir}
}

func daemon(t *testing.T) (*httptest.Server, *backendtest.Fake) {
	t.Helper()
	d := startDaemon(t, nil)
	return d.srv, d.fake
}

// install puts every file of a catalog model into the store, as a finished pull would.
func (d *testDaemon) install(t *testing.T, name string) {
	t.Helper()
	reg, err := registry.Load()
	if err != nil {
		t.Fatal(err)
	}
	entry, ok := reg.Find(name)
	if !ok {
		t.Fatalf("%s is not in the registry", name)
	}
	for _, f := range entry.Files() {
		path := d.store.Path(entry.HFRepo, f.File)
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func run(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	root := NewRootCommand()
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), err
}

// lineWith is the first line of out that contains all the parts, or "" when there is none.
func lineWith(out string, parts ...string) string {
	for _, line := range strings.Split(out, "\n") {
		if !slices.ContainsFunc(parts, func(p string) bool { return !strings.Contains(line, p) }) {
			return line
		}
	}
	return ""
}

func TestListPsLoadUnloadRm(t *testing.T) { //nolint:gocyclo // one ordered scenario over every command, kept as one function so that the sequence reads top to bottom
	srv, fake := daemon(t)
	host := "--host=" + srv.URL
	out, err := run(t, "list", host)
	if err != nil || !strings.Contains(out, "gemma4:12b") || !strings.Contains(out, "available") || !strings.Contains(out, "NAME") {
		t.Fatalf("list: %v\n%s", err, out)
	}
	if out, err := run(t, "pull", "gemma4:12b", host); err != nil || !strings.Contains(out, "done") || !strings.Contains(out, "mtp-gemma-4-12B-it-Q4_0.gguf") {
		t.Fatalf("pull: %v\n%s", err, out)
	}
	fake.SetModels(backend.ModelStatus{Name: "gemma4:12b", Status: backend.StatusPulled})
	out, err = run(t, "load", "gemma4:12b", "--context", "8192", host)
	if err != nil || !strings.Contains(out, "loaded gemma4:12b") || !strings.Contains(out, "8192") || fake.Loads[0].Options.Context != 8192 {
		t.Fatalf("load: %v\n%s", err, out)
	}
	if out, err := run(t, "ps", host); err != nil || !strings.Contains(out, "gemma4:12b") || !strings.Contains(out, "8192") {
		t.Fatalf("ps: %v\n%s", err, out)
	}
	if out, err := run(t, "unload", "gemma4:12b", host); err != nil || !strings.Contains(out, "unloaded gemma4:12b") {
		t.Fatalf("unload: %v\n%s", err, out)
	}
	if out, err := run(t, "rm", "gemma4:12b", host); err != nil || !strings.Contains(out, "removed gemma4:12b") {
		t.Fatalf("rm: %v\n%s", err, out)
	}
	if _, err := run(t, "load", "nope", host); err == nil || !strings.Contains(err.Error(), "unknown model") {
		t.Fatalf("load unknown: %v", err)
	}
	fake.Fail("load", backend.ErrFaulty)
	fake.SetModels(backend.ModelStatus{Name: "gemma4:12b", Status: backend.StatusFaulty})
	if _, err := run(t, "load", "gemma4:12b", host); err == nil || !strings.Contains(err.Error(), "force") {
		t.Fatalf("faulty hint: %v", err)
	}
}

func TestDaemonDownIsExplained(t *testing.T) {
	_, err := run(t, "list", "--host=http://127.0.0.1:1")
	if err == nil || !strings.Contains(err.Error(), "taraengine is not running") || !strings.Contains(err.Error(), "taraengine serve") {
		t.Fatalf("got %v", err)
	}
}

func TestEveryDaemonCommandExplainsADaemonThatIsDown(t *testing.T) {
	const host = "http://127.0.0.1:1"
	for _, args := range [][]string{{"list"}, {"ps"}, {"rm", "m"}, {"load", "m"}, {"unload", "m"}, {"pull", "m"}} {
		t.Run(args[0], func(t *testing.T) {
			out, err := run(t, slices.Concat(args, []string{"--host=" + host})...)
			if err == nil || !strings.Contains(err.Error(), "taraengine is not running at "+host+": start it with `taraengine serve`") {
				t.Fatalf("got %v", err)
			}
			if out != "" {
				t.Fatalf("a failed command prints nothing of its own: %q", out)
			}
		})
	}
}

func TestDoctorShowsMachineRuntimeAndDaemon(t *testing.T) {
	srv, _ := daemon(t)
	out, err := run(t, "doctor", "--host="+srv.URL, "--data-dir", t.TempDir())
	if err != nil || !strings.Contains(out, "Machine") || !strings.Contains(out, "Runtime") || !strings.Contains(out, "Daemon") || !strings.Contains(out, "0.1.0-test") {
		t.Fatalf("doctor: %v\n%s", err, out)
	}
	out, err = run(t, "doctor", "--host=http://127.0.0.1:1", "--data-dir", t.TempDir())
	if err != nil || !strings.Contains(out, "not running") || !strings.Contains(out, "runtime install") {
		t.Fatalf("doctor without daemon and runtime: %v\n%s", err, out)
	}
}
