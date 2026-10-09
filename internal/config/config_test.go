package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultValues(t *testing.T) {
	c := Default()
	if c.Listen != "127.0.0.1:8272" || c.ModelsMax != 1 || c.SleepIdleSeconds != 900 || c.Context != 32768 || c.LoadTimeout != 600 {
		t.Fatalf("unexpected defaults: %+v", c)
	}
	if c.DataDir == "" {
		t.Fatal("data dir is empty")
	}
}

func TestLoadMissingFileIsTheDefault(t *testing.T) {
	c, err := Load(filepath.Join(t.TempDir(), "absent.yaml"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.Listen != Default().Listen {
		t.Fatalf("listen %q", c.Listen)
	}
}

func TestLoadOverlaysTheFileAndTheEnvironment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	body := "listen: 0.0.0.0:8272\napi_key: file-key\nsleep_idle_seconds: -1\ncontext_by_tier:\n  \"16\": 16384\npreset_global:\n  n-gpu-layers: \"0\"\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TARAENGINE_API_KEY", "env-key")
	t.Setenv("TARAENGINE_HF_TOKEN", "hf-token")
	c, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.Listen != "0.0.0.0:8272" || c.APIKey != "env-key" || c.HFToken != "hf-token" || c.SleepIdleSeconds != -1 {
		t.Fatalf("overlay wrong: %+v", c)
	}
	if c.ContextByTier["16"] != 16384 || c.PresetGlobal["n-gpu-layers"] != "0" {
		t.Fatalf("maps wrong: %+v", c)
	}
	if c.ModelsMax != 1 {
		t.Fatalf("models_max lost its default: %d", c.ModelsMax)
	}
}

func TestLoadRejectsBadYAML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("listen: [\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "parse") {
		t.Fatalf("want a parse error, got %v", err)
	}
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(c *Config)
		want   string
	}{
		{"listen without a port", func(c *Config) { c.Listen = "127.0.0.1" }, "host:port"},
		{"empty data dir", func(c *Config) { c.DataDir = "" }, "data_dir"},
		{"models_max zero", func(c *Config) { c.ModelsMax = 0 }, "models_max"},
		{"sleep below -1", func(c *Config) { c.SleepIdleSeconds = -2 }, "sleep_idle_seconds"},
		{"context too small", func(c *Config) { c.Context = 100 }, "context 100"},
		{"tier context too small", func(c *Config) { c.ContextByTier = map[string]int{"32": 8} }, "context_by_tier[32]"},
		{"load timeout zero", func(c *Config) { c.LoadTimeout = 0 }, "load_timeout"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := Default()
			tc.mutate(&c)
			err := c.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
	if err := Default().Validate(); err != nil {
		t.Fatalf("defaults must validate: %v", err)
	}
}

func TestContextFor(t *testing.T) {
	c := Default()
	c.ContextByTier = map[string]int{"16": 16384}
	cases := []struct {
		tier   string
		native int
		want   int
	}{
		{"32", 0, 32768},
		{"32", 262144, 32768},
		{"32", 8192, 8192},
		{"16", 0, 16384},
		{"16", 4096, 4096},
	}
	for _, tc := range cases {
		if got := c.ContextFor(tc.tier, tc.native); got != tc.want {
			t.Errorf("ContextFor(%q, %d) = %d, want %d", tc.tier, tc.native, got, tc.want)
		}
	}
}

func TestListenBeyondLocalhost(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1:8272": false,
		"localhost:8272": false,
		"[::1]:8272":     false,
		"0.0.0.0:8272":   true,
		":8272":          true,
		"192.0.2.3:8272": true,
		"engine:8272":    true,
		"bad":            false,
	}
	for listen, want := range cases {
		c := Config{Listen: listen}
		if got := c.ListenBeyondLocalhost(); got != want {
			t.Errorf("%q: got %v, want %v", listen, got, want)
		}
	}
}

func TestLoadReportsAnUnreadablePath(t *testing.T) {
	_, err := Load(t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "read") {
		t.Fatalf("want a read error for a directory, got %v", err)
	}
}

func TestLoadRejectsAFileThatFailsValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("listen: 127.0.0.1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "host:port") {
		t.Fatalf("want a validation error, got %v", err)
	}
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// wantKeys are the keys of the config file, in the order the error lists them.
var wantKeys = []string{
	"listen", "api_key", "data_dir", "models_max", "sleep_idle_seconds", "context", "context_by_tier", "hf_token",
	"load_timeout", "runtime_dir", "preset_global", "allowed_origins",
}

func TestLoadRefusesAKeyItDoesNotKnow(t *testing.T) {
	path := writeConfig(t, "listen: 0.0.0.0:8272\napi-key: s3cret\n")
	cfg, err := Load(path)
	if err == nil {
		t.Fatalf("a misspelled api_key must not load as an engine with no key: %+v", cfg)
	}
	want := []string{path, `unknown key "api-key" (line 2)`, "the accepted keys are " + strings.Join(wantKeys, ", ")}
	for _, part := range want {
		if !strings.Contains(err.Error(), part) {
			t.Errorf("the error lacks %q: %v", part, err)
		}
	}
	if strings.Contains(err.Error(), "s3cret") {
		t.Errorf("the error must not repeat the value of the key: %v", err)
	}
}

func TestLoadNamesEveryKeyItDoesNotKnow(t *testing.T) {
	path := writeConfig(t, "listn: 127.0.0.1:1\nmodels_max: 2\nhf-token: x\n")
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), `unknown keys "listn" (line 1), "hf-token" (line 3)`) {
		t.Fatalf("both keys are named: %v", err)
	}
}

func TestLoadKeepsAnUnknownKeyApartFromAWrongValue(t *testing.T) {
	if _, err := Load(writeConfig(t, "models_max: many\n")); err == nil || !strings.Contains(err.Error(), "parse") ||
		strings.Contains(err.Error(), "unknown key") {
		t.Fatalf("a value of the wrong type is a parse error, not an unknown key: %v", err)
	}
}

func TestLoadTakesAFileWithNothingInIt(t *testing.T) {
	for name, body := range map[string]string{"empty": "", "comments only": "# nothing to set\n"} {
		c, err := Load(writeConfig(t, body))
		if err != nil || c.Listen != Default().Listen || c.ModelsMax != 1 {
			t.Errorf("%s is the defaults: %+v %v", name, c, err)
		}
	}
}

func TestValidateRefusesSleepIdleSecondsZero(t *testing.T) {
	// llama-server stops with "cannot be 0 or less than -1" on --sleep-idle-seconds 0, so a 0 in the file would
	// start a child that dies at once on every restart.
	for _, n := range []int{0, -2} {
		c := Default()
		c.SleepIdleSeconds = n
		err := c.Validate()
		if err == nil || !strings.Contains(err.Error(), "sleep_idle_seconds") || !strings.Contains(err.Error(), "-1 (never sleep) or at least 1") {
			t.Errorf("%d: %v", n, err)
		}
	}
	for _, n := range []int{-1, 1, 900} {
		c := Default()
		c.SleepIdleSeconds = n
		if err := c.Validate(); err != nil {
			t.Errorf("%d must be accepted: %v", n, err)
		}
	}
}

func TestLoadReadsAllowedOrigins(t *testing.T) {
	c, err := Load(writeConfig(t, "allowed_origins:\n  - https://app.example\n  - app://obsidian.md\n"))
	if err != nil || len(c.AllowedOrigins) != 2 || c.AllowedOrigins[0] != "https://app.example" {
		t.Fatalf("%+v %v", c, err)
	}
	if got := Default().AllowedOrigins; len(got) != 0 {
		t.Fatalf("no origin is listed by default: %v", got)
	}
}

func TestValidateChecksEveryAllowedOrigin(t *testing.T) {
	for _, bad := range []string{
		"", "*", "null", "app.example", "//app.example", "https://", "https://app.example/", "https://app.example/x",
		"https://app.example?x=1", "https://app.example#top", "https://user@app.example", "https:app.example",
		"https://app.example:port",
	} {
		c := Default()
		c.AllowedOrigins = []string{"https://fine.example", bad}
		err := c.Validate()
		if err == nil || !strings.Contains(err.Error(), "allowed_origins[1]") || !strings.Contains(err.Error(), "such as https://app.example") {
			t.Errorf("%q: %v", bad, err)
		}
	}
	c := Default()
	c.AllowedOrigins = []string{"https://app.example", "http://localhost:5173", "http://[::1]:8080", "app://obsidian.md", "HTTPS://App.Example:8443"}
	if err := c.Validate(); err != nil {
		t.Fatalf("origins as browsers write them: %v", err)
	}
}

func TestParseOriginReturnsTheSchemeAndTheHostName(t *testing.T) {
	for _, tc := range []struct{ origin, scheme, host string }{
		{"https://app.example", "https", "app.example"},
		{"http://localhost:5173", "http", "localhost"},
		{"http://127.0.0.1:8272", "http", "127.0.0.1"},
		{"http://[::1]:8272", "http", "::1"},
		{"HTTPS://App.Example:8443", "https", "app.example"},
		{"http://localhost.:3000", "http", "localhost"},
		{"app://obsidian.md", "app", "obsidian.md"},
	} {
		scheme, host, err := ParseOrigin(tc.origin)
		if err != nil || scheme != tc.scheme || host != tc.host {
			t.Errorf("%q: %q %q %v", tc.origin, scheme, host, err)
		}
	}
}
