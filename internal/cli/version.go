package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/tara-vision/taraengine/internal/version"
)

func newVersionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the taraengine version",
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := fmt.Fprintf(cmd.OutOrStdout(), "taraengine %s\n", version.Version)
			return err
		},
	}
}
