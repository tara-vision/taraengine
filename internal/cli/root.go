// Package cli is the taraengine command line: serve, pull, list, ps, load, unload, rm, doctor, runtime, version.
package cli

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
)

// globalFlags are shared by the commands that talk to a running daemon.
type globalFlags struct {
	host   string
	apiKey string
	config string
}

// NewRootCommand builds the command tree. Tests build a fresh tree per case, so nothing here is global.
func NewRootCommand() *cobra.Command {
	flags := &globalFlags{}
	root := &cobra.Command{
		Use:           "taraengine",
		Short:         "The Tara Vision inference engine: a local OpenAI-compatible server built on llama.cpp",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVar(&flags.host, "host", "",
		"address of the running daemon (default: the listen address of the config, http://127.0.0.1:8272)")
	root.PersistentFlags().StringVar(&flags.apiKey, "api-key", "",
		"API key of the daemon (default: TARAENGINE_API_KEY, then api_key of the config)")
	root.PersistentFlags().StringVar(&flags.config, "config", "", "config file (default: the user config directory)")
	root.AddCommand(newVersionCommand(), newServeCommand(flags), newPullCommand(flags), newListCommand(flags),
		newPsCommand(flags), newLoadCommand(flags), newUnloadCommand(flags), newRmCommand(flags),
		newDoctorCommand(flags), newRuntimeCommand(flags))
	return root
}

// notifyContext is signal.NotifyContext. It is a variable only so that a test can watch what is done with the
// signals.
var notifyContext = signal.NotifyContext

// Execute runs the command tree against os.Args and returns the error for main to turn into an exit code. SIGINT and
// SIGTERM cancel the context of the command, so that a pull which is interrupted says so and the process exits with 1,
// and the first of them is the only one that is caught: a command that does not stop at once ends with the second.
// serve takes its context from this one and watches for the same signals itself, with SIGHUP added. A signal reaches
// both, and they end the same shutdown, so the two do not compete.
func Execute() error {
	ctx, stop := untilSignal(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return NewRootCommand().ExecuteContext(ctx)
}

// untilSignal is a context that ends when one of the signals arrives. It gives the signals back to the system at that
// moment, so that a second signal, from someone whose command did not stop at the first, ends the process in the
// usual way. The stop function does the same on any other day.
func untilSignal(parent context.Context, sigs ...os.Signal) (context.Context, context.CancelFunc) {
	ctx, stop := notifyContext(parent, sigs...)
	context.AfterFunc(ctx, stop)
	return ctx, stop
}
