package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	goruntime "runtime"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/tara-vision/taraengine/internal/api"
	"github.com/tara-vision/taraengine/internal/backend/llamaserver"
	"github.com/tara-vision/taraengine/internal/config"
	"github.com/tara-vision/taraengine/internal/logging"
	"github.com/tara-vision/taraengine/internal/runtime"
	"github.com/tara-vision/taraengine/internal/version"
)

const (
	// shutdownGrace is how long requests in flight may run on after SIGINT or SIGTERM before they are cut off.
	shutdownGrace = 5 * time.Second
	// readHeaderTimeout bounds how long a client may take to send the headers of a request.
	readHeaderTimeout = 10 * time.Second
	// lsofTimeout bounds the search for the process that holds a port.
	lsofTimeout = 5 * time.Second
)

// listenTCP opens the listen address. It is a variable only so that a test can bind a loopback port in place of an
// address that would open the machine to the network.
var listenTCP = func(address string) (net.Listener, error) { return net.Listen("tcp", address) }

// serveFlags are the flags of serve that lay over the configuration, the two that attach it to a router, and the one
// that says a service manager runs it.
type serveFlags struct {
	listen, dataDir, runtimeDir, attach, attachKey string
	service                                        bool
}

// apply returns cfg with the flags that were given laid over it. apiKey is the global --api-key: for serve it is the
// key the daemon requires, and it wins over TARAENGINE_API_KEY, which config.Load has already laid over the file.
func (sf serveFlags) apply(cfg config.Config, apiKey string) config.Config {
	if sf.listen != "" {
		cfg.Listen = sf.listen
	}
	if sf.dataDir != "" {
		cfg.DataDir = sf.dataDir
	}
	if sf.runtimeDir != "" {
		cfg.RuntimeDir = sf.runtimeDir
	}
	if apiKey != "" {
		cfg.APIKey = apiKey
	}
	return cfg
}

func newServeCommand(flags *globalFlags) *cobra.Command {
	var sf serveFlags
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the engine: supervise llama-server and serve the API on the listen address",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig(flags)
			if err != nil {
				return err
			}
			cfg = sf.apply(cfg, flags.apiKey)
			if err := cfg.Validate(); err != nil {
				return err
			}
			return serve(cmd.Context(), cfg, WireOptions{Attach: sf.attach, AttachKey: sf.attachKey, Service: sf.service})
		},
	}
	cmd.Flags().StringVar(&sf.listen, "listen", "", "address to listen on (default: the config's, 127.0.0.1:8272)")
	cmd.Flags().StringVar(&sf.dataDir, "data-dir", "", "data directory (default: the config's)")
	cmd.Flags().StringVar(&sf.runtimeDir, "runtime-dir", "",
		"runtime directory (default: the pinned build under the data directory)")
	cmd.Flags().BoolVar(&sf.service, "service", false,
		"run under a service manager: keep stderr to warnings and errors, the full log stays in the data directory")
	cmd.Flags().StringVar(&sf.attach, "attach", "",
		"attach to a llama-server already running at this URL instead of starting one")
	cmd.Flags().StringVar(&sf.attachKey, "attach-key", "", "API key of the attached server")
	_ = cmd.Flags().MarkHidden("attach")
	_ = cmd.Flags().MarkHidden("attach-key")
	return cmd
}

// shutdownSignals are the signals that stop the daemon. A hangup counts as a termination, because a terminal that
// closes must not leave the child behind, unless the process started with the hangup ignored, as it does under
// nohup: then the person who started it asked for it to outlive the terminal. Registering for an ignored hangup
// would take that away.
func shutdownSignals() []os.Signal {
	sigs := []os.Signal{syscall.SIGINT, syscall.SIGTERM}
	if !signal.Ignored(syscall.SIGHUP) {
		sigs = append(sigs, syscall.SIGHUP)
	}
	return sigs
}

// wireDaemon is Wire. It is a variable only so that a test can hold a daemon in its starting state.
var wireDaemon = Wire

// serve runs the daemon until the parent context ends or a shutdown signal arrives. It binds the listen address first,
// so that a daemon that cannot listen fails before it touches the data directory, apart from the log directory. It
// then answers at once: the health route says that it is starting and what it is doing, and every route that needs
// the backend is refused with a Retry-After, until the daemon is wired and the full handler takes over. A stop that
// arrives while it starts ends it without an error.
func serve(parent context.Context, cfg config.Config, opts WireOptions) error {
	logger, closeLog, err := logging.Setup(cfg.DataDir, stderrLevel(opts.Service))
	if err != nil {
		return err
	}
	defer func() { _ = closeLog() }()
	ctx, stop := notifyContext(parent, shutdownSignals()...)
	defer stop()
	ln, err := listenTCP(cfg.Listen)
	if err != nil {
		return listenError(ctx, cfg.Listen, err)
	}
	defer func() { _ = ln.Close() }()
	opts.Logger = logger
	steps := &progress{}
	steps.set(stepChecking)
	handler := newFront(startingHandler(ctx, cfg, steps, logger))
	srv, failed := startHTTP(ln, handler, cfg.DataDir, logger)
	wired, err := start(ctx, cfg, opts, steps)
	if err != nil {
		shutdownServer(srv, shutdownGrace, logger)
		if ctx.Err() != nil {
			return nil // asked to stop while starting
		}
		return err
	}
	handler.set(wired.Handler)
	return await(ctx, srv, failed, wired.Backend, logger, shutdownGrace)
}

// stderrLevel is the level from which the daemon's lines reach stderr: info when run by hand, warn when a service
// manager runs it and keeps what it prints.
func stderrLevel(service bool) slog.Level {
	if service {
		return slog.LevelWarn
	}
	return slog.LevelInfo
}

// startingHandler is what the daemon answers with before it is wired. Its version route names the build this
// taraengine pins; the full handler names the one it runs, which differs only for a configured runtime_dir.
func startingHandler(ctx context.Context, cfg config.Config, steps *progress, logger *slog.Logger) http.Handler {
	build := ""
	if lock, err := pinnedLock(); err == nil {
		build = lock.Build
	}
	return api.NewStarting(api.StartingDeps{
		Config: cfg, Version: version.Version, Build: build, Detail: steps.detail, Logger: logger,
		Platform: platformKey(goruntime.GOOS, goruntime.GOARCH, runtime.DriverVersion(ctx, runtime.ExecRunner)),
	})
}

// start takes the data directory's lock and wires the daemon on it, saying at each step where it is. Once the backend
// adopted the lock, its Stop lets go of it; a lock nobody adopted is let go of here.
func start(ctx context.Context, cfg config.Config, opts WireOptions, steps *progress) (*Wired, error) {
	lock, err := llamaserver.LockDataDir(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	steps.set(stepStarting)
	opts.HeldLock = lock
	wired, err := wireDaemon(ctx, cfg, opts)
	if err != nil {
		_ = lock.Close() // a backend whose start failed closed it already, which makes this a no-op
		return nil, err
	}
	return wired, nil
}

// startHTTP serves h on ln in the background and logs where. failed carries the error of a server that stops by
// itself.
func startHTTP(ln net.Listener, h http.Handler, dataDir string, logger *slog.Logger) (*http.Server, <-chan error) {
	srv := &http.Server{Handler: h, ReadHeaderTimeout: readHeaderTimeout}
	failed := make(chan error, 1)
	go func() { failed <- srv.Serve(ln) }()
	logger.Info("taraengine listening", "listen", ln.Addr().String(), "data_dir", dataDir)
	return srv, failed
}

// await serves until ctx ends or the server stops by itself. It then shuts down in order: the HTTP server, within
// the grace period, and the backend.
func await(
	ctx context.Context, srv *http.Server, failed <-chan error, be *llamaserver.Backend, logger *slog.Logger,
	grace time.Duration,
) error {
	select {
	case err := <-failed:
		return errors.Join(fmt.Errorf("serve: %w", err), be.Stop(context.Background()))
	case <-ctx.Done():
	}
	logger.Info("taraengine stopping")
	shutdownServer(srv, grace, logger)
	return be.Stop(context.Background())
}

// shutdownServer lets the requests in flight finish for the grace period and then closes whatever is left, so that
// a stream that would run for minutes cannot hold the daemon up.
func shutdownServer(srv *http.Server, grace time.Duration, logger *slog.Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		logger.Warn("requests were still running after the grace period; closing them", "grace", grace.String(), "error", err)
		_ = srv.Close()
	}
}

// listenError explains a failed bind and names the process that holds the port when lsof can tell.
func listenError(ctx context.Context, listen string, err error) error {
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		err = opErr.Err // "bind: address already in use"; the address is named once, in front
	}
	return fmt.Errorf("listen %s: %w%s", listen, err, portHolder(ctx, listen, runLsof))
}

// lsofFunc asks lsof for the processes that listen on a TCP port and returns its -Fpc output.
type lsofFunc func(ctx context.Context, port string) ([]byte, error)

// portHolder names the process listening on the port of listen as " (held by PID 123 name)", or says nothing when
// lsof is missing, finds nobody or the address has no port.
func portHolder(ctx context.Context, listen string, lsof lsofFunc) string {
	_, port, err := net.SplitHostPort(listen)
	if err != nil {
		return ""
	}
	// lsof exits with 1 when it has warnings or finds nothing, so what it printed counts whether or not it failed.
	out, _ := lsof(ctx, port)
	pid, name := firstHolder(string(out))
	if pid == "" {
		return ""
	}
	return " (held by PID " + strings.TrimSpace(pid+" "+name) + ")"
}

// firstHolder is the process id and the command name of the first process in lsof's -Fpc output.
func firstHolder(out string) (pid, name string) {
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "p"):
			if pid != "" {
				return pid, name
			}
			pid = line[1:]
		case strings.HasPrefix(line, "c") && name == "":
			name = line[1:]
		}
	}
	return pid, name
}

// runLsof asks lsof for the processes that listen on a TCP port. It gives up after lsofTimeout, because lsof can
// hang on a file system that does not answer, and a daemon that cannot bind should not hang with it.
func runLsof(ctx context.Context, port string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, lsofTimeout)
	defer cancel()
	return exec.CommandContext(ctx, "lsof", "-nP", "-iTCP:"+port, "-sTCP:LISTEN", "-Fpc").Output()
}
