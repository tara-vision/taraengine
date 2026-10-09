package cli

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/spf13/cobra"

	"github.com/tara-vision/taraengine/pkg/client"
)

func newLoadCommand(flags *globalFlags) *cobra.Command {
	var window int
	var force bool
	cmd := &cobra.Command{
		Use:   "load <model>",
		Short: "Load a model, optionally at a context window",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			f, err := flags.resolved()
			if err != nil {
				return err
			}
			m, err := f.client().Load(cmd.Context(), args[0], window, force)
			if err != nil {
				return f.loadFailure(args[0], err)
			}
			suffix := ""
			if m.Memory != nil {
				suffix = ", " + memory(m.Memory)
			}
			p := &printer{w: cmd.OutOrStdout()}
			p.printf("loaded %s at %d tokens of context%s\n", m.Name, m.Context, suffix)
			return p.err
		},
	}
	cmd.Flags().IntVar(&window, "context", 0, "context window in tokens (default: the preset's)")
	cmd.Flags().BoolVar(&force, "force", false, "load a model marked faulty")
	return cmd
}

// loadFailure explains a refused load. On a load a 409 can only mean that the model is marked faulty, and the way
// out is this command's own flag, so the advice is written here and not taken from the server's wording.
func (f *globalFlags) loadFailure(name string, err error) error {
	var refused *client.Error
	if errors.As(err, &refused) && refused.Status == http.StatusConflict {
		return fmt.Errorf("%s is marked faulty after two failed loads: `taraengine doctor` shows why, "+
			"and `taraengine load %s --force` tries again", name, name)
	}
	return f.explain(err)
}

func newUnloadCommand(flags *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "unload <model>",
		Short: "Unload a model",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			f, err := flags.resolved()
			if err != nil {
				return err
			}
			if err := f.client().Unload(cmd.Context(), args[0]); err != nil {
				return f.explain(err)
			}
			p := &printer{w: cmd.OutOrStdout()}
			p.printf("unloaded %s\n", args[0])
			return p.err
		},
	}
}
