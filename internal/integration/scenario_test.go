//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/tara-vision/taraengine/internal/config"
	"github.com/tara-vision/taraengine/internal/pull"
	"github.com/tara-vision/taraengine/internal/registry"
	"github.com/tara-vision/taraengine/pkg/api"
	"github.com/tara-vision/taraengine/pkg/client"
)

const (
	// callTimeout bounds a call that only reads or flips state.
	callTimeout = 30 * time.Second
	// pullTimeout bounds a pull of the one-megabyte test model, which is mostly the time to reach the hub.
	pullTimeout = 3 * time.Minute
	// loadTimeout bounds a load. The daemon itself gives up at load_timeout, which is shorter.
	loadTimeout = 3 * time.Minute
	// chatTimeout bounds a completion of a few tokens.
	chatTimeout = 60 * time.Second
	// pollEvery is the pause between two looks at something that is about to change.
	pollEvery = 50 * time.Millisecond
	// contextWindow is the window the test model runs at. It is also the smallest window that a load may ask for.
	contextWindow = 512
	// largerWindow is another window for the same model. A load may not ask for less than contextWindow, so the other
	// window is above it, and the model was trained on 2048 tokens, so the router runs it at exactly this window.
	largerWindow = 2 * contextWindow
	// loadTimeoutSeconds is the load_timeout of the daemon under test.
	loadTimeoutSeconds = 120
)

// environment is what scripts/integration.sh hands the test: the built binary and the verified pinned runtime.
type environment struct {
	bin        string
	runtimeDir string
}

// requireEnvironment reads the environment of the test. Without it the test is skipped, so that a plain `go test`
// with the integration tag does not fail for want of a runtime. Where the variable CI is set the test fails
// instead: CI exists to run it, and a skipped test would let a broken script turn the job green.
func requireEnvironment(t *testing.T) environment {
	t.Helper()
	bin, runtimeDir := os.Getenv("TARAENGINE_BIN"), os.Getenv("TARAENGINE_RUNTIME_DIR")
	if bin == "" || runtimeDir == "" {
		const problem = "TARAENGINE_BIN and TARAENGINE_RUNTIME_DIR are required; run scripts/integration.sh"
		if os.Getenv("CI") != "" {
			t.Fatal(problem + " (CI is set, so the test fails instead of skipping)")
		}
		t.Skip(problem)
	}
	return environment{bin: existing(t, bin), runtimeDir: existing(t, runtimeDir)}
}

func existing(t *testing.T, path string) string {
	t.Helper()
	abs, err := filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(abs); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return abs
}

// within is a context that ends after d or with the test, whichever is first.
func within(t *testing.T, d time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), d)
	t.Cleanup(cancel)
	return ctx
}

// testEntry is the one-megabyte stories260K model of the llama.cpp project as a catalog entry. The checksum and the
// size are those the hub reports for the file; the pull refuses anything else.
func testEntry(name string) registry.Entry {
	return registry.Entry{
		Name:         name,
		Family:       "tinyllamas",
		Params:       "260K",
		HFRepo:       "ggml-org/models-moved",
		File:         "tinyllamas/stories260K.gguf",
		SHA256:       "270cba1bd5109f42d03350f60406024560464db173c0e387d91f0426d3bd256d",
		Size:         1185376,
		Quantization: "F32",
		Context:      contextWindow,
		Capabilities: []string{},
		Tier:         "small",
		Default:      true,
		MinRuntime:   "b11514",
	}
}

// scenario is one daemon, the catalog it serves and a client of it.
type scenario struct {
	env     environment
	d       *daemon
	c       *client.Client
	entry   registry.Entry
	dataDir string
}

// newScenario writes a catalog of the one test model under the given name and a configuration that points at the
// verified runtime, with the test model's own window as the default, starts the daemon on them and returns once it
// answers.
func newScenario(t *testing.T, env environment, name string) *scenario {
	t.Helper()
	return newScenarioAt(t, env, name, contextWindow)
}

// newScenarioAt is newScenario with another default window. The data directory is private to the test and goes away
// with it. Its path holds a space, as the default data directory of macOS does
// (~/Library/Application Support/taraengine), so that every model path of the presets goes through the real router
// with one.
func newScenarioAt(t *testing.T, env environment, name string, window int) *scenario {
	t.Helper()
	dataDir := filepath.Join(t.TempDir(), "Application Support", "taraengine")
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		t.Fatal(err)
	}
	entry := testEntry(name)
	writeYAML(t, filepath.Join(dataDir, "registry.yaml"), registry.Registry{Entries: []registry.Entry{entry}})
	cfg := config.Default()
	cfg.Listen = "127.0.0.1:0"
	cfg.DataDir = dataDir
	cfg.RuntimeDir = env.runtimeDir
	cfg.Context = window
	cfg.LoadTimeout = loadTimeoutSeconds
	cfg.PresetGlobal = map[string]string{"n-gpu-layers": "0"} // the test runs on any machine, with or without a GPU
	configPath := filepath.Join(dataDir, "config.yaml")
	writeYAML(t, configPath, cfg)
	d := startDaemon(t, env, dataDir, configPath)
	return &scenario{env: env, d: d, c: client.New(d.baseURL, ""), entry: entry, dataDir: dataDir}
}

func writeYAML(t *testing.T, path string, v any) {
	t.Helper()
	data, err := yaml.Marshal(v)
	if err != nil {
		t.Fatalf("encode %s: %v", filepath.Base(path), err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// modelPath is where the pulled model file lives in the store of the daemon.
func (s *scenario) modelPath() string {
	return pull.Store{Dir: filepath.Join(s.dataDir, "models")}.Path(s.entry.HFRepo, s.entry.File)
}

// model is what the API says about the scenario's model now.
func (s *scenario) model(t *testing.T) api.Model {
	t.Helper()
	return s.named(t, s.entry.Name)
}

// named is what the API says about the model of that name now.
func (s *scenario) named(t *testing.T, name string) api.Model {
	t.Helper()
	models, err := s.c.Models(within(t, callTimeout))
	if err != nil {
		t.Fatalf("models: %v", err)
	}
	for _, m := range models {
		if m.Name == name {
			return m
		}
	}
	t.Fatalf("%s is not in the catalog: %+v", name, models)
	return api.Model{}
}

// version is the version the running daemon reports.
func (s *scenario) version(t *testing.T) api.Version {
	t.Helper()
	v, err := s.c.Version(within(t, callTimeout))
	if err != nil {
		t.Fatalf("version: %v", err)
	}
	return v
}

// listedModel is one entry of the model list that /v1/models serves, as far as the test looks at it.
type listedModel struct {
	ID     string `json:"id"`
	Status struct {
		Value string `json:"value"`
	} `json:"status"`
	Meta struct {
		NCtx int `json:"n_ctx"`
	} `json:"meta"`
}

// listed is the model list of /v1/models: the router's own answer, with the ids turned into catalog names.
func (s *scenario) listed(t *testing.T) []listedModel {
	t.Helper()
	status, _, body := s.do(t, http.MethodGet, "/v1/models", "", callTimeout)
	if status != http.StatusOK {
		t.Fatalf("GET /v1/models: %d %s", status, body)
	}
	var answer struct {
		Data []listedModel `json:"data"`
	}
	if err := json.Unmarshal(body, &answer); err != nil {
		t.Fatalf("GET /v1/models: %v\n%s", err, body)
	}
	return answer.Data
}

// do sends one request to the daemon and returns the status, the headers and the whole body.
func (s *scenario) do(t *testing.T, method, path, body string, timeout time.Duration) (int, http.Header, []byte) {
	t.Helper()
	resp := s.open(t, method, path, body, timeout)
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s %s: reading the answer: %v", method, path, err)
	}
	return resp.StatusCode, resp.Header, data
}

// open sends one request and returns the answer with its body still open, for the streams.
func (s *scenario) open(t *testing.T, method, path, body string, timeout time.Duration) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(within(t, timeout), method, s.d.baseURL+path, bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}
