package cli

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/tara-vision/taraengine/internal/api"
	"github.com/tara-vision/taraengine/internal/backend"
)

// configFile writes a config file with the lines and returns its path.
func configFile(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// listenOf is the host:port a test daemon listens on.
func listenOf(t *testing.T, d *testDaemon) string {
	t.Helper()
	u, err := url.Parse(d.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return u.Host
}

func TestEveryClientCommandReadsTheKeyAndTheAddressFromTheConfigFile(t *testing.T) {
	d := startDaemon(t, func(deps *api.Deps) { deps.Config.APIKey = "sesame" })
	d.install(t, "gemma4:12b")
	d.fake.SetModels(backend.ModelStatus{Name: "gemma4:12b", Status: backend.StatusPulled})
	cfg := configFile(t, "listen: "+listenOf(t, d), "api_key: sesame")
	for _, args := range [][]string{
		{"list"}, {"ps"}, {"load", "gemma4:12b"}, {"unload", "gemma4:12b"}, {"pull", "glm-4.7-flash"}, {"rm", "glm-4.7-flash"},
		{"doctor", "--data-dir", t.TempDir()},
	} {
		out, err := run(t, append(args, "--config", cfg)...)
		if err != nil {
			t.Errorf("%s with the key and the address in the config file: %v\n%s", args[0], err, out)
		}
		if args[0] == "doctor" && !strings.Contains(out, "Daemon: taraengine 0.1.0-test") {
			t.Errorf("doctor does not reach the daemon:\n%s", out)
		}
	}
}

func TestTheKeyComesFromTheFlagThenTheEnvironmentThenTheConfigFile(t *testing.T) {
	d := startDaemon(t, func(deps *api.Deps) { deps.Config.APIKey = "sesame" })
	listen := "listen: " + listenOf(t, d) // the address comes from the file too, which is what lets its key be used
	right := configFile(t, listen, "api_key: sesame")
	wrong := configFile(t, listen, "api_key: wrong")
	if _, err := run(t, "list", "--config", right); err != nil {
		t.Fatalf("the file: %v", err)
	}
	if _, err := run(t, "list", "--config", wrong); err == nil || !strings.Contains(err.Error(), "API key") {
		t.Fatalf("a wrong key in the file is refused by the daemon: %v", err)
	}
	t.Setenv("TARAENGINE_API_KEY", "sesame")
	if _, err := run(t, "list", "--config", wrong); err != nil {
		t.Fatalf("the environment wins over the file: %v", err)
	}
	if _, err := run(t, "list", "--config", right, "--api-key", "wrong"); err == nil {
		t.Fatal("the flag wins over the environment and the file")
	}
	t.Setenv("TARAENGINE_API_KEY", "wrong")
	if _, err := run(t, "list", "--config", right, "--api-key", "sesame"); err != nil {
		t.Fatalf("the flag wins over the environment: %v", err)
	}
}

// recordingDaemon answers every call with an empty list or object and keeps the Authorization header of each. The
// function it returns hands over what was recorded since the last call.
func recordingDaemon(t *testing.T) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/models" || r.URL.Path == "/api/loaded" {
			_, _ = io.WriteString(w, "[]")
			return
		}
		_, _ = io.WriteString(w, "{}")
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		out := seen
		seen = nil
		return out
	}
}

func TestTheKeyOfTheConfigFileGoesOnlyWithTheAddressOfTheConfigFile(t *testing.T) {
	withConfigHome(t)
	srv, taken := recordingDaemon(t)
	cfg := configFile(t, "listen: "+strings.TrimPrefix(srv.URL, "http://"), "api_key: local-daemon-secret")
	clients := [][]string{{"list"}, {"ps"}, {"load", "m"}, {"unload", "m"}, {"rm", "m"}, {"pull", "m"}, {"doctor", "--data-dir", t.TempDir()}}
	check := func(what string, want string, extra ...string) {
		t.Helper()
		for _, args := range clients {
			_, _ = run(t, append(slices.Concat(args, extra), "--config", cfg)...)
			got := taken()
			if len(got) == 0 {
				t.Errorf("%s, %s: the daemon was not asked", what, args[0])
			}
			for _, authorization := range got {
				if authorization != want {
					t.Errorf("%s, %s: sent %q, want %q", what, args[0], authorization, want)
				}
			}
		}
	}
	check("the address and the key of the file", "Bearer local-daemon-secret")
	check("another address named with --host", "", "--host="+srv.URL)
	check("the key flag with the address of the file", "Bearer from-flag", "--api-key", "from-flag")
	check("another address and the key flag", "Bearer from-flag", "--host="+srv.URL, "--api-key", "from-flag")
	t.Setenv("TARAENGINE_API_KEY", "from-env")
	check("another address and the environment", "Bearer from-env", "--host="+srv.URL)
	check("the address of the file and the environment", "Bearer from-env")
}

func TestTheHostFlagWinsOverTheListenAddressOfTheConfigFile(t *testing.T) {
	d := startDaemon(t, nil)
	dead := configFile(t, "listen: 127.0.0.1:1")
	if _, err := run(t, "list", "--config", dead); err == nil || !strings.Contains(err.Error(), "taraengine is not running at http://127.0.0.1:1:") {
		t.Fatalf("the address of the config file is the one that is tried, and named: %v", err)
	}
	if _, err := run(t, "list", "--config", dead, "--host="+d.srv.URL); err != nil {
		t.Fatalf("the flag: %v", err)
	}
}

func TestAWildcardListenAddressIsReachedOnTheLoopback(t *testing.T) {
	d := startDaemon(t, nil)
	_, port, _ := strings.Cut(listenOf(t, d), ":")
	if _, err := run(t, "list", "--config", configFile(t, "listen: 0.0.0.0:"+port)); err != nil {
		t.Fatalf("a daemon that listens on every interface answers on 127.0.0.1: %v", err)
	}
	for listen, want := range map[string]string{
		"127.0.0.1:8272": "http://127.0.0.1:8272", "localhost:9": "http://localhost:9", "0.0.0.0:8272": "http://127.0.0.1:8272",
		":8272": "http://127.0.0.1:8272", "[::]:8272": "http://[::1]:8272", "[::1]:8272": "http://[::1]:8272",
		"192.0.2.7:8272": "http://192.0.2.7:8272", "bad": "http://127.0.0.1:8272",
	} {
		if got := daemonURL(listen); got != want {
			t.Errorf("daemonURL(%q) = %q, want %q", listen, got, want)
		}
	}
}

func TestWithTheHostGivenTheConfigFileIsNotRead(t *testing.T) {
	d := startDaemon(t, func(deps *api.Deps) { deps.Config.APIKey = "sesame" })
	missing := filepath.Join(t.TempDir(), "missing.yaml")
	broken := configFile(t, "api-key: sesame")
	for _, cfg := range []string{missing, broken} {
		if _, err := run(t, "list", "--host="+d.srv.URL, "--api-key", "sesame", "--config", cfg); err != nil {
			t.Fatalf("a client that was told where to go needs no file, whatever it says: %v", err)
		}
		if _, err := run(t, "list", "--host="+d.srv.URL, "--config", cfg); err == nil || !strings.Contains(err.Error(), "API key") {
			t.Fatalf("the file has nothing to give another address, so it is not read, and the key is what is missing: %v", err)
		}
	}
}

func TestTheDefaultConfigFileIsReadByTheClientCommands(t *testing.T) {
	d := startDaemon(t, func(deps *api.Deps) { deps.Config.APIKey = "sesame" })
	path := withConfigHome(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("listen: "+listenOf(t, d)+"\napi_key: sesame\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := run(t, "list"); err != nil || !strings.Contains(out, "NAME") {
		t.Fatalf("no flag at all: %v\n%s", err, out)
	}
}

func TestEveryClientCommandReturnsTheErrorOfAConfigFileItNeeds(t *testing.T) {
	broken := configFile(t, "api-key: sesame")
	for _, args := range [][]string{{"list"}, {"ps"}, {"load", "m"}, {"unload", "m"}, {"rm", "m"}, {"pull", "m"}} {
		out, err := run(t, append(args, "--config", broken)...)
		if err == nil || !strings.Contains(err.Error(), `unknown key "api-key"`) || out != "" {
			t.Errorf("%s: %v %q", args[0], err, out)
		}
	}
}
