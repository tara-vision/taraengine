package llamaserver

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/tara-vision/taraengine/internal/backend"
	"github.com/tara-vision/taraengine/internal/measure"
	"github.com/tara-vision/taraengine/internal/state"
)

// Options wire the backend.
type Options struct {
	RuntimeDir       string // holds llama-server and its libraries; required unless Attach is set
	DataDir          string // presets.ini, logs/, state.json, child.key
	Build            string // the runtime build, for Health
	Version          string // the taraengine version, stamped on proxied answers
	Port             int    // the child's port; 0 picks a free one
	APIKey           string // the child's key; generated when empty
	ModelsMax        int
	SleepIdleSeconds int // passed to the router as it is: -1 never sleeps; 0 and below -1 are refused
	LoadTimeout      time.Duration
	Poll             time.Duration
	Global           map[string]string
	// Models lists the pulled models, registry entries first: of two names that share a router id, only
	// the one listed first is served.
	Models    func(ctx context.Context, overrides map[string]int) ([]PresetModel, error)
	State     state.Store // required: it must name a file
	Probe     measure.Probe
	Logger    *slog.Logger
	Attach    string // URL of a router already running; nothing is spawned
	AttachPID int    // the process to measure in attach mode
	// OnProps, when set, hears the props of every successful props read, under the catalog name, after the backend
	// has let go of its lock; the serve wiring saves an import's capabilities with it.
	OnProps func(name string, props backend.Props)
}

// PresetPath is the preset file inside a data directory.
func PresetPath(dataDir string) string { return filepath.Join(dataDir, "presets.ini") }

// LogPath is the child's log inside a data directory.
func LogPath(dataDir string) string { return filepath.Join(dataDir, "logs", "llama-server.log") }

// KeyPath is the file that carries the child's API key (mode 0600), so the key never shows on the
// child's command line.
func KeyPath(dataDir string) string { return filepath.Join(dataDir, "child.key") }

// PIDPath is the file that holds the pid of the running child (mode 0600), so a daemon that starts after
// one that died without stopping its child can end that child first.
func PIDPath(dataDir string) string { return filepath.Join(dataDir, "child.pid") }

// LockPath is the file whose lock the daemon holds while it runs, so no second daemon uses the data
// directory at the same time. It carries the pid of the daemon that holds it.
func LockPath(dataDir string) string { return filepath.Join(dataDir, "lock") }

// CachePath is the empty directory the child gets as its download cache, so the router never finds a
// model the catalog does not have.
func CachePath(dataDir string) string { return filepath.Join(dataDir, "llama-cache") }

func (o Options) validate() error {
	switch {
	case o.DataDir == "":
		return errors.New("llamaserver: data dir is required")
	case o.Models == nil:
		return errors.New("llamaserver: a models function is required")
	case o.State.Path == "":
		return errors.New("llamaserver: a state path is required")
	case o.Attach == "" && o.RuntimeDir == "":
		return errors.New("llamaserver: a runtime dir is required unless a router is attached")
	case o.SleepIdleSeconds == 0 || o.SleepIdleSeconds < -1: // llama-server exits on either
		return fmt.Errorf("llamaserver: sleep idle seconds %d must be -1 (never sleep) or at least 1", o.SleepIdleSeconds)
	}
	return nil
}

func (o Options) withDefaults() (Options, error) {
	if o.ModelsMax == 0 {
		o.ModelsMax = 1
	}
	if o.LoadTimeout == 0 {
		o.LoadTimeout = 10 * time.Minute
	}
	if o.Poll == 0 {
		o.Poll = 500 * time.Millisecond
	}
	if o.Global == nil {
		o.Global = DefaultGlobal()
	} else {
		o.Global = maps.Clone(o.Global)
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.APIKey == "" {
		o.APIKey = rand.Text()
	}
	if o.Attach == "" && o.Port == 0 {
		port, err := reservePort()
		if err != nil {
			return o, err
		}
		o.Port = port
	}
	return o, nil
}

// routerURL is where the router listens: the attached one, or the child on its port.
func (o Options) routerURL() (*url.URL, error) {
	raw := o.Attach
	if raw == "" {
		raw = fmt.Sprintf("http://127.0.0.1:%d", o.Port)
	}
	u, err := url.Parse(strings.TrimRight(raw, "/"))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("llamaserver: router URL %q must look like http://host:port", raw)
	}
	return u, nil
}

// listenTCP is net.Listen; a variable only so the tests can reach the failure of reservePort.
var listenTCP = net.Listen

// reservePort asks the kernel for a free loopback port. The port is free when this returns, not after.
func reservePort() (int, error) {
	l, err := listenTCP("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("llamaserver: %w", err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port, nil
}
