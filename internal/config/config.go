// Package config loads the daemon configuration: one YAML file with a default for everything.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"reflect"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// The bounds of a context window, in tokens: the smallest window a model is asked to run at, and a ceiling above
// every window a model has.
const (
	MinContext = 512
	MaxContext = 4194304
)

// Config is the daemon configuration. Load overlays the file and the environment on Default().
type Config struct {
	Listen           string            `yaml:"listen"`
	APIKey           string            `yaml:"api_key"`
	DataDir          string            `yaml:"data_dir"`
	ModelsMax        int               `yaml:"models_max"`
	SleepIdleSeconds int               `yaml:"sleep_idle_seconds"`
	Context          int               `yaml:"context"`
	ContextByTier    map[string]int    `yaml:"context_by_tier"`
	HFToken          string            `yaml:"hf_token"`
	LoadTimeout      int               `yaml:"load_timeout"`
	RuntimeDir       string            `yaml:"runtime_dir"`
	PresetGlobal     map[string]string `yaml:"preset_global"`
	// AllowedOrigins lists web origins, such as https://app.example, whose pages may use the API besides the pages
	// served from localhost, 127.0.0.1 and [::1].
	AllowedOrigins []string `yaml:"allowed_origins"`
}

// Default is the configuration without a file: localhost only, one resident model, a 15-minute idle
// sleep, a 32,768-token context, a 10-minute load timeout.
func Default() Config {
	return Config{
		Listen:           "127.0.0.1:8272",
		DataDir:          DefaultDataDir(),
		ModelsMax:        1,
		SleepIdleSeconds: 900,
		Context:          32768,
		LoadTimeout:      600,
	}
}

// Load reads path when it exists and overlays it on Default(); a missing file is not an error. A key the file
// has that this version does not know is an error that names it, so that a misspelled api_key can never leave an
// engine open. TARAENGINE_API_KEY and TARAENGINE_HF_TOKEN override the file. The result is validated.
func Load(path string) (Config, error) {
	cfg := Default()
	data, err := os.ReadFile(path) //nolint:gosec // G304: the path is the operator's own config file, not request input
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return Config{}, fmt.Errorf("config: read %s: %w", path, err)
	default:
		if cfg, err = parse(data, cfg); err != nil {
			return Config{}, fmt.Errorf("config: parse %s: %w", path, err)
		}
	}
	if v := os.Getenv("TARAENGINE_API_KEY"); v != "" {
		cfg.APIKey = v
	}
	if v := os.Getenv("TARAENGINE_HF_TOKEN"); v != "" {
		cfg.HFToken = v
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// parse overlays the YAML in data on base and returns the result. A file with no document in it changes nothing.
func parse(data []byte, base Config) (Config, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	cfg := base
	if err := dec.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
		if unknown := unknownKeys(err); len(unknown) > 0 {
			return Config{}, unknownKeyError(unknown)
		}
		return Config{}, err
	}
	return cfg, nil
}

// unknownKey is a key of the file that the configuration has no field for, and the line it stands on.
type unknownKey struct {
	name string
	line int
}

// unknownField matches the report of the YAML decoder for a key without a field.
var unknownField = regexp.MustCompile(`^line (\d+): field (.+) not found in type `)

// unknownKeys are the keys the decoder found no field for, in file order.
func unknownKeys(err error) []unknownKey {
	var typeErr *yaml.TypeError
	if !errors.As(err, &typeErr) {
		return nil
	}
	var keys []unknownKey
	for _, report := range typeErr.Errors {
		if m := unknownField.FindStringSubmatch(report); m != nil {
			line, _ := strconv.Atoi(m[1]) // the pattern matched digits
			keys = append(keys, unknownKey{name: m[2], line: line})
		}
	}
	return keys
}

// unknownKeyError names the keys and lists the keys the file may have.
func unknownKeyError(unknown []unknownKey) error {
	named := make([]string, 0, len(unknown))
	for _, k := range unknown {
		named = append(named, fmt.Sprintf("%q (line %d)", k.name, k.line))
	}
	noun := "key"
	if len(named) > 1 {
		noun = "keys"
	}
	return fmt.Errorf("unknown %s %s; the accepted keys are %s", noun, strings.Join(named, ", "),
		strings.Join(acceptedKeys(), ", "))
}

// acceptedKeys are the keys of the file, in the order of the fields of Config.
func acceptedKeys() []string {
	t := reflect.TypeFor[Config]()
	keys := make([]string, 0, t.NumField())
	for i := range t.NumField() {
		keys = append(keys, t.Field(i).Tag.Get("yaml"))
	}
	return keys
}

// Validate checks the values a file can get wrong and names the key in every error.
func (c Config) Validate() error {
	if _, port, err := net.SplitHostPort(c.Listen); err != nil || port == "" {
		return fmt.Errorf("config: listen %q must be host:port", c.Listen)
	}
	if c.DataDir == "" {
		return errors.New("config: data_dir must not be empty")
	}
	if c.ModelsMax < 1 {
		return fmt.Errorf("config: models_max %d must be at least 1", c.ModelsMax)
	}
	// llama-server takes -1 or a positive number of seconds and stops at once on 0, so 0 never gets as far as a child.
	if c.SleepIdleSeconds == 0 || c.SleepIdleSeconds < -1 {
		return fmt.Errorf("config: sleep_idle_seconds %d must be -1 (never sleep) or at least 1", c.SleepIdleSeconds)
	}
	if c.Context < MinContext {
		return fmt.Errorf("config: context %d must be at least %d", c.Context, MinContext)
	}
	for tier, n := range c.ContextByTier {
		if n < MinContext {
			return fmt.Errorf("config: context_by_tier[%s] %d must be at least %d", tier, n, MinContext)
		}
	}
	if c.LoadTimeout < 1 {
		return fmt.Errorf("config: load_timeout %d must be at least 1 second", c.LoadTimeout)
	}
	for i, origin := range c.AllowedOrigins {
		if _, _, err := ParseOrigin(origin); err != nil {
			return fmt.Errorf("config: allowed_origins[%d] %w", i, err)
		}
	}
	return nil
}

// ContextFor is the context window a model of the given tier runs at: the tier's own value when set,
// else the default, and never more than the model's native maximum when that is known (native > 0).
func (c Config) ContextFor(tier string, native int) int {
	n := c.Context
	if v, ok := c.ContextByTier[tier]; ok {
		n = v
	}
	if native > 0 && native < n {
		return native
	}
	return n
}

// ListenBeyondLocalhost reports whether the listen address accepts connections from other machines.
// An address that does not parse is treated as local (Validate refuses it anyway).
func (c Config) ListenBeyondLocalhost() bool {
	host, _, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return false
	}
	if host == "" {
		return true
	}
	if host == "localhost" {
		return false
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip == nil || !ip.IsLoopback()
}
