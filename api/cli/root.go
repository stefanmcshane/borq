package cli

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/stefanmcshane/borq/foundations/logger"
)

// NewRootCommand builds the borq CLI root command.
func NewRootCommand(ctx context.Context, log logger.Logger, cfg Config) *cobra.Command {
	command := &cobra.Command{
		Use:           "borq",
		Short:         "Borq manages opinionated Kubernetes-backed deployment clouds.",
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(command *cobra.Command, arguments []string) error {
			return validateCommandProfile(command)
		},
	}
	command.PersistentFlags().String("profile", "", "Named local API, organization, space, and cluster preset; an empty value uses base defaults.")

	command.AddCommand(newManagementCommand(ctx, log, cfg))
	command.AddCommand(newApplicationServerCommand(cfg))
	command.AddCommand(newApplicationCommands()...)
	command.AddCommand(newEnvironmentGroupCommand())
	command.AddCommand(newAddonCommand())
	managed := newManagedAddonCommand()
	managed.Use = "am"
	custom := newCustomChartCommand()
	custom.Use = "ac"
	command.AddCommand(managed, custom)
	command.AddCommand(newClientConfigCommand())

	command.SetOut(os.Stdout)
	command.SetErr(os.Stderr)
	command.SetContext(ctx)
	command.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		return fmt.Errorf("%s: %w", cmd.CommandPath(), err)
	})

	return command
}
