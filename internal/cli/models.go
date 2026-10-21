package cli

import (
	"errors"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/tara-vision/taraengine/pkg/api"
)

func newListCommand(flags *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List the catalog and the pulled models",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			f, err := flags.connect(cmd)
			if err != nil {
				return err
			}
			models, err := f.client().Models(cmd.Context())
			if err != nil {
				return f.explain(err)
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			p := &printer{w: tw}
			p.printf("NAME\tSTATUS\tSIZE\tTIER\tORIGIN\tCONTEXT\tCAPABILITIES\n")
			for _, m := range models {
				p.printf("%s\t%s\t%s\t%s\t%s\t%d\t%s\n",
					m.Name, m.Status, size(m.Size), m.Tier, m.Origin, m.Context, capabilities(m))
			}
			return errors.Join(p.err, tw.Flush())
		},
	}
}

// capabilities lists what a model can do, marked when the engine guessed it from the file.
func capabilities(m api.Model) string {
	caps := strings.Join(m.Capabilities, ",")
	if m.CapabilitiesInferred {
		caps = strings.TrimSpace(caps + " (inferred)")
	}
	return caps
}

func newPsCommand(flags *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "ps",
		Short: "Show the loaded models and their memory",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			f, err := flags.connect(cmd)
			if err != nil {
				return err
			}
			loaded, err := f.client().Loaded(cmd.Context())
			if err != nil {
				return f.explain(err)
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			p := &printer{w: tw}
			p.printf("NAME\tSTATUS\tCONTEXT\tMEMORY\n")
			for _, m := range loaded {
				p.printf("%s\t%s\t%d\t%s\n", m.Name, orDash(m.Status), m.Context, memory(m.Memory))
			}
			return errors.Join(p.err, tw.Flush())
		},
	}
}

// memory is what a loaded model holds and how that was found out, or a dash when there is no figure.
func memory(m *api.Memory) string {
	if m == nil {
		return "-"
	}
	return gib(m.Bytes) + " " + m.Source
}

// orDash is s, or a dash for a value the daemon did not send.
func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func newRmCommand(flags *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "rm <model>",
		Short: "Remove a pulled model and its siblings from disk",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			f, err := flags.connect(cmd)
			if err != nil {
				return err
			}
			if err := f.client().Delete(cmd.Context(), args[0]); err != nil {
				return f.explain(err)
			}
			p := &printer{w: cmd.OutOrStdout()}
			p.printf("removed %s\n", args[0])
			return p.err
		},
	}
}
