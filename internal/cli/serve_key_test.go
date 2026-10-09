package cli

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tara-vision/taraengine/internal/llamatest"
)

// dropIdleConnections closes the connections that earlier clients keep open for reuse. A daemon that is asked to stop
// waits for a connection that has not sent a request yet, which a client may have opened ahead of need, and a test
// should not wait for it.
func dropIdleConnections() { http.DefaultTransport.(*http.Transport).CloseIdleConnections() }

// statusFor is the status of a GET of url, sent with the key as a Bearer token when there is one. Every call uses a
// connection of its own and closes it.
func statusFor(t *testing.T, url, key string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	once := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	resp, err := once.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

func TestServeRequiresTheKeyGivenWithTheAPIKeyFlag(t *testing.T) {
	s := startServe(t, llamatest.Options{}, nil, "--api-key", "sesame")
	url := "http://" + s.listen + "/api/models"
	if got := statusFor(t, url, ""); got != http.StatusUnauthorized {
		t.Fatalf("a request without the key must be refused, got %d", got)
	}
	if got := statusFor(t, url, "wrong"); got != http.StatusUnauthorized {
		t.Fatalf("a request with another key must be refused, got %d", got)
	}
	if got := statusFor(t, url, "sesame"); got != http.StatusOK {
		t.Fatalf("a request with the key must be served, got %d", got)
	}
	if got := statusFor(t, "http://"+s.listen+"/api/health", ""); got != http.StatusOK {
		t.Fatalf("the health route stays open, got %d", got)
	}
	if _, err := run(t, "list", s.host(), "--api-key", "sesame"); err != nil {
		t.Fatalf("the same flag opens the daemon from the client side: %v", err)
	}
	dropIdleConnections()
}

func TestServeTakesTheAPIKeyFromTheFlagThenTheEnvironmentThenTheFile(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cfgFile, []byte("api_key: file-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name    string
		env     string
		flags   []string
		accept  string
		refused []string
	}{
		{"the flag beats the environment and the file", "env-key", []string{"--api-key", "flag-key"}, "flag-key", []string{"env-key", "file-key"}},
		{"the environment beats the file", "env-key", nil, "env-key", []string{"file-key"}},
		{"the file when nothing else names a key", "", nil, "file-key", []string{"flag-key"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("TARAENGINE_API_KEY", c.env)
			s := startServe(t, llamatest.Options{}, nil, append([]string{"--config", cfgFile}, c.flags...)...)
			url := "http://" + s.listen + "/api/models"
			if got := statusFor(t, url, c.accept); got != http.StatusOK {
				t.Errorf("the key %q must be accepted, got %d", c.accept, got)
			}
			for _, key := range append(c.refused, "") {
				if got := statusFor(t, url, key); got != http.StatusUnauthorized {
					t.Errorf("the key %q must be refused, got %d", key, got)
				}
			}
		})
	}
}
