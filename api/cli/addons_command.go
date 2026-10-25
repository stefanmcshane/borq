package cli

import "github.com/spf13/cobra"

// Cobra requires pointers to attach mutable subcommand trees.
func newAddonCommand() *cobra.Command {
	command := &cobra.Command{Use: "addons", Short: "Manage addons within a space.", Args: cobra.NoArgs}
	command.AddCommand(newCustomChartCommand(), newManagedAddonCommand())
	return command
}
