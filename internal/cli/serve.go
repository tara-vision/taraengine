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
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/tara-vision/taraengine/internal/config"
	"github.com/tara-vision/taraengine/internal/logging"
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

// serveFlags are the flags of serve that lay over the configuration, and the two that attach it to a router.
type serveFlags struct {
	listen, dataDir, runtimeDir, attach, attachKey string
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
			return serve(cmd.Context(), cfg, WireOptions{Attach: sf.attach, AttachKey: sf.attachKey})
		},
	}
	cmd.Flags().StringVar(&sf.listen, "listen", "", "address to listen on (default: the config's, 127.0.0.1:8272)")
	cmd.Flags().StringVar(&sf.dataDir, "data-dir", "", "data directory (default: the config's)")
	cmd.Flags().StringVar(&sf.runtimeDir, "runtime-dir", "",
		"runtime directory (default: the pinned build under the data directory)")
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

// serve runs the daemon until the parent context ends or a shutdown signal arrives. serve binds the listen address
// first, so that a daemon that cannot listen fails before it has started a child or written the presets, the state
// or the key: a second daemon started on an address that is in use leaves the data directory as it is, apart from
// the log directory. Nothing else keeps two daemons with different addresses off one data directory.
func serve(parent context.Context, cfg config.Config, opts WireOptions) error {
	logger, closeLog, err := logging.Setup(cfg.DataDir)
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
	wired, err := Wire(ctx, cfg, opts)
	if err != nil {
		return err
	}
	return serveOn(ctx, ln, wired, logger, shutdownGrace)
}

// serveOn serves the wired daemon on ln until ctx ends. It then shuts down in order: the HTTP server, within the
// grace period, and the backend.
func serveOn(ctx context.Context, ln net.Listener, wired *Wired, logger *slog.Logger, grace time.Duration) error {
	srv := &http.Server{Handler: wired.Handler, ReadHeaderTimeout: readHeaderTimeout}
	failed := make(chan error, 1)
	go func() { failed <- srv.Serve(ln) }()
	logger.Info("taraengine listening", "listen", ln.Addr().String(), "data_dir", wired.Config.DataDir)
	select {
	case err := <-failed:
		return errors.Join(fmt.Errorf("serve: %w", err), wired.Backend.Stop(context.Background()))
	case <-ctx.Done():
	}
	logger.Info("taraengine stopping")
	shutdownServer(srv, grace, logger)
	return wired.Backend.Stop(context.Background())
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
