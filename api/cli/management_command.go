package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/stefanmcshane/borq/business/managementbootstrap"
	"github.com/stefanmcshane/borq/foundations/logger"
)

func newManagementCommand(ctx context.Context, log logger.Logger) *cobra.Command {
	command := &cobra.Command{
		Use:   "management",
		Short: "Operate the borq management cluster.",
	}

	command.AddCommand(newManagementBootstrapCommand(ctx, log))
	command.AddCommand(newManagementServerCommand(log))

	return command
}

func newManagementBootstrapCommand(ctx context.Context, log logger.Logger) *cobra.Command {
	request := managementbootstrap.DefaultRequest()

	command := &cobra.Command{
		Use:   "bootstrap",
		Short: "Bootstrap the local borq management cluster.",
		RunE: func(cmd *cobra.Command, args []string) error {
			executionContext := cmd.Context()
			if executionContext == nil {
				executionContext = ctx
			}

			result, err := managementbootstrap.Execute(executionContext, log, request)
			if err != nil {
				return err
			}

			_, err = fmt.Fprintf(
				cmd.OutOrStdout(),
				"management cluster %q is reachable at %s\n",
				result.ClusterName,
				result.ProbeURL,
			)
			return err
		},
	}

	command.Flags().StringVar(&request.ClusterName, "cluster-name", request.ClusterName, "Management kind cluster name.")
	command.Flags().DurationVar(&request.Timeout, "timeout", request.Timeout, "Overall bootstrap timeout.")
	command.Flags().StringVar(&request.Namespace, "namespace", request.Namespace, "Namespace for borq management bootstrap resources.")

	command.Flags().Lookup("timeout").DefValue = (10 * time.Minute).String()

	return command
}
