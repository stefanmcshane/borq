package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newVersionCommand(version string) *cobra.Command {
	return &cobra.Command{
		Use: "version", Short: "Print the image tag embedded in this binary.", Args: cobra.NoArgs,
		// Version inspection must work without a readable client profile or server.
		PersistentPreRunE: func(*cobra.Command, []string) error { return nil },
		RunE: func(command *cobra.Command, _ []string) error {
			_, err := fmt.Fprintf(command.OutOrStdout(), "borq %s\n", version)
			return err
		},
	}
}
