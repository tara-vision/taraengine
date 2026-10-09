package llamaserver

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tara-vision/taraengine/internal/state"
)

func emptyCatalog(context.Context, map[string]int) ([]PresetModel, error) { return nil, nil }

func TestNewValidatesTheOptions(t *testing.T) {
	dir := t.TempDir()
	valid := func() Options {
		return Options{DataDir: dir, State: testStore(dir), Models: emptyCatalog, Attach: "http://127.0.0.1:9", SleepIdleSeconds: 900}
	}
	rows := map[string]struct {
		change func(*Options)
		want   string
	}{
		"no data dir":        {func(o *Options) { o.DataDir = "" }, "data dir"},
		"no models function": {func(o *Options) { o.Models = nil }, "models function"},
		"no state path":      {func(o *Options) { o.State = state.Store{} }, "state path"},
		"no runtime dir and nothing to attach to":        {func(o *Options) { o.Attach = "" }, "runtime dir"},
		"an attach address that is no URL":               {func(o *Options) { o.Attach = "::" }, "router URL"},
		"an attach address that is not HTTP":             {func(o *Options) { o.Attach = "ftp://127.0.0.1:21" }, "router URL"},
		"an attach address without a host":               {func(o *Options) { o.Attach = "http://" }, "router URL"},
		"an idle sleep below -1":                         {func(o *Options) { o.SleepIdleSeconds = -2 }, "sleep idle seconds -2"},
		"an idle sleep of 0, which llama-server refuses": {func(o *Options) { o.SleepIdleSeconds = 0 }, "sleep idle seconds 0 must be -1"},
	}
	if _, err := New(valid()); err != nil {
		t.Fatalf("the base options must be valid: %v", err)
	}
	for name, row := range rows {
		o := valid()
		row.change(&o)
		if _, err := New(o); err == nil || !strings.Contains(err.Error(), row.want) {
			t.Errorf("%s: want an error with %q, got %v", name, row.want, err)
		}
	}
}

func TestNewFillsTheDefaults(t *testing.T) {
	dir := t.TempDir()
	b, err := New(Options{DataDir: dir, RuntimeDir: dir, State: testStore(dir), Models: emptyCatalog, SleepIdleSeconds: 900})
	if err != nil {
		t.Fatal(err)
	}
	o := b.opts
	if o.ModelsMax != 1 || o.SleepIdleSeconds != 900 || o.LoadTimeout != 10*time.Minute || o.Poll != 500*time.Millisecond {
		t.Fatalf("defaults: %+v (the idle sleep has no default and passes as configured)", o)
	}
	if o.Global["jinja"] != "true" || o.Logger == nil || len(o.APIKey) < 20 {
		t.Fatalf("global %v, logger %v, key %q", o.Global, o.Logger, o.APIKey)
	}
	if o.Port <= 0 {
		t.Fatalf("a spawned child needs a port, got %d", o.Port)
	}
	if want := "http://127.0.0.1:" + strconv.Itoa(o.Port); b.client.BaseURL != want || b.client.APIKey != o.APIKey {
		t.Fatalf("client %q with key %q, want %q", b.client.BaseURL, b.client.APIKey, want)
	}
	attached, err := New(Options{DataDir: dir, Attach: "http://example.test:8080/", State: testStore(dir), Models: emptyCatalog, SleepIdleSeconds: 900})
	if err != nil {
		t.Fatal(err)
	}
	if attached.opts.Port != 0 || attached.client.BaseURL != "http://example.test:8080" {
		t.Fatalf("attach mode picks no port: port %d, client %q", attached.opts.Port, attached.client.BaseURL)
	}
}

func TestNewKeepsWhatTheCallerSet(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(&syncBuffer{}, nil))
	global := map[string]string{"jinja": "false"}
	b, err := New(Options{
		DataDir: dir, RuntimeDir: dir, State: testStore(dir), Models: emptyCatalog, Port: 12345, APIKey: "mine",
		ModelsMax: 3, SleepIdleSeconds: 60, LoadTimeout: time.Minute, Poll: time.Second, Logger: logger, Global: global,
	})
	if err != nil {
		t.Fatal(err)
	}
	o := b.opts
	if o.Port != 12345 || o.APIKey != "mine" || o.ModelsMax != 3 || o.SleepIdleSeconds != 60 || o.LoadTimeout != time.Minute || o.Poll != time.Second || o.Logger != logger {
		t.Fatalf("options changed: %+v", o)
	}
	if b.client.BaseURL != "http://127.0.0.1:12345" || b.client.APIKey != "mine" {
		t.Fatalf("client %q key %q", b.client.BaseURL, b.client.APIKey)
	}
	global["jinja"] = "changed later by the caller"
	if o.Global["jinja"] != "false" {
		t.Fatalf("the backend must not share the caller's global map: %v", o.Global)
	}
}

func TestNewPassesTheIdleSleepThrough(t *testing.T) {
	dir := t.TempDir()
	for _, seconds := range []int{-1, 1, 900} {
		b, err := New(Options{DataDir: dir, RuntimeDir: dir, State: testStore(dir), Models: emptyCatalog, SleepIdleSeconds: seconds})
		if err != nil {
			t.Fatalf("%d: %v", seconds, err)
		}
		args := strings.Join(b.childArgs(), " ")
		if !strings.Contains(args, "--sleep-idle-seconds "+strconv.Itoa(seconds)+" ") {
			t.Fatalf("%d must reach the router as it is: %s", seconds, args)
		}
	}
}

func TestNewGeneratesAKeyPerBackend(t *testing.T) {
	dir := t.TempDir()
	keys := map[string]bool{}
	for range 5 {
		b, err := New(Options{DataDir: dir, RuntimeDir: dir, State: testStore(dir), Models: emptyCatalog, SleepIdleSeconds: 900})
		if err != nil {
			t.Fatal(err)
		}
		keys[b.opts.APIKey] = true
	}
	if len(keys) != 5 {
		t.Fatalf("generated keys must differ: %v", keys)
	}
}

func TestNewFailsWhenNoPortCanBeReserved(t *testing.T) {
	original := listenTCP
	listenTCP = func(string, string) (net.Listener, error) { return nil, errors.New("no sockets left") }
	t.Cleanup(func() { listenTCP = original })
	dir := t.TempDir()
	if _, err := New(Options{DataDir: dir, RuntimeDir: dir, State: testStore(dir), Models: emptyCatalog, SleepIdleSeconds: 900}); err == nil || !strings.Contains(err.Error(), "no sockets left") {
		t.Fatalf("want the listen error, got %v", err)
	}
}

func TestPaths(t *testing.T) {
	dir := filepath.Join("data", "dir")
	if PresetPath(dir) != filepath.Join(dir, "presets.ini") ||
		LogPath(dir) != filepath.Join(dir, "logs", "llama-server.log") ||
		KeyPath(dir) != filepath.Join(dir, "child.key") ||
		PIDPath(dir) != filepath.Join(dir, "child.pid") ||
		CachePath(dir) != filepath.Join(dir, "llama-cache") {
		t.Fatalf("paths: %s %s %s %s %s", PresetPath(dir), LogPath(dir), KeyPath(dir), PIDPath(dir), CachePath(dir))
	}
}
