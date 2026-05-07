package cli

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/stefanmcshane/borq/api/config"
	"github.com/stefanmcshane/borq/foundations/logger"
)

// NewRootCommand builds the borq CLI root command.
func NewRootCommand(ctx context.Context, log logger.Logger, cfg config.Config) *cobra.Command {
	command := &cobra.Command{
		Use:           "borq",
		Short:         "Borq manages opinionated Kubernetes-backed deployment clouds.",
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	command.AddCommand(newManagementCommand(ctx, log, cfg))

	command.SetOut(os.Stdout)
	command.SetErr(os.Stderr)
	command.SetContext(ctx)
	command.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		return fmt.Errorf("%s: %w", cmd.CommandPath(), err)
	})

	return command
}
