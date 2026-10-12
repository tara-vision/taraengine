package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tara-vision/taraengine/internal/backend"
	"github.com/tara-vision/taraengine/internal/backend/backendtest"
	"github.com/tara-vision/taraengine/internal/catalog"
	"github.com/tara-vision/taraengine/internal/config"
	"github.com/tara-vision/taraengine/internal/machine"
	"github.com/tara-vision/taraengine/internal/pull"
	"github.com/tara-vision/taraengine/internal/registry"
	"github.com/tara-vision/taraengine/pkg/api"
)

// fakeFetcher answers Resolve from a table and writes dest files of the artifact's size. A fetch of gateFile
// leaves the part file that a download in progress has, announces itself on started and then waits for gate to
// close or for its request to end, so a test can hold a pull in flight. A request that ends leaves the part file
// where it is, as the downloader does, and a fetch that goes through promotes it.
type fakeFetcher struct {
	artifacts map[string]pull.Artifact // key repo/file
	fail      string                   // a file name whose fetch fails
	gateFile  string                   // a file name whose fetch waits for gate
	bodies    map[string][]byte        // a file name whose fetch writes these bytes in place of the artifact's x's
	gate      chan struct{}
	started   chan struct{} // receives one value when a gated fetch begins
	stopped   chan error    // receives the reason a gated fetch ended without the gate opening
}

func (f *fakeFetcher) Resolve(_ context.Context, repo, file string) (pull.Artifact, error) {
	art, ok := f.artifacts[repo+"/"+file]
	if !ok {
		return pull.Artifact{}, fmt.Errorf("pull: %s has no file %s", repo, file)
	}
	return art, nil
}

func (f *fakeFetcher) Fetch(ctx context.Context, art pull.Artifact, dest string, onProgress func(pull.Progress)) error {
	if art.File == f.fail {
		return fmt.Errorf("pull: download %s: boom", art.File)
	}
	if err := f.hold(ctx, dest, art.File); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o750); err != nil {
		return err
	}
	onProgress(pull.Progress{Stage: "downloading", Bytes: art.Size / 2, Total: art.Size})
	onProgress(pull.Progress{Stage: "verifying", Bytes: art.Size, Total: art.Size})
	data := bytes.Repeat([]byte("x"), int(art.Size))
	if body, ok := f.bodies[art.File]; ok {
		data = body
	}
	if err := os.WriteFile(dest, data, 0o600); err != nil {
		return err
	}
	if err := os.Remove(dest + ".part"); err != nil && !os.IsNotExist(err) {
		return err
	}
	onProgress(pull.Progress{Stage: "done", Bytes: art.Size, Total: art.Size})
	return nil
}

// hold leaves the part file of a fetch of the gated file and blocks it until the gate opens or the request ends.
func (f *fakeFetcher) hold(ctx context.Context, dest, file string) error {
	if f.gate == nil || file != f.gateFile {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o750); err != nil {
		return err
	}
	if err := os.WriteFile(dest+".part", []byte("half"), 0o600); err != nil {
		return err
	}
	f.started <- struct{}{}
	select {
	case <-f.gate:
		return nil
	case <-ctx.Done():
		f.stopped <- ctx.Err()
		return ctx.Err()
	}
}

// lockedBuffer collects log output written by the server goroutines.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// testClient gives up on a request that hangs, so a regression that leaves a handler waiting fails the test
// within seconds instead of running into the package timeout.
var testClient = &http.Client{Timeout: 20 * time.Second}

type harness struct {
	srv     *httptest.Server
	fake    *backendtest.Fake
	cat     catalog.Catalog
	fetcher *fakeFetcher
	dataDir string
	deps    Deps
	logs    *lockedBuffer
}

// newHarness serves the API over a fake backend and a fake fetcher. An option may change the deps before the
// server is built.
func newHarness(t *testing.T, apiKey string, opts ...func(*Deps)) *harness {
	t.Helper()
	reg, err := registry.Load()
	if err != nil {
		t.Fatal(err)
	}
	dataDir := t.TempDir()
	store := pull.Store{Dir: filepath.Join(dataDir, "models")}
	cat := catalog.Catalog{Registry: reg, Store: store, Index: &catalog.Index{Path: filepath.Join(store.Dir, "index.json")}}
	fetcher := &fakeFetcher{artifacts: map[string]pull.Artifact{}}
	for _, e := range reg.Entries {
		for _, f := range e.Files() {
			fetcher.artifacts[e.HFRepo+"/"+f.File] = pull.Artifact{Repo: e.HFRepo, File: f.File, Size: 64, SHA256: f.SHA256}
		}
	}
	fetcher.artifacts["org/repo/extra.gguf"] = pull.Artifact{Repo: "org/repo", File: "extra.gguf", Size: 32, SHA256: "abc"}
	cfg := config.Default()
	cfg.APIKey = apiKey
	cfg.DataDir = dataDir
	fake := backendtest.New()
	logs := &lockedBuffer{}
	deps := Deps{
		Backend: fake, Catalog: cat, Config: cfg, Fetcher: fetcher,
		Machine: func(context.Context) machine.Info {
			return machine.Info{OS: "linux", Arch: "amd64", GPU: "NVIDIA GeForce RTX 5090", GPUMemoryBytes: 32 << 30, RAMBytes: 48 << 30, Tier: "48"}
		},
		Runtime: func() api.RuntimeStatus {
			return api.RuntimeStatus{Installed: true, Verified: true, Build: "bTEST", Dir: "/rt/bTEST"}
		},
		Version: "0.1.0-test", Build: "bTEST", Platform: "linux/amd64/cuda12",
		Logger: slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
	for _, opt := range opts {
		opt(&deps)
	}
	h := &harness{fake: fake, cat: cat, fetcher: fetcher, dataDir: dataDir, deps: deps, logs: logs}
	h.srv = httptest.NewServer(New(deps))
	t.Cleanup(h.srv.Close)
	return h
}

// request sends one request and returns the status and body; it is safe to call from any goroutine. An edit may
// change the request before it goes out.
func (h *harness) request(method, path, contentType, key string, body io.Reader, edits ...func(*http.Request)) (int, http.Header, []byte, error) {
	req, err := http.NewRequest(method, h.srv.URL+path, body)
	if err != nil {
		return 0, nil, nil, err
	}
	for _, edit := range edits {
		edit(req)
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := testClient.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, data, err
}

func (h *harness) do(t *testing.T, method, path, key string, body any) (*http.Response, []byte) {
	t.Helper()
	var payload io.Reader
	contentType := ""
	if body != nil {
		data, _ := json.Marshal(body)
		payload = bytes.NewReader(data)
		contentType = "application/json"
	}
	status, header, data, err := h.request(method, path, contentType, key, payload)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Response{StatusCode: status, Header: header}, data
}

// doRaw sends body as it is, with the given Content-Type (none when empty).
func (h *harness) doRaw(t *testing.T, method, path, contentType, body string) (int, []byte) {
	t.Helper()
	status, _, data, err := h.request(method, path, contentType, "", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return status, data
}

// gatedBackend holds one kind of call, "unload" or "models", until the gate opens or the request ends, and
// announces each held call on started.
type gatedBackend struct {
	backend.Backend
	holds   string
	gate    chan struct{}
	started chan struct{}
}

func newGatedBackend(t *testing.T, holds string) *gatedBackend {
	t.Helper()
	g := &gatedBackend{holds: holds, gate: make(chan struct{}), started: make(chan struct{}, 4)}
	t.Cleanup(func() {
		select {
		case <-g.gate:
		default:
			close(g.gate)
		}
	})
	return g
}

func (g *gatedBackend) hold(ctx context.Context, call string) error {
	if call != g.holds {
		return nil
	}
	g.started <- struct{}{}
	select {
	case <-g.gate:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (g *gatedBackend) Unload(ctx context.Context, name string) error {
	if err := g.hold(ctx, "unload"); err != nil {
		return err
	}
	return g.Backend.Unload(ctx, name)
}

func (g *gatedBackend) Models(ctx context.Context) ([]backend.ModelStatus, error) {
	if err := g.hold(ctx, "models"); err != nil {
		return nil, err
	}
	return g.Backend.Models(ctx)
}

// transportErr is what a dead router looks like to the code that dialled it: it names the child's address.
const transportErr = `Get "http://127.0.0.1:41234/models": dial tcp 127.0.0.1:41234: connect: connection refused`

// assertNoTransportText fails the test when an answer shows where the child listens.
func assertNoTransportText(t *testing.T, what string, data []byte) {
	t.Helper()
	for _, leak := range []string{"127.0.0.1", "dial tcp", "connection refused"} {
		if strings.Contains(string(data), leak) {
			t.Errorf("%s shows %q to the client: %s", what, leak, data)
		}
	}
}

func decode[T any](t *testing.T, data []byte) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatalf("decode %T: %v\n%s", v, err, data)
	}
	return v
}

// pulled puts every file of a catalog model in the store.
func pulled(t *testing.T, h *harness, name string) {
	t.Helper()
	e, ok := h.cat.Registry.Find(name)
	if !ok {
		t.Fatalf("%s not in the registry", name)
	}
	for _, f := range e.Files() {
		p := h.cat.Store.Path(e.HFRepo, f.File)
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// addHFModel stores a blob and indexes it as an hf:// pull would.
func addHFModel(t *testing.T, h *harness, name, repo, file string) string {
	t.Helper()
	p := h.cat.Store.Path(repo, file)
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	entry := catalog.IndexEntry{Name: name, Repo: repo, File: file, SHA256: "abc", Path: p, Size: 1}
	if err := h.cat.Index.Add(entry); err != nil {
		t.Fatal(err)
	}
	return p
}

// breakIndex leaves a file in place of the index that cannot be parsed.
func breakIndex(t *testing.T, h *harness) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(h.cat.Index.Path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.cat.Index.Path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestHealthNeedsNoKeyEverythingElseDoes(t *testing.T) {
	h := newHarness(t, "secret")
	if resp, data := h.do(t, http.MethodGet, "/api/health", "", nil); resp.StatusCode != 200 || decode[api.Health](t, data).Status != "ok" {
		t.Fatalf("health: %d %s", resp.StatusCode, data)
	}
	for _, path := range []string{"/api/version", "/api/models", "/api/loaded", "/api/doctor", "/v1/models"} {
		if resp, _ := h.do(t, http.MethodGet, path, "", nil); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s without a key: %d", path, resp.StatusCode)
		}
		if resp, _ := h.do(t, http.MethodGet, path, "wrong", nil); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s with a wrong key: %d", path, resp.StatusCode)
		}
		if resp, _ := h.do(t, http.MethodGet, path, "secret", nil); resp.StatusCode != 200 {
			t.Errorf("%s with the key: %d", path, resp.StatusCode)
		}
	}
	open := newHarness(t, "")
	if resp, _ := open.do(t, http.MethodGet, "/api/version", "", nil); resp.StatusCode != 200 {
		t.Fatalf("no key configured must leave the API open: %d", resp.StatusCode)
	}
	h.fake.HealthVal = backend.Health{OK: false, Backend: "llama.cpp", Detail: "gone"}
	if _, data := h.do(t, http.MethodGet, "/api/health", "", nil); decode[api.Health](t, data).Status != "degraded" {
		t.Fatalf("degraded health: %s", data)
	}
}

func TestHealthReportsWhatTheBackendSays(t *testing.T) {
	h := newHarness(t, "")
	h.fake.HealthVal = backend.Health{OK: false, Backend: "llama.cpp", Detail: "llama-server is not running"}
	resp, data := h.do(t, http.MethodGet, "/api/health", "", nil)
	got := decode[api.Health](t, data)
	if resp.StatusCode != 200 || got.Status != "degraded" || got.Backend != "llama.cpp" || got.Detail != "llama-server is not running" {
		t.Fatalf("degraded: %d %+v", resp.StatusCode, got)
	}
	h.fake.Fail("health", errors.New(transportErr))
	resp, data = h.do(t, http.MethodGet, "/api/health", "", nil)
	got = decode[api.Health](t, data)
	if resp.StatusCode != 200 || got.Status != "degraded" || got.Detail != "the backend did not answer its health probe" {
		t.Fatalf("a failing probe must show as degraded with a fixed detail: %d %+v", resp.StatusCode, got)
	}
	assertNoTransportText(t, "the open health route", data)
	if logs := h.logs.String(); !strings.Contains(logs, "dial tcp 127.0.0.1:41234") {
		t.Fatalf("the probe error belongs in the log: %s", logs)
	}
}

func TestHeadOnHealthIsOpenLikeGet(t *testing.T) {
	h := newHarness(t, "secret")
	status, _, body, err := h.request(http.MethodHead, "/api/health", "", "", nil)
	if err != nil || status != 200 || len(body) != 0 {
		t.Fatalf("HEAD /api/health without a key: %d %q %v", status, body, err)
	}
	for _, path := range []string{"/api/version", "/api/models", "/v1/models"} {
		if status, _, _, err := h.request(http.MethodHead, path, "", "", nil); err != nil || status != http.StatusUnauthorized {
			t.Errorf("HEAD %s without a key: %d %v", path, status, err)
		}
	}
}

// withHost makes a request name another host than the one it is sent to.
func withHost(host string) func(*http.Request) {
	return func(r *http.Request) { r.Host = host }
}

func hostStatus(t *testing.T, h *harness, method, path, host, key string) (int, []byte) {
	t.Helper()
	status, _, data, err := h.request(method, path, "", key, nil, withHost(host))
	if err != nil {
		t.Fatalf("%s %s as %q: %v", method, path, host, err)
	}
	return status, data
}

func TestWithoutAKeyOnlyTheDaemonsOwnNamesAreServed(t *testing.T) {
	h := newHarness(t, "")
	allowed := []string{
		"localhost", "localhost:8272", "LocalHost:8272", "localhost.:8272", "127.0.0.1", "127.0.0.1:8272", "[::1]",
		"[::1]:8272", "::1",
	}
	refused := []string{
		"evil.example", "evil.example:8272", "127.0.0.1.evil.example", "localhost.evil.example:8272", "[::2]:8272",
		"198.51.100.9:8272", "127.0.0.2", "a:b:c",
	}
	for _, path := range []string{"/api/health", "/api/version", "/v1/models"} {
		for _, host := range allowed {
			if status, data := hostStatus(t, h, http.MethodGet, path, host, ""); status != 200 {
				t.Errorf("GET %s as %q: %d %s", path, host, status, data)
			}
		}
		for _, host := range refused {
			status, data := hostStatus(t, h, http.MethodGet, path, host, "")
			if body := decode[api.Error](t, data).Error; status != http.StatusForbidden || body.Message == "" || body.Hint == "" {
				t.Errorf("GET %s as %q: %d %s", path, host, status, data)
			}
		}
	}
	status, _ := hostStatus(t, h, http.MethodPost, "/api/load", "evil.example", "")
	if status != http.StatusForbidden || len(h.fake.Loads) != 0 {
		t.Fatalf("a refused host must not reach a handler: %d %+v", status, h.fake.Loads)
	}
}

func TestAnAnswerToARefusedHostSaysNothingAboutTheRequest(t *testing.T) {
	h := newHarness(t, "")
	_, data := hostStatus(t, h, http.MethodGet, "/api/version", "evil.example:8272", "")
	if strings.Contains(string(data), "evil.example") {
		t.Fatalf("the answer must not repeat the host it refuses: %s", data)
	}
}

func TestAHandlerCalledWithoutAHostIsRefusedToo(t *testing.T) {
	h := newHarness(t, "")
	req := httptest.NewRequest(http.MethodGet, "/api/version", nil)
	req.Host = ""
	rec := httptest.NewRecorder()
	New(h.deps).ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("a request that names no host: %d", rec.Code)
	}
	req.Host = "127.0.0.1:8272"
	rec = httptest.NewRecorder()
	New(h.deps).ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("the daemon's own address: %d", rec.Code)
	}
}

func TestTheListenAddressNamesAnAllowedHost(t *testing.T) {
	cases := []struct {
		listen  string
		allowed []string
		refused []string
	}{
		{"192.0.2.7:8272", []string{"192.0.2.7:8272", "192.0.2.7", "127.0.0.1"}, []string{"192.0.2.8:8272", "evil.example"}},
		{"engine.lan:8272", []string{"engine.lan:8272", "ENGINE.LAN"}, []string{"engine.lan.evil.example", "other.lan"}},
		{"[2001:db8::7]:8272", []string{"[2001:db8::7]:8272", "[2001:db8::7]"}, []string{"[2001:db8::8]:8272"}},
		{"localhost:8272", []string{"localhost"}, []string{"192.0.2.7"}},
		{"not an address", []string{"localhost", "127.0.0.1"}, []string{"192.0.2.7", "evil.example"}},
	}
	for _, tc := range cases {
		h := newHarness(t, "", func(d *Deps) { d.Config.Listen = tc.listen })
		for _, host := range tc.allowed {
			if status, data := hostStatus(t, h, http.MethodGet, "/api/version", host, ""); status != 200 {
				t.Errorf("listen %q, Host %q: %d %s", tc.listen, host, status, data)
			}
		}
		for _, host := range tc.refused {
			if status, data := hostStatus(t, h, http.MethodGet, "/api/version", host, ""); status != http.StatusForbidden {
				t.Errorf("listen %q, Host %q: %d %s", tc.listen, host, status, data)
			}
		}
	}
}

func TestAWildcardListenAddressCannotNameItsHostsSoItServesAnyHost(t *testing.T) {
	for _, listen := range []string{"0.0.0.0:8272", ":8272", "[::]:8272"} {
		h := newHarness(t, "", func(d *Deps) { d.Config.Listen = listen })
		if status, data := hostStatus(t, h, http.MethodGet, "/api/version", "192.0.2.50:8272", ""); status != 200 {
			t.Errorf("listen %q: %d %s", listen, status, data)
		}
	}
}

func TestWithAKeyTheHostIsNotChecked(t *testing.T) {
	h := newHarness(t, "secret")
	if status, data := hostStatus(t, h, http.MethodGet, "/api/version", "evil.example", "secret"); status != 200 {
		t.Fatalf("the key is what keeps a page out, so a known key is served under any name: %d %s", status, data)
	}
	if status, _ := hostStatus(t, h, http.MethodGet, "/api/version", "evil.example", ""); status != http.StatusUnauthorized {
		t.Fatalf("without the key it is the key check that answers: %d", status)
	}
	if status, _ := hostStatus(t, h, http.MethodGet, "/api/health", "evil.example", ""); status != 200 {
		t.Fatalf("health stays open under any name when a key is set: %d", status)
	}
}

func TestEveryOtherRouteNeedsTheKeyToo(t *testing.T) {
	h := newHarness(t, "secret")
	routes := []struct{ method, path string }{
		{http.MethodPost, "/api/load"}, {http.MethodPost, "/api/unload"}, {http.MethodPost, "/api/pull"},
		{http.MethodDelete, "/api/models/gemma4:12b"}, {http.MethodGet, "/api/models/gemma4:12b"},
		{http.MethodPost, "/api/health"},
	}
	for _, rt := range routes {
		for _, key := range []string{"", "wrong"} {
			resp, data := h.do(t, rt.method, rt.path, key, api.UnloadRequest{Model: "gemma4:12b"})
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("%s %s with key %q: %d", rt.method, rt.path, key, resp.StatusCode)
				continue
			}
			body := decode[api.Error](t, data).Error
			if body.Message != "missing or wrong API key" || !strings.Contains(body.Hint, "Bearer") {
				t.Errorf("%s %s: error body %+v", rt.method, rt.path, body)
			}
			if got := resp.Header.Get("WWW-Authenticate"); got != "Bearer" {
				t.Errorf("%s %s: WWW-Authenticate %q", rt.method, rt.path, got)
			}
		}
	}
	if len(h.fake.Loads) != 0 || len(h.fake.Unloads) != 0 || h.fake.Refreshes != 0 {
		t.Fatalf("a refused request must not reach the backend: %+v %+v %d", h.fake.Loads, h.fake.Unloads, h.fake.Refreshes)
	}
}

func TestBearerSchemeAndKeyAreMatchedExactly(t *testing.T) {
	h := newHarness(t, "secret")
	cases := map[string]int{
		"Bearer secret":  http.StatusOK,
		"bearer secret":  http.StatusOK,
		"BEARER secret":  http.StatusOK,
		"secret":         http.StatusUnauthorized,
		"Bearer":         http.StatusUnauthorized,
		"Bearer ":        http.StatusUnauthorized,
		"Bearer  secret": http.StatusUnauthorized,
		"Bearer secre":   http.StatusUnauthorized,
		"Bearer secrets": http.StatusUnauthorized,
		"Basic c2VjcmV0": http.StatusUnauthorized,
		"":               http.StatusUnauthorized,
	}
	for header, want := range cases {
		req, _ := http.NewRequest(http.MethodGet, h.srv.URL+"/api/version", nil)
		if header != "" {
			req.Header.Set("Authorization", header)
		}
		resp, err := testClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("Authorization %q: %d, want %d", header, resp.StatusCode, want)
		}
	}
}

func TestAnOpenDaemonAcceptsAnyCredentials(t *testing.T) {
	h := newHarness(t, "")
	for _, key := range []string{"", "whatever"} {
		if resp, _ := h.do(t, http.MethodGet, "/api/version", key, nil); resp.StatusCode != 200 {
			t.Errorf("a daemon without a key must not check the one a client sends (%q): %d", key, resp.StatusCode)
		}
	}
}

func TestVersionAndDoctor(t *testing.T) {
	h := newHarness(t, "")
	_, data := h.do(t, http.MethodGet, "/api/version", "", nil)
	v := decode[api.Version](t, data)
	if v.Version != "0.1.0-test" || v.Runtime.Build != "bTEST" || v.Runtime.Backend != "llama.cpp" || v.Platform != "linux/amd64/cuda12" {
		t.Fatalf("version: %+v", v)
	}
	_, data = h.do(t, http.MethodGet, "/api/doctor", "", nil)
	d := decode[api.Doctor](t, data)
	if d.Machine.GPU != "NVIDIA GeForce RTX 5090" || d.Machine.Tier != "48" || !d.Runtime.Verified || d.Recommendation["48"] != "qwen3.6:35b" || d.Recommendation["16"] != "gemma4:12b" {
		t.Fatalf("doctor: %+v", d)
	}
}

func TestDoctorCarriesEveryMachineAndRuntimeField(t *testing.T) {
	h := newHarness(t, "", func(d *Deps) {
		d.Machine = func(context.Context) machine.Info {
			return machine.Info{
				OS: "darwin", Arch: "arm64", GPU: "Apple M4 Max", GPUMemoryBytes: 64 << 30, UnifiedMemory: true,
				RAMBytes: 64 << 30, Tier: "48", DriverVersion: "570.1",
			}
		}
		d.Runtime = func() api.RuntimeStatus {
			return api.RuntimeStatus{Installed: true, Build: "b1", Dir: "/rt/b1", Error: "checksum mismatch"}
		}
	})
	_, data := h.do(t, http.MethodGet, "/api/doctor", "", nil)
	d := decode[api.Doctor](t, data)
	want := api.Machine{
		OS: "darwin", Arch: "arm64", GPU: "Apple M4 Max", GPUMemoryBytes: 64 << 30, UnifiedMemory: true,
		RAMBytes: 64 << 30, Tier: "48", DriverVersion: "570.1",
	}
	if d.Machine != want {
		t.Fatalf("machine: %+v", d.Machine)
	}
	if rt := d.Runtime; !rt.Installed || rt.Verified || rt.Build != "b1" || rt.Dir != "/rt/b1" || rt.Error != "checksum mismatch" {
		t.Fatalf("runtime: %+v", rt)
	}
	for tier, name := range map[string]string{"small": "gemma4:e4b", "16": "gemma4:12b", "32": "glm-4.7-flash", "48": "qwen3.6:35b"} {
		if d.Recommendation[tier] != name {
			t.Errorf("recommendation for %s: %q, want %q", tier, d.Recommendation[tier], name)
		}
	}
}

func TestNilProvidersAndLoggerGetDefaults(t *testing.T) {
	logs := &lockedBuffer{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	h := newHarness(t, "", func(d *Deps) {
		d.Machine, d.Runtime, d.Logger = nil, nil, nil
	})
	resp, data := h.do(t, http.MethodGet, "/api/doctor", "", nil)
	d := decode[api.Doctor](t, data)
	if resp.StatusCode != 200 || d.Machine != (api.Machine{}) || d.Runtime != (api.RuntimeStatus{}) || d.Recommendation["48"] != "qwen3.6:35b" {
		t.Fatalf("doctor without providers: %d %s", resp.StatusCode, data)
	}
	if resp, _ := h.do(t, http.MethodPost, "/api/pull?wait=1", "", api.PullRequest{Model: "glm-4.7-flash"}); resp.StatusCode != 200 {
		t.Fatalf("pull: %d", resp.StatusCode)
	}
	if !strings.Contains(logs.String(), "pulled") {
		t.Fatalf("without a logger of its own the server logs through the default one: %q", logs.String())
	}
}

func TestProxyIsMounted(t *testing.T) {
	h := newHarness(t, "secret")
	resp, data := h.do(t, http.MethodGet, "/v1/models", "secret", nil)
	if resp.StatusCode != 200 || string(data) != `{"proxied":true}` {
		t.Fatalf("proxy: %d %s", resp.StatusCode, data)
	}
}

// flushCounter counts the flushes a handler asks for.
type flushCounter struct {
	*httptest.ResponseRecorder
	flushes int
}

func (f *flushCounter) Flush() {
	f.flushes++
	f.ResponseRecorder.Flush()
}

func TestSSEWriterFlushesTheHeadersAndEveryEvent(t *testing.T) {
	rec := &flushCounter{ResponseRecorder: httptest.NewRecorder()}
	s := newSSE(rec)
	if rec.flushes != 1 || rec.Code != http.StatusOK {
		t.Fatalf("opening the stream must commit and flush the headers: %d flushes, status %d", rec.flushes, rec.Code)
	}
	s.send(api.PullEvent{Stage: "resolving", File: "a.gguf"})
	s.send(api.PullEvent{Stage: "done"})
	if rec.flushes != 3 {
		t.Fatalf("every event must be flushed: %d flushes", rec.flushes)
	}
	if got := rec.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("content type %q", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
		t.Fatalf("cache control %q", got)
	}
	if got := rec.Header().Get("X-Accel-Buffering"); got != "no" {
		t.Fatalf("a reverse proxy must be told not to buffer the stream: %q", got)
	}
	want := `data: {"stage":"resolving","file":"a.gguf","bytes":0,"total":0}` + "\n\n" +
		`data: {"stage":"done","bytes":0,"total":0}` + "\n\n"
	if rec.Body.String() != want {
		t.Fatalf("stream body\n got: %q\nwant: %q", rec.Body.String(), want)
	}
}

// plainWriter hides the Flush method of the writer it wraps.
type plainWriter struct{ http.ResponseWriter }

func TestSSEWriterToleratesAWriterThatCannotFlush(t *testing.T) {
	rec := httptest.NewRecorder()
	s := newSSE(plainWriter{rec})
	s.send(api.PullEvent{Stage: "done"})
	if !strings.Contains(rec.Body.String(), `"stage":"done"`) {
		t.Fatalf("event lost: %q", rec.Body.String())
	}
}

func TestErrorsAreWrittenInTheSharedShape(t *testing.T) {
	rec := httptest.NewRecorder()
	writeError(rec, http.StatusTeapot, "short and stout", "tip me over")
	if rec.Code != http.StatusTeapot || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("status %d, content type %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	got := decode[api.Error](t, rec.Body.Bytes())
	if got.Error.Message != "short and stout" || got.Error.Hint != "tip me over" {
		t.Fatalf("body %+v", got)
	}
}
